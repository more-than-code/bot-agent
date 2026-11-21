package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// dockerScript answers container_list by pass number (1-based): up(n) says
// whether pass n succeeds.
type dockerScript struct {
	mu sync.Mutex
	n  int
	up func(int) bool
}

func (s *dockerScript) call(verb string, body []byte) ([]byte, error) {
	switch verb {
	case "list":
		return programs.EncodeList(nil), nil
	case images.VerbList:
		s.mu.Lock()
		s.n++
		ok := s.up(s.n)
		s.mu.Unlock()
		if !ok {
			return nil, errHelper
		}
		return images.Encode(images.ListResponse{Items: []images.ContainerInfo{}}), nil
	default:
		return nil, errors.New(verb)
	}
}

func (s *dockerScript) passes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func helloDockers(t *testing.T, srv *fakeserver.Server) []bool {
	t.Helper()
	var out []bool
	for _, f := range srv.Frames() {
		if f.Type != "hello" {
			continue
		}
		var h protocol.Hello
		if err := json.Unmarshal(f.Raw, &h); err != nil || h.Programs == nil {
			t.Fatalf("hello %s", f.Raw)
		}
		out = append(out, h.Programs.Docker)
	}
	return out
}

// runDockerScript runs the agent against the fake server with discovery
// every 20ms until passes container_list calls, and returns the hellos'
// docker values and the log.
func runDockerScript(t *testing.T, up func(int) bool, passes int) ([]bool, string) {
	t.Helper()
	clearAgentEnv(t)
	proc, sys, passwd := fixtureProc(t, 9)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	script := &dockerScript{up: up}
	var buf safeBuffer
	opt := Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd,
		PolicyPath: writeImageRunner(t), PolicyUID: rootUID,
		Helper:        &fakeHelper{fn: script.call},
		EnvFile:       writeCredEnv(t, srv, t.TempDir()),
		EUID:          func() int { return 1000 },
		DiscoverEvery: 20 * time.Millisecond,
		Sleep:         func(context.Context, time.Duration) error { return nil },
		Log:           log.New(&buf, "", 0),
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, opt) }()
	deadline := time.Now().Add(5 * time.Second)
	for script.passes() < passes && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stopRun(t, cancel, errCh)
	if script.passes() < passes {
		t.Fatalf("only %d passes", script.passes())
	}
	return helloDockers(t, srv), buf.String()
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// v1.17 item 3: Docker coming up and staying up for two passes reconnects
// once; the new hello says docker true.
func TestDockerChangeReconnectsOnce(t *testing.T) {
	// Pass 1 is the first hello's discovery. Docker is up from pass 3.
	hellos, log := runDockerScript(t, func(n int) bool { return n >= 3 }, 12)
	if len(hellos) != 2 || hellos[0] || !hellos[1] {
		t.Fatalf("hellos %v\n%s", hellos, log)
	}
	if n := strings.Count(log, "bot-agent: docker available changed to true; reconnecting"); n != 1 {
		t.Fatalf("logs %d\n%s", n, log)
	}
}

// One pass that disagrees with the hello is a blip and does not reconnect.
func TestDockerBlipDoesNotReconnect(t *testing.T) {
	hellos, log := runDockerScript(t, func(n int) bool { return n == 3 || n == 6 }, 10)
	if len(hellos) != 1 || hellos[0] {
		t.Fatalf("hellos %v\n%s", hellos, log)
	}
	if strings.Contains(log, "reconnecting") {
		t.Fatal(log)
	}
	// The discovery failure line is logged once per run of failures: passes
	// 1-2, 4-5 and 7-10.
	if n := strings.Count(log, "bot-agent: container discovery failed"); n != 3 {
		t.Fatalf("failure lines %d\n%s", n, log)
	}
}

// A change to false waits for dockerReconnectGap after the last such
// reconnect; a change back to true reconnects as soon as it is confirmed.
func TestDockerChangeRateLimited(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{opt: Options{Log: stdLog(&buf)}}
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	// Up -> down: two passes reconnect.
	a.sentDocker(true)
	a.noteDocker(false)
	if a.dockerChanged(t0, false) {
		t.Fatal("one pass reconnected")
	}
	a.noteDocker(false)
	if !a.dockerChanged(t0, false) {
		t.Fatal("two passes did not reconnect")
	}
	// Down -> up a minute later: no gap for a change to true.
	a.sentDocker(a.dockerUp())
	a.noteDocker(true)
	a.noteDocker(true)
	if !a.dockerChanged(t0.Add(time.Minute), false) {
		t.Fatal("change to true waited for the gap")
	}
	// Up -> down again inside the gap waits for it.
	a.sentDocker(a.dockerUp())
	a.noteDocker(false)
	a.noteDocker(false)
	if a.dockerChanged(t0.Add(dockerReconnectGap-time.Second), false) {
		t.Fatal("change to false reconnected inside the gap")
	}
	a.noteDocker(false)
	if !a.dockerChanged(t0.Add(dockerReconnectGap), false) {
		t.Fatal("held change to false did not reconnect after the gap")
	}
	if n := strings.Count(buf.String(), "reconnecting"); n != 3 {
		t.Fatalf("logs %d\n%s", n, buf.String())
	}
	// A change to true right after is not held back either.
	a.sentDocker(a.dockerUp())
	a.noteDocker(true)
	a.noteDocker(true)
	if !a.dockerChanged(t0.Add(dockerReconnectGap+time.Second), false) {
		t.Fatal("second change to true waited")
	}
	// Agreement resets the count.
	a.sentDocker(false)
	a.noteDocker(true)
	a.noteDocker(false)
	a.noteDocker(true)
	if a.dockerChanged(t0.Add(time.Hour), false) {
		t.Fatal("non-consecutive passes reconnected")
	}
}

// v1.17 item 3: no reconnect while a task is running or waiting; the
// confirmed change reconnects at the first pass after the tasks end.
func TestDockerChangeWaitsForTasks(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{opt: Options{Log: stdLog(&buf)}}
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	a.sentDocker(false)
	a.noteDocker(true)
	a.noteDocker(true)
	a.taskRunning = 1
	if a.dockerChanged(t0, a.tasksBusy()) {
		t.Fatal("reconnected with a task running")
	}
	a.taskRunning, a.taskWaiting = 0, 1
	a.noteDocker(true)
	if a.dockerChanged(t0, a.tasksBusy()) {
		t.Fatal("reconnected with a task waiting")
	}
	a.taskWaiting = 0
	a.noteDocker(true)
	if !a.dockerChanged(t0, a.tasksBusy()) {
		t.Fatal("confirmed change did not reconnect after the tasks")
	}
	if n := strings.Count(buf.String(), "reconnecting"); n != 1 {
		t.Fatalf("logs %d\n%s", n, buf.String())
	}
}

// v1.17 item 2: the helper's shared-networks refusal is outcome failed with
// a line that says why, for a service and for a job.
func TestDeploySharedNetworksRefusalLine(t *testing.T) {
	for _, spec := range [][]byte{serviceSpec(false, 5, 8080), jobSpec(30)} {
		a, h, _, seq := imageRun(t)
		seq.logs = []string{"old log"}
		seq.pre = missingStatus()
		h.fn = func(verb string, body []byte) ([]byte, error) {
			if verb == images.VerbDeploy {
				return refuse("error", programs.ReasonNetworksDocker), nil
			}
			return seq.call(verb, body)
		}
		res := mustImage(t, a, "image_deploy", spec, 60)
		if res.State != "failed" || res.ErrorCode == nil || *res.ErrorCode != "failed" {
			t.Fatalf("%+v", res)
		}
		lines, _ := res.Payload["lines"].([]any)
		if len(lines) != 2 || lines[0] != networksLine || lines[1] != "old log" {
			t.Fatalf("lines %#v calls %v", res.Payload["lines"], h.Calls())
		}
		if res.Payload["outcome"] != "failed" {
			t.Fatalf("%v", res.Payload)
		}
	}
}

// The refusal keeps at most tailLines lines: the reason line and the last
// tailLines-1 log lines, for a service and for a job.
func TestDeploySharedNetworksRefusalLineCap(t *testing.T) {
	logs := make([]string, tailLines)
	for i := range logs {
		logs[i] = fmt.Sprintf("log %d", i)
	}
	for _, spec := range [][]byte{serviceSpec(false, 5, 8080), jobSpec(30)} {
		a, h, _, seq := imageRun(t)
		seq.logs = logs
		seq.pre = missingStatus()
		h.fn = func(verb string, body []byte) ([]byte, error) {
			if verb == images.VerbDeploy {
				return refuse("error", programs.ReasonNetworksDocker), nil
			}
			return seq.call(verb, body)
		}
		res := mustImage(t, a, "image_deploy", spec, 60)
		lines, _ := res.Payload["lines"].([]any)
		if len(lines) != tailLines || lines[0] != networksLine || lines[1] != "log 1" || lines[tailLines-1] != fmt.Sprintf("log %d", tailLines-1) {
			t.Fatalf("lines %d %#v", len(lines), lines)
		}
	}
}

// v1.17 item 2: a job with shared networks and an existing container is
// not removed by the agent before container_deploy, so a refusal on an
// old Docker leaves the previous container.
func TestJobNetworksRefusalKeepsPrevious(t *testing.T) {
	a, h, _, seq := imageRun(t)
	openDD(a)
	seq.pre = exitedStatus(0)
	h.fn = func(verb string, body []byte) ([]byte, error) {
		if verb == images.VerbDeploy {
			return refuse("error", programs.ReasonNetworksDocker), nil
		}
		return seq.call(verb, body)
	}
	spec := []byte(strings.Replace(string(jobSpec(30)), `"timeout_sec"`, `"networks":["dd"],"timeout_sec"`, 1))
	res := mustImage(t, a, "image_deploy", spec, 60)
	if res.State != "failed" {
		t.Fatalf("%+v", res)
	}
	for _, c := range h.Calls() {
		if c == images.VerbRemove {
			t.Fatalf("removed the previous job container: %v", h.Calls())
		}
	}
	// Without networks the agent still removes it first.
	a, h, _, seq = imageRun(t)
	seq.pre = exitedStatus(0)
	seq.post = exitedStatus(0)
	mustImage(t, a, "image_deploy", jobSpec(30), 60)
	removed := false
	for _, c := range h.Calls() {
		removed = removed || c == images.VerbRemove
	}
	if !removed {
		t.Fatalf("calls %v", h.Calls())
	}
}

// The agent never sends docker_check: it is the installer's verb.
func TestAgentNeverSendsDockerCheck(t *testing.T) {
	h := socketHelper{path: "/nonexistent", dial: nil}
	if _, err := h.Call(context.Background(), programs.VerbDockerCheck, []byte(`{}`)); !errors.Is(err, errHelper) {
		t.Fatal(err)
	}
}
