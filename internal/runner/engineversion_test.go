package runner

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/edge"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// versionEnv is an image env whose fake daemon answers /_ping with api.
func versionEnv(t *testing.T, policy, api string) (*imgEnv, *logSink) {
	t.Helper()
	e := newImgEnv(t, policy)
	e.fake.api = api
	sink := &logSink{}
	e.sys.Logf = sink.logf
	return e, sink
}

// assertSpoken checks that the first call is the ping and every other call
// uses /v<want>.
func assertSpoken(t *testing.T, f *fakeDocker, want string) {
	t.Helper()
	calls := afterPing(t, f.Calls())
	if len(calls) == 0 {
		t.Fatal("no engine call after the ping")
	}
	for _, c := range calls {
		if !strings.HasPrefix(c.Path, "/v"+want+"/") {
			t.Fatalf("%s %s, want /v%s", c.Method, c.Path, want)
		}
	}
}

// v1.17 item 1: every verb speaks min(1.47, daemon) after one ping per run.
func TestNegotiatedVersionEveryVerb(t *testing.T) {
	for _, tc := range []struct{ daemon, spoken string }{
		{"1.41", "1.41"}, {"1.43", "1.43"}, {"1.44", "1.44"}, {"1.47", "1.47"}, {"1.52", "1.47"},
	} {
		t.Run(tc.daemon, func(t *testing.T) {
			e, _ := versionEnv(t, imagePolicy, tc.daemon)
			entry := mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
			steps := []struct {
				verb string
				body []byte
			}{
				{images.VerbList, []byte(`{}`)},
				{images.VerbPull, mustJSON(t, images.PullRequest{Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1"})},
				{images.VerbDeploy, mustJSON(t, serviceDeploy(hashC))},
				{images.VerbStatus, entry},
				{images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 10})},
				{images.VerbStop, entry},
				{images.VerbStart, entry},
				{images.VerbRestart, entry},
				{images.VerbRemove, entry},
			}
			for _, s := range steps {
				before := len(e.fake.Calls())
				raw := e.call(s.verb, s.body)
				if code := imgCode(t, raw); code != "" && code != programs.ReasonConflict {
					t.Fatalf("%s: %s", s.verb, raw)
				}
				calls := e.fake.Calls()[before:]
				sub := &fakeDocker{calls: calls}
				assertSpoken(t, sub, tc.spoken)
			}
		})
	}
}

// v1.17 item 1: a daemon below 1.41 is docker_unavailable, after the ping
// and nothing else, with a line naming both versions.
func TestVersionBelowFloor(t *testing.T) {
	e, sink := versionEnv(t, imagePolicy, "1.40")
	for _, verb := range []string{images.VerbList, images.VerbDeploy} {
		body := []byte(`{}`)
		if verb == images.VerbDeploy {
			body = mustJSON(t, serviceDeploy(hashC))
		}
		before := len(e.fake.Calls())
		if code := imgCode(t, e.call(verb, body)); code != programs.ReasonDocker {
			t.Fatalf("%s: %s", verb, code)
		}
		calls := e.fake.Calls()[before:]
		if len(calls) != 1 || calls[0].Path != "/_ping" {
			t.Fatalf("%s: %v", verb, calls)
		}
	}
	if !sink.has("bot-agent-runner: docker API 1.40 is older than the minimum 1.41 (Docker 20.10)") {
		t.Fatalf("log %v", sink.lines)
	}
}

func TestVersionPingFailures(t *testing.T) {
	e, sink := versionEnv(t, imagePolicy, "1.47")
	e.fake.pingNoHeader = true
	if code := imgCode(t, e.call(images.VerbList, []byte(`{}`))); code != "" {
		t.Fatal(code)
	}
	assertSpoken(t, e.fake, "1.41")
	if !sink.has("bot-agent-runner: docker: no usable API-Version header; assuming 1.41") {
		t.Fatalf("log %v", sink.lines)
	}

	e, sink = versionEnv(t, imagePolicy, "1.47")
	e.fake.pingCode = http.StatusInternalServerError
	if code := imgCode(t, e.call(images.VerbList, []byte(`{}`))); code != programs.ReasonDocker {
		t.Fatal(code)
	}
	if n := len(e.fake.Calls()); n != 1 {
		t.Fatalf("%d calls", n)
	}
	if !sink.has("bot-agent-runner: docker: GET /_ping: status 500") {
		t.Fatalf("log %v", sink.lines)
	}

	e, sink = versionEnv(t, imagePolicy, "1.47")
	e.sys.DockerSock = filepath.Join(t.TempDir(), "none.sock")
	if code := imgCode(t, e.call(images.VerbList, []byte(`{}`))); code != programs.ReasonDocker {
		t.Fatal(code)
	}
	if !sink.has("bot-agent-runner: docker: GET /_ping: no answer") {
		t.Fatalf("log %v", sink.lines)
	}
}

// v1.17 item 2: below 1.44 network create asks Docker to refuse a duplicate
// name; from 1.44 the body is as before.
func TestNetworkCreateCheckDuplicate(t *testing.T) {
	for _, tc := range []struct {
		api  string
		want bool
	}{{"1.41", true}, {"1.43", true}, {"1.44", false}, {"1.52", false}} {
		e, _ := versionEnv(t, imagePolicy, tc.api)
		if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))); code != "" {
			t.Fatalf("%s: %s", tc.api, code)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(e.fake.body(http.MethodPost, "/networks/create"), &body); err != nil {
			t.Fatal(err)
		}
		dup, ok := body["CheckDuplicate"]
		if tc.want && (!ok || string(dup) != "true") {
			t.Fatalf("%s: %v", tc.api, body)
		}
		if !tc.want && ok {
			t.Fatalf("%s: sent CheckDuplicate", tc.api)
		}
	}
}

// v1.17 item 2: a deploy with shared networks below 1.44 is refused after
// the ping and before any other Engine call; the line names both versions.
func TestSharedNetworksNeedDocker25(t *testing.T) {
	for _, api := range []string{"1.41", "1.43"} {
		e, sink := versionEnv(t, sharedPolicy, api)
		cur := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
		raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
		if string(raw) != `{"error":"`+programs.ReasonNetworksDocker+`"}` {
			t.Fatalf("%s: %s", api, raw)
		}
		if calls := e.fake.Calls(); len(calls) != 1 || calls[0].Path != "/_ping" {
			t.Fatalf("%s: %v", api, calls)
		}
		if currentByID(e, cur) == nil || e.fake.ctrs["mohiguide-web"].state != "running" {
			t.Fatal("current container touched")
		}
		if !sink.has("bot-agent-runner: shared networks need Docker 25 (API 1.44); this host speaks API " + api) {
			t.Fatalf("log %v", sink.lines)
		}
		// Without networks the same host deploys.
		if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashD)))); code != "" {
			t.Fatalf("%s: plain deploy %s", api, code)
		}
	}
	for _, api := range []string{"1.44", "1.47", "1.52"} {
		e, _ := versionEnv(t, sharedPolicy, api)
		if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))); code != "" {
			t.Fatalf("%s: %s", api, code)
		}
	}
}

// v1.17 item 4: an Engine failure is logged with method, path without the
// query, status, the message and the negotiated version.
func TestEngineFailureLogged(t *testing.T) {
	e, sink := versionEnv(t, imagePolicy, "1.43")
	e.fake.netCreateFail = "deploybot-mohiguide"
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))); code != programs.ReasonDocker {
		t.Fatal(code)
	}
	if !sink.has(`bot-agent-runner: docker: POST /networks/create: status 500 (API 1.43): "boom"`) {
		t.Fatalf("log %v", sink.lines)
	}
	for _, l := range sink.lines {
		if strings.Contains(l, "?") || strings.Contains(l, "production") {
			t.Fatalf("query or env in log: %q", l)
		}
	}

	e, sink = versionEnv(t, imagePolicy, "1.47")
	e.fake.pullCode = http.StatusUnauthorized
	token := "registry-token-SENTINEL"
	raw := e.call(images.VerbPull, mustJSON(t, images.PullRequest{
		Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1",
		RegistryAuth: &images.RegistryAuth{Host: "ghcr.io", Username: "bot", Token: token},
	}))
	if imgCode(t, raw) != programs.ReasonImage {
		t.Fatal(string(raw))
	}
	if !sink.has(`bot-agent-runner: docker: POST /images/create: status 401 (API 1.47): ""`) {
		t.Fatalf("log %v", sink.lines)
	}
}

func TestEngineMessageRedactedAndCut(t *testing.T) {
	long := strings.Repeat("é", 400)
	got := engineMessage(mustJSON(t, map[string]string{"message": long}), nil)
	if len(got) > maxEngineMessage+2 || !strings.HasPrefix(got, `"é`) {
		t.Fatalf("%d %q", len(got), got)
	}
	got = engineMessage([]byte(`{"message":"bad env SECRET-VALUE-123 and token SENTINEL-TOK\nnext"}`), []string{"SECRET-VALUE-123", "SENTINEL-TOK"})
	if strings.Contains(got, "SECRET-VALUE-123") || strings.Contains(got, "SENTINEL-TOK") || strings.Contains(got, "\n") {
		t.Fatalf("%q", got)
	}
	if v := createEnvValues([]byte(`{"Env":["A=b1","NODE_ENV=production","EMPTY="]}`)); len(v) != 2 || v[1] != "production" {
		t.Fatalf("%v", v)
	}
}

// L3: an Engine message on pull or create that echoes the request's
// token or an env value never reaches the log. Long values are redacted;
// with a value shorter than minScrubbed the message is withheld.
func TestEngineMessageSecretsNotLogged(t *testing.T) {
	pull := func(t *testing.T, token string) *logSink {
		t.Helper()
		e, sink := versionEnv(t, imagePolicy, "1.47")
		e.fake.pullCode = http.StatusInternalServerError
		e.fake.pullMsg = "auth " + token + " rejected"
		e.call(images.VerbPull, mustJSON(t, images.PullRequest{
			Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1",
			RegistryAuth: &images.RegistryAuth{Host: "ghcr.io", Username: "bot", Token: token},
		}))
		for _, l := range sink.lines {
			if strings.Contains(l, token) {
				t.Fatalf("token in log: %q", l)
			}
		}
		return sink
	}
	if sink := pull(t, "registry-token-SENTINEL"); !sink.has(`bot-agent-runner: docker: POST /images/create: status 500 (API 1.47): "auth [REDACTED] rejected"`) {
		t.Fatalf("log %v", sink.lines)
	}
	if sink := pull(t, "tk9"); !sink.has(`bot-agent-runner: docker: POST /images/create: status 500 (API 1.47): message withheld`) {
		t.Fatalf("log %v", sink.lines)
	}

	create := func(t *testing.T, env map[string]string, echo string) *logSink {
		t.Helper()
		e, sink := versionEnv(t, imagePolicy, "1.47")
		e.fake.createFailMsg = "bad env " + echo
		req := serviceDeploy(hashC)
		req.Env = env
		if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code == "" {
			t.Fatal("create failure succeeded")
		}
		for _, l := range sink.lines {
			if strings.Contains(l, echo) {
				t.Fatalf("env value in log: %q", l)
			}
		}
		return sink
	}
	if sink := create(t, map[string]string{"DB_URL": "env-secret-VALUE"}, "env-secret-VALUE"); !sink.has(`bot-agent-runner: docker: POST /containers/create: status 500 (API 1.47): "bad env [REDACTED]"`) {
		t.Fatalf("log %v", sink.lines)
	}
	if sink := create(t, map[string]string{"DB_URL": "env-secret-VALUE", "PIN": "s3k"}, "s3k"); !sink.has(`bot-agent-runner: docker: POST /containers/create: status 500 (API 1.47): message withheld`) {
		t.Fatalf("log %v", sink.lines)
	}
}

// L4: only MAJOR.MINOR with 1-3 digits each parses.
func TestParseAPIVersion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want apiVersion
		ok   bool
	}{
		{"1.9", apiVersion{1, 9}, true},
		{"1.41", apiVersion{1, 41}, true},
		{"", apiVersion{}, false},
		{"v1.41", apiVersion{}, false},
		{"1.4.1", apiVersion{}, false},
		{"2.0", apiVersion{2, 0}, true},
		{"1.041", apiVersion{1, 41}, true},
		{"1.47", apiVersion{1, 47}, true},
		{"1.52", apiVersion{1, 52}, true},
	} {
		got, ok := parseAPIVersion(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: %v %t, want %v %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	// 1.9 is below the floor, 2.0 above the ceiling.
	if v, _ := parseAPIVersion("1.9"); !v.less(apiMin) {
		t.Fatal("1.9 not below 1.41")
	}
	if v, _ := parseAPIVersion("2.0"); !apiMax.less(v) {
		t.Fatal("2.0 not above 1.47")
	}
}

// L1: the ping has its own deadline; a daemon that never answers it is
// docker_unavailable well before the caller's deadline.
func TestPingDeadline(t *testing.T) {
	old := pingTimeout
	pingTimeout = 100 * time.Millisecond
	t.Cleanup(func() { pingTimeout = old })
	e, sink := versionEnv(t, imagePolicy, "1.47")
	e.fake.pingDelay = 2 * time.Second
	start := time.Now()
	if code := imgCode(t, e.call(images.VerbList, []byte(`{}`))); code != programs.ReasonDocker {
		t.Fatal(code)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %s", d)
	}
	if !sink.has("bot-agent-runner: docker: GET /_ping: no answer") {
		t.Fatalf("log %v", sink.lines)
	}
}

// v1.17 item 5: docker_check needs no policy and reports the daemon.
func TestDockerCheckVerb(t *testing.T) {
	check := func(t *testing.T, sys Sys) DockerCheck {
		t.Helper()
		raw := Handle(t.Context(), programs.VerbDockerCheck, []byte(`{}`), sys)
		var out DockerCheck
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		for _, k := range []string{`"present"`, `"api"`, `"ok"`, `"networks_ok"`, `"reason"`} {
			if !strings.Contains(string(raw), k) {
				t.Fatalf("%s lacks %s", raw, k)
			}
		}
		return out
	}
	for _, tc := range []struct {
		api  string
		want DockerCheck
	}{
		{"1.40", DockerCheck{Present: true, API: "1.40", Reason: "too old"}},
		{"1.41", DockerCheck{Present: true, API: "1.41", OK: true, Reason: "ok"}},
		{"1.43", DockerCheck{Present: true, API: "1.43", OK: true, Reason: "ok"}},
		{"1.44", DockerCheck{Present: true, API: "1.44", OK: true, NetworksOK: true, Reason: "ok"}},
		{"1.52", DockerCheck{Present: true, API: "1.52", OK: true, NetworksOK: true, Reason: "ok"}},
	} {
		fake, sock := startFake(t)
		fake.api = tc.api
		// No policy file at all: the verb is not gated.
		sys := Sys{DockerSock: sock, Policy: filepath.Join(t.TempDir(), "missing.toml")}
		if got := check(t, sys); got != tc.want {
			t.Fatalf("%s: %+v", tc.api, got)
		}
		if calls := fake.Calls(); len(calls) != 1 || calls[0].Path != "/_ping" {
			t.Fatalf("%s: %v", tc.api, calls)
		}
	}

	fake, sock := startFake(t)
	fake.pingCode = http.StatusInternalServerError
	if got := check(t, Sys{DockerSock: sock}); got != (DockerCheck{Present: true, Reason: "not answering"}) {
		t.Fatalf("%+v", got)
	}
	if got := check(t, Sys{DockerSock: filepath.Join(t.TempDir(), "none.sock")}); got != (DockerCheck{Reason: "absent"}) {
		t.Fatalf("%+v", got)
	}
	// L5: no API-Version header: api empty, and the reason says 1.41 was
	// assumed.
	fake, sock = startFake(t)
	fake.pingNoHeader = true
	if got := check(t, Sys{DockerSock: sock}); got != (DockerCheck{Present: true, OK: true, Reason: "ok, API assumed 1.41"}) {
		t.Fatalf("%+v", got)
	}
	raw := Handle(t.Context(), programs.VerbDockerCheck, []byte(`{"x":1}`), Sys{DockerSock: sock})
	if !strings.Contains(string(raw), `"reason":"bad request"`) {
		t.Fatalf("%s", raw)
	}
}

// v1.17 item 1: the edge verbs negotiate too; below the floor they refuse.
func TestEdgeNegotiates(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	e.fake.api = "1.43"
	if r := edgeDecode(t, e.call(edge.VerbStatus, []byte(`{}`))); r.State != edge.StateAbsent {
		t.Fatalf("%+v", r)
	}
	assertSpoken(t, e.fake, "1.43")

	e = newEdgeEnv(t, edgePolicy)
	e.fake.api = "1.40"
	if r := edgeDecode(t, e.call(edge.VerbStatus, []byte(`{}`))); r.Error != edge.ReasonDocker {
		t.Fatalf("%+v", r)
	}
	if calls := e.fake.Calls(); len(calls) != 1 || calls[0].Path != "/_ping" {
		t.Fatalf("%v", calls)
	}
}
