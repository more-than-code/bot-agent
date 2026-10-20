package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

const hcFast = `{"test":["wget","-q","http://127.0.0.1/"],"interval_sec":5,"timeout_sec":1,"retries":1,"start_period_sec":0}`

func TestDockerDeployWait(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		a, h, clock, seq := dockerRun(t, []string{"healthy"})
		res := mustImage(t, a, "image_deploy", dockerSpec(3031, hcFast, true), 4)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		if dig(m) != imgDigest || clock.sleeps() != 0 || seq.listN != 1 || seq.rollbacks != 0 {
			t.Fatalf("sleeps %d lists %d rolls %d %v", clock.sleeps(), seq.listN, seq.rollbacks, m)
		}
		if countVerb(h, images.VerbList) != 1 || countVerb(h, images.VerbRollback) != 0 {
			t.Fatal(h.Calls())
		}
	})

	t.Run("unhealthy", func(t *testing.T) {
		a, h, clock, seq := dockerRun(t, []string{"unhealthy"})
		res := mustImage(t, a, "image_deploy", dockerSpec(3031, hcFast, true), 120)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 0 || seq.listN != 1 || seq.rollbacks != 1 {
			t.Fatalf("sleeps %d lists %d rolls %d %v", clock.sleeps(), seq.listN, seq.rollbacks, h.Calls())
		}
	})

	t.Run("starting then healthy", func(t *testing.T) {
		a, _, clock, seq := dockerRun(t, []string{"starting", "starting", "healthy"})
		start := clock.now()
		res := mustImage(t, a, "image_deploy", dockerSpec(3031, hcFast, true), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if clock.sleeps() != 2 || !clock.now().Equal(start.Add(4*time.Second)) || seq.listN != 3 || seq.rollbacks != 0 {
			t.Fatalf("sleeps %d now %s lists %d", clock.sleeps(), clock.now(), seq.listN)
		}
	})

	t.Run("formula extends a short timeout", func(t *testing.T) {
		a, _, clock, seq := dockerRun(t, []string{"starting"})
		start := clock.now()
		res := mustImage(t, a, "image_deploy", dockerSpec(3031, hcFast, true), 4)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 5 || !clock.now().Equal(start.Add(10*time.Second)) || seq.listN != 5 || seq.rollbacks != 1 {
			t.Fatalf("sleeps %d now %s lists %d rolls %d", clock.sleeps(), clock.now(), seq.listN, seq.rollbacks)
		}
	})

	t.Run("task timeout outlasts the formula", func(t *testing.T) {
		a, _, clock, seq := dockerRun(t, []string{"starting"})
		start := clock.now()
		res := mustImage(t, a, "image_deploy", dockerSpec(3031, hcFast, true), 20)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 10 || !clock.now().Equal(start.Add(20*time.Second)) || seq.rollbacks != 1 {
			t.Fatalf("sleeps %d now %s rolls %d", clock.sleeps(), clock.now(), seq.rollbacks)
		}
	})

	t.Run("start period is in the limit", func(t *testing.T) {
		hc := `{"test":["wget"],"interval_sec":5,"timeout_sec":1,"retries":1,"start_period_sec":6}`
		a, _, clock, seq := dockerRun(t, []string{"starting"})
		start := clock.now()
		res := mustImage(t, a, "image_deploy", dockerSpec(3031, hc, true), 4)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 8 || !clock.now().Equal(start.Add(16*time.Second)) || seq.rollbacks != 1 {
			t.Fatalf("sleeps %d now %s", clock.sleeps(), clock.now())
		}
	})

	t.Run("healthy and checks pass", func(t *testing.T) {
		ln := listenLocal(t)
		port := ln.Addr().(*net.TCPAddr).Port
		a, _, clock, seq := dockerRun(t, []string{"healthy"})
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "web", Protocol: "tcp", Port: port, IntervalSec: 60,
		}}
		res := mustImage(t, a, "image_deploy", dockerSpec(port, hcFast, true), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if clock.sleeps() != 0 || seq.listN != 1 || seq.rollbacks != 0 {
			t.Fatalf("sleeps %d lists %d", clock.sleeps(), seq.listN)
		}
	})

	t.Run("healthy checks keep waiting", func(t *testing.T) {
		a, _, clock, seq := dockerRun(t, []string{"healthy"})
		start := clock.now()
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "web", Protocol: "tcp", Port: 1, IntervalSec: 60,
		}}
		res := mustImage(t, a, "image_deploy", dockerSpec(1, hcFast, true), 4)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 5 || !clock.now().Equal(start.Add(10*time.Second)) || seq.rollbacks != 1 {
			t.Fatalf("sleeps %d now %s rolls %d", clock.sleeps(), clock.now(), seq.rollbacks)
		}
	})

	t.Run("starting then healthy with checks", func(t *testing.T) {
		ln := listenLocal(t)
		port := ln.Addr().(*net.TCPAddr).Port
		a, _, clock, seq := dockerRun(t, []string{"starting", "starting", "healthy"})
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "web", Protocol: "tcp", Port: port, IntervalSec: 60,
		}}
		res := mustImage(t, a, "image_deploy", dockerSpec(port, hcFast, true), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if clock.sleeps() != 2 || seq.listN != 3 || seq.rollbacks != 0 {
			t.Fatalf("sleeps %d lists %d", clock.sleeps(), seq.listN)
		}
	})

	t.Run("list error does not roll back", func(t *testing.T) {
		a, h, clock, seq := dockerRun(t, nil)
		seq.listErr = true
		res := mustImage(t, a, "image_deploy", dockerSpec(3031, hcFast, true), 120)
		assertDeploy(t, res, "failed", "failed")
		if clock.sleeps() != 0 || countVerb(h, images.VerbRollback) != 0 || countVerb(h, images.VerbList) != 1 {
			t.Fatal(h.Calls())
		}
	})
}

// deadlineDuringWait is a sleeper that returns ctx.Err() once a real
// deadline fires, and a list helper that can fail the same way. The fake
// clock does not move, so the time.Time deadline stays in the future.
type deadlineDuringWait struct {
	seq        *dockerSeq
	blockList  bool
	sleeps     int
	sleptLive  bool
	lists      int
	listedLive bool
}

func (h *deadlineDuringWait) Call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	if h.blockList && verb == images.VerbList {
		h.lists++
		if ctx.Err() == nil {
			h.listedLive = true
			<-ctx.Done()
		}
		return nil, ctx.Err()
	}
	return h.seq.call(verb, body)
}

func (h *deadlineDuringWait) sleep(ctx context.Context, _ time.Duration) error {
	h.sleeps++
	if ctx.Err() == nil {
		h.sleptLive = true
		<-ctx.Done()
	}
	return ctx.Err()
}

func TestDockerDeadlineDuringWait(t *testing.T) {
	const budget = 500 * time.Millisecond
	run := func(t *testing.T, blockList bool) (*deadlineDuringWait, protocol.TaskResult) {
		t.Helper()
		a, _, _, seq := dockerRun(t, []string{"starting"})
		h := &deadlineDuringWait{seq: seq, blockList: blockList}
		a.opt.Helper = h
		a.opt.Sleep = h.sleep
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(budget))
		defer cancel()
		rej, res, ok := a.execute(ctx, imageTask("image_deploy", dockerSpec(3031, hcFast, true), 120))
		if !ok {
			t.Fatalf("rejected %s", rej.ErrorCode)
		}
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("context %v", ctx.Err())
		}
		return h, res
	}

	t.Run("sleep while starting", func(t *testing.T) {
		h, res := run(t, false)
		assertDeploy(t, res, "rolled_back", "failed")
		if !h.sleptLive || h.sleeps != 1 || h.seq.listN != 1 || h.seq.rollbacks != 1 {
			t.Fatalf("sleeps %d live %v lists %d rolls %d", h.sleeps, h.sleptLive, h.seq.listN, h.seq.rollbacks)
		}
	})

	t.Run("list", func(t *testing.T) {
		h, res := run(t, true)
		assertDeploy(t, res, "rolled_back", "failed")
		if !h.listedLive || h.lists != 1 || h.sleeps != 0 || h.seq.rollbacks != 1 {
			t.Fatalf("lists %d live %v sleeps %d rolls %d", h.lists, h.listedLive, h.sleeps, h.seq.rollbacks)
		}
	})
}

func TestDockerRollbackWait(t *testing.T) {
	t.Run("kept healthy", func(t *testing.T) {
		a, h, clock, seq := dockerRun(t, []string{"healthy"})
		seq.prev = prevDigest(imgDigest)
		res := mustImage(t, a, "image_rollback", dockerRollbackSpec(hcFast), 4)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		if dig(m) != imgDigest || clock.sleeps() != 0 || seq.listN != 1 || seq.rollbacks != 1 || seq.pullRef != "" {
			t.Fatalf("ref %q sleeps %d lists %d rolls %d", seq.pullRef, clock.sleeps(), seq.listN, seq.rollbacks)
		}
		if h.Calls()[0] != images.VerbStatus || h.Calls()[1] != images.VerbRollback {
			t.Fatal(h.Calls())
		}
	})

	t.Run("kept unhealthy", func(t *testing.T) {
		a, h, clock, seq := dockerRun(t, []string{"unhealthy"})
		seq.prev = prevDigest(imgDigest)
		res := mustImage(t, a, "image_rollback", dockerRollbackSpec(hcFast), 120)
		m := assertDeploy(t, res, "failed", "failed")
		if dig(m) != imgDigest || clock.sleeps() != 0 || seq.rollbacks != 1 || countVerb(h, images.VerbRollback) != 1 {
			t.Fatalf("%v calls %v", m, h.Calls())
		}
	})

	t.Run("kept healthy with checks", func(t *testing.T) {
		ln := listenLocal(t)
		port := ln.Addr().(*net.TCPAddr).Port
		a, _, clock, seq := dockerRun(t, []string{"healthy"})
		seq.prev = prevDigest(imgDigest)
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "web", Protocol: "tcp", Port: port, IntervalSec: 60,
		}}
		res := mustImage(t, a, "image_rollback", dockerRollbackSpecPort(port, hcFast), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if clock.sleeps() != 0 || seq.listN != 1 || seq.rollbacks != 1 || seq.pullRef != "" {
			t.Fatalf("sleeps %d lists %d ref %q", clock.sleeps(), seq.listN, seq.pullRef)
		}
	})

	t.Run("other digest unhealthy", func(t *testing.T) {
		a, h, clock, seq := dockerRun(t, []string{"unhealthy"})
		seq.prev = prevDigest(otherDigest)
		res := mustImage(t, a, "image_rollback", dockerRollbackSpec(hcFast), 120)
		m := assertDeploy(t, res, "rolled_back", "failed")
		if dig(m) != imgDigest || clock.sleeps() != 0 || seq.rollbacks != 1 || seq.pullRef != "ghcr.io/acme/web@"+imgDigest {
			t.Fatalf("ref %s %v calls %v", seq.pullRef, m, h.Calls())
		}
		if strings.Count(strings.Join(h.Calls(), " "), images.VerbRollback) != 1 {
			t.Fatal(h.Calls())
		}
	})
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func prevDigest(d string) *images.PrevView {
	return &images.PrevView{Digest: &d, SpecHash: imgHash}
}

func dockerSpec(port int, hc string, force bool) []byte {
	flag := "false"
	if force {
		flag = "true"
	}
	return []byte(fmt.Sprintf(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","ports":[{"host":%d,"container":%d,"public":false}],"healthy_after_sec":30,"healthcheck":%s},"env":{},"registry_auth":null,"force":%s,"spec_hash":"%s"}`, port, port, hc, flag, imgHash))
}

func dockerRollbackSpec(hc string) []byte {
	return dockerRollbackSpecPort(3031, hc)
}

func dockerRollbackSpecPort(port int, hc string) []byte {
	body := dockerSpec(port, hc, true)
	return []byte(strings.TrimSuffix(string(body), "}") + `,"digest":"` + imgDigest + `"}`)
}

func countVerb(h *fakeHelper, verb string) int {
	n := 0
	for _, c := range h.Calls() {
		if c == verb {
			n++
		}
	}
	return n
}

type dockerSeq struct {
	healths   []string
	listN     int
	listErr   bool
	prev      *images.PrevView
	pullRef   string
	deployed  bool
	rollbacks int
}

func (s *dockerSeq) call(verb string, body []byte) ([]byte, error) {
	switch verb {
	case images.VerbPull:
		var req images.PullRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		s.pullRef = req.Ref
		return images.Encode(images.PullResponse{Digest: imgDigest, ImageID: "sha256:image"}), nil
	case images.VerbStatus:
		st := missingStatus()
		if s.deployed {
			st = runningStatus(0)
		}
		return images.Encode(images.StatusResult{Status: st, SpecHash: imgHash, Prev: s.prev}), nil
	case images.VerbDeploy:
		s.deployed = true
		return images.Encode(images.IDResponse{ContainerID: "c1"}), nil
	case images.VerbRollback:
		if err := readRollback(body); err != nil {
			return nil, err
		}
		s.rollbacks++
		return images.Encode(images.IDResponse{ContainerID: "prev"}), nil
	case images.VerbLogs:
		return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{}, Truncated: false}), nil
	case images.VerbList:
		if s.listErr {
			return nil, errHelper
		}
		h := ""
		if s.listN < len(s.healths) {
			h = s.healths[s.listN]
		} else if len(s.healths) > 0 {
			h = s.healths[len(s.healths)-1]
		}
		s.listN++
		d := imgDigest
		item := images.ContainerInfo{
			ID: "abc", Name: "/acme-web", State: "running", Digest: &d,
			Ports: []images.PublishedPort{{HostIP: "127.0.0.1", HostPort: 3031, ContainerPort: 3031}},
		}
		if h != "" {
			v := h
			item.Health = &v
		}
		return images.Encode(images.ListResponse{Items: []images.ContainerInfo{item}}), nil
	default:
		return nil, errHelper
	}
}

func dockerRun(t *testing.T, healths []string) (*agent, *fakeHelper, *testClock, *dockerSeq) {
	t.Helper()
	a, h, clock, _ := imageRun(t)
	seq := &dockerSeq{healths: healths}
	h.fn = seq.call
	return a, h, clock, seq
}
