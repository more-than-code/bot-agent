package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

// agent-protocol v1.26: an entry with a host env file runs only the shape
// root pinned in runner.toml.

const pinHealthLine = `host_env_healthcheck = ["mohiguide/web=/bin/app health http://127.0.0.1:3031/health"]` + "\n"

func pinnedHealthcheck() *images.Healthcheck {
	return &images.Healthcheck{Test: []string{"/bin/app", "health", "http://127.0.0.1:3031/health"}, IntervalSec: 30, TimeoutSec: 5, Retries: 3}
}

func TestHostEnvRunRefused(t *testing.T) {
	cases := []struct {
		field  string
		mutate func(*images.DeployRequest)
	}{
		{"command", func(r *images.DeployRequest) { r.Spec.Command = []string{"sh", "-c", "env | base64"} }},
		{"command", func(r *images.DeployRequest) { r.Spec.Command = []string{"serve", "x"} }},
		{"command", func(r *images.DeployRequest) { r.Spec.Command = nil }},
		{"entrypoint", func(r *images.DeployRequest) { r.Spec.Entrypoint = []string{"/bin/sh", "-c"} }},
		{"healthcheck", func(r *images.DeployRequest) {
			r.Spec.Healthcheck = &images.Healthcheck{Test: []string{"/bin/sh", "-c", "env > /proc/1/fd/1"}, IntervalSec: 30, TimeoutSec: 5, Retries: 3}
		}},
		{"user", func(r *images.DeployRequest) { r.Spec.User = "app" }},
		{"volumes", func(r *images.DeployRequest) {
			r.Spec.Volumes = []images.Volume{{Name: "data", Target: "/usr/local/bin"}}
		}},
		{"files", func(r *images.DeployRequest) {
			r.Spec.Files = []images.SpecFile{{File: "app.conf", Target: "/etc/app.conf"}}
			r.Files = []images.MountFile{{Name: "app.conf", Target: "/etc/app.conf", Content: "v"}}
		}},
		{"env", func(r *images.DeployRequest) { r.Env = map[string]string{"HTTPS_PROXY": "http://10.0.0.9:3128"} }},
		{"env", func(r *images.DeployRequest) { r.Spec.Env = map[string]string{"SSL_CERT_FILE": "/etc/app/ca.pem"} }},
		{"env", func(r *images.DeployRequest) { r.Spec.EnvSets = []string{"proxy"} }},
		{"env", func(r *images.DeployRequest) { r.Spec.SecretEnv = map[string]string{"TOKEN": "proxy.TOKEN"} }},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("%d-%s", i, c.field), func(t *testing.T) {
			var logs []string
			e := newHostEnv(t, hostEnvLine)
			e.sys.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			req := hostEnvDeploy(hashC)
			c.mutate(&req)
			raw := e.call(images.VerbDeploy, mustJSON(t, req))
			jsonEqual(t, raw, `{"error":"policy_denied","detail":"host_env_run","key":"`+c.field+`"}`)
			if len(e.fake.Calls()) != 0 {
				t.Fatal("engine called")
			}
			all := string(raw) + strings.Join(logs, "\n")
			if strings.Contains(all, hostEnvValue) || strings.Contains(all, "10.0.0.9") {
				t.Fatalf("value leaked: %s", all)
			}
			if !strings.Contains(strings.Join(logs, "\n"), "policy_denied host_env_run "+c.field) {
				t.Fatalf("logs %v", logs)
			}
		})
	}
}

// A healthcheck needs a pin; with one, only its test argv is fixed.
func TestHostEnvRunHealthcheck(t *testing.T) {
	e := newHostEnv(t, hostEnvLine)
	req := hostEnvDeploy(hashC)
	req.Spec.Healthcheck = pinnedHealthcheck()
	jsonEqual(t, e.call(images.VerbDeploy, mustJSON(t, req)), `{"error":"policy_denied","detail":"host_env_run","key":"healthcheck"}`)

	e = newHostEnv(t, hostEnvLine+pinHealthLine)
	req.Spec.Healthcheck.IntervalSec, req.Spec.Healthcheck.Retries = 10, 5
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != "" {
		t.Fatal(code)
	}
	var got struct {
		Healthcheck struct{ Test []string }
	}
	if err := json.Unmarshal(e.fake.body(http.MethodPost, "/containers/create"), &got); err != nil {
		t.Fatal(err)
	}
	if want := []string{"CMD", "/bin/app", "health", "http://127.0.0.1:3031/health"}; !reflect.DeepEqual(got.Healthcheck.Test, want) {
		t.Fatalf("test %v", got.Healthcheck.Test)
	}
	// With a pin, no healthcheck at all is refused too.
	req.Spec.Healthcheck = nil
	jsonEqual(t, e.call(images.VerbDeploy, mustJSON(t, req)), `{"error":"policy_denied","detail":"host_env_run","key":"healthcheck"}`)
}

// The create body of a pinned deploy has only the pinned Cmd, no
// Entrypoint, no User, no mounts and only the file's env.
func TestHostEnvRunCreateBody(t *testing.T) {
	e := newHostEnv(t, hostEnvLine)
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, hostEnvDeploy(hashC)))); code != "" {
		t.Fatal(code)
	}
	var got struct {
		Cmd        []string
		Entrypoint []string
		User       string
		Env        []string
		HostConfig struct{ Mounts []any }
	}
	if err := json.Unmarshal(e.fake.body(http.MethodPost, "/containers/create"), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Cmd, []string{"serve"}) || got.Entrypoint != nil || got.User != "" || got.HostConfig.Mounts != nil || len(got.Env) != 2 {
		t.Fatalf("%+v", got)
	}
	for _, kv := range got.Env {
		if !strings.HasPrefix(kv, "DATABASE_URL=") && !strings.HasPrefix(kv, "secrets_key=") {
			t.Fatalf("env %q", kv)
		}
	}
}

// A job run is checked the same way; an entry without a line is not.
func TestHostEnvRunJobAndUnlisted(t *testing.T) {
	e := newHostEnv(t, hostEnvLine)
	job := jobDeploy()
	job.Spec.Command = []string{"sh"}
	jsonEqual(t, e.call(images.VerbDeploy, mustJSON(t, job)), `{"error":"policy_denied","detail":"host_env_run","key":"command"}`)
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))); code != "" {
		t.Fatal(code)
	}
	// mohiguide/web has no line here: the full spec deploys as before.
	e = newHostEnv(t, `host_env_files = ["mohiguide/api=/etc/deploybot/api.env"]`+"\n"+`host_env_images = ["mohiguide/api=ghcr.io/joe-and-his-friends/mo-webadmin"]`+"\n")
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))); code != "" {
		t.Fatal(code)
	}
}
