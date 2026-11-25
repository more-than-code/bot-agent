package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// agent-protocol v1.21 items 3 and 4 in the helper: network "host" and the
// host env file merged at container_deploy.

const (
	hostEnvValue = "postgres://deploybot:verysecretpassword@127.0.0.1/deploybot"
	hostEnvLine  = `host_env_files = ["mohiguide/web=/etc/deploybot/api.env", "mohiguide/job=/etc/deploybot/api.env"]` + "\n" +
		`host_env_images = ["mohiguide/web=ghcr.io/joe-and-his-friends/mo-webadmin", "mohiguide/job=ghcr.io/joe-and-his-friends/mo-job"]` + "\n" +
		`host_env_command = ["mohiguide/web=serve", "mohiguide/job=run"]` + "\n"
	hostNetLine = `host_network_projects = ["mohiguide"]` + "\n"
)

// newHostEnv is an image env whose policy has a host env file for
// mohiguide/web and mohiguide/job, with the file owned by "root".
func newHostEnv(t *testing.T, extra string) *imgEnv {
	t.Helper()
	e := newImgEnv(t, imagePolicy+extra)
	e.sys.envOwner = func(os.FileInfo) (uint32, uint32, bool) { return 0, 0, true }
	e.writeHostEnv("DATABASE_URL="+hostEnvValue+"\nsecrets_key=0123456789abcdef\n", 0o600)
	return e
}

func (e *imgEnv) writeHostEnv(body string, mode os.FileMode) {
	e.t.Helper()
	path := filepath.Join(e.sys.Root, "etc", "deploybot", "api.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		e.t.Fatal(err)
	}
}

// hostEnvDeploy is serviceDeploy in the shape v1.26 allows for an entry
// with a host env file: the pinned command, the image's entrypoint and
// user, no mounts and no env of its own.
func hostEnvDeploy(hash string) images.DeployRequest {
	r := serviceDeploy(hash)
	r.Spec.Entrypoint, r.Spec.User, r.Spec.Volumes, r.Spec.Env, r.Env = nil, "", nil, nil, nil
	return r
}

func lastBody(f *fakeDocker, method, suffix string) []byte {
	var out []byte
	for _, c := range f.Calls() {
		if c.Method == method && strings.HasSuffix(c.Path, suffix) {
			out = c.Body
		}
	}
	return out
}

func hostNetSpec() images.ImageSpec {
	s := serviceSpec()
	host := images.NetworkHost
	s.Network = &host
	return s
}

func TestHostNetworkCreateBody(t *testing.T) {
	e := newImgEnv(t, imagePolicy+hostNetLine)
	req := serviceDeploy(hashC)
	req.Spec = hostNetSpec()
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != "" {
		t.Fatal(code)
	}
	body := e.fake.body(http.MethodPost, "/containers/create")
	var got struct {
		ExposedPorts map[string]any
		HostConfig   struct {
			NetworkMode  string
			PortBindings map[string]any
			Memory       int64
		}
		NetworkingConfig struct {
			EndpointsConfig map[string]any
		}
		User string
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.HostConfig.NetworkMode != "host" || got.HostConfig.PortBindings != nil || got.ExposedPorts != nil {
		t.Fatalf("%s", body)
	}
	if got.NetworkingConfig.EndpointsConfig == nil || len(got.NetworkingConfig.EndpointsConfig) != 0 {
		t.Fatalf("endpoints %s", body)
	}
	// User and memory limit unchanged.
	if got.User != "app" || got.HostConfig.Memory != 64*1024*1024 {
		t.Fatalf("%s", body)
	}
	// Without network the body still publishes on loopback.
	e2 := newImgEnv(t, imagePolicy+hostNetLine)
	if imgCode(t, e2.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))) != "" {
		t.Fatal("bridge deploy")
	}
	if b := e2.fake.body(http.MethodPost, "/containers/create"); !strings.Contains(string(b), `"NetworkMode":"deploybot-mohiguide"`) || !strings.Contains(string(b), `"HostIp":"127.0.0.1"`) {
		t.Fatalf("%s", b)
	}
}

func TestHostNetworkRefused(t *testing.T) {
	// Not listed: policy_denied before any Engine call.
	e := newImgEnv(t, imagePolicy)
	req := serviceDeploy(hashC)
	req.Spec = hostNetSpec()
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != programs.ReasonPolicy {
		t.Fatalf("got %q", code)
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatal("engine called")
	}
	// Host with shared networks: bad_spec.
	e = newImgEnv(t, imagePolicy+hostNetLine+"shared_networks = [\"dd=10.200.10.0/24\"]\nshared_network_projects = [\"dd:mohiguide\"]\n")
	req.Spec.Networks = []string{"dd"}
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != programs.ReasonBadSpec {
		t.Fatalf("got %q", code)
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatal("engine called")
	}
}

func createEnv(t *testing.T, body []byte) []string {
	t.Helper()
	var got struct{ Env []string }
	if json.Unmarshal(body, &got) != nil {
		t.Fatal(string(body))
	}
	return got.Env
}

func TestHostEnvMerged(t *testing.T) {
	for _, kind := range []string{"service", "job"} {
		t.Run(kind, func(t *testing.T) {
			var logs []string
			e := newHostEnv(t, hostEnvLine)
			e.sys.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			req := hostEnvDeploy(hashC)
			if kind == "job" {
				req = jobDeploy()
			}
			req.EnvHash = envHashA
			if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != "" {
				t.Fatal(code)
			}
			body := e.fake.body(http.MethodPost, "/containers/create")
			env := strings.Join(createEnv(t, body), "\n")
			if !strings.Contains(env, "DATABASE_URL="+hostEnvValue) || !strings.Contains(env, "secrets_key=0123456789abcdef") || len(createEnv(t, body)) != 2 {
				t.Fatalf("env %q", env)
			}
			labels := createLabels(t, body)
			if len(labels[images.LabelHostEnv]) != 64 || labels[images.LabelEnvHash] != envHashA {
				t.Fatalf("labels %v", labels)
			}
			if strings.Contains(strings.Join(logs, "\n"), hostEnvValue) {
				t.Fatal("value logged")
			}
		})
	}
	// An entry without a line gets no host env and no label.
	e := newHostEnv(t, `host_env_files = ["mohiguide/api=/etc/deploybot/api.env"]`+"\n"+`host_env_images = ["mohiguide/api=ghcr.io/joe-and-his-friends/mo-webadmin"]`+"\n")
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))) != "" {
		t.Fatal("deploy")
	}
	body := e.fake.body(http.MethodPost, "/containers/create")
	if strings.Contains(string(body), "DATABASE_URL") || createLabels(t, body)[images.LabelHostEnv] != "" {
		t.Fatalf("%s", body)
	}
}

// v1.26: an entry with a host env file may not set env of its own, so a
// key the entry also sets is refused as host_env_run env before the file
// is read (it was env_conflict <KEY> in v1.21). env_conflict remains for
// a file key on the denylist.
func TestHostEnvConflict(t *testing.T) {
	cases := map[string]func(*images.DeployRequest){
		"in env":      func(r *images.DeployRequest) { r.Env = map[string]string{"DATABASE_URL": "other-value-123"} },
		"in spec env": func(r *images.DeployRequest) { r.Spec.Env = map[string]string{"DATABASE_URL": "x"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var logs []string
			e := newHostEnv(t, hostEnvLine)
			e.sys.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			req := hostEnvDeploy(hashC)
			mutate(&req)
			raw := e.call(images.VerbDeploy, mustJSON(t, req))
			jsonEqual(t, raw, `{"error":"policy_denied","detail":"host_env_run","key":"env"}`)
			if len(e.fake.Calls()) != 0 {
				t.Fatal("engine called")
			}
			all := string(raw) + strings.Join(logs, "\n")
			if strings.Contains(all, hostEnvValue) || strings.Contains(all, "other-value-123") {
				t.Fatalf("value leaked: %s", all)
			}
			if !strings.Contains(strings.Join(logs, "\n"), "host_env_run env") {
				t.Fatalf("logs %v", logs)
			}
		})
	}
	t.Run("denylisted key", func(t *testing.T) {
		e := newHostEnv(t, hostEnvLine)
		e.writeHostEnv("NODE_OPTIONS=--require=/tmp/evil.js\n", 0o600)
		raw := e.call(images.VerbDeploy, mustJSON(t, hostEnvDeploy(hashC)))
		jsonEqual(t, raw, `{"error":"bad_spec","detail":"env_conflict","key":"NODE_OPTIONS"}`)
		if strings.Contains(string(raw), "evil") {
			t.Fatal("value leaked")
		}
	})
}

// The file is read only at deploy (v1.21 item 7): a bad file refuses that
// entry's deploys with policy_denied / host_env_file, before any Engine
// call, and every other verb keeps working.
func TestHostEnvBadFileRefusesOnlyDeploys(t *testing.T) {
	for name, spoil := range map[string]func(*imgEnv){
		"mode 0644":  func(e *imgEnv) { e.writeHostEnv("DATABASE_URL="+hostEnvValue+"\n", 0o644) },
		"not root":   func(e *imgEnv) { e.sys.envOwner = func(os.FileInfo) (uint32, uint32, bool) { return 1000, 1000, true } },
		"bad line":   func(e *imgEnv) { e.writeHostEnv("not a line\n", 0o600) },
		"dir is 777": func(e *imgEnv) { _ = os.Chmod(filepath.Join(e.sys.Root, "etc", "deploybot"), 0o777) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newHostEnv(t, hostEnvLine)
			// A good deploy first, then a second one parks it on -prev.
			for i := 0; i < 2; i++ {
				if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, hostEnvDeploy(hashC)))) != "" {
					t.Fatal("deploy")
				}
			}
			spoil(e)
			before := len(e.fake.Calls())
			raw := e.call(images.VerbDeploy, mustJSON(t, hostEnvDeploy(hashC)))
			jsonEqual(t, raw, `{"error":"policy_denied","detail":"host_env_file"}`)
			if len(e.fake.Calls()) != before {
				t.Fatal("engine called")
			}
			entry := mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
			for _, verb := range []string{images.VerbStatus, images.VerbStop, images.VerbStart, images.VerbRestart, images.VerbRollback} {
				if code := imgCode(t, e.call(verb, entry)); code != "" {
					t.Fatalf("%s refused %s", verb, code)
				}
			}
			if code := imgCode(t, e.call(images.VerbList, []byte("{}"))); code != "" {
				t.Fatalf("list refused %s", code)
			}
			raw = e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 5}))
			if imgCode(t, raw) != "" || strings.Contains(string(raw), hostEnvValue) {
				t.Fatalf("logs %s", raw)
			}
			// Programs and the edge read the same policy, which stays valid.
			pol := policy.Load(policy.Files{Policy: e.sys.Policy, UID: e.sys.UID, SkipEnvFiles: true})
			if !pol.Valid || !pol.ImagesEnabled() {
				t.Fatalf("policy %s", pol.Reason)
			}
			prog := Handle(context.Background(), "list", []byte("{}"), e.sys)
			if strings.Contains(string(prog), programs.ReasonPolicy) {
				t.Fatalf("programs list %s", prog)
			}
		})
	}
}

// host_env_images (v1.21 item 7): the file's secrets reach only the bound
// image, by tag and by the pinned digest.
func TestHostEnvImageBinding(t *testing.T) {
	e := newHostEnv(t, hostEnvLine)
	req := hostEnvDeploy(hashC)
	req.Spec.Image = "ghcr.io/joe-and-his-friends/other:1"
	raw := e.call(images.VerbDeploy, mustJSON(t, req))
	jsonEqual(t, raw, `{"error":"policy_denied","detail":"host_env_image"}`)
	if len(e.fake.Calls()) != 0 {
		t.Fatal("engine called")
	}
	// The job entry is bound to mo-job, not mo-webadmin.
	job := jobDeploy()
	job.Spec.Image = "ghcr.io/joe-and-his-friends/mo-webadmin:1.2"
	raw = e.call(images.VerbDeploy, mustJSON(t, job))
	jsonEqual(t, raw, `{"error":"policy_denied","detail":"host_env_image"}`)
	// The bound image deploys.
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, hostEnvDeploy(hashC)))) != "" {
		t.Fatal("bound image refused")
	}
}

// env_hash in container_status holds only while the host env file is the
// one the container was created with (v1.21 item 4).
func TestHostEnvStatusEnvHash(t *testing.T) {
	e := newHostEnv(t, hostEnvLine)
	req := hostEnvDeploy(hashC)
	req.EnvHash = envHashA
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != "" {
		t.Fatal("deploy")
	}
	status := func() images.StatusResult {
		t.Helper()
		raw := e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
		if strings.Contains(string(raw), hostEnvValue) || strings.Contains(string(raw), "host_env") {
			t.Fatalf("status leaks %s", raw)
		}
		var st images.StatusResult
		if json.Unmarshal(raw, &st) != nil {
			t.Fatal(string(raw))
		}
		return st
	}
	if st := status(); st.EnvHash != envHashA {
		t.Fatalf("same file: %+v", st)
	}
	e.writeHostEnv("DATABASE_URL=changed-value-xyz\nsecrets_key=0123456789abcdef\n", 0o600)
	if st := status(); st.EnvHash != "" {
		t.Fatalf("changed file still matches: %+v", st)
	}
	// Deploy again with the new file: the new container matches, and the
	// parked one (old file) does not.
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != "" {
		t.Fatal("redeploy")
	}
	st := status()
	if st.EnvHash != envHashA || st.Prev == nil || st.Prev.EnvHash != "" {
		t.Fatalf("%+v %+v", st, st.Prev)
	}
	if lastBody(e.fake, http.MethodPost, "/containers/create") == nil {
		t.Fatal("no create")
	}
	// A line removed from the policy: the container's label no longer
	// matches "no file", so env_hash is not reported.
	e.setPolicy(imagePolicy)
	if st := status(); st.EnvHash != "" {
		t.Fatalf("line removed: %+v", st)
	}
}

func TestHostEnvLogsRedacted(t *testing.T) {
	e := newHostEnv(t, hostEnvLine)
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, hostEnvDeploy(hashC)))) != "" {
		t.Fatal("deploy")
	}
	e.fake.logBody = append(frame(1, "connecting to "+hostEnvValue+"\n"), frame(1, "ok\n")...)
	logs := func() images.LogsResponse {
		t.Helper()
		raw := e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 20}))
		if strings.Contains(string(raw), hostEnvValue) {
			t.Fatalf("value in logs %s", raw)
		}
		var lr images.LogsResponse
		if json.Unmarshal(raw, &lr) != nil {
			t.Fatal(string(raw))
		}
		return lr
	}
	if lr := logs(); len(lr.Lines) != 2 || !strings.Contains(lr.Lines[0], "[REDACTED]") {
		t.Fatalf("%+v", lr)
	}
	// Rotation: the file now holds a new value; the container still has
	// the old one, which can no longer be matched. Lines are withheld.
	e.writeHostEnv("DATABASE_URL=rotated-value-123456\nsecrets_key=0123456789abcdef\n", 0o600)
	if lr := logs(); len(lr.Lines) != 1 || lr.Lines[0] != withheldLine {
		t.Fatalf("rotation: %+v", lr)
	}
	// Unreadable (bad mode) is withheld too.
	e.writeHostEnv("DATABASE_URL="+hostEnvValue+"\nsecrets_key=0123456789abcdef\n", 0o644)
	if lr := logs(); len(lr.Lines) != 1 || lr.Lines[0] != withheldLine {
		t.Fatalf("unreadable: %+v", lr)
	}
	// The line removed from the policy: the container still has the env.
	e.setPolicy(imagePolicy)
	if lr := logs(); len(lr.Lines) != 1 || lr.Lines[0] != withheldLine {
		t.Fatalf("line removed: %+v", lr)
	}
}

// Host mode hardening and the manual start, restart and rollback re-check
// (v1.21 item 7).
func TestHostNetworkHardeningAndRecheck(t *testing.T) {
	e := newImgEnv(t, imagePolicy+hostNetLine)
	req := serviceDeploy(hashC)
	req.Spec = hostNetSpec()
	for i := 0; i < 2; i++ {
		if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != "" {
			t.Fatal(code)
		}
	}
	body := e.fake.body(http.MethodPost, "/containers/create")
	var got struct {
		HostConfig struct {
			CapDrop     []string
			SecurityOpt []string
		}
		Labels map[string]string
	}
	if json.Unmarshal(body, &got) != nil || len(got.HostConfig.CapDrop) != 1 || got.HostConfig.CapDrop[0] != "NET_RAW" ||
		len(got.HostConfig.SecurityOpt) != 1 || got.HostConfig.SecurityOpt[0] != "no-new-privileges" || got.Labels[images.LabelHostNetwork] != "true" {
		t.Fatalf("%s", body)
	}
	entry := mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
	// Still allowed: start and restart work (rollback below, once).
	for _, verb := range []string{images.VerbStart, images.VerbRestart} {
		if code := imgCode(t, e.call(verb, entry)); code != "" {
			t.Fatalf("%s: %s", verb, code)
		}
	}
	// No longer allowed: refused; stop and status still work.
	e.setPolicy(imagePolicy)
	for _, verb := range []string{images.VerbStart, images.VerbRestart, images.VerbRollback} {
		if code := imgCode(t, e.call(verb, entry)); code != programs.ReasonPolicy {
			t.Fatalf("%s: %q", verb, code)
		}
	}
	for _, verb := range []string{images.VerbStop, images.VerbStatus} {
		if code := imgCode(t, e.call(verb, entry)); code != "" {
			t.Fatalf("%s: %s", verb, code)
		}
	}
	// Allowed again: the rollback (the path a deploy's automatic rollback
	// takes) runs.
	e.setPolicy(imagePolicy + hostNetLine)
	if code := imgCode(t, e.call(images.VerbRollback, entry)); code != "" {
		t.Fatalf("rollback: %s", code)
	}
	// A bridge container has none of this.
	b := newImgEnv(t, imagePolicy)
	if imgCode(t, b.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))) != "" {
		t.Fatal("bridge")
	}
	if cb := b.fake.body(http.MethodPost, "/containers/create"); strings.Contains(string(cb), "CapDrop") || strings.Contains(string(cb), "SecurityOpt") || strings.Contains(string(cb), images.LabelHostNetwork) {
		t.Fatalf("%s", cb)
	}
}
