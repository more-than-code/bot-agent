package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

const (
	sharedPolicy = imagePolicy +
		"shared_networks = [\"dd=10.200.10.0/24\", \"ee=10.200.11.0/24\"]\n" +
		"shared_network_projects = [\"dd:mohiguide\"]\n"
	sharedConflict = "bot-agent-runner: shared network deploybot-shared-dd exists with a different subnet or labels; remove it by hand (docker network rm) after stopping its containers"
	sharedOverlap  = "bot-agent-runner: subnet 10.200.10.0/24 overlaps an existing Docker network"
)

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) has(line string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lines {
		if x == line {
			return true
		}
	}
	return false
}

func sharedEnv(t *testing.T) (*imgEnv, *logSink) {
	e := newImgEnv(t, sharedPolicy)
	sink := &logSink{}
	e.sys.Logf = sink.logf
	return e, sink
}

func sharedDeploy(nets ...string) images.DeployRequest {
	req := serviceDeploy(hashC)
	req.Spec.Networks = nets
	return req
}

func (e *imgEnv) sharedLabels() map[string]string {
	return map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host,
		images.LabelKind: "shared", images.LabelNetwork: "dd",
	}
}

func (f *fakeDocker) netCreateBody(name string) []byte {
	for _, c := range f.Calls() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/networks/create") && strings.Contains(string(c.Body), `"Name":"`+name+`"`) {
			return c.Body
		}
	}
	return nil
}

func (f *fakeDocker) countPath(method, suffix string) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Method == method && strings.HasSuffix(c.Path, suffix) {
			n++
		}
	}
	return n
}

// v1.13 items 4 and 5: a missing shared network is created with the four
// labels and the policy subnet; the container gets a second endpoint with
// no alias, and NetworkMode stays the project network.
func TestSharedNetworkCreateAndEndpoints(t *testing.T) {
	e, _ := sharedEnv(t)
	raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	jsonEqual(t, e.fake.netCreateBody("deploybot-shared-dd"), `{
		"Name":"deploybot-shared-dd","Driver":"bridge",
		"Labels":{"dev.deploybot.host":"`+e.host+`","dev.deploybot.kind":"shared",
			"dev.deploybot.network":"dd","dev.deploybot.owner":"bot-agent"},
		"IPAM":{"Config":[{"Subnet":"10.200.10.0/24"}]}
	}`)
	body := e.fake.body(http.MethodPost, "/containers/create")
	var c struct {
		HostConfig       struct{ NetworkMode string }
		NetworkingConfig struct {
			EndpointsConfig map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	if c.HostConfig.NetworkMode != "deploybot-mohiguide" {
		t.Fatal(c.HostConfig.NetworkMode)
	}
	eps := c.NetworkingConfig.EndpointsConfig
	if len(eps) != 2 || string(eps["deploybot-mohiguide"]) != `{"Aliases":["web"]}` || string(eps["deploybot-shared-dd"]) != `{}` {
		t.Fatalf("%s", body)
	}
	// A second deploy adopts the network it made: no second create.
	raw = e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	creates := 0
	for _, call := range e.fake.Calls() {
		if strings.HasSuffix(call.Path, "/networks/create") && strings.Contains(string(call.Body), "deploybot-shared-dd") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("creates %d", creates)
	}
}

// A matching network made earlier (or by hand with the same labels and
// subnet) is adopted without a create.
func TestSharedNetworkAdoptsMatching(t *testing.T) {
	e, _ := sharedEnv(t)
	e.fake.nets["deploybot-shared-dd"] = &fakeNet{driver: "bridge", labels: e.sharedLabels(), subnets: []string{"10.200.10.0/24"}}
	raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if e.fake.netCreateBody("deploybot-shared-dd") != nil {
		t.Fatal("created over an existing network")
	}
}

// An existing network with another subnet, driver or labels is a conflict
// with the fixed diagnostic. The running container is not touched and no
// container is created; the network is not removed.
func TestSharedNetworkConflict(t *testing.T) {
	cases := map[string]func(e *imgEnv) *fakeNet{
		"subnet": func(e *imgEnv) *fakeNet {
			return &fakeNet{driver: "bridge", labels: e.sharedLabels(), subnets: []string{"10.200.20.0/24"}}
		},
		"two subnets": func(e *imgEnv) *fakeNet {
			return &fakeNet{driver: "bridge", labels: e.sharedLabels(), subnets: []string{"10.200.10.0/24", "10.200.12.0/24"}}
		},
		"no subnet": func(e *imgEnv) *fakeNet {
			return &fakeNet{driver: "bridge", labels: e.sharedLabels()}
		},
		"driver": func(e *imgEnv) *fakeNet {
			return &fakeNet{driver: "macvlan", labels: e.sharedLabels(), subnets: []string{"10.200.10.0/24"}}
		},
		"no labels": func(e *imgEnv) *fakeNet {
			return &fakeNet{driver: "bridge", subnets: []string{"10.200.10.0/24"}}
		},
		"other host": func(e *imgEnv) *fakeNet {
			l := e.sharedLabels()
			l[images.LabelHost] = "0000000000000000"
			return &fakeNet{driver: "bridge", labels: l, subnets: []string{"10.200.10.0/24"}}
		},
		"kind": func(e *imgEnv) *fakeNet {
			l := e.sharedLabels()
			l[images.LabelKind] = "project"
			return &fakeNet{driver: "bridge", labels: l, subnets: []string{"10.200.10.0/24"}}
		},
		"network label": func(e *imgEnv) *fakeNet {
			l := e.sharedLabels()
			l[images.LabelNetwork] = "ee"
			return &fakeNet{driver: "bridge", labels: l, subnets: []string{"10.200.10.0/24"}}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			e, sink := sharedEnv(t)
			e.fake.add("mohiguide-web", "running", "img", e.labels("web"))
			prev := e.fake.add("mohiguide-web-prev", "exited", "img", e.labels("web"))
			foreign := mk(e)
			e.fake.nets["deploybot-shared-dd"] = foreign
			raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
			if imgCode(t, raw) != programs.ReasonConflict {
				t.Fatal(string(raw))
			}
			if !sink.has(sharedConflict) {
				t.Fatalf("%q", sink.lines)
			}
			if e.fake.countPath(http.MethodPost, "/containers/create") != 0 || e.fake.countPath(http.MethodPost, "/stop") != 0 ||
				e.fake.countPath(http.MethodPost, "/rename") != 0 || e.fake.countPath(http.MethodPost, "/networks/create") != 0 {
				t.Fatalf("calls after conflict: %v", e.fake.Calls())
			}
			if e.fake.ctrs["mohiguide-web"].state != "running" {
				t.Fatal("running container touched")
			}
			if n := e.fake.countPath(http.MethodDelete, ""); n != 0 {
				t.Fatalf("%d deletes", n)
			}
			if e.fake.nets["deploybot-shared-dd"] != foreign {
				t.Fatal("foreign network replaced")
			}
			if c := e.fake.ctrs["mohiguide-web-prev"]; c == nil || c.id != prev || c.state != "exited" {
				t.Fatal("-prev touched")
			}
		})
	}
}

// A create that loses a race (409) re-inspects and runs the adoption check,
// so a foreign network of that name is refused rather than adopted.
func TestSharedNetworkRaceIsChecked(t *testing.T) {
	e, sink := sharedEnv(t)
	e.fake.netRace = map[string]*fakeNet{"deploybot-shared-dd": {driver: "bridge", subnets: []string{"10.200.10.0/24"}}}
	raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
	if imgCode(t, raw) != programs.ReasonConflict || !sink.has(sharedConflict) {
		t.Fatalf("%s %q", raw, sink.lines)
	}
	// A race won by a matching network is adopted.
	e2, _ := sharedEnv(t)
	e2.fake.netRace = map[string]*fakeNet{"deploybot-shared-dd": {driver: "bridge", labels: e2.sharedLabels(), subnets: []string{"10.200.10.0/24"}}}
	raw = e2.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
}

// Docker's pool overlap refusal (a create whose subnet overlaps an existing
// network's) is conflict with the overlap diagnostic, before any container
// is touched.
func TestSharedNetworkOverlap(t *testing.T) {
	t.Run("hand-made network", func(t *testing.T) {
		e, sink := sharedEnv(t)
		e.fake.nets["legacy"] = &fakeNet{driver: "bridge", subnets: []string{"10.200.0.0/16"}}
		cur := e.fake.add("mohiguide-web", "running", "img", e.labels("web"))
		raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
		if imgCode(t, raw) != programs.ReasonConflict || !sink.has(sharedOverlap) {
			t.Fatalf("%s %q", raw, sink.lines)
		}
		assertUntouched(t, e, cur)
		if _, ok := e.fake.nets["deploybot-shared-dd"]; ok {
			t.Fatal("network created over an overlap")
		}
	})
	t.Run("default bridge", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy+
			"shared_networks = [\"dd=172.17.5.0/24\"]\n"+
			"shared_network_projects = [\"dd:mohiguide\"]\n")
		sink := &logSink{}
		e.sys.Logf = sink.logf
		raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
		if imgCode(t, raw) != programs.ReasonConflict || !sink.has("bot-agent-runner: subnet 172.17.5.0/24 overlaps an existing Docker network") {
			t.Fatalf("%s %q", raw, sink.lines)
		}
		if e.fake.countPath(http.MethodPost, "/containers/create") != 0 {
			t.Fatal("container created")
		}
	})
}

// assertUntouched checks that the running container id is still running
// under its name and that no container was stopped, renamed, removed or
// created.
func assertUntouched(t *testing.T, e *imgEnv, id string) {
	t.Helper()
	c := e.fake.byID[id]
	if c == nil || c.state != "running" || e.fake.ctrs[c.name] != c {
		t.Fatal("running container touched")
	}
	for _, call := range e.fake.Calls() {
		if call.Method == http.MethodDelete || strings.HasSuffix(call.Path, "/stop") ||
			strings.HasSuffix(call.Path, "/rename") || strings.HasSuffix(call.Path, "/containers/create") {
			t.Fatalf("container call %s %s", call.Method, call.Path)
		}
	}
}

// v1.13 item 3: the helper re-check refuses a network that is undefined or
// does not list the project, before any Engine call.
func TestSharedNetworkRecheck(t *testing.T) {
	for name, nets := range map[string][]string{
		"not a member": {"ee"},
		"undefined":    {"zz"},
		"one of two":   {"dd", "ee"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := sharedEnv(t)
			raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy(nets...)))
			if imgCode(t, raw) != programs.ReasonPolicy {
				t.Fatal(string(raw))
			}
			if n := len(e.fake.Calls()); n != 0 {
				t.Fatalf("%d engine calls", n)
			}
		})
	}
	// Bad shapes are bad_spec, also before Docker.
	e, _ := sharedEnv(t)
	for _, raw := range []string{
		strings.Replace(string(mustJSON(t, serviceDeploy(hashC))), `"kind":"service"`, `"networks":[],"kind":"service"`, 1),
		strings.Replace(string(mustJSON(t, serviceDeploy(hashC))), `"kind":"service"`, `"networks":["dd","dd"],"kind":"service"`, 1),
		strings.Replace(string(mustJSON(t, serviceDeploy(hashC))), `"kind":"service"`, `"networks":["a","b","c","d","e"],"kind":"service"`, 1),
		strings.Replace(string(mustJSON(t, serviceDeploy(hashC))), `"kind":"service"`, `"networks":["Bad"],"kind":"service"`, 1),
	} {
		if code := imgCode(t, e.call(images.VerbDeploy, []byte(raw))); code != programs.ReasonBadSpec {
			t.Fatalf("%s: %s", raw, code)
		}
	}
	if n := len(e.fake.Calls()); n != 0 {
		t.Fatalf("%d engine calls", n)
	}
}

// A job joins shared networks the same way.
func TestSharedNetworkJob(t *testing.T) {
	e, _ := sharedEnv(t)
	req := jobDeploy()
	req.Spec.Networks = []string{"dd"}
	raw := e.call(images.VerbDeploy, mustJSON(t, req))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if e.fake.netCreateBody("deploybot-shared-dd") == nil || !strings.Contains(string(e.fake.body(http.MethodPost, "/containers/create")), `"deploybot-shared-dd":{}`) {
		t.Fatal("job not on the shared network")
	}
}

func literalSpec() images.ImageSpec {
	mem := 64
	return images.ImageSpec{
		Kind: "service", Image: "ghcr.io/joe-and-his-friends/mo-webadmin:1.2",
		Command: []string{"serve"}, Ports: []images.Port{{Host: 8080, Container: 80}},
		Volumes: []images.Volume{{Name: "data", Target: "/var/www"}},
		Restart: "unless-stopped", User: "app", MemoryMB: &mem,
	}
}

func literalCreate(project string, spec images.ImageSpec) ([]byte, string) {
	return buildCreate(project, "web", digestA, hashC, "", "0123456789abcdef", spec,
		map[string]string{"A": "b"}, nil, []string{"ghcr.io/joe-and-his-friends/*"}, false)
}

// v1.13 item 5: a spec without networks gives the v1.12 body byte for byte.
// createBodyV028 was produced by buildCreate at d5df8d6 (agent 0.2.8) with
// these inputs, and regenerated from an export of that commit to recheck.
func TestCreateBodyWithoutNetworksUnchanged(t *testing.T) {
	got, code := literalCreate("mohiguide", literalSpec())
	if code != "" {
		t.Fatal(code)
	}
	if string(got) != createBodyV028 {
		t.Fatalf("got\n%s\nwant\n%s", got, createBodyV028)
	}
}

// With networks, the body is the v1.12 body plus one empty endpoint per
// shared network, after the project endpoint.
func TestCreateBodySharedEndpoints(t *testing.T) {
	spec := literalSpec()
	spec.Networks = []string{"dd", "ee"}
	got, code := literalCreate("mohiguide", spec)
	if code != "" {
		t.Fatal(code)
	}
	want := strings.Replace(createBodyV028, `"deploybot-mohiguide":{"Aliases":["web"]}`,
		`"deploybot-mohiguide":{"Aliases":["web"]},"deploybot-shared-dd":{},"deploybot-shared-ee":{}`, 1)
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// A project slug "shared-dd" has project network deploybot-shared-dd, the
// same name as shared network dd, so joining dd is a conflict.
func TestCreateBodySharedSlugCollision(t *testing.T) {
	spec := literalSpec()
	spec.Networks = []string{"dd"}
	if _, code := literalCreate("shared-dd", spec); code != programs.ReasonConflict {
		t.Fatal(code)
	}
}

const createBodyV028 = `{"Image":"ghcr.io/joe-and-his-friends/mo-webadmin@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Cmd":["serve"],"Env":["A=b"],"Labels":{"dev.deploybot.entry":"web","dev.deploybot.host":"0123456789abcdef","dev.deploybot.owner":"bot-agent","dev.deploybot.project":"mohiguide","dev.deploybot.spec_hash":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},"ExposedPorts":{"80/tcp":{}},"User":"app","HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]},"Mounts":[{"Type":"volume","Source":"mohiguide-data","Target":"/var/www","ReadOnly":false}],"RestartPolicy":{"Name":"unless-stopped"},"NetworkMode":"deploybot-mohiguide","CgroupnsMode":"private","PidsLimit":1024,"Memory":67108864,"MemorySwap":67108864,"LogConfig":{"Type":"json-file","Config":{"max-size":"10m","max-file":"3"}}},"NetworkingConfig":{"EndpointsConfig":{"deploybot-mohiguide":{"Aliases":["web"]}}}}`

// v1.13 item 5 and A6: the -prev container keeps the networks it was
// created with. A rollback after the network is revoked starts it as it is:
// no network create or inspect, and its endpoints stay.
func TestSharedNetworkRollbackKeepsEndpoints(t *testing.T) {
	e, _ := sharedEnv(t)
	cur := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	prev := e.fake.add("mohiguide-web-prev", "exited", "ghcr.io/joe-and-his-friends/app@"+digestB, e.labels("web"))
	e.fake.byID[prev].networks = []string{"deploybot-mohiguide", "deploybot-shared-dd"}
	e.setPolicy(imagePolicy) // dd revoked
	raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	for _, c := range e.fake.Calls() {
		if strings.Contains(c.Path, "/networks/") {
			t.Fatalf("network call %s %s", c.Method, c.Path)
		}
	}
	c := e.fake.byID[prev]
	if c.name != "mohiguide-web" || c.state != "running" || c.image != "ghcr.io/joe-and-his-friends/app@"+digestB {
		t.Fatalf("%+v", c)
	}
	if strings.Join(c.networks, ",") != "deploybot-mohiguide,deploybot-shared-dd" || !reflect.DeepEqual(c.labels, e.labels("web")) {
		t.Fatalf("%+v", c)
	}
	if e.fake.byID[cur] != nil && e.fake.ctrs["mohiguide-web"] == e.fake.byID[cur] {
		t.Fatal("original still current")
	}
}

// A Docker failure while ensuring a shared network is docker_unavailable,
// before any container is touched.
func TestSharedNetworkDockerErrors(t *testing.T) {
	for name, set := range map[string]func(f *fakeDocker){
		"inspect 500": func(f *fakeDocker) { f.netInspectFail = "deploybot-shared-dd" },
		"create 500":  func(f *fakeDocker) { f.netCreateFail = "deploybot-shared-dd" },
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := sharedEnv(t)
			cur := e.fake.add("mohiguide-web", "running", "img", e.labels("web"))
			set(e.fake)
			raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd")))
			if imgCode(t, raw) != programs.ReasonDocker {
				t.Fatal(string(raw))
			}
			assertUntouched(t, e, cur)
		})
	}
}

// With two networks, the first is created and the second conflicts: the
// first stays (nothing is removed) and the container is not touched.
func TestSharedNetworkSecondConflicts(t *testing.T) {
	e := newImgEnv(t, imagePolicy+
		"shared_networks = [\"dd=10.200.10.0/24\", \"ee=10.200.11.0/24\"]\n"+
		"shared_network_projects = [\"dd:mohiguide\", \"ee:mohiguide\"]\n")
	sink := &logSink{}
	e.sys.Logf = sink.logf
	cur := e.fake.add("mohiguide-web", "running", "img", e.labels("web"))
	e.fake.nets["deploybot-shared-ee"] = &fakeNet{driver: "bridge", subnets: []string{"10.200.11.0/24"}}
	raw := e.call(images.VerbDeploy, mustJSON(t, sharedDeploy("dd", "ee")))
	if imgCode(t, raw) != programs.ReasonConflict ||
		!sink.has("bot-agent-runner: shared network deploybot-shared-ee exists with a different subnet or labels; remove it by hand (docker network rm) after stopping its containers") {
		t.Fatalf("%s %q", raw, sink.lines)
	}
	if n := e.fake.nets["deploybot-shared-dd"]; n == nil || n.labels[images.LabelNetwork] != "dd" {
		t.Fatal("first network not kept")
	}
	assertUntouched(t, e, cur)
}

func labelsFor(host, project, entry string) map[string]string {
	return map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: host,
		images.LabelProject: project, images.LabelEntry: entry, images.LabelSpecHash: hashC,
	}
}

// A8: project slug shared-dd cannot join dd. The verb answers conflict
// before any Docker call, so no network is created.
func TestSharedNetworkSlugJoinConflict(t *testing.T) {
	e := newImgEnv(t, "enabled = true\n"+
		"images = [\"ghcr.io/joe-and-his-friends/*\"]\n"+
		"projects = [\"shared-dd\"]\nmax_containers = 20\n"+
		"shared_networks = [\"dd=10.200.10.0/24\"]\n"+
		"shared_network_projects = [\"dd:shared-dd\"]\n")
	req := sharedDeploy("dd")
	req.Project = "shared-dd"
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != programs.ReasonConflict {
		t.Fatal(code)
	}
	if n := len(e.fake.Calls()); n != 0 {
		t.Fatalf("%d engine calls: %v", n, e.fake.Calls())
	}
}

// Project shared-dd deploying without networks while shared network dd
// exists: its project network would be deploybot-shared-dd, which is the
// shared network, so the deploy is a conflict and the running container is
// not touched.
func TestSharedNetworkSlugProjectNetConflict(t *testing.T) {
	e := newImgEnv(t, "enabled = true\n"+
		"images = [\"ghcr.io/joe-and-his-friends/*\"]\n"+
		"projects = [\"shared-dd\"]\nmax_containers = 20\n")
	e.fake.nets["deploybot-shared-dd"] = &fakeNet{driver: "bridge", subnets: []string{"10.200.10.0/24"}, labels: map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host,
		images.LabelKind: images.KindShared, images.LabelNetwork: "dd",
	}}
	cur := e.fake.add("shared-dd-web", "running", "img", labelsFor(e.host, "shared-dd", "web"))
	req := serviceDeploy(hashC)
	req.Project = "shared-dd"
	if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != programs.ReasonConflict {
		t.Fatal(code)
	}
	assertUntouched(t, e, cur)
	if e.fake.countPath(http.MethodPost, "/networks/create") != 0 {
		t.Fatal("network created")
	}
}

// defect + drainer-db and defect-drainer + db share the container name
// defect-drainer-db. Listing both projects makes the policy invalid for
// images (v1.5.2), so the deploy is policy_denied before any Docker call.
// With only one listed, a leftover container of the other is a conflict:
// nothing is stopped, removed or created, and no network is made.
func TestSharedNetworkNameCollision(t *testing.T) {
	const head = "enabled = true\n" +
		"images = [\"ghcr.io/joe-and-his-friends/*\"]\nmax_containers = 20\n" +
		"shared_networks = [\"dd=10.200.10.0/24\"]\n"
	for _, pair := range [][4]string{
		{"defect-drainer", "db", "defect", "drainer-db"},
		{"defect", "drainer-db", "defect-drainer", "db"},
	} {
		t.Run(pair[2]+"/"+pair[3], func(t *testing.T) {
			req := sharedDeploy("dd")
			req.Project, req.Entry = pair[2], pair[3]

			e := newImgEnv(t, head+
				"projects = [\"defect\", \"defect-drainer\"]\n"+
				"shared_network_projects = [\"dd:defect\", \"dd:defect-drainer\"]\n")
			e.fake.add("defect-drainer-db", "running", "img", labelsFor(e.host, pair[0], pair[1]))
			if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != programs.ReasonPolicy {
				t.Fatal(code)
			}
			if n := len(e.fake.Calls()); n != 0 {
				t.Fatalf("%d engine calls", n)
			}

			e = newImgEnv(t, head+
				"projects = [\""+pair[2]+"\"]\n"+
				"shared_network_projects = [\"dd:"+pair[2]+"\"]\n")
			cur := e.fake.add("defect-drainer-db", "running", "img", labelsFor(e.host, pair[0], pair[1]))
			if code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))); code != programs.ReasonConflict {
				t.Fatal(code)
			}
			assertUntouched(t, e, cur)
			for _, c := range e.fake.Calls() {
				if strings.Contains(c.Path, "/networks/") || strings.Contains(c.Path, "/volumes/") {
					t.Fatalf("call %s %s", c.Method, c.Path)
				}
			}
		})
	}
}
