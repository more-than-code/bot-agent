package run

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/results"
)

// agent-protocol v1.21 item 5 end to end: a job deploy started on one
// connection finishes after the server drops it, its result arrives on the
// next connection with late: true, and task_result_ack deletes the file.
func TestDetachedDeployE2E(t *testing.T) {
	clearAgentEnv(t)
	proc, sys, passwd := fixtureProc(t, 9)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	seq := &imageSeq{
		pre:  images.ImageStatus{State: "missing", Ports: []int{}},
		post: exitedStatus(0),
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	helper := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		if verb == images.VerbDeploy {
			once.Do(func() { close(entered) })
			<-release
		}
		return seq.call(verb, body)
	}}
	state := t.TempDir()
	var buf safeBuffer
	opt := Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd,
		PolicyPath: writeImageRunner(t), PolicyUID: rootUID,
		Helper:  helper,
		EnvFile: writeCredEnv(t, srv, state),
		EUID:    func() int { return 1000 },
		Sleep:   func(context.Context, time.Duration) error { return nil },
		Log:     log.New(&buf, "", 0),
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, opt) }()
	defer stopRun(t, cancel, errCh)
	if !srv.WaitType("hello", 1, 5*time.Second) {
		t.Fatalf("no hello\n%s", buf.String())
	}
	task := imageTask("image_deploy", jobSpec(60), 120)
	task.HostID = srv.HostID
	// SendTask can race the end of the hello exchange; retry briefly.
	deadline := time.Now().Add(5 * time.Second)
	for srv.SendTask(task) != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("deploy not reached\n%s", buf.String())
	}
	srv.CloseConn()
	if !srv.WaitType("hello", 2, 5*time.Second) {
		t.Fatalf("no second hello\n%s", buf.String())
	}
	srv.SetAckResults(true)
	close(release)
	if !srv.Wait(5*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "task_result") >= 1 }) {
		t.Fatalf("no result\n%s", buf.String())
	}
	var res protocol.TaskResult
	for _, f := range srv.Frames() {
		if f.Type == "task_result" {
			if err := json.Unmarshal(f.Raw, &res); err != nil {
				t.Fatal(err)
			}
		}
	}
	if res.TaskID != "task-1" || res.State != "succeeded" || !res.Late {
		t.Fatalf("%+v", res)
	}
	path := filepath.Join(state, results.DirName, "task-1.json")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stored result kept after task_result_ack: %v", err)
	}
	if v := srv.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}
