package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestImageUnchangedRequiresAll(t *testing.T) {
	otherDigest := "sha256:" + strings.Repeat("cd", 32)
	otherHash := strings.Repeat("ab", 32)
	cases := []struct {
		name string
		pre  images.ImageStatus
		hash string
	}{
		{"other digest", runningDigest(otherDigest), imgHash},
		{"other hash", runningStatus(0), otherHash},
		{"exited", exitedStatus(0), imgHash},
		{"paused", stateStatus("paused"), imgHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h, _, seq := imageRun(t)
			seq.pre = tc.pre
			seq.preHash = tc.hash
			seq.post = runningStatus(0)
			res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
			m := assertDeploy(t, res, "succeeded", "succeeded")
			if m["outcome"] == "unchanged" || !seq.deployed {
				t.Fatalf("unchanged %v calls %v", m["outcome"], h.Calls())
			}
		})
	}
}

func TestImageStatusErrorSkipsRollback(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.waitErr = true
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		assertDeploy(t, res, "failed", "failed")
		if h.saw(images.VerbRollback) {
			t.Fatalf("rolled back %v", h.Calls())
		}
	})
	t.Run("docker_unavailable", func(t *testing.T) {
		a, h, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.waitCode = "docker_unavailable"
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		assertDeploy(t, res, "failed", "failed")
		if h.saw(images.VerbRollback) {
			t.Fatalf("rolled back %v", h.Calls())
		}
	})
}

func TestImageHTTPChecks(t *testing.T) {
	t.Run("200", func(t *testing.T) {
		port := httpProbe(t, http.StatusOK)
		a, _, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "web", Protocol: "http", Port: port, Path: "/health",
			ExpectStatusMin: 200, ExpectStatusMax: 299,
		}}
		res := mustImage(t, a, "image_deploy", serviceSpec(true, 30, port), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if clock.sleeps() != 0 {
			t.Fatalf("sleeps %d", clock.sleeps())
		}
	})
	t.Run("500", func(t *testing.T) {
		port := httpProbe(t, http.StatusInternalServerError)
		a, _, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "web", Protocol: "http", Port: port, Path: "/health",
			ExpectStatusMin: 200, ExpectStatusMax: 299,
		}}
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, port), 6)
		assertDeploy(t, res, "rolled_back", "failed")
		if clock.sleeps() != 3 {
			t.Fatalf("sleeps %d", clock.sleeps())
		}
	})
	t.Run("other service uses the timer", func(t *testing.T) {
		a, _, clock, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "api", Protocol: "tcp", Port: 3031,
		}}
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if clock.sleeps() != 3 {
			t.Fatalf("sleeps %d", clock.sleeps())
		}
	})
}

func TestImagePausedWaitsOutTheTask(t *testing.T) {
	for _, state := range []string{"paused", "created"} {
		t.Run(state, func(t *testing.T) {
			a, h, clock, seq := imageRun(t)
			start := clock.now()
			seq.pre = missingStatus()
			seq.post = stateStatus(state)
			res := mustImage(t, a, "image_deploy", serviceSpec(false, 5, 3031), 120)
			assertDeploy(t, res, "rolled_back", "failed")
			if clock.sleeps() != 60 || !clock.now().Equal(start.Add(120*time.Second)) {
				t.Fatalf("sleeps %d now %s calls %v", clock.sleeps(), clock.now(), h.Calls())
			}
			if !h.saw(images.VerbRollback) {
				t.Fatal("no rollback")
			}
		})
	}
}

func TestJobCancelIsNotTimeout(t *testing.T) {
	a, h, _, seq := imageRun(t)
	seq.pre = missingStatus()
	seq.post = runningStatus(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.opt.Sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	_, res, ok := a.execute(ctx, imageTask("image_deploy", jobSpec(10), 120))
	if !ok {
		t.Fatal("rejected")
	}
	assertDeploy(t, res, "failed", "failed")
	if h.saw(images.VerbStop) {
		t.Fatalf("stopped %v", h.Calls())
	}
}

func TestDeployTailRedactsSpaceAndURL(t *testing.T) {
	const space = "correct horse"
	const urlSecret = "p@ssword extra"
	spec := []byte(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","ports":[{"host":3031,"container":3031,"public":false}],"healthy_after_sec":5},"env":{"PASSWORD":"` + space + `","DATABASE_URL":"` + urlSecret + `"},"registry_auth":null,"force":true,"spec_hash":"` + imgHash + `"}`)
	a, _, _, seq := imageRun(t)
	var buf bytes.Buffer
	a.opt.Log = stdLog(&buf)
	seq.pre = missingStatus()
	seq.post = runningStatus(0)
	seq.logs = []string{
		"password=" + space,
		"mongodb://app:" + urlSecret + "@db.internal/prod",
	}
	res := mustImage(t, a, "image_deploy", spec, 120)
	raw := marshalPayload(t, res.Payload)
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	lines, _ := payload["lines"].([]any)
	for _, line := range lines {
		s, _ := line.(string)
		text.WriteString(s)
		text.WriteByte('\n')
	}
	blob := text.String() + buf.String()
	for _, frag := range []string{space, "horse", urlSecret, "@ssword"} {
		if strings.Contains(blob, frag) {
			t.Fatalf("kept %q in %s", frag, blob)
		}
	}
	if len(seq.logReqs) != 1 {
		t.Fatalf("log calls %d", len(seq.logReqs))
	}
	got := map[string]bool{}
	for _, v := range seq.logReqs[0].Redact {
		got[v] = true
	}
	if !got[space] || !got[urlSecret] {
		t.Fatalf("redact %v", seq.logReqs[0].Redact)
	}
	if strings.Contains(blob, `"redact"`) {
		t.Fatal("redact list in the result")
	}
}

func TestWideImageSpecIsDeployed(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","healthy_after_sec":5},"env":{`)
	val := strings.Repeat("a", 4096)
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"K%02d":"%s"`, i, val)
	}
	b.WriteString(`},"registry_auth":null,"force":false,"spec_hash":"` + imgHash + `"}`)
	spec := []byte(b.String())
	if len(spec) <= programs.MaxFrame || len(spec) > images.MaxFrame {
		t.Fatalf("spec %d", len(spec))
	}
	a, h, _, seq := imageRun(t)
	seq.pre = missingStatus()
	seq.post = runningStatus(0)
	res := mustImage(t, a, "image_deploy", spec, 120)
	assertDeploy(t, res, "succeeded", "succeeded")
	if !seq.deployed || !h.saw(images.VerbDeploy) {
		t.Fatalf("calls %v deployed %v", h.Calls(), seq.deployed)
	}
}

func TestImageTimeoutNoWrap(t *testing.T) {
	ceiling := time.Duration(maxImageTimeoutSec) * time.Second
	if imageTimeout(0) != 15*time.Second || imageTimeout(120) != 120*time.Second {
		t.Fatal("frame timeout")
	}
	if imageTimeout(maxImageTimeoutSec) != ceiling || imageTimeout(maxImageTimeoutSec+1) != ceiling {
		t.Fatal("clamp")
	}
	if imageTimeout(int(^uint(0)>>1)) != ceiling {
		t.Fatal("wrapped or unclamped")
	}
}

func httpProbe(t *testing.T, status int) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func runningDigest(digest string) images.ImageStatus {
	d := digest
	return images.ImageStatus{
		Container: protocol.Str("acme-web"), State: "running", Digest: &d, Ports: []int{3031},
	}
}

func stateStatus(state string) images.ImageStatus {
	d := imgDigest
	return images.ImageStatus{
		Container: protocol.Str("acme-web"), State: state, Digest: &d, Ports: []int{3031},
	}
}
