package run

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/config"
	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

const (
	agentHost = "11111111-1111-4111-8111-111111111111"
	imgDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	imgHash   = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func TestImageSequences(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		seq.pre = runningStatus(3)
		seq.preHash = imgHash
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		m := assertDeploy(t, res, "unchanged", "succeeded")
		if dig(m) != imgDigest || m["exit_code"] != nil {
			t.Fatalf("%v", m)
		}
		if seq.deployed || clock.sleeps() != 0 || seq.linesReq != 50 || seq.pullRef != "ghcr.io/acme/web:1" {
			t.Fatalf("deployed %v sleeps %d lines %d ref %s", seq.deployed, clock.sleeps(), seq.linesReq, seq.pullRef)
		}
		verbOrder(t, h, images.VerbPull, images.VerbStatus, images.VerbLogs)
	})

	t.Run("force recreates", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		start := clock.now()
		seq.pre = runningStatus(0)
		seq.preHash = imgHash
		seq.post = runningStatus(0)
		res := mustImage(t, a, "image_deploy", serviceSpec(true, 5, 3031), 120)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		if dig(m) != imgDigest || m["exit_code"] != nil {
			t.Fatalf("%v", m)
		}
		if !seq.deployed || clock.sleeps() != 3 || !clock.now().Equal(start.Add(6*time.Second)) {
			t.Fatalf("deployed %v sleeps %d now %s", seq.deployed, clock.sleeps(), clock.now())
		}
		if !strings.Contains(string(seq.deployBody), imgDigest) || !strings.Contains(string(seq.deployBody), imgHash) {
			t.Fatalf("deploy %s", seq.deployBody)
		}
		want := []string{images.VerbPull, images.VerbStatus, images.VerbDeploy}
		for i := 0; i < 4; i++ {
			want = append(want, images.VerbStatus)
		}
		want = append(want, images.VerbLogs)
		verbOrder(t, h, want...)
	})

	t.Run("restart rolls back", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(1)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 0 {
			t.Fatalf("sleeps %d", clock.sleeps())
		}
		verbOrder(t, h, images.VerbPull, images.VerbStatus, images.VerbDeploy, images.VerbStatus, images.VerbRollback, images.VerbLogs, images.VerbStatus)
	})

	t.Run("exited rolls back", func(t *testing.T) {
		a, _, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = exitedStatus(1)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, 3031), 120)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 0 {
			t.Fatalf("sleeps %d", clock.sleeps())
		}
	})

	t.Run("rollback failed", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = exitedStatus(1)
		seq.rollback = "conflict"
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		m := assertDeploy(t, res, "rollback_failed", "failed")
		if dig(m) != imgDigest || clock.sleeps() != 0 {
			t.Fatalf("%v sleeps %d", m, clock.sleeps())
		}
		verbOrder(t, h, images.VerbPull, images.VerbStatus, images.VerbDeploy, images.VerbStatus, images.VerbRollback, images.VerbLogs)
	})

	t.Run("pull unavailable", func(t *testing.T) {
		for _, shape := range []string{"error", "reason"} {
			t.Run(shape, func(t *testing.T) {
				a, h, _, seq := imageRun(t)
				seq.pullCode = "image_unavailable"
				seq.pullShape = shape
				res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
				m := assertDeploy(t, res, "image_unavailable", "failed")
				if m["digest"] != nil || seq.deployed {
					t.Fatalf("%v deployed %v", m, seq.deployed)
				}
				verbOrder(t, h, images.VerbPull, images.VerbLogs)
			})
		}
	})

	t.Run("bad pull digest", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		h.fn = func(verb string, body []byte) ([]byte, error) {
			switch verb {
			case images.VerbPull:
				return images.Encode(images.PullResponse{Digest: "nope", ImageID: "id"}), nil
			case images.VerbLogs:
				return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{}, Truncated: false}), nil
			default:
				return nil, errors.New(verb)
			}
		}
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		m := assertDeploy(t, res, "failed", "failed")
		if m["digest"] != nil {
			t.Fatalf("%v", m)
		}
		verbOrder(t, h, images.VerbPull, images.VerbLogs)
	})

	t.Run("empty status state", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = images.ImageStatus{State: "", Ports: []int{}}
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		m := assertDeploy(t, res, "failed", "failed")
		if dig(m) != imgDigest || seq.deployed {
			t.Fatalf("%v deployed %v", m, seq.deployed)
		}
		verbOrder(t, h, images.VerbPull, images.VerbStatus, images.VerbLogs)
	})

	t.Run("job exit 0", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		seq.pre = images.ImageStatus{State: "created", Ports: []int{}}
		seq.post = exitedStatus(0)
		res := mustImage(t, a, "image_deploy", jobSpec(10), 120)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		code, ok := exitOf(m)
		if !ok || code != 0 || dig(m) != imgDigest || clock.sleeps() != 0 {
			t.Fatalf("%v sleeps %d", m, clock.sleeps())
		}
		verbOrder(t, h, images.VerbPull, images.VerbStatus, images.VerbRemove, images.VerbDeploy, images.VerbStatus, images.VerbLogs)
	})

	t.Run("job exit 3", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = exitedStatus(3)
		res := mustImage(t, a, "image_deploy", jobSpec(10), 120)
		m := assertDeploy(t, res, "failed", "failed")
		code, ok := exitOf(m)
		if !ok || code != 3 || clock.sleeps() != 0 {
			t.Fatalf("%v sleeps %d", m, clock.sleeps())
		}
		if strings.Contains(strings.Join(h.Calls(), " "), images.VerbRemove) {
			t.Fatalf("removed a missing job: %v", h.Calls())
		}
		verbOrder(t, h, images.VerbPull, images.VerbStatus, images.VerbDeploy, images.VerbStatus, images.VerbLogs)
	})

	t.Run("job timeout", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		start := clock.now()
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		seq.stopErr = true
		res := mustImage(t, a, "image_deploy", jobSpec(10), 120)
		m := assertDeploy(t, res, "timeout", "failed")
		if m["exit_code"] != nil || dig(m) != imgDigest {
			t.Fatalf("%v", m)
		}
		if clock.sleeps() != 5 || !clock.now().Equal(start.Add(10*time.Second)) {
			t.Fatalf("sleeps %d now %s", clock.sleeps(), clock.now())
		}
		want := []string{images.VerbPull, images.VerbStatus, images.VerbDeploy}
		for i := 0; i < 6; i++ {
			want = append(want, images.VerbStatus)
		}
		want = append(want, images.VerbStop, images.VerbLogs)
		verbOrder(t, h, want...)
	})

	t.Run("rollback by digest", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		res := mustImage(t, a, "image_rollback", serviceRollbackSpec(), 120)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		if dig(m) != imgDigest || seq.pullRef != "ghcr.io/acme/web@"+imgDigest || clock.sleeps() != 3 {
			t.Fatalf("ref %s sleeps %d %v", seq.pullRef, clock.sleeps(), m)
		}
		if strings.Contains(strings.Join(h.Calls(), " "), images.VerbRemove) {
			t.Fatalf("job remove on rollback: %v", h.Calls())
		}
	})

	t.Run("rollback of a job uses the service wait", func(t *testing.T) {
		a, h, clock, seq := imageRun(t)
		start := clock.now()
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		res := mustImage(t, a, "image_rollback", jobRollbackSpec(), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if seq.pullRef != "ghcr.io/acme/web@"+imgDigest {
			t.Fatalf("ref %s", seq.pullRef)
		}
		if clock.sleeps() != 15 || !clock.now().Equal(start.Add(30*time.Second)) {
			t.Fatalf("sleeps %d now %s", clock.sleeps(), clock.now())
		}
		joined := strings.Join(h.Calls(), " ")
		if strings.Contains(joined, images.VerbRemove) || strings.Contains(joined, images.VerbStop) {
			t.Fatalf("calls %v", h.Calls())
		}
	})
}

func TestImageHealthWait(t *testing.T) {
	t.Run("checks replace the timer", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port
		a, h, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "web", Protocol: "tcp", Port: port, IntervalSec: 60,
		}}
		res := mustImage(t, a, "image_deploy", serviceSpec(true, 30, port), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if clock.sleeps() != 0 {
			t.Fatalf("sleeps %d", clock.sleeps())
		}
		verbOrder(t, h, images.VerbPull, images.VerbStatus, images.VerbDeploy, images.VerbStatus, images.VerbLogs)
	})

	t.Run("refused probes wait out the task", func(t *testing.T) {
		a, _, clock, seq := imageRun(t)
		start := clock.now()
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "web", Protocol: "tcp", Port: 3031, IntervalSec: 60,
		}}
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, 3031), 6)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 3 || !clock.now().Equal(start.Add(6*time.Second)) {
			t.Fatalf("sleeps %d now %s", clock.sleeps(), clock.now())
		}
	})

	t.Run("check on another port uses the timer", func(t *testing.T) {
		a, _, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		a.configured = []protocol.HealthCheck{{
			ID: "other", Service: "web", Protocol: "tcp", Port: 9, IntervalSec: 60,
		}}
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if clock.sleeps() != 3 {
			t.Fatalf("sleeps %d", clock.sleeps())
		}
	})
}

func TestImageRefusals(t *testing.T) {
	cases := []struct {
		code, outcome string
	}{
		{"image_unavailable", "image_unavailable"},
		{"conflict", "conflict"},
		{"policy_denied", "forbidden_spec"},
		{"bad_spec", "forbidden_spec"},
		{"docker_unavailable", "failed"},
	}
	for _, tc := range cases {
		for _, shape := range []string{"error", "reason"} {
			t.Run(tc.code+"/"+shape, func(t *testing.T) {
				a, h, _, _ := imageRun(t)
				h.fn = func(verb string, body []byte) ([]byte, error) {
					if verb != images.VerbStart {
						t.Fatalf("verb %s", verb)
					}
					return refuse(shape, tc.code), nil
				}
				res := mustImage(t, a, "image_start", []byte(`{"project":"acme","entry":"web"}`), 60)
				assertStatus(t, res, tc.outcome, "failed")
			})
		}
	}

	t.Run("ok true is not success", func(t *testing.T) {
		a, _, _, _ := imageRun(t)
		a.opt.Helper.(*fakeHelper).fn = func(string, []byte) ([]byte, error) {
			return []byte(`{"ok":true}`), nil
		}
		res := mustImage(t, a, "image_start", []byte(`{"project":"acme","entry":"web"}`), 60)
		assertStatus(t, res, "failed", "failed")
	})

	t.Run("logs refusal", func(t *testing.T) {
		for _, shape := range []string{"error", "reason"} {
			a, _, _, _ := imageRun(t)
			a.opt.Helper.(*fakeHelper).fn = func(string, []byte) ([]byte, error) {
				return refuse(shape, "policy_denied"), nil
			}
			res := mustImage(t, a, "image_logs", []byte(`{"project":"acme","entry":"web","lines":20}`), 60)
			raw := marshalPayload(t, res.Payload)
			if err := protocol.ValidateImageLogs(raw); err != nil {
				t.Fatal(err)
			}
			if res.State != "failed" || res.ErrorCode == nil || *res.ErrorCode != "forbidden_spec" {
				t.Fatalf("%+v", res)
			}
			if !strings.Contains(string(raw), `"container":"acme-web"`) || !strings.Contains(string(raw), `"lines":[]`) {
				t.Fatalf("%s", raw)
			}
		}
	})
}

func TestImageActionsAndLogs(t *testing.T) {
	cases := []struct {
		kind, verb string
	}{
		{"image_start", images.VerbStart},
		{"image_stop", images.VerbStop},
		{"image_restart", images.VerbRestart},
		{"image_remove", images.VerbRemove},
		{"image_status", images.VerbStatus},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			a, h, _, _ := imageRun(t)
			h.fn = func(verb string, body []byte) ([]byte, error) {
				if verb != tc.verb {
					t.Fatalf("verb %s", verb)
				}
				st := runningStatus(0)
				if tc.kind == "image_status" {
					return images.Encode(images.StatusResult{Status: st, SpecHash: imgHash}), nil
				}
				return images.Encode(images.ActionResult{Status: st}), nil
			}
			res := mustImage(t, a, tc.kind, []byte(`{"project":"acme","entry":"web"}`), 60)
			raw := assertStatus(t, res, "succeeded", "succeeded")
			if strings.Contains(string(raw), "spec_hash") || strings.Contains(string(raw), imgHash) {
				t.Fatalf("%s", raw)
			}
		})
	}

	t.Run("logs", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		h.fn = func(verb string, body []byte) ([]byte, error) {
			if verb != images.VerbLogs {
				t.Fatalf("verb %s", verb)
			}
			var req images.LogsRequest
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatal(err)
			}
			if req.Lines != 20 || len(req.Redact) != 0 || strings.Contains(string(body), `"redact"`) {
				t.Fatalf("lines %d body %s", req.Lines, body)
			}
			return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{"ready"}, Truncated: false}), nil
		}
		res := mustImage(t, a, "image_logs", []byte(`{"project":"acme","entry":"web","lines":20}`), 60)
		raw := marshalPayload(t, res.Payload)
		if err := protocol.ValidateImageLogs(raw); err != nil {
			t.Fatal(err)
		}
		if res.State != "succeeded" || res.ErrorCode != nil || !strings.Contains(string(raw), `"ready"`) {
			t.Fatalf("%s %+v", raw, res)
		}
	})
}

func TestImageSecretsNotRetained(t *testing.T) {
	const secret = `sek"ret`
	const token = `tok<en`
	spec := secretSpec()
	a, _, _, seq := imageRun(t)
	seq.pre = missingStatus()
	seq.post = runningStatus(0)
	seq.logs = secretForms(secret, token)
	var buf bytes.Buffer
	a.opt.Log = stdLog(&buf)
	res := mustImage(t, a, "image_deploy", spec, 120)
	raw := marshalPayload(t, res.Payload)
	if err := protocol.ValidateImageDeploy(raw); err != nil {
		t.Fatal(err)
	}
	for _, b := range spec {
		if b != 0 {
			t.Fatal("spec bytes kept")
		}
	}
	kept := append([]string{secret, token}, encodings(secret)...)
	kept = append(kept, encodings(token)...)
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	var decoded strings.Builder
	if lines, ok := payload["lines"].([]any); ok {
		for _, line := range lines {
			if s, ok := line.(string); ok {
				decoded.WriteString(s)
				decoded.WriteByte('\n')
			}
		}
	}
	blob := decoded.String() + buf.String()
	for _, form := range kept {
		if form == "" || strings.Contains(blob, form) {
			t.Fatalf("kept %q", form)
		}
	}
	if !strings.Contains(string(raw), "short") || !strings.Contains(string(raw), `"status"`) {
		t.Fatalf("%s", raw)
	}
	var dep images.DeployRequest
	if err := json.Unmarshal(seq.deployBody, &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Env["DB_URL"] != secret || dep.Env["TINY"] != "short" {
		t.Fatalf("deploy env %+v", dep.Env)
	}
	if strings.Contains(string(seq.deployBody), token) {
		t.Fatalf("token in deploy %s", seq.deployBody)
	}
	var pull images.PullRequest
	if err := json.Unmarshal(seq.pullBody, &pull); err != nil {
		t.Fatal(err)
	}
	if pull.RegistryAuth == nil || pull.RegistryAuth.Token != token {
		t.Fatalf("pull %+v", pull.RegistryAuth)
	}

	t.Run("transport error text stays out", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		h.fn = func(verb string, body []byte) ([]byte, error) {
			if verb == images.VerbPull {
				return images.Encode(images.PullResponse{Digest: imgDigest, ImageID: "id"}), nil
			}
			return nil, errors.New(token)
		}
		spec := secretSpec()
		res := mustImage(t, a, "image_deploy", spec, 120)
		raw := marshalPayload(t, res.Payload)
		if strings.Contains(string(raw), token) || strings.Contains(string(raw), secret) {
			t.Fatalf("%s", raw)
		}
		if res.State != "failed" || res.ErrorCode == nil || *res.ErrorCode != "failed" {
			t.Fatalf("%+v", res)
		}
		for _, b := range spec {
			if b != 0 {
				t.Fatal("spec bytes kept")
			}
		}
	})
}

func TestImageTimeoutHonoured(t *testing.T) {
	for _, tc := range []struct {
		sec      int
		min, max time.Duration
	}{
		{120, 100 * time.Second, 121 * time.Second},
		{0, 10 * time.Second, 16 * time.Second},
	} {
		t.Run(fmt.Sprintf("sec %d", tc.sec), func(t *testing.T) {
			h := &deadlineHelper{}
			a := &agent{
				cfg: config.Config{HostID: agentHost},
				policy: policy.Policy{
					Exists: true, Valid: true, Enabled: true,
					Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
				},
				opt: Options{Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil }, Helper: h, Log: stdLog(&bytes.Buffer{})},
			}
			res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), tc.sec)
			assertDeploy(t, res, "image_unavailable", "failed")
			if h.left < tc.min || h.left > tc.max {
				t.Fatalf("deadline remaining %s", h.left)
			}
		})
	}
}

func TestDockerLatch(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	var fail atomic.Bool
	fail.Store(true)
	h := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case images.VerbList:
			if fail.Load() {
				return nil, errors.New("down")
			}
			return images.Encode(images.ListResponse{Items: []images.ContainerInfo{}}), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	var buf bytes.Buffer
	a := &agent{
		opt: Options{ProcRoot: proc, Passwd: passwd, Helper: h, Log: stdLog(&buf)},
		policy: policy.Policy{
			Exists: true, Valid: true, Enabled: true,
			Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
		},
	}
	if _, complete := a.discover(context.Background()); complete || a.dockerUp() {
		t.Fatal("first failure latched docker up or completed")
	}
	fail.Store(false)
	if _, complete := a.discover(context.Background()); !complete || a.dockerUp() {
		t.Fatal("later success flipped docker")
	}
	if n := strings.Count(buf.String(), "bot-agent: container discovery failed"); n != 1 {
		t.Fatalf("logs %d\n%s", n, buf.String())
	}

	h2 := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case images.VerbList:
			if fail.Load() {
				return nil, errors.New("down")
			}
			return images.Encode(images.ListResponse{Items: []images.ContainerInfo{}}), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	fail.Store(false)
	up := &agent{
		opt:    Options{ProcRoot: proc, Passwd: passwd, Helper: h2, Log: stdLog(&bytes.Buffer{})},
		policy: a.policy,
	}
	if _, complete := up.discover(context.Background()); !complete || !up.dockerUp() {
		t.Fatal("successful list did not latch docker")
	}
	fail.Store(true)
	if _, complete := up.discover(context.Background()); complete || !up.dockerUp() {
		t.Fatal("later failure cleared docker")
	}

	off := &agent{
		opt: Options{
			ProcRoot: proc, Passwd: passwd,
			Helper: &fakeHelper{fn: func(string, []byte) ([]byte, error) {
				t.Error("helper called")
				return nil, errors.New("called")
			}},
			Log: stdLog(&bytes.Buffer{}),
		},
		policy: policy.Policy{Exists: true, Valid: true, Enabled: false},
	}
	if _, complete := off.discover(context.Background()); !complete {
		t.Fatal("images off failed discovery")
	}
}

func TestContainerListAfterScanFailure(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	breakScan(t, proc)
	var saw atomic.Bool
	a := &agent{
		opt: Options{
			ProcRoot: proc, Passwd: passwd, Log: stdLog(&bytes.Buffer{}),
			Helper: &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
				switch verb {
				case "list":
					return programs.EncodeList(nil), nil
				case images.VerbList:
					saw.Store(true)
					return images.Encode(images.ListResponse{Items: []images.ContainerInfo{{
						ID: "1", Name: "acme-web", State: "running",
						Labels: ownedTestLabels("0123456789abcdef", "acme", "web"),
					}}}), nil
				default:
					return nil, errors.New(verb)
				}
			}},
		},
		policy: policy.Policy{
			Exists: true, Valid: true, Enabled: true,
			Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
		},
	}
	if _, complete := a.discover(context.Background()); complete || !saw.Load() || !a.dockerUp() {
		t.Fatal("scan failure skipped container_list or applied it")
	}
	if len(a.services) != 0 {
		t.Fatalf("applied %+v", a.services)
	}
}

func TestMachineIDUnreadableUsesLabels(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	path := filepath.Join(t.TempDir(), "SENTINEL-machine-id")
	var buf bytes.Buffer
	a := &agent{
		opt: Options{
			ProcRoot: proc, Passwd: passwd, MachineID: path, Log: stdLog(&buf),
			Helper: &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
				switch verb {
				case "list":
					return programs.EncodeList(nil), nil
				case images.VerbList:
					digest := imgDigest
					return images.Encode(images.ListResponse{Items: []images.ContainerInfo{{
						ID: "1", Name: "acme-web", State: "running", Digest: &digest,
						Labels: ownedTestLabels("abcdef0123456789", "acme", "web"),
					}}}), nil
				default:
					return nil, errors.New(verb)
				}
			}},
		},
		policy: policy.Policy{
			Exists: true, Valid: true, Enabled: true,
			Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
		},
	}
	res, complete := a.discover(context.Background())
	if !complete || !hasService(res.Services, "web") {
		t.Fatalf("complete %v %+v", complete, res.Services)
	}
	if _, complete := a.discover(context.Background()); !complete {
		t.Fatal("second pass")
	}
	if n := strings.Count(buf.String(), "bot-agent: machine-id unreadable; container ownership uses labels only"); n != 1 {
		t.Fatalf("logs %d\n%s", n, buf.String())
	}
	if strings.Contains(buf.String(), "SENTINEL") {
		t.Fatalf("path logged: %s", buf.String())
	}
}

func TestHelloImageFlags(t *testing.T) {
	proc, sys, _ := fixtureProc(t, 9)
	appendUnmapped(t, proc)
	passwd := joePasswd(t)
	group := joeGroup(t)
	off := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		if verb == images.VerbList {
			t.Fatal("container_list while images are off")
		}
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	hello, raw := helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd, GroupPath: group,
		PolicyPath: writeRunner(t, true, false), PolicyUID: rootUID,
		Helper: off,
	})
	if hello.Programs == nil || hello.Programs.Images || hello.Programs.Docker || !hello.Programs.Files {
		t.Fatalf("%+v", hello.Programs)
	}
	if !strings.Contains(string(raw), `"images":false`) || !strings.Contains(string(raw), `"docker":false`) || !strings.Contains(string(raw), `"files":true`) {
		t.Fatalf("%s", raw)
	}

	idPath, host := writeMachineID(t)
	digest := imgDigest
	pf, err := os.OpenFile(filepath.Join(proc, "net", "tcp"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(pf, "   4: 00000000:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 556 1 0000000000000000 100 0 0 10 0\n"); err != nil {
		t.Fatal(err)
	}
	if err := pf.Close(); err != nil {
		t.Fatal(err)
	}
	on := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case images.VerbList:
			return images.Encode(images.ListResponse{Items: containerItems(host, digest)}), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	hello, raw = helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd, GroupPath: group,
		PolicyPath: writeImageRunner(t), PolicyUID: rootUID,
		Helper: on, MachineID: idPath,
	})
	if hello.Programs == nil || !hello.Programs.Images || !hello.Programs.Docker || !hello.Programs.Files || !hello.ServicesComplete {
		t.Fatalf("programs %+v complete %v", hello.Programs, hello.ServicesComplete)
	}
	if !strings.Contains(string(raw), `"files":true`) {
		t.Fatalf("%s", raw)
	}
	if strings.Contains(string(raw), "SENTINEL") || strings.Contains(string(raw), "ghcr.io/acme/*") || strings.Contains(string(raw), "port-3031") || strings.Contains(string(raw), "port-6379") {
		t.Fatalf("%s", raw)
	}
	assertContainerSet(t, hello.Services, digest)
}

func TestHelloOverlappingProjects(t *testing.T) {
	proc, sys, _ := fixtureProc(t, 9)
	appendUnmapped(t, proc)
	off := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		if verb == images.VerbList {
			t.Fatal("container_list while overlapping projects disable images")
		}
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	body := "enabled = true\nrun_as = [\"joe\"]\ndirs = [\"/home/joe/workspace-SENTINEL-dir\"]\nexec_allow = [\"/usr/bin/pnpm-SENTINEL\"]\nallow_shell = false\nmax_programs = 20\nimages = [\"ghcr.io/acme/*\"]\nprojects = [\"acme\", \"acme-web\"]\n"
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	hello, raw := helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: joePasswd(t), GroupPath: joeGroup(t),
		PolicyPath: path, PolicyUID: rootUID,
		Helper: off,
	})
	if hello.Programs == nil || !hello.Programs.Enabled || hello.Programs.Images || !hello.Programs.Files {
		t.Fatalf("%+v", hello.Programs)
	}
	if !strings.Contains(string(raw), `"images":false`) {
		t.Fatalf("%s", raw)
	}
	if strings.Contains(string(raw), "acme-web") || strings.Contains(string(raw), "ghcr.io/acme") {
		t.Fatalf("%s", raw)
	}
}

func TestContainerListFailureSkipsFrame(t *testing.T) {
	proc, sys, _ := fixtureProc(t, 9)
	appendUnmapped(t, proc)
	idPath, host := writeMachineID(t)
	digest := imgDigest
	var down atomic.Bool
	down.Store(true)
	helper := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case images.VerbList:
			if down.Load() {
				return nil, errors.New("down")
			}
			return images.Encode(images.ListResponse{Items: containerItems(host, digest)}), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	r := startTuned(t, proc, sys, joePasswd(t), func(_ *fakeserver.Server, opt *Options) {
		opt.PolicyPath = writeImageRunner(t)
		opt.PolicyUID = rootUID
		opt.GroupPath = joeGroup(t)
		opt.Helper = helper
		opt.MachineID = idPath
	})
	defer stopRun(t, r.cancel, r.errCh)

	r.until(t, 60*time.Second)
	hello, raw := helloOf(t, r)
	if hello.ServicesComplete || len(hello.Services) != 0 || hello.Programs == nil || !hello.Programs.Images || hello.Programs.Docker {
		t.Fatalf("hello %s", raw)
	}
	r.fire(t, 60*time.Second)
	r.wantServices(t, 0)
	if n := strings.Count(r.log.String(), "bot-agent: container discovery failed"); n != 1 {
		t.Fatalf("logs %d\n%s", n, r.log.String())
	}

	down.Store(false)
	r.fire(t, 120*time.Second)
	r.wantServices(t, 1)
	sets := r.serviceSets(t)
	if len(sets) != 1 {
		t.Fatalf("sets %d", len(sets))
	}
	assertContainerSet(t, sets[0], digest)
	if n := strings.Count(r.log.String(), "bot-agent: container discovery failed"); n != 1 {
		t.Fatalf("logs %d", n)
	}
	r.done(t)
}

type imageSeq struct {
	pullCode   string
	pullShape  string
	pullRef    string
	pullBody   []byte
	deployBody []byte
	pre        images.ImageStatus
	preHash    string
	preFiles   string
	post       images.ImageStatus
	postHash   string
	postFiles  string
	deployed   bool
	rollback   string
	stopErr    bool
	logs       []string
	linesReq   int
	waitErr    bool
	waitCode   string
	logReqs    []images.LogsRequest
}

func (s *imageSeq) call(verb string, body []byte) ([]byte, error) {
	cp := append([]byte(nil), body...)
	switch verb {
	case images.VerbPull:
		var req images.PullRequest
		if err := json.Unmarshal(cp, &req); err != nil {
			return nil, err
		}
		s.pullRef = req.Ref
		s.pullBody = cp
		if s.pullCode != "" {
			shape := s.pullShape
			if shape == "" {
				shape = "error"
			}
			return refuse(shape, s.pullCode), nil
		}
		return images.Encode(images.PullResponse{Digest: imgDigest, ImageID: "sha256:image"}), nil
	case images.VerbStatus:
		st, hash, files := s.pre, s.preHash, s.preFiles
		if s.deployed {
			if s.waitErr {
				return nil, errors.New("status")
			}
			if s.waitCode != "" {
				return refuse("error", s.waitCode), nil
			}
			st, hash, files = s.post, s.postHash, s.postFiles
		}
		return images.Encode(images.StatusResult{Status: st, SpecHash: hash, FilesHash: files}), nil
	case images.VerbDeploy:
		s.deployed = true
		s.deployBody = cp
		return images.Encode(images.IDResponse{ContainerID: "c1"}), nil
	case images.VerbRemove:
		return images.Encode(images.ActionResult{Status: missingStatus()}), nil
	case images.VerbRollback:
		if err := readRollback(cp); err != nil {
			return nil, err
		}
		if s.rollback != "" {
			return refuse("error", s.rollback), nil
		}
		return images.Encode(images.IDResponse{ContainerID: "prev"}), nil
	case images.VerbStop:
		if s.stopErr {
			return nil, errors.New("stop")
		}
		return images.Encode(images.ActionResult{Status: missingStatus()}), nil
	case images.VerbLogs:
		var req images.LogsRequest
		if err := json.Unmarshal(cp, &req); err != nil {
			return nil, err
		}
		s.logReqs = append(s.logReqs, req)
		s.linesReq = req.Lines
		lines := s.logs
		if lines == nil {
			lines = []string{}
		}
		return images.Encode(images.LogsResponse{Container: "acme-web", Lines: lines, Truncated: false}), nil
	default:
		return nil, errors.New(verb)
	}
}

// readRollback is the strict body check every container_rollback fake uses.
// A deploy object has fields EntryRequest does not, so Decode rejects it.
func readRollback(body []byte) error {
	var req images.EntryRequest
	if err := images.Decode(body, &req); err != nil {
		return err
	}
	if req.Project != "acme" || req.Entry != "web" {
		return errors.New("rollback target")
	}
	return nil
}

func imageRun(t *testing.T) (*agent, *fakeHelper, *testClock, *imageSeq) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	seq := &imageSeq{}
	h := &fakeHelper{fn: seq.call}
	a := &agent{
		cfg: config.Config{HostID: agentHost},
		policy: policy.Policy{
			Exists: true, Valid: true, Enabled: true,
			Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
		},
		opt: Options{Now: clock.now, Sleep: clock.sleep, Helper: h, Log: stdLog(&bytes.Buffer{})},
	}
	return a, h, clock, seq
}

func imageTask(kind string, spec []byte, timeout int) protocol.Task {
	return protocol.Task{
		V: protocol.Version, Type: "task", TaskID: "task-1",
		HostID: agentHost, Kind: kind,
		// Relative to the wall clock: a fixed date expired on 2026-10-16 12:05 UTC
		// for tests that use time.Now. Tests with a fixed fake clock are earlier still.
		ExpiresAt:  protocol.FormatTime(time.Now().Add(time.Hour).UTC()),
		TimeoutSec: timeout, Spec: spec,
	}
}

func mustImage(t *testing.T, a *agent, kind string, spec []byte, timeout int) protocol.TaskResult {
	t.Helper()
	rej, res, ok := a.execute(context.Background(), imageTask(kind, spec, timeout))
	if !ok {
		t.Fatalf("rejected %s", rej.ErrorCode)
	}
	return res
}

func serviceSpec(force bool, healthy, port int) []byte {
	flag := "false"
	if force {
		flag = "true"
	}
	return []byte(fmt.Sprintf(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","ports":[{"host":%d,"container":%d,"public":false}],"healthy_after_sec":%d},"env":{},"registry_auth":null,"force":%s,"spec_hash":"%s"}`, port, port, healthy, flag, imgHash))
}

func jobSpec(timeout int) []byte {
	return []byte(fmt.Sprintf(`{"project":"acme","entry":"web","spec":{"kind":"job","image":"ghcr.io/acme/web:1","timeout_sec":%d},"env":{},"registry_auth":null,"force":false,"spec_hash":"%s"}`, timeout, imgHash))
}

func serviceRollbackSpec() []byte {
	return []byte(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","healthy_after_sec":5},"env":{},"registry_auth":null,"force":false,"spec_hash":"` + imgHash + `","digest":"` + imgDigest + `"}`)
}

func jobRollbackSpec() []byte {
	return []byte(`{"project":"acme","entry":"web","spec":{"kind":"job","image":"ghcr.io/acme/web:1","timeout_sec":10},"env":{},"registry_auth":null,"force":false,"spec_hash":"` + imgHash + `","digest":"` + imgDigest + `"}`)
}

func secretSpec() []byte {
	return []byte(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","ports":[{"host":3031,"container":3031,"public":false}],"env":{"NODE_ENV":"production"},"secret_env":{"DB_URL":"services.DB_URL"},"healthy_after_sec":5},"env":{"NODE_ENV":"production","DB_URL":"sek\"ret","MODE":"status","TINY":"short"},"registry_auth":{"host":"ghcr.io","username":"bot","token":"tok<en"},"force":true,"spec_hash":"` + imgHash + `"}`)
}

func runningStatus(restarts int) images.ImageStatus {
	d := imgDigest
	return images.ImageStatus{
		Container: protocol.Str("acme-web"), State: "running",
		RestartCount: restarts, Digest: &d, Ports: []int{3031},
	}
}

func exitedStatus(code int) images.ImageStatus {
	d := imgDigest
	return images.ImageStatus{
		Container: protocol.Str("acme-web"), State: "exited",
		ExitCode: &code, Digest: &d, Ports: []int{},
	}
}

func refuse(shape, code string) []byte {
	if shape == "error" {
		return []byte(`{"error":"` + code + `"}`)
	}
	return []byte(`{"ok":false,"reason":"` + code + `"}`)
}

func assertDeploy(t *testing.T, res protocol.TaskResult, outcome, state string) map[string]any {
	t.Helper()
	if res.Type != "task_result" || res.V != protocol.Version || res.TaskID != "task-1" || res.State != state {
		t.Fatalf("%+v", res)
	}
	if outcome == "succeeded" || outcome == "unchanged" {
		if res.ErrorCode != nil {
			t.Fatalf("error %v", res.ErrorCode)
		}
	} else if res.ErrorCode == nil || *res.ErrorCode != outcome {
		t.Fatalf("error %v", res.ErrorCode)
	}
	raw := marshalPayload(t, res.Payload)
	if err := protocol.ValidateImageDeploy(raw); err != nil {
		t.Fatalf("%v %s", err, raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["outcome"] != outcome {
		t.Fatalf("outcome %v", m["outcome"])
	}
	return m
}

func assertStatus(t *testing.T, res protocol.TaskResult, outcome, state string) []byte {
	t.Helper()
	if res.State != state {
		t.Fatalf("state %s", res.State)
	}
	if outcome == "succeeded" {
		if res.ErrorCode != nil {
			t.Fatalf("error %v", res.ErrorCode)
		}
	} else if res.ErrorCode == nil || *res.ErrorCode != outcome {
		t.Fatalf("error %v", res.ErrorCode)
	}
	raw := marshalPayload(t, res.Payload)
	if err := protocol.ValidateImageStatus(raw); err != nil {
		t.Fatalf("%v %s", err, raw)
	}
	return raw
}

func dig(m map[string]any) string {
	s, _ := m["digest"].(string)
	return s
}

func exitOf(m map[string]any) (int, bool) {
	switch n := m["exit_code"].(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func verbOrder(t *testing.T, h *fakeHelper, want ...string) {
	t.Helper()
	got := h.Calls()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v want %v", got, want)
	}
}

func secretForms(secret, token string) []string {
	lines := []string{"short"}
	lines = append(lines, secret, token)
	lines = append(lines, encodings(secret)...)
	lines = append(lines, encodings(token)...)
	return lines
}

func encodings(s string) []string {
	return []string{
		base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawStdEncoding.EncodeToString([]byte(s)),
		base64.URLEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)),
		jsonBodyTest(s, false),
		jsonBodyTest(s, true),
	}
}

func jsonBodyTest(s string, html bool) string {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(html)
	if err := enc.Encode(s); err != nil {
		return ""
	}
	raw := strings.TrimRight(buf.String(), "\n")
	if len(raw) < 2 {
		return ""
	}
	return raw[1 : len(raw)-1]
}

type deadlineHelper struct {
	left time.Duration
}

func (h *deadlineHelper) Call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	if verb == images.VerbPull {
		dl, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("no deadline")
		}
		h.left = time.Until(dl)
		return []byte(`{"error":"image_unavailable"}`), nil
	}
	if verb == images.VerbLogs {
		return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{}, Truncated: false}), nil
	}
	return nil, errors.New(verb)
}

func ownedTestLabels(host, project, entry string) map[string]string {
	return map[string]string{
		images.LabelOwner:    images.OwnerValue,
		images.LabelHost:     host,
		images.LabelProject:  project,
		images.LabelEntry:    entry,
		images.LabelSpecHash: imgHash,
	}
}

func containerItems(host, digest string) []images.ContainerInfo {
	return []images.ContainerInfo{
		{
			ID: "aaa", Name: "acme-web", State: "running", Digest: &digest,
			Ports:  []images.PublishedPort{{HostIP: "127.0.0.1", HostPort: 3031, ContainerPort: 80}},
			Labels: ownedTestLabels(host, "acme", "web"),
		},
		{
			ID: "bbb", Name: "redis", State: "running",
			Ports: []images.PublishedPort{{HostIP: "0.0.0.0", HostPort: 6379, ContainerPort: 6379}},
		},
	}
}

func assertContainerSet(t *testing.T, list []protocol.Discovered, digest string) {
	t.Helper()
	var web, redis *protocol.Discovered
	for i := range list {
		switch list[i].Name {
		case "web":
			web = &list[i]
		case "redis":
			redis = &list[i]
		case "port-3031":
			t.Fatal("published port also reported as unmapped")
		}
	}
	if web == nil || web.Kind != "container" || web.Owner == nil || web.Owner.Kind != "image" || web.Owner.Project != "acme" || web.Owner.Entry != "web" {
		t.Fatalf("web %+v", web)
	}
	if web.ContainerName == nil || *web.ContainerName != "acme-web" || web.ImageDigest == nil || *web.ImageDigest != digest {
		t.Fatalf("web identity %+v", web)
	}
	if redis == nil || redis.Kind != "container" || redis.Owner != nil || redis.ImageDigest != nil {
		t.Fatalf("redis %+v", redis)
	}
	if redis.ContainerName == nil || *redis.ContainerName != "redis" {
		t.Fatalf("redis name %+v", redis)
	}
	raw, err := protocol.Marshal(*redis)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"image_digest":null`) || !strings.Contains(string(raw), `"owner":null`) {
		t.Fatalf("%s", raw)
	}
	if !hasService(list, "nginx") {
		t.Fatalf("nginx missing %+v", list)
	}
}

func writeMachineID(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "machine-id")
	body := "test-machine-id\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	id, ok := images.HostID(body)
	if !ok {
		t.Fatal("host id")
	}
	return path, id
}

func writeImageRunner(t *testing.T) string {
	t.Helper()
	body := "enabled = true\nrun_as = [\"joe\"]\ndirs = [\"/home/joe/workspace-SENTINEL-dir\"]\nexec_allow = [\"/usr/bin/pnpm-SENTINEL\"]\nallow_shell = false\nmax_programs = 20\nimages = [\"ghcr.io/acme/*\"]\nprojects = [\"acme\"]\n"
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
