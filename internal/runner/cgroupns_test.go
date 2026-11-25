package runner

import (
	"net/http"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/edge"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// v1.24: container_cgroupns = "host" in runner.toml makes the helper ask
// Docker for the host cgroup namespace (a kernel without cgroup namespaces,
// such as OpenVZ). The default stays private (TestGoldenCreateBody).
func TestCreateBodyCgroupnsHost(t *testing.T) {
	e := newImgEnv(t, imagePolicy+"container_cgroupns = \"host\"\n")
	raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	got := string(e.fake.body(http.MethodPost, "/containers/create"))
	if !strings.Contains(got, `"CgroupnsMode":"host"`) || strings.Contains(got, `"CgroupnsMode":"private"`) {
		t.Fatalf("create body: %s", got)
	}
}

func TestEdgeCreateBodyCgroupns(t *testing.T) {
	for _, mode := range []string{policy.CgroupnsPrivate, policy.CgroupnsHost} {
		body, err := edgeCreateBody("docker.io/library/caddy@sha256:"+strings.Repeat("a", 64), "0123456789abcdef", strings.Repeat("b", 64), mode)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"CgroupnsMode":"`+mode+`"`) {
			t.Fatalf("%s: %s", mode, body)
		}
	}
}

// v1.24: a start Docker answers with 500 is container_start_failed, not
// docker_unavailable, and the created container is removed.
func TestJobStartFailureRemovesContainer(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.mu.Lock()
	e.fake.startFail = true
	e.fake.mu.Unlock()
	raw := e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))
	if imgCode(t, raw) != programs.ReasonStartFailed {
		t.Fatalf("%s", raw)
	}
	var created, removed string
	for _, c := range e.fake.Calls() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/start") {
			created = strings.TrimSuffix(c.Path[strings.LastIndex(c.Path[:len(c.Path)-len("/start")], "/")+1:], "/start")
		}
		if c.Method == http.MethodDelete && created != "" && strings.HasSuffix(c.Path, "/containers/"+created) {
			removed = created
		}
	}
	if created == "" || removed != created {
		t.Fatalf("created %q removed %q", created, removed)
	}
}

// The service path: the fresh container's start is refused, so the deploy is
// container_start_failed, the new container is removed and no id is returned.
func TestServiceStartFailureRemovesContainer(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.mu.Lock()
	e.fake.startFail = true
	e.fake.mu.Unlock()
	raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))
	if imgCode(t, raw) != programs.ReasonStartFailed || strings.Contains(string(raw), "container_id") {
		t.Fatalf("%s", raw)
	}
	var started []string
	deleted := map[string]bool{}
	for _, c := range e.fake.Calls() {
		parts := strings.Split(c.Path, "/")
		if c.Method == http.MethodPost && len(parts) >= 2 && parts[len(parts)-1] == "start" {
			started = append(started, parts[len(parts)-2])
		}
		if c.Method == http.MethodDelete {
			deleted[parts[len(parts)-1]] = true
		}
	}
	if len(started) == 0 || !deleted[started[0]] {
		t.Fatalf("started %v deleted %v", started, deleted)
	}
}

// The edge create body follows the policy key through doEdgeApply.
func TestEdgeApplyCgroupnsHost(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy+"container_cgroupns = \"host\"\n")
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
	if r.Error != "" {
		t.Fatalf("%+v", r)
	}
	got := string(e.fake.body(http.MethodPost, "/containers/create"))
	if !strings.Contains(got, `"CgroupnsMode":"host"`) {
		t.Fatalf("edge create body: %s", got)
	}
}
