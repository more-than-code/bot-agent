package run

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/config"
	"github.com/more-than-code/bot-agent/internal/agent/dockerusage"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

const dfReply = `{"summary":{"totals":{` +
	`"images":{"count":2,"active":1,"size":900,"reclaimable":400},` +
	`"containers":{"count":1,"active":1,"size":10,"reclaimable":0},` +
	`"volumes":{"count":1,"active":0,"size":5,"reclaimable":5},` +
	`"build_cache":{"count":0,"active":0,"size":0,"reclaimable":0}},` +
	`"images":[{"ref":"ghcr.io/o/app:1.2","id":"0123456789ab","size":500,"containers":1},{"ref":"<none>","id":"ba9876543210","size":null,"containers":0}],` +
	`"volumes":[{"name":"pgdata","size":5,"in_use":false}],` +
	`"containers":[{"name":"deploybot-api","state":"running","size":10}]}}`

func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		t.Fatalf("json %s / %s", a, b)
	}
	return reflect.DeepEqual(x, y)
}

func dfAgent(t *testing.T, pol policy.Policy, fn func(string, []byte) ([]byte, error)) (*agent, *fakeHelper) {
	t.Helper()
	h := &fakeHelper{fn: fn}
	return &agent{
		cfg:    config.Config{HostID: agentHost},
		policy: pol,
		opt:    Options{Helper: h, Log: stdLog(&bytes.Buffer{})},
	}, h
}

var dfImagesOn = policy.Policy{Exists: true, Valid: true, Enabled: true, Images: []string{"ghcr.io/o/*"}, Projects: []string{"acme"}}

func TestDockerUsageTaskResults(t *testing.T) {
	summary := `{"containers":[{"name":"deploybot-api","size":10,"state":"running"}],` +
		`"images":[{"containers":1,"id":"0123456789ab","ref":"ghcr.io/o/app:1.2","size":500},{"containers":0,"id":"ba9876543210","ref":"<none>","size":null}],` +
		`"totals":{"build_cache":{"active":0,"count":0,"reclaimable":0,"size":0},"containers":{"active":1,"count":1,"reclaimable":0,"size":10},` +
		`"images":{"active":1,"count":2,"reclaimable":400,"size":900},"volumes":{"active":0,"count":1,"reclaimable":5,"size":5}},` +
		`"volumes":[{"in_use":false,"name":"pgdata","size":5}]}`
	for _, tc := range []struct {
		name    string
		pol     policy.Policy
		reply   string
		err     error
		state   string
		payload string
	}{
		{"ok", dfImagesOn, dfReply, nil, "succeeded", `{"outcome":"ok","summary":` + summary + `}`},
		{"edge only", policy.Policy{Exists: true, Valid: true, Edge: true}, dfReply, nil, "succeeded", `{"outcome":"ok","summary":` + summary + `}`},
		{"disabled", dfImagesOn, `{"error":"docker_usage_disabled"}`, nil, "failed", `{"outcome":"failed","reason":"docker_usage_disabled"}`},
		{"docker", dfImagesOn, `{"error":"docker_unavailable"}`, nil, "failed", `{"outcome":"failed","reason":"docker_unavailable"}`},
		{"timeout", dfImagesOn, `{"error":"timeout"}`, nil, "failed", `{"outcome":"failed","reason":"timeout"}`},
		{"other code", dfImagesOn, `{"error":"bad_spec"}`, nil, "failed", `{"outcome":"failed","reason":"docker_unavailable"}`},
		{"free text", dfImagesOn, `{"error":"/etc/shadow"}`, nil, "failed", `{"outcome":"failed","reason":"docker_unavailable"}`},
		{"extra key", dfImagesOn, `{"summary":{},"x":1}`, nil, "failed", `{"outcome":"failed","reason":"docker_unavailable"}`},
		{"transport", dfImagesOn, ``, errHelper, "failed", `{"outcome":"failed","reason":"docker_unavailable"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var verb string
			var body []byte
			a, _ := dfAgent(t, tc.pol, func(v string, b []byte) ([]byte, error) {
				verb, body = v, b
				return []byte(tc.reply), tc.err
			})
			res := edgeResult(t, a, dockerusage.Kind, []byte(`{}`))
			if verb != dockerusage.Verb || string(body) != `{}` {
				t.Fatalf("helper %q %s", verb, body)
			}
			if res.State != tc.state || !sameJSON(t, payloadJSON(t, res), tc.payload) {
				t.Fatalf("%s %s", res.State, payloadJSON(t, res))
			}
			if tc.state == "failed" {
				if res.ErrorCode == nil || *res.ErrorCode != res.Payload["reason"] {
					t.Fatalf("error_code %v", res.ErrorCode)
				}
			} else if res.ErrorCode != nil {
				t.Fatal("error_code on success")
			}
		})
	}
}

// Neither images nor edge: refused without asking the helper (a host
// without the helper gets the same answer).
func TestDockerUsageDisabledByPolicy(t *testing.T) {
	a, h := dfAgent(t, policy.Policy{Exists: true, Valid: true}, func(string, []byte) ([]byte, error) {
		return []byte(dfReply), nil
	})
	res := edgeResult(t, a, dockerusage.Kind, []byte(`{}`))
	if res.State != "failed" || payloadJSON(t, res) != `{"outcome":"failed","reason":"docker_usage_disabled"}` || len(h.Calls()) != 0 {
		t.Fatalf("%s %s %v", res.State, payloadJSON(t, res), h.Calls())
	}
}

func TestDockerUsageSpecRejected(t *testing.T) {
	a, h := dfAgent(t, dfImagesOn, nil)
	rej, _, ok := a.execute(context.Background(), imageTask(dockerusage.Kind, []byte(`{"all":true}`), 45))
	if ok || rej.ErrorCode != "bad_spec" || len(h.Calls()) != 0 {
		t.Fatalf("%v %+v", ok, rej)
	}
}

// slowHelper answers only when its context ends.
type slowHelper struct{}

func (slowHelper) Call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The frame's timeout_sec, 45 s when unset, at most 60 s. A helper call cut
// off by the deadline is reason timeout.
func TestDockerUsageTimeout(t *testing.T) {
	for sec, want := range map[int]time.Duration{45: 45 * time.Second, 0: 45 * time.Second, -3: 45 * time.Second, 600: 60 * time.Second, 5: 5 * time.Second} {
		if got := dockerUsageTimeout(sec); got != want {
			t.Fatalf("%d: %v", sec, got)
		}
	}
	a := &agent{cfg: config.Config{HostID: agentHost}, policy: dfImagesOn, opt: Options{Helper: slowHelper{}, Log: stdLog(&bytes.Buffer{})}}
	_, res, ok := a.execute(context.Background(), imageTask(dockerusage.Kind, []byte(`{}`), 1))
	if !ok || res.State != "failed" || payloadJSON(t, res) != `{"outcome":"failed","reason":"timeout"}` {
		t.Fatalf("%s %s", res.State, payloadJSON(t, res))
	}
}

// A parent that ends first (the session closing, or its own deadline) is
// not this task's timeout.
func TestDockerUsageParentGone(t *testing.T) {
	a := &agent{cfg: config.Config{HostID: agentHost}, policy: dfImagesOn, opt: Options{Helper: slowHelper{}, Log: stdLog(&bytes.Buffer{})}}
	for _, mk := range []func() (context.Context, context.CancelFunc){
		func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(50*time.Millisecond, cancel)
			return ctx, cancel
		},
		func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		},
	} {
		ctx, cancel := mk()
		res := a.runDockerUsage(ctx, imageTask(dockerusage.Kind, []byte(`{}`), 45))
		cancel()
		if res.State != "failed" || payloadJSON(t, res) != `{"outcome":"failed","reason":"docker_unavailable"}` {
			t.Fatalf("%s %s", res.State, payloadJSON(t, res))
		}
	}
}
