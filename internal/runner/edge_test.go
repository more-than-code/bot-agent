package runner

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/edge"
	"github.com/more-than-code/bot-agent/internal/agent/images"
)

const (
	edgePolicy  = "edge = true\n"
	edgeConfigA = `{"admin":{"listen":"unix//run/edge/admin.sock"},"apps":{"http":{"servers":{"a":{"listen":[":443"],"routes":[]}}}}}`
	edgeConfigB = `{"admin":{"listen":"unix//run/edge/admin.sock"},"apps":{"http":{"servers":{"b":{"listen":[":443"],"routes":[]}}}}}`
)

// fakeCaddy is the edge's admin API on the unix socket in the edge
// directory. refuse answers /load with Caddy's 400 shape.
type fakeCaddy struct {
	sock   string
	srv    *http.Server
	mu     sync.Mutex
	loads  [][]byte
	hosts  []string
	refuse string
	down   bool
}

func (c *fakeCaddy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hosts = append(c.hosts, r.Host)
	if c.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/config/":
		w.Write([]byte("{}"))
	case r.Method == http.MethodPost && r.URL.Path == "/load":
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "content type", http.StatusBadRequest)
			return
		}
		if c.refuse != "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": c.refuse})
			return
		}
		c.loads = append(c.loads, body)
	default:
		http.NotFound(w, r)
	}
}

func (c *fakeCaddy) Loads() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.loads...)
}

// testImage stands in for the release digest while edge.PinnedImage is
// the placeholder (which keeps the edge off).
const testImage = "docker.io/library/caddy@sha256:" + "abababababababababababababababababababababababababababababababab"

func pinTestImage(t *testing.T) {
	t.Helper()
	old := edge.Image
	edge.Image = testImage
	t.Cleanup(func() { edge.Image = old })
}

type edgeEnv struct {
	*imgEnv
	dir   string
	caddy *fakeCaddy
	proc  string
}

// newEdgeEnv is an image env with the edge directory and a fake /proc under
// a short /tmp path (unix socket paths are short on macOS), resolved so
// the O_NOFOLLOW store walk does not meet the /tmp link.
func newEdgeEnv(t *testing.T, policy string) *edgeEnv {
	t.Helper()
	pinTestImage(t)
	e := newImgEnv(t, policy)
	base, err := os.MkdirTemp("/tmp", "baedge")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "edge")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A new directory takes its parent's group on macOS.
	if err := os.Chown(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	proc := filepath.Join(base, "proc")
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tcp", "udp"} {
		if err := os.WriteFile(filepath.Join(proc, "net", name), []byte(procNetHeader), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.sys.edgeStore = dir
	e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
	e.sys.Proc = proc
	caddy := &fakeCaddy{sock: filepath.Join(dir, edge.AdminSocket)}
	caddy.srv = &http.Server{Handler: caddy, ErrorLog: log.New(io.Discard, "", 0)}
	t.Cleanup(func() { caddy.srv.Close() })
	caddy.listen(t)
	ee := &edgeEnv{imgEnv: e, dir: dir, caddy: caddy, proc: proc}
	// A started edge creates its admin socket, as Caddy does; the helper
	// removes a stale one before create or start.
	e.fake.startHook = func(string) { caddy.listen(t) }
	return ee
}

const procNetHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// listen (re)creates the admin socket, replacing a stale path.
func (c *fakeCaddy) listen(t *testing.T) {
	_ = os.Remove(c.sock)
	ln, err := net.Listen("unix", c.sock)
	if err != nil {
		t.Error(err)
		return
	}
	go c.srv.Serve(ln)
}

func (e *edgeEnv) edgeLabels(hash string) map[string]string {
	return map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host,
		images.LabelKind: edge.KindValue, edge.LabelConfigHash: hash,
	}
}

func applyReq(cfg string) []byte {
	return []byte(`{"config":` + cfg + `,"config_hash":"` + edge.HashConfig([]byte(cfg)) + `"}`)
}

func (e *edgeEnv) writeConfig(cfg string) {
	e.t.Helper()
	p := filepath.Join(e.dir, edge.ConfigFile)
	_ = os.Remove(p)
	if err := os.WriteFile(p, []byte(cfg), 0o444); err != nil {
		e.t.Fatal(err)
	}
}

func (e *edgeEnv) config() string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, edge.ConfigFile))
	if err != nil {
		return ""
	}
	return string(b)
}

// listen adds a /proc/net/tcp listener on port with inode, held by pid
// (0: no process found) with that comm and cgroup.
func (e *edgeEnv) listen(port int, inode uint64, pid int, comm, cgroup string) {
	e.t.Helper()
	e.socket("tcp", "0A", port, inode, pid, comm, cgroup)
}

// socket adds a row to /proc/net/<table> in state (0A listening, 07 UDP).
func (e *edgeEnv) socket(table, state string, port int, inode uint64, pid int, comm, cgroup string) {
	e.t.Helper()
	path := filepath.Join(e.proc, "net", table)
	b, _ := os.ReadFile(path)
	if len(b) == 0 {
		b = []byte(procNetHeader)
	}
	line := "   0: 00000000:" + strings.ToUpper(strconv.FormatInt(int64(port)+0x10000, 16)[1:]) +
		" 00000000:0000 " + state + " 00000000:00000000 00:00000000 00000000     0        0 " +
		strconv.FormatUint(inode, 10) + " 1 0000000000000000 100 0 0 10 0\n"
	if err := os.WriteFile(path, append(b, line...), 0o600); err != nil {
		e.t.Fatal(err)
	}
	if pid == 0 {
		return
	}
	pd := filepath.Join(e.proc, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(pd, "fd"), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Symlink("socket:["+strconv.FormatUint(inode, 10)+"]", filepath.Join(pd, "fd", strconv.FormatUint(inode, 10))); err != nil {
		e.t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pd, "comm"), []byte(comm+"\n"), 0o600)
	os.WriteFile(filepath.Join(pd, "cgroup"), []byte(cgroup+"\n"), 0o600)
}

type edgeReply struct {
	Error      string  `json:"error"`
	Detail     string  `json:"detail"`
	Outcome    string  `json:"outcome"`
	ConfigHash string  `json:"config_hash"`
	Image      string  `json:"image"`
	State      string  `json:"state"`
	StartedAt  *string `json:"started_at"`
}

func edgeDecode(t *testing.T, raw []byte) edgeReply {
	t.Helper()
	var r edgeReply
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return r
}

func (e *edgeEnv) countCalls(method, path string) int {
	n := 0
	for _, c := range e.fake.Calls() {
		if c.Method == method && strings.HasSuffix(c.Path, path) {
			n++
		}
	}
	return n
}

// The first apply on a host: pull by the pinned digest, the labelled volume,
// one host-network container with the exact config as config.json.
func TestEdgeApplyStarts(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
	hash := edge.HashConfig([]byte(edgeConfigA))
	if r.Error != "" || r.Outcome != edge.OutcomeStarted || r.ConfigHash != hash || r.Image != edge.Image {
		t.Fatalf("%+v", r)
	}
	if e.config() != edgeConfigA {
		t.Fatalf("config %q", e.config())
	}
	fi, err := os.Stat(filepath.Join(e.dir, edge.ConfigFile))
	if err != nil || fi.Mode().Perm() != 0o444 {
		t.Fatalf("%v %v", fi.Mode(), err)
	}
	for _, leftover := range []string{edgeNewFile, edgePrevFile} {
		if _, err := os.Stat(filepath.Join(e.dir, leftover)); !os.IsNotExist(err) {
			t.Fatalf("%s left: %v", leftover, err)
		}
	}
	var pull, create *engCall
	for _, c := range e.fake.Calls() {
		c := c
		switch {
		case strings.HasSuffix(c.Path, "/images/create"):
			pull = &c
		case strings.HasSuffix(c.Path, "/containers/create"):
			create = &c
		}
	}
	if pull == nil || pull.Query != (url.Values{"fromImage": {edge.Image}}).Encode() {
		t.Fatalf("pull %+v", pull)
	}
	if create == nil || create.Query != "name=deploybot-edge" {
		t.Fatalf("create %+v", create)
	}
	var body map[string]any
	if err := json.Unmarshal(create.Body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Image": edge.Image,
		"Cmd":   []any{"caddy", "run", "--config", "/run/edge/config.json"},
		"Labels": map[string]any{
			images.LabelOwner: images.OwnerValue, images.LabelHost: e.host,
			images.LabelKind: "edge", edge.LabelConfigHash: hash,
		},
		"HostConfig": map[string]any{
			"NetworkMode":   "host",
			"RestartPolicy": map[string]any{"Name": "unless-stopped"},
			"Mounts": []any{
				map[string]any{"Type": "volume", "Source": "deploybot-edge-data", "Target": "/data", "ReadOnly": false},
				map[string]any{"Type": "bind", "Source": "/var/lib/bot-agent-runner/edge", "Target": "/run/edge", "ReadOnly": false},
			},
			"CgroupnsMode":   "private",
			"PidsLimit":      float64(1024),
			"LogConfig":      map[string]any{"Type": "json-file", "Config": map[string]any{"max-size": "10m", "max-file": "3"}},
			"CapDrop":        []any{"ALL"},
			"CapAdd":         []any{"NET_BIND_SERVICE"},
			"SecurityOpt":    []any{"no-new-privileges:true"},
			"ReadonlyRootfs": true,
			"Tmpfs":          map[string]any{"/config": "rw,nosuid,nodev,size=16m", "/tmp": "rw,nosuid,nodev,size=16m"},
			"Memory":         float64(512 << 20),
			"MemorySwap":     float64(512 << 20),
		},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("create body\n got %v\nwant %v", body, want)
	}
	vol := e.fake.vols[edge.VolumeName]
	if vol == nil || vol.driver != "local" || !reflect.DeepEqual(vol.labels, map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host, images.LabelKind: "edge",
	}) {
		t.Fatalf("volume %+v", vol)
	}
	if c := e.fake.ctrs[edge.ContainerName]; c == nil || c.state != "running" {
		t.Fatalf("container %+v", c)
	}
	if len(e.caddy.Loads()) != 0 {
		t.Fatal("a fresh edge was also loaded")
	}
	for _, h := range e.caddy.hosts {
		if h != "127.0.0.1" {
			t.Fatalf("admin host %q", h)
		}
	}
}

// Idempotent: the same hash on a running edge with the pinned image is
// unchanged, with no Docker mutation and no load.
func TestEdgeApplyUnchanged(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	hash := edge.HashConfig([]byte(edgeConfigA))
	e.fake.add(edge.ContainerName, "running", edge.Image, e.edgeLabels(hash))
	e.writeConfig(edgeConfigA)
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
	if r.Outcome != edge.OutcomeUnchanged || r.ConfigHash != hash || r.Image != edge.Image {
		t.Fatalf("%+v", r)
	}
	for _, c := range e.fake.Calls() {
		if c.Method != http.MethodGet {
			t.Fatalf("mutation %s %s", c.Method, c.Path)
		}
	}
	if len(e.caddy.Loads()) != 0 {
		t.Fatal("loaded")
	}
}

// A new config on a running edge is loaded through the admin socket, byte
// for byte, and only then becomes config.json. The container stays.
func TestEdgeApplyReloads(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	id := e.fake.add(edge.ContainerName, "running", edge.Image, e.edgeLabels(edge.HashConfig([]byte(edgeConfigA))))
	e.writeConfig(edgeConfigA)
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigB)))
	if r.Outcome != edge.OutcomeApplied || r.ConfigHash != edge.HashConfig([]byte(edgeConfigB)) {
		t.Fatalf("%+v", r)
	}
	if loads := e.caddy.Loads(); len(loads) != 1 || string(loads[0]) != edgeConfigB {
		t.Fatalf("loads %q", loads)
	}
	if e.config() != edgeConfigB {
		t.Fatalf("config %q", e.config())
	}
	if c := e.fake.ctrs[edge.ContainerName]; c == nil || c.id != id || e.countCalls(http.MethodPost, "/containers/create") != 0 {
		t.Fatal("container replaced")
	}
}

// Caddy refuses the config: invalid_config with its first error line; the
// previous config.json stays and no .new file is left.
func TestEdgeApplyInvalidConfig(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	e.fake.add(edge.ContainerName, "running", edge.Image, e.edgeLabels(edge.HashConfig([]byte(edgeConfigA))))
	e.writeConfig(edgeConfigA)
	e.caddy.refuse = "loading new config: http app module: start: listening on :443: bad\nsecond line"
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigB)))
	if r.Error != edge.ReasonInvalidConfig || r.Detail != "loading new config: http app module: start: listening on :443: bad" {
		t.Fatalf("%+v", r)
	}
	if e.config() != edgeConfigA {
		t.Fatalf("config replaced %q", e.config())
	}
	if _, err := os.Stat(filepath.Join(e.dir, edgeNewFile)); !os.IsNotExist(err) {
		t.Fatal(".new left")
	}
}

// Every refusal that comes before any change, as a table.
func TestEdgeApplyRefusals(t *testing.T) {
	cfg := applyReq(edgeConfigA)
	for _, tc := range []struct {
		name   string
		policy string
		body   []byte
		setup  func(e *edgeEnv)
		code   string
		detail string
	}{
		{name: "no key", policy: "enabled = true\n", body: cfg, code: edge.ReasonDisabled},
		{name: "key false", policy: "edge = false\n", body: cfg, code: edge.ReasonDisabled},
		{name: "invalid policy", policy: "edge = true\nnope = 1\n", body: cfg, code: edge.ReasonDisabled},
		{name: "bad hash", policy: edgePolicy, body: []byte(`{"config":` + edgeConfigA + `,"config_hash":"` + strings.Repeat("0", 64) + `"}`), code: edge.ReasonBadSpec, detail: "config_hash"},
		{name: "tcp admin", policy: edgePolicy, body: applyReq(`{"admin":{"listen":"localhost:2019"},"apps":{}}`), code: edge.ReasonBadSpec, detail: "admin.listen"},
		{name: "Admin case", policy: edgePolicy, body: applyReq(`{"admin":{"listen":"unix//run/edge/admin.sock"},"Admin":{"listen":"localhost:2019"},"apps":{}}`), code: edge.ReasonBadSpec, detail: "config.Admin"},
		{name: "metadata upstream", policy: edgePolicy, code: edge.ReasonBadSpec, detail: "apps.http.servers.edge.routes[0].handle[0].upstreams[0].dial",
			body: applyReq(`{"admin":{"listen":"unix//run/edge/admin.sock"},"apps":{"http":{"servers":{"edge":{"listen":[":443"],"routes":[{"match":[{"host":["a.example"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"169.254.169.254:80"}]}]}]}}}}}`)},
		{name: "foreign container", policy: edgePolicy, body: cfg, code: edge.ReasonConflict, setup: func(e *edgeEnv) {
			e.fake.add(edge.ContainerName, "running", "caddy:2", map[string]string{})
		}},
		{name: "project label", policy: edgePolicy, body: cfg, code: edge.ReasonConflict, setup: func(e *edgeEnv) {
			l := e.edgeLabels(hashC)
			l[images.LabelProject] = "deploybot"
			e.fake.add(edge.ContainerName, "running", edge.Image, l)
		}},
		{name: "other host", policy: edgePolicy, body: cfg, code: edge.ReasonConflict, setup: func(e *edgeEnv) {
			l := e.edgeLabels(hashC)
			l[images.LabelHost] = "0123456789abcdef"
			e.fake.add(edge.ContainerName, "running", edge.Image, l)
		}},
		{name: "container on 443", policy: edgePolicy, body: cfg, code: edge.ReasonPortInUse, detail: "port 443 is used by daikou-caddy", setup: func(e *edgeEnv) {
			e.fake.add("daikou-caddy", "running", "caddy:2", nil)
			e.fake.ctrs["daikou-caddy"].ports = []portView{{HostIP: "0.0.0.0", HostPort: 443, ContainerPort: 443}}
		}},
		{name: "host process on 80", policy: edgePolicy, body: cfg, code: edge.ReasonPortInUse, detail: "port 80 is used by nginx", setup: func(e *edgeEnv) {
			e.listen(80, 4242, 77, "nginx", "0::/system.slice/nginx.service")
		}},
		{name: "unknown holder", policy: edgePolicy, body: cfg, code: edge.ReasonPortInUse, detail: "port 443 is in use", setup: func(e *edgeEnv) {
			e.listen(443, 4243, 0, "", "")
		}},
		{name: "foreign volume", policy: edgePolicy, body: cfg, code: edge.ReasonConflict, setup: func(e *edgeEnv) {
			e.fake.vols[edge.VolumeName] = &fakeVol{driver: "local", labels: map[string]string{
				images.LabelOwner: images.OwnerValue, images.LabelHost: e.host, images.LabelProject: "deploybot",
			}}
		}},
		{name: "bind volume", policy: edgePolicy, body: cfg, code: edge.ReasonConflict, setup: func(e *edgeEnv) {
			e.fake.vols[edge.VolumeName] = &fakeVol{driver: "local", options: map[string]string{"o": "bind"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEdgeEnv(t, tc.policy)
			if tc.setup != nil {
				tc.setup(e)
			}
			r := edgeDecode(t, e.call(edge.VerbApply, tc.body))
			if r.Error != tc.code || r.Detail != tc.detail {
				t.Fatalf("%+v", r)
			}
			for _, c := range e.fake.Calls() {
				if c.Method != http.MethodGet && !strings.HasSuffix(c.Path, "/volumes/create") {
					t.Fatalf("mutation %s %s", c.Method, c.Path)
				}
			}
			if e.config() != "" {
				t.Fatal("config written")
			}
		})
	}
}

// The edge's own listeners do not make it busy: a socket held by a process
// in its cgroup, or one no process can be found for while it runs.
func TestEdgeOwnPortsNotBusy(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	hash := edge.HashConfig([]byte(edgeConfigA))
	id := e.fake.add(edge.ContainerName, "running", edge.Image, e.edgeLabels(hash))
	e.writeConfig(edgeConfigA)
	e.listen(80, 5000, 90, "caddy", "0::/system.slice/docker-"+id+".scope")
	e.listen(443, 5001, 0, "", "")
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
	if r.Outcome != edge.OutcomeUnchanged {
		t.Fatalf("%+v", r)
	}
}

// A labelled volume is reused and an unlabelled local one (made by an
// operator to copy certificates in) is adopted; neither is created again.
func TestEdgeVolumeReuse(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"labelled":   nil,
		"unlabelled": {},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEdgeEnv(t, edgePolicy)
			if labels == nil {
				labels = map[string]string{images.LabelOwner: images.OwnerValue, images.LabelHost: e.host, images.LabelKind: "edge"}
			}
			e.fake.vols[edge.VolumeName] = &fakeVol{driver: "local", labels: labels}
			r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
			if r.Outcome != edge.OutcomeStarted || e.countCalls(http.MethodPost, "/volumes/create") != 0 {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// Another image, or a stopped edge: the container is replaced (pull only
// for a new image) and started with the new config.
func TestEdgeApplyRecreates(t *testing.T) {
	for _, tc := range []struct {
		name, state, image string
		pulls              int
	}{
		{"old image", "running", "docker.io/library/caddy@" + digestA, 1},
		{"stopped", "exited", "", 0}, // "" is the pinned image
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEdgeEnv(t, edgePolicy)
			if tc.image == "" {
				tc.image = edge.Image
			}
			old := e.fake.add(edge.ContainerName, tc.state, tc.image, e.edgeLabels(edge.HashConfig([]byte(edgeConfigA))))
			e.writeConfig(edgeConfigA)
			r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigB)))
			if r.Outcome != edge.OutcomeStarted {
				t.Fatalf("%+v", r)
			}
			c := e.fake.ctrs[edge.ContainerName]
			if c == nil || c.id == old || c.image != edge.Image || c.state != "running" {
				t.Fatalf("%+v", c)
			}
			if e.fake.byID[old] != nil {
				t.Fatal("old container kept")
			}
			if n := e.countCalls(http.MethodPost, "/images/create"); n != tc.pulls {
				t.Fatalf("pulls %d", n)
			}
			if e.config() != edgeConfigB {
				t.Fatalf("config %q", e.config())
			}
		})
	}
}

// A new container that will not start with the config: invalid_config with
// Caddy's exit line, and the previous config.json is back in place.
func TestEdgeApplyStartFails(t *testing.T) {
	for _, hadPrev := range []bool{true, false} {
		e := newEdgeEnv(t, edgePolicy)
		if hadPrev {
			e.fake.add(edge.ContainerName, "exited", edge.Image, e.edgeLabels(edge.HashConfig([]byte(edgeConfigA))))
			e.writeConfig(edgeConfigA)
		}
		relisten := e.fake.startHook
		e.fake.startHook = func(id string) {
			relisten(id)
			e.fake.byID[id].restarts = 1
		}
		e.fake.logBody = []byte("{\"level\":\"info\",\"msg\":\"using config\"}\nError: loading initial config: bad route\n")
		r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigB)))
		if r.Error != edge.ReasonInvalidConfig || r.Detail != "Error: loading initial config: bad route" {
			t.Fatalf("prev=%v %+v", hadPrev, r)
		}
		want := ""
		if hadPrev {
			want = edgeConfigA
		}
		if e.config() != want {
			t.Fatalf("prev=%v config %q", hadPrev, e.config())
		}
		if _, err := os.Stat(filepath.Join(e.dir, edgePrevFile)); !os.IsNotExist(err) {
			t.Fatal(".prev left")
		}
	}
}

// A started edge that never answers on its admin socket is
// docker_unavailable, not success.
func TestEdgeApplyNotReady(t *testing.T) {
	old := edgeReadyBudget
	edgeReadyBudget = 300 * time.Millisecond
	t.Cleanup(func() { edgeReadyBudget = old })
	e := newEdgeEnv(t, edgePolicy)
	e.caddy.down = true
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
	if r.Error != edge.ReasonDocker {
		t.Fatalf("%+v", r)
	}
}

func TestEdgeStop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  string
		labels  func(e *edgeEnv) map[string]string
		outcome string
		code    string
	}{
		{name: "absent", policy: edgePolicy, outcome: edge.OutcomeAbsent},
		{name: "stopped", policy: edgePolicy, outcome: edge.OutcomeStopped, labels: func(e *edgeEnv) map[string]string { return e.edgeLabels(hashC) }},
		// stop works after the key is removed.
		{name: "key off", policy: "enabled = false\n", outcome: edge.OutcomeStopped, labels: func(e *edgeEnv) map[string]string { return e.edgeLabels(hashC) }},
		{name: "foreign", policy: edgePolicy, code: edge.ReasonConflict, labels: func(*edgeEnv) map[string]string { return map[string]string{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEdgeEnv(t, tc.policy)
			e.fake.vols[edge.VolumeName] = &fakeVol{driver: "local"}
			e.writeConfig(edgeConfigA)
			if tc.labels != nil {
				e.fake.add(edge.ContainerName, "running", edge.Image, tc.labels(e))
			}
			r := edgeDecode(t, e.call(edge.VerbStop, []byte(`{}`)))
			if r.Error != tc.code || r.Outcome != tc.outcome {
				t.Fatalf("%+v", r)
			}
			gone := e.fake.ctrs[edge.ContainerName] == nil
			if gone != (tc.code == "") {
				t.Fatalf("container gone=%v", gone)
			}
			if e.fake.vols[edge.VolumeName] == nil || e.config() != edgeConfigA {
				t.Fatal("volume or config removed")
			}
		})
	}
	e := newEdgeEnv(t, edgePolicy)
	if r := edgeDecode(t, e.call(edge.VerbStop, []byte(`{"x":1}`))); r.Error != edge.ReasonBadSpec {
		t.Fatalf("%+v", r)
	}
}

func TestEdgeStatus(t *testing.T) {
	e := newEdgeEnv(t, "")
	r := edgeDecode(t, e.call(edge.VerbStatus, []byte(`{}`)))
	if r.State != edge.StateAbsent || r.Image != "" || r.ConfigHash != "" || r.StartedAt != nil {
		t.Fatalf("%+v", r)
	}
	id := e.fake.add(edge.ContainerName, "running", edge.Image, e.edgeLabels(hashC))
	e.fake.byID[id].started = "2026-11-17T10:00:00.123Z"
	e.writeConfig(edgeConfigA)
	r = edgeDecode(t, e.call(edge.VerbStatus, []byte(`{}`)))
	if r.State != edge.StateRunning || r.Image != edge.Image || r.ConfigHash != edge.HashConfig([]byte(edgeConfigA)) ||
		r.StartedAt == nil || *r.StartedAt == "" {
		t.Fatalf("%+v", r)
	}
	e.fake.byID[id].state = "exited"
	r = edgeDecode(t, e.call(edge.VerbStatus, []byte(`{}`)))
	if r.State != edge.StateStopped || r.StartedAt != nil {
		t.Fatalf("%+v", r)
	}
	for _, c := range e.fake.Calls() {
		if c.Method != http.MethodGet {
			t.Fatalf("status mutated %s %s", c.Method, c.Path)
		}
	}
	if r := edgeDecode(t, e.call(edge.VerbStatus, []byte(`[]`))); r.Error != edge.ReasonBadSpec {
		t.Fatalf("%+v", r)
	}
}

// container_list serves edge discovery on a host with only the edge on;
// other image verbs stay refused there.
func TestEdgeOnlyPolicyListsContainers(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	e.fake.add(edge.ContainerName, "running", edge.Image, e.edgeLabels(hashC))
	var list images.ListResponse
	if err := json.Unmarshal(e.call(images.VerbList, []byte(`{}`)), &list); err != nil || len(list.Items) != 1 ||
		list.Items[0].Labels[images.LabelKind] != "edge" {
		t.Fatalf("%+v %v", list, err)
	}
	if code := imgCode(t, e.call(images.VerbStatus, []byte(`{"project":"mohiguide","entry":"web"}`))); code != "policy_denied" {
		t.Fatal(code)
	}
	e.setPolicy("enabled = true\n")
	if code := imgCode(t, e.call(images.VerbList, []byte(`{}`))); code != "policy_denied" {
		t.Fatal(code)
	}
}

// The edge container does not count toward max_containers.
func TestCountOwnedSkipsEdge(t *testing.T) {
	rows := []listed{
		{Labels: map[string]string{images.LabelOwner: images.OwnerValue, images.LabelKind: edge.KindValue}},
		{Labels: map[string]string{images.LabelOwner: images.OwnerValue, images.LabelProject: "p"}},
		{Labels: map[string]string{}},
	}
	if n := countOwned(rows); n != 1 {
		t.Fatal(n)
	}
}

// The refusal is the code plus a sanitised detail; nothing else.
func TestEdgeErrShape(t *testing.T) {
	var lines []string
	sys := Sys{Logf: func(f string, a ...any) { lines = append(lines, f) }, logVerb: edge.VerbApply}
	raw := edgeErr(sys, "made_up", "two\nlines")
	if string(raw) != `{"error":"helper_unavailable","detail":"two"}` {
		t.Fatal(string(raw))
	}
	if len(lines) != 2 {
		t.Fatal(lines)
	}
	if got := Handle(context.Background(), edge.VerbStatus, []byte(strings.Repeat(" ", images.MaxFrame+1)), Sys{}); string(got) != `{"error":"bad_spec"}` {
		t.Fatal(string(got))
	}
}

// M4: while the image is the placeholder, apply is edge_disabled before any
// Docker call, even with the key on.
func TestEdgeApplyPlaceholderDigest(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	edge.Image = "docker.io/library/caddy@sha256:" + strings.Repeat("0", 64)
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
	if r.Error != edge.ReasonDisabled || len(e.fake.Calls()) != 0 {
		t.Fatalf("%+v %d", r, len(e.fake.Calls()))
	}
}

// L7: the socket tables are read strictly, and UDP 443 (HTTP/3) counts.
func TestEdgePortTables(t *testing.T) {
	t.Run("tcp unreadable", func(t *testing.T) {
		e := newEdgeEnv(t, edgePolicy)
		os.Remove(filepath.Join(e.proc, "net", "tcp"))
		r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
		if r.Error != edge.ReasonDocker || e.countCalls(http.MethodPost, "/containers/create") != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("udp unreadable", func(t *testing.T) {
		e := newEdgeEnv(t, edgePolicy)
		os.Remove(filepath.Join(e.proc, "net", "udp"))
		if r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA))); r.Error != edge.ReasonDocker {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("udp 443 held", func(t *testing.T) {
		e := newEdgeEnv(t, edgePolicy)
		e.socket("udp6", "07", 443, 6100, 55, "quicd", "0::/system.slice/quicd.service")
		r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
		if r.Error != edge.ReasonPortInUse || r.Detail != "udp port 443 is used by quicd" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("udp 443 is the edge's", func(t *testing.T) {
		e := newEdgeEnv(t, edgePolicy)
		hash := edge.HashConfig([]byte(edgeConfigA))
		id := e.fake.add(edge.ContainerName, "running", edge.Image, e.edgeLabels(hash))
		e.writeConfig(edgeConfigA)
		e.socket("udp", "07", 443, 6101, 91, "caddy", "0::/system.slice/docker-"+id+".scope")
		if r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA))); r.Outcome != edge.OutcomeUnchanged {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("udp other port free", func(t *testing.T) {
		e := newEdgeEnv(t, edgePolicy)
		e.socket("udp", "07", 53, 6102, 0, "", "")
		e.socket("tcp", "01", 443, 6103, 0, "", "") // an established connection, not a listener
		if r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA))); r.Outcome != edge.OutcomeStarted {
			t.Fatalf("%+v", r)
		}
	})
}

// L9: a stale admin socket is removed before the new container starts.
func TestEdgeStaleSocketRemoved(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	sawStale := false
	relisten := e.fake.startHook
	e.fake.startHook = func(id string) {
		if _, err := os.Lstat(e.caddy.sock); err == nil {
			sawStale = true
		}
		relisten(id)
	}
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigA)))
	if r.Outcome != edge.OutcomeStarted || sawStale {
		t.Fatalf("%+v stale=%v", r, sawStale)
	}
	// Something that is not a socket under that name is never removed.
	e2 := newEdgeEnv(t, edgePolicy)
	os.Remove(e2.caddy.sock)
	if err := os.WriteFile(e2.caddy.sock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = edgeDecode(t, e2.call(edge.VerbApply, applyReq(edgeConfigA)))
	if r.Error != edge.ReasonBadSpec {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(e2.caddy.sock); string(b) != "x" {
		t.Fatal("file removed")
	}
}

// M5: when the new container will not start, the previous one is recreated
// from its own image with the previous config, and runs again.
func TestEdgeStartFailRestoresPrevious(t *testing.T) {
	e := newEdgeEnv(t, edgePolicy)
	oldImage := "docker.io/library/caddy@" + digestA
	hashA := edge.HashConfig([]byte(edgeConfigA))
	old := e.fake.add(edge.ContainerName, "running", oldImage, e.edgeLabels(hashA))
	e.writeConfig(edgeConfigA)
	relisten := e.fake.startHook
	failed := false
	e.fake.startHook = func(id string) {
		if !failed && e.fake.byID[id].image == edge.Image {
			failed = true
			e.fake.startFailID = id
		}
		relisten(id)
	}
	r := edgeDecode(t, e.call(edge.VerbApply, applyReq(edgeConfigB)))
	if r.Error != edge.ReasonDocker {
		t.Fatalf("%+v", r)
	}
	c := e.fake.ctrs[edge.ContainerName]
	if c == nil || c.id == old || c.image != oldImage || c.state != "running" || c.labels[edge.LabelConfigHash] != hashA {
		t.Fatalf("%+v", c)
	}
	if e.config() != edgeConfigA || len(e.fake.ctrs) != 1 {
		t.Fatalf("config %q, %d containers", e.config(), len(e.fake.ctrs))
	}
}
