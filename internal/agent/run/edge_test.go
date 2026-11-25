package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/config"
	"github.com/more-than-code/bot-agent/internal/agent/edge"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/tasks"
)

// A config in the generator's shape (agent-protocol v1.15).
const edgeTaskConfig = `{"admin":{"listen":"unix//run/edge/admin.sock"},"apps":{"http":{"servers":{"edge":{"listen":[":443"],"routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"127.0.0.1:3000"}]}],"match":[{"host":["daikou.online"]}]}]}}}}}`

const testImage = "docker.io/library/caddy@sha256:" + "abababababababababababababababababababababababababababababababab"

// pinTestImage stands in for the release digest while edge.PinnedImage is
// the placeholder (which keeps the edge off).
func pinTestImage(t *testing.T) {
	t.Helper()
	old := edge.Image
	edge.Image = testImage
	t.Cleanup(func() { edge.Image = old })
}

func edgeApplySpec() []byte {
	return []byte(`{"config":` + edgeTaskConfig + `,"config_hash":"` + edge.HashConfig([]byte(edgeTaskConfig)) + `"}`)
}

func edgeAgent(t *testing.T, on bool, fn func(string, []byte) ([]byte, error)) (*agent, *fakeHelper) {
	pinTestImage(t)
	h := &fakeHelper{fn: fn}
	return &agent{
		cfg:    config.Config{HostID: agentHost},
		policy: policy.Policy{Exists: true, Valid: true, Edge: on},
		opt:    Options{Helper: h, Log: stdLog(&bytes.Buffer{})},
	}, h
}

func edgeResult(t *testing.T, a *agent, kind string, spec []byte) protocol.TaskResult {
	t.Helper()
	rej, res, ok := a.execute(context.Background(), imageTask(kind, spec, 0))
	if !ok {
		t.Fatalf("rejected %s", rej.ErrorCode)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateAgent(raw); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return res
}

func payloadJSON(t *testing.T, res protocol.TaskResult) string {
	t.Helper()
	b, err := json.Marshal(res.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Each helper reply and how it reaches the server.
func TestEdgeTaskOutcomes(t *testing.T) {
	hash := edge.HashConfig([]byte(edgeTaskConfig))
	for _, tc := range []struct {
		name    string
		kind    string
		spec    []byte
		reply   string
		err     error
		state   string
		code    string
		payload string
	}{
		{"applied", edge.KindApply, edgeApplySpec(), `{"outcome":"applied","config_hash":"` + hash + `","image":"` + testImage + `"}`, nil,
			"succeeded", "", `{"config_hash":"` + hash + `","image":"` + testImage + `","outcome":"applied"}`},
		{"unchanged", edge.KindApply, edgeApplySpec(), `{"outcome":"unchanged","config_hash":"` + hash + `","image":"` + testImage + `"}`, nil,
			"succeeded", "", `{"config_hash":"` + hash + `","image":"` + testImage + `","outcome":"unchanged"}`},
		{"started", edge.KindApply, edgeApplySpec(), `{"outcome":"started","config_hash":"` + hash + `","image":"` + testImage + `"}`, nil,
			"succeeded", "", `{"config_hash":"` + hash + `","image":"` + testImage + `","outcome":"started"}`},
		{"port in use", edge.KindApply, edgeApplySpec(), `{"error":"port_in_use","detail":"port 443 is used by daikou-caddy"}`, nil,
			"failed", "port_in_use", `{"detail":"port 443 is used by daikou-caddy","outcome":"port_in_use"}`},
		{"invalid config", edge.KindApply, edgeApplySpec(), `{"error":"invalid_config","detail":"loading new config: bad"}`, nil,
			"failed", "invalid_config", `{"detail":"loading new config: bad","outcome":"invalid_config"}`},
		{"helper disabled", edge.KindApply, edgeApplySpec(), `{"error":"edge_disabled"}`, nil,
			"failed", "edge_disabled", `{"detail":"","outcome":"edge_disabled"}`},
		{"free text code", edge.KindApply, edgeApplySpec(), `{"error":"/etc/shadow"}`, nil,
			"failed", "helper_unavailable", `{"detail":"","outcome":"helper_unavailable"}`},
		{"unknown outcome", edge.KindApply, edgeApplySpec(), `{"outcome":"maybe","config_hash":"","image":""}`, nil,
			"failed", "helper_unavailable", `{"detail":"","outcome":"helper_unavailable"}`},
		{"extra field", edge.KindStop, []byte(`{}`), `{"outcome":"stopped","x":1}`, nil,
			"failed", "helper_unavailable", `{"detail":"","outcome":"helper_unavailable"}`},
		{"transport", edge.KindStop, []byte(`{}`), ``, errHelper,
			"failed", "helper_unavailable", `{"detail":"","outcome":"helper_unavailable"}`},
		{"stopped", edge.KindStop, []byte(`{}`), `{"outcome":"stopped"}`, nil,
			"succeeded", "", `{"outcome":"stopped"}`},
		{"absent", edge.KindStop, []byte(`{}`), `{"outcome":"absent"}`, nil,
			"succeeded", "", `{"outcome":"absent"}`},
		{"status running", edge.KindStatus, []byte(`{}`), `{"state":"running","image":"` + testImage + `","config_hash":"` + hash + `","started_at":"2026-11-17T10:00:00Z"}`, nil,
			"succeeded", "", `{"config_hash":"` + hash + `","image":"` + testImage + `","started_at":"2026-11-17T10:00:00Z","state":"running"}`},
		{"status absent", edge.KindStatus, []byte(`{}`), `{"state":"absent","image":"","config_hash":"","started_at":null}`, nil,
			"succeeded", "", `{"config_hash":"","image":"","started_at":null,"state":"absent"}`},
		{"status conflict", edge.KindStatus, []byte(`{}`), `{"error":"conflict"}`, nil,
			"failed", "conflict", `{"detail":"","outcome":"conflict"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var verb string
			var body []byte
			a, _ := edgeAgent(t, true, func(v string, b []byte) ([]byte, error) {
				verb, body = v, append([]byte(nil), b...)
				return []byte(tc.reply), tc.err
			})
			res := edgeResult(t, a, tc.kind, tc.spec)
			code := ""
			if res.ErrorCode != nil {
				code = *res.ErrorCode
			}
			if res.State != tc.state || code != tc.code || payloadJSON(t, res) != tc.payload {
				t.Fatalf("%s %q %s", res.State, code, payloadJSON(t, res))
			}
			want, _ := edge.VerbFor(tc.kind)
			if verb != want || !bytes.Equal(body, tc.spec) {
				t.Fatalf("helper got %q %s", verb, body)
			}
		})
	}
}

// A host whose policy has no edge key: edge_apply fails edge_disabled
// without a helper call; stop and status still go to the helper.
func TestEdgeApplyDisabledByPolicy(t *testing.T) {
	a, h := edgeAgent(t, false, func(v string, b []byte) ([]byte, error) {
		return []byte(`{"outcome":"absent"}`), nil
	})
	res := edgeResult(t, a, edge.KindApply, edgeApplySpec())
	if res.State != "failed" || *res.ErrorCode != edge.ReasonDisabled || len(h.Calls()) != 0 {
		t.Fatalf("%+v %v", res, h.Calls())
	}
	res = edgeResult(t, a, edge.KindStop, []byte(`{}`))
	if res.State != "succeeded" || !h.saw(edge.VerbStop) {
		t.Fatalf("%+v", res)
	}
}

// Spec checks run before admission: a bad spec or a service is task_reject
// bad_spec, and an unknown edge-like kind is forbidden as before.
func TestEdgeTaskRejects(t *testing.T) {
	a, h := edgeAgent(t, true, nil)
	svc := "web"
	for _, tc := range []struct {
		kind    string
		spec    string
		service *string
		code    string
	}{
		{edge.KindApply, `{"config":{},"config_hash":"` + strings.Repeat("0", 64) + `"}`, nil, "bad_spec"},
		{edge.KindApply, `{"config":` + edgeTaskConfig + `}`, nil, "bad_spec"},
		{edge.KindStop, `{"x":1}`, nil, "bad_spec"},
		{edge.KindStatus, `{}`, &svc, "bad_spec"},
		{"edge_restart", `{}`, nil, "forbidden"},
	} {
		task := imageTask(tc.kind, []byte(tc.spec), 0)
		task.Service = tc.service
		rej, _, ok := a.execute(context.Background(), task)
		if ok || rej.ErrorCode != tc.code {
			t.Fatalf("%s %s: %v %q", tc.kind, tc.spec, ok, rej.ErrorCode)
		}
	}
	if len(h.Calls()) != 0 {
		t.Fatal(h.Calls())
	}
	code, _ := tasks.Decide(imageTask(edge.KindApply, edgeApplySpec(), 0), agentHost, time.Now(), nil, tasks.Gate{})
	if code != "" {
		t.Fatalf("zero gate refused a valid apply: %s", code)
	}
}

// The helper frame keeps the request bytes: no HTML escaping, no reorder.
func TestMarshalFrameKeepsBytes(t *testing.T) {
	for _, spec := range [][]byte{edgeApplySpec(), []byte(`{"a":"<&>","b":[1,2]}`)} {
		frame, err := marshalFrame(edge.VerbApply, spec)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"verb":"edge.apply","request":` + string(spec) + `}`; string(frame) != want {
			t.Fatalf("%s", frame)
		}
	}
}

// v1.15 item 2: hello.programs.edge mirrors the key and names the pinned
// image, and the frame passes the validator.
func TestHelloEdge(t *testing.T) {
	pinTestImage(t)
	for _, on := range []bool{true, false} {
		a := &agent{opt: Options{Log: stdLog(&bytes.Buffer{})}}
		a.policy = policy.Policy{Exists: true, Valid: true, Edge: on}
		p, caps := a.helloPrograms()
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		want := `"env_hash":true,"edge":{"enabled":` + map[bool]string{true: "true", false: "false"}[on] + `,"image":"` + edge.Image + `"},"host_files":[],"agent_upgrade":false}`
		if !strings.HasSuffix(string(b), want) {
			t.Fatalf("%s", b)
		}
		hello := protocol.Hello{
			V: protocol.Version, Type: "hello", HostID: "h", CredentialID: "c", AgentVersion: protocol.AgentVersion,
			OS: "linux", Arch: "arm64", Capabilities: caps, Programs: p,
			Services: []protocol.Discovered{}, ServicesComplete: true,
		}
		raw, _ := json.Marshal(hello)
		if err := protocol.ValidateAgent(raw); err != nil {
			t.Fatal(err)
		}
	}
	// An invalid policy with the key never says enabled.
	a := &agent{opt: Options{Log: stdLog(&bytes.Buffer{})}}
	a.policy = policy.Policy{Exists: true, Edge: true}
	if p, _ := a.helloPrograms(); p.Edge == nil || p.Edge.Enabled {
		t.Fatalf("%+v", p.Edge)
	}
}

// The validator accepts only the edge row shape for owner kind edge.
func TestValidateEdgeOwner(t *testing.T) {
	good := protocol.Discovered{
		Name: "deploybot-edge", Kind: "container", Ports: []int{80, 443},
		ContainerName: protocol.Str("deploybot-edge"), Owner: &protocol.Owner{Kind: "edge"},
	}
	check := func(d protocol.Discovered) error {
		raw, _ := json.Marshal(protocol.Services{V: protocol.Version, Type: "services", Services: []protocol.Discovered{d}})
		return protocol.ValidateAgent(raw)
	}
	if err := check(good); err != nil {
		t.Fatal(err)
	}
	withProject := good
	withProject.Owner = &protocol.Owner{Kind: "edge", Project: "acme"}
	renamed := good
	renamed.Name = "edge"
	if check(withProject) == nil || check(renamed) == nil {
		t.Fatal("bad edge row accepted")
	}
}

// container_list runs for discovery on a host with only the edge on.
func TestDiscoverEdgeOnlyLists(t *testing.T) {
	a, h := edgeAgent(t, true, func(v string, b []byte) ([]byte, error) {
		if v == images.VerbList {
			return []byte(`{"items":[],"truncated":false}`), nil
		}
		return nil, errors.New(v)
	})
	a.opt.ProcRoot = t.TempDir()
	a.discover(context.Background())
	if !h.saw(images.VerbList) {
		t.Fatal("no container_list")
	}
}

// M4: with the placeholder digest, hello says enabled false and apply is
// edge_disabled without a helper call.
func TestEdgePlaceholderDigestOff(t *testing.T) {
	a, h := edgeAgent(t, true, nil)
	edge.Image = "docker.io/library/caddy@sha256:" + strings.Repeat("0", 64)
	if p, _ := a.helloPrograms(); p.Edge == nil || p.Edge.Enabled {
		t.Fatalf("%+v", p.Edge)
	}
	res := edgeResult(t, a, edge.KindApply, edgeApplySpec())
	if *res.ErrorCode != edge.ReasonDisabled || len(h.Calls()) != 0 {
		t.Fatalf("%+v", res)
	}
}
