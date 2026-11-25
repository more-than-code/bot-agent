package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/results"
)

// agent-protocol v1.21: detached deploys with stored results (item 5), the
// hello flag (item 2) and the container_deploy refusal details (item 4).

var errCaptured = errors.New("captured")

// captureWriter is a writer whose frames are recorded and never written.
type captureWriter struct {
	mu     sync.Mutex
	frames []protocol.TaskResult
}

func (c *captureWriter) writer() *writer {
	return &writer{onSend: func(v any) error {
		if res, ok := v.(protocol.TaskResult); ok {
			c.mu.Lock()
			c.frames = append(c.frames, res)
			c.mu.Unlock()
		}
		return errCaptured
	}}
}

func (c *captureWriter) results() []protocol.TaskResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.TaskResult(nil), c.frames...)
}

func (c *captureWriter) wait(t *testing.T, n int) []protocol.TaskResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.results(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited for %d results, have %d", n, len(c.results()))
	return nil
}

// detachedAgent is imageRun with Run's pieces a session test needs: a life
// context, task slots and a results store. The deploy blocks until release
// is closed; entered is closed when it is reached.
func detachedAgent(t *testing.T) (a *agent, dir string, entered, release chan struct{}) {
	t.Helper()
	a, h, _, seq := imageRun(t)
	seq.pre = runningStatus(0)
	seq.post = runningStatus(0)
	entered = make(chan struct{})
	release = make(chan struct{})
	var once sync.Once
	inner := seq.call
	h.fn = func(verb string, body []byte) ([]byte, error) {
		if verb == images.VerbDeploy {
			once.Do(func() { close(entered) })
			<-release
		}
		return inner(verb, body)
	}
	dir = t.TempDir()
	rs, err := results.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.results = rs
	a.life = context.Background()
	a.taskSlots = make(chan struct{}, maxRunningTasks)
	return a, dir, entered, release
}

func taskFrame(t *testing.T, task protocol.Task) []byte {
	t.Helper()
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The deploy keeps running after its session ends, and its result goes out
// on the next connection with late: true. It is on disk, 0600, before that.
func TestDetachedDeployOutlivesSession(t *testing.T) {
	a, dir, entered, release := detachedAgent(t)
	first := &captureWriter{}
	w1 := first.writer()
	sess1, cancel1 := context.WithCancel(context.Background())
	a.beginConn(w1)
	a.handleTask(sess1, w1, taskFrame(t, imageTask("image_deploy", serviceSpec(false, 5, 3031), 60)))
	<-entered
	// The API connection drops in the middle of the deploy.
	cancel1()
	a.endConn(w1)
	second := &captureWriter{}
	w2 := second.writer()
	a.beginConn(w2)
	close(release)
	got := second.wait(t, 1)
	res := got[0]
	assertDeploy(t, res, "succeeded", "succeeded")
	if !res.Late {
		t.Fatal("late not set")
	}
	if len(first.results()) != 0 {
		t.Fatal("result sent on the dropped connection")
	}
	path := filepath.Join(dir, results.DirName, "task-1.json")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("stored file %v %v", fi, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "late") || !strings.Contains(string(b), `"task_id":"task-1"`) {
		t.Fatalf("stored body %s", b)
	}
	if err := protocol.ValidateAgent(mustMarshal(t, res)); err != nil {
		t.Fatalf("frame: %v", err)
	}

	// After each hello the stored result goes again, still late.
	third := &captureWriter{}
	w3 := third.writer()
	a.beginConn(w3)
	if err := a.replayStored(context.Background(), w3); !errors.Is(err, errCaptured) {
		t.Fatalf("replay %v", err)
	}
	if got := third.results(); len(got) != 1 || !got[0].Late || got[0].TaskID != "task-1" {
		t.Fatalf("replay %+v", got)
	}
	// A repeated task frame gets the stored result, not a second run.
	a.handleTask(context.Background(), w3, taskFrame(t, imageTask("image_deploy", serviceSpec(false, 5, 3031), 60)))
	if got := third.results(); len(got) != 2 || got[1].TaskID != "task-1" {
		t.Fatalf("repeat %+v", got)
	}

	// task_result_ack deletes the file; nothing is replayed after that.
	a.onResultAck([]byte(`{"v":1,"type":"task_result_ack","task_id":"task-1"}`))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file kept after ack: %v", err)
	}
	fourth := &captureWriter{}
	if err := a.replayStored(context.Background(), fourth.writer()); err != nil {
		t.Fatal(err)
	}
	if len(fourth.results()) != 0 {
		t.Fatal("replayed after ack")
	}
	// The old task_ack does not delete a stored result (an older API).
	if a.results.Put(res) != nil {
		t.Fatal("put")
	}
	a.pending.ack("task-1")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("task_ack removed the stored file")
	}
}

// A deploy that ends on the connection it started on is sent without late.
func TestDetachedDeploySameConnection(t *testing.T) {
	a, _, _, release := detachedAgent(t)
	close(release)
	cw := &captureWriter{}
	w := cw.writer()
	a.beginConn(w)
	a.handleTask(context.Background(), w, taskFrame(t, imageTask("image_deploy", serviceSpec(false, 5, 3031), 60)))
	got := cw.wait(t, 1)
	if got[0].Late || got[0].State != "succeeded" {
		t.Fatalf("%+v", got[0])
	}
	if _, ok := a.results.Get("task-1"); !ok {
		t.Fatal("not stored")
	}
	// A result read back after a restart has no start record: late.
	a.taskConn = nil
	if res, ok := a.storedResult("task-1"); !ok || !res.Late {
		t.Fatalf("%+v %v", res, ok)
	}
}

// Without a results store the result is kept in memory, as before.
func TestDetachedWithoutStore(t *testing.T) {
	a, _, _, release := detachedAgent(t)
	a.results = nil
	close(release)
	cw := &captureWriter{}
	w := cw.writer()
	a.beginConn(w)
	a.handleTask(context.Background(), w, taskFrame(t, imageTask("image_deploy", serviceSpec(false, 5, 3031), 60)))
	cw.wait(t, 1)
	if _, ok := a.pending.get("task-1"); !ok {
		t.Fatal("not kept in memory")
	}
}

// Other kinds still run on the session context.
func TestTaskBase(t *testing.T) {
	life := context.WithValue(context.Background(), struct{}{}, 1)
	sess := context.Background()
	a := &agent{life: life}
	for kind, want := range map[string]context.Context{
		"image_deploy": life, "image_rollback": life, "image_status": sess, "image_logs": sess, "program_start": sess,
	} {
		if a.taskBase(sess, kind) != want {
			t.Fatal(kind)
		}
	}
	if (&agent{}).taskBase(sess, "image_deploy") != sess {
		t.Fatal("nil life")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := protocol.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHelloHostNetwork(t *testing.T) {
	for _, tc := range []struct {
		pol  policy.Policy
		want bool
	}{
		{policy.Policy{Exists: true, Valid: true, HostNetworkProjects: []string{"deploybot"}}, true},
		{policy.Policy{Exists: true, Valid: true, HostNetworkProjects: []string{}}, false},
		{policy.Policy{Exists: true, Valid: false, HostNetworkProjects: []string{"deploybot"}}, false},
	} {
		a := &agent{policy: tc.pol}
		p, _ := a.helloPrograms()
		if p == nil || p.HostNetwork != tc.want {
			t.Fatalf("%+v: %+v", tc.pol, p)
		}
		// v1.21 item 7: sent only when true.
		b, _ := json.Marshal(p)
		if strings.Contains(string(b), `"host_network"`) != tc.want {
			t.Fatalf("%s", b)
		}
	}
	if p, _ := (&agent{}).helloPrograms(); p != nil {
		t.Fatal("programs without a policy")
	}
}

// v1.21 item 7: a detached task cut off by the agent stopping stores and
// sends nothing.
func TestDetachedStoppingAgentNoResult(t *testing.T) {
	a, dir, entered, release := detachedAgent(t)
	life, stop := context.WithCancel(context.Background())
	a.life = life
	cw := &captureWriter{}
	w := cw.writer()
	a.beginConn(w)
	a.handleTask(context.Background(), w, taskFrame(t, imageTask("image_deploy", serviceSpec(false, 5, 3031), 60)))
	<-entered
	stop()
	close(release)
	waitDone(t, a, "task-1")
	if len(cw.results()) != 0 {
		t.Fatalf("sent %+v", cw.results())
	}
	if _, err := os.Stat(filepath.Join(dir, results.DirName, "task-1.json")); !os.IsNotExist(err) {
		t.Fatalf("stored: %v", err)
	}
	if _, ok := a.pending.get("task-1"); ok {
		t.Fatal("kept in memory")
	}
	if taskConnLen(a) != 0 {
		t.Fatal("start record kept")
	}
}

// rejectCapture records task_reject frames as well.
type rejectCapture struct {
	mu      sync.Mutex
	rejects []protocol.TaskReject
}

func (c *rejectCapture) writer() *writer {
	return &writer{onSend: func(v any) error {
		if r, ok := v.(protocol.TaskReject); ok {
			c.mu.Lock()
			c.rejects = append(c.rejects, r)
			c.mu.Unlock()
		}
		return errCaptured
	}}
}

// A reject of an admitted detached task goes to the connection that is
// current when it is decided (v1.21 item 7).
func TestDetachedRejectOnCurrentConnection(t *testing.T) {
	a, _, _, _ := detachedAgent(t)
	old := &rejectCapture{}
	cur := &rejectCapture{}
	a.beginConn(old.writer())
	wOld := old.writer()
	a.beginConn(cur.writer())
	task := imageTask("image_deploy", serviceSpec(false, 5, 3031), 60)
	task.HostID = "99999999-9999-4999-8999-999999999999" // execute rejects: wrong_host
	a.inflight.start(task.TaskID)
	a.noteStarted(task.TaskID, 1)
	a.taskWaiting = 1
	a.runAdmitted(context.Background(), wOld, task)
	if len(old.rejects) != 0 || len(cur.rejects) != 1 || cur.rejects[0].TaskID != "task-1" {
		t.Fatalf("old %+v cur %+v", old.rejects, cur.rejects)
	}
	if taskConnLen(a) != 0 {
		t.Fatal("start record kept")
	}
}

// v1.21 item 7: the start records stay bounded. A result kept in memory
// drops its record at once; with an older server that only sends task_ack,
// records follow the store across hellos; the map never passes its cap.
func TestTaskConnBounded(t *testing.T) {
	a, _, _, release := detachedAgent(t)
	close(release)
	cw := &captureWriter{}
	w := cw.writer()
	for i := 0; i < 5; i++ {
		a.beginConn(w)
		task := imageTask("image_deploy", serviceSpec(true, 5, 3031), 60)
		task.TaskID = fmt.Sprintf("task-%d", i)
		a.handleTask(context.Background(), w, taskFrame(t, task))
		waitDone(t, a, task.TaskID)
		// The old server answers task_ack only.
		a.pending.ack(task.TaskID)
		_ = a.replayStored(context.Background(), w)
	}
	if n := taskConnLen(a); n > len(a.results.List()) {
		t.Fatalf("%d records for %d stored", n, len(a.results.List()))
	}
	// The store drops files by its caps; the next replay drops their records.
	for _, res := range a.results.List() {
		_ = a.results.Delete(res.TaskID)
	}
	_ = a.replayStored(context.Background(), w)
	if taskConnLen(a) != 0 {
		t.Fatal("records kept")
	}
	// Without a store the record goes when the task ends.
	a.results = nil
	task := imageTask("image_deploy", serviceSpec(true, 5, 3031), 60)
	task.TaskID = "mem-1"
	a.handleTask(context.Background(), w, taskFrame(t, task))
	waitDone(t, a, "mem-1")
	got := cw.results()
	last := got[len(got)-1]
	if last.TaskID != "mem-1" || last.Late {
		t.Fatalf("%+v", last)
	}
	if taskConnLen(a) != 0 {
		t.Fatal("records kept")
	}
	// The cap.
	for i := 0; i < maxTaskConn+10; i++ {
		a.noteStarted(fmt.Sprintf("x%d", i), 1)
	}
	if n := taskConnLen(a); n != maxTaskConn {
		t.Fatalf("%d", n)
	}
}

func taskConnLen(a *agent) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.taskConn)
}

// waitDone waits until the task's goroutine has finished.
func waitDone(t *testing.T, a *agent, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.taskMu.Lock()
		idle := a.taskRunning == 0 && a.taskWaiting == 0
		a.taskMu.Unlock()
		if idle && !a.inflight.has(id) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s still running", id)
}

// container_deploy refusals with a v1.21 detail become fixed reasons that
// name the key only.
func TestDeployRefusalDetail(t *testing.T) {
	const value = "postgres://u:pw-not-to-be-seen@127.0.0.1/db"
	cases := []struct {
		body    string
		outcome string
		reason  string
	}{
		{`{"error":"bad_spec","detail":"env_conflict","key":"DATABASE_URL"}`, "forbidden_spec", "env_conflict DATABASE_URL: the host env file sets a key that the entry's env also sets or that no container env may set"},
		{`{"error":"policy_denied","detail":"host_env_file"}`, "forbidden_spec", whyHostEnvFile},
		// v1.26: the field is named only from the fixed list.
		{`{"error":"policy_denied","detail":"host_env_run","key":"command"}`, "forbidden_spec", "host_env_run command: this entry gets a host env file, so its command and healthcheck must match host_env_command and host_env_healthcheck in runner.toml, and it may not set entrypoint, user, volumes, files or its own env"},
		{`{"error":"policy_denied","detail":"host_env_run","key":"env"}`, "forbidden_spec", hostEnvRunWhy("env")},
		{`{"error":"policy_denied","detail":"host_env_run","key":"` + value + `"}`, "forbidden_spec", hostEnvRunWhy("")},
		{`{"error":"policy_denied","detail":"host_env_run","key":"DATABASE_URL"}`, "forbidden_spec", hostEnvRunWhy("")},
		// A key that is not a key name is not repeated.
		{`{"error":"bad_spec","detail":"env_conflict","key":"` + value + `"}`, "forbidden_spec", codeWhy("bad_spec")},
		{`{"error":"bad_spec","detail":"other"}`, "forbidden_spec", codeWhy("bad_spec")},
		{`{"error":"bad_spec"}`, "forbidden_spec", codeWhy("bad_spec")},
	}
	for _, tc := range cases {
		a, h, _, seq := imageRun(t)
		inner := seq.call
		h.fn = func(verb string, body []byte) ([]byte, error) {
			if verb == images.VerbDeploy {
				return []byte(tc.body), nil
			}
			return inner(verb, body)
		}
		seq.pre = runningStatus(0)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 60)
		if res.ErrorCode != nil && *res.ErrorCode != tc.outcome {
			t.Fatalf("%s: %s %v", tc.body, *res.ErrorCode, res.Payload)
		}
		m := assertDeploy(t, res, tc.outcome, "failed")
		if m["reason"] != tc.reason {
			t.Fatalf("%s: reason %q", tc.body, m["reason"])
		}
		if strings.Contains(string(mustMarshal(t, res)), "pw-not-to-be-seen") {
			t.Fatal("value leaked")
		}
	}
	// The older one-key refusal still parses.
	if r := parseRefusal([]byte(`{"error":"conflict"}`)); r.code != "conflict" || r.detail != "" {
		t.Fatalf("%+v", r)
	}
	if r := parseRefusal([]byte(`{"error":"conflict","other":1}`)); r.code != "" {
		t.Fatalf("%+v", r)
	}
}
