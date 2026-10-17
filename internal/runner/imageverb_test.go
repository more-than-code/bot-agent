package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hashC   = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	hashD   = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	secret  = "SUPERSECRETVALUE"

	imagePolicy = "enabled = true\n" +
		"images = [\"ghcr.io/joe-and-his-friends/*\"]\n" +
		"projects = [\"mohiguide\"]\n" +
		"max_containers = 20\n" +
		"allow_public_ports = false\n"
)

type slotSnap struct {
	op    string
	files map[string]string
}

type engCall struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

type fakeCtr struct {
	id, name, state, image string
	labels                 map[string]string
	exit, restarts         int
	started, finished      string
	ports                  []portView
}

type fakeVol struct {
	driver  string
	options map[string]string
	labels  map[string]string
}

type fakeNet struct {
	driver string
	labels map[string]string
}

type fakeDocker struct {
	mu           sync.Mutex
	calls        []engCall
	ctrs         map[string]*fakeCtr
	byID         map[string]*fakeCtr
	vols         map[string]*fakeVol
	nets         map[string]*fakeNet
	next         int
	pullCode     int
	pullErr      string
	pullRedirect string
	pullHuge     bool
	pullWrote    chan struct{}
	pullRelease  chan struct{}
	logHuge      bool
	logWrote     chan struct{}
	logRelease   chan struct{}
	omitDigest   bool
	badDigest    bool
	renameFail   bool
	startFail    bool
	// renameLand is the 1-based rename that is applied and then rejected.
	// Zero lets every rename finish. renameFail still rejects before applying.
	renameLand int
	renameN    int
	renameHit  chan struct{}
	renameWait chan struct{}
	// listFailAfter rejects lists whose 1-based count is at least this.
	// Zero lists every call.
	listFailAfter int
	listN         int
	// slotDir, when set, is read at create and at start. The snapshot is
	// the file contents in that directory at the moment the call arrives.
	slotDir    string
	slotSeen   []slotSnap
	pullID     string
	logBody    []byte
	volumeRace string
	block      chan struct{}
	entered    chan struct{}
	once       sync.Once
}

func startFake(t *testing.T) (*fakeDocker, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "baimg")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	f := &fakeDocker{
		ctrs: map[string]*fakeCtr{}, byID: map[string]*fakeCtr{},
		vols: map[string]*fakeVol{}, nets: map[string]*fakeNet{},
	}
	srv := &http.Server{Handler: f, ErrorLog: log.New(io.Discard, "", 0)}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		os.RemoveAll(dir)
	})
	return f, sock
}

func (f *fakeDocker) Calls() []engCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]engCall(nil), f.calls...)
}

func (f *fakeDocker) add(name, state, image string, labels map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := padHex(f.next)
	cp := map[string]string{}
	for k, v := range labels {
		cp[k] = v
	}
	c := &fakeCtr{
		id: id, name: name, state: state, image: image, labels: cp,
		started: "0001-01-01T00:00:00Z", finished: "0001-01-01T00:00:00Z",
	}
	f.ctrs[name] = c
	f.byID[id] = c
	return id
}

func padHex(n int) string {
	const digits = "0123456789abcdef"
	var b [16]byte
	v := uint64(n)
	i := len(b) - 1
	for {
		b[i] = digits[v&0xf]
		v >>= 4
		if v == 0 {
			break
		}
		i--
	}
	tail := string(b[i:])
	return strings.Repeat("0", 64-len(tail)) + tail
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	f.mu.Lock()
	f.calls = append(f.calls, engCall{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Header: r.Header.Clone(), Body: append([]byte(nil), body...),
	})
	f.mu.Unlock()
	// Record the call, then block only the first request. A second request
	// that skipped the flock is visible in Calls before release.
	if f.block != nil {
		f.once.Do(func() {
			close(f.entered)
			<-f.block
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case path == "/v1.47/images/create" && r.Method == http.MethodPost:
		f.servePull(w, r)
	case path == "/v1.47/containers/json" && r.Method == http.MethodGet:
		f.serveList(w)
	case path == "/v1.47/containers/create" && r.Method == http.MethodPost:
		f.serveCreate(w, r, body)
	case strings.HasPrefix(path, "/v1.47/containers/"):
		f.serveContainer(w, r, strings.TrimPrefix(path, "/v1.47/containers/"))
	case path == "/v1.47/volumes/create" && r.Method == http.MethodPost:
		f.serveVolCreate(w, body)
	case strings.HasPrefix(path, "/v1.47/volumes/") && r.Method == http.MethodGet:
		f.serveVol(w, strings.TrimPrefix(path, "/v1.47/volumes/"))
	case path == "/v1.47/networks/create" && r.Method == http.MethodPost:
		f.serveNetCreate(w, body)
	case strings.HasPrefix(path, "/v1.47/networks/") && r.Method == http.MethodGet:
		f.serveNet(w, strings.TrimPrefix(path, "/v1.47/networks/"))
	default:
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}

func (f *fakeDocker) servePull(w http.ResponseWriter, r *http.Request) {
	if f.pullRedirect != "" {
		w.Header().Set("Location", f.pullRedirect)
		w.WriteHeader(http.StatusFound)
		return
	}
	if f.pullHuge {
		f.writeStall(w, maxPullStream+1, 'x', f.pullWrote, f.pullRelease)
		return
	}
	if f.pullCode != 0 {
		w.WriteHeader(f.pullCode)
		return
	}
	if f.pullErr != "" {
		json.NewEncoder(w).Encode(map[string]string{"error": f.pullErr})
		return
	}
	got := digestA
	from := r.URL.Query().Get("fromImage")
	if i := strings.LastIndex(from, "@"); i >= 0 {
		got = from[i+1:]
	}
	if f.badDigest {
		got = digestB
	}
	if f.omitDigest {
		if f.pullID != "" {
			json.NewEncoder(w).Encode(map[string]any{"aux": map[string]string{"ID": f.pullID}})
		}
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "Digest: " + got})
	aux := map[string]string{"Digest": got}
	if f.pullID != "" {
		aux["ID"] = f.pullID
	}
	json.NewEncoder(w).Encode(map[string]any{"aux": aux})
}

func (f *fakeDocker) writeStall(w http.ResponseWriter, total int, fill byte, wrote, release chan struct{}) {
	w.WriteHeader(http.StatusOK)
	flush(w)
	chunk := []byte(strings.Repeat(string(fill), 32<<10))
	left := total
	for left > 0 {
		n := len(chunk)
		if n > left {
			n = left
		}
		if _, err := w.Write(chunk[:n]); err != nil {
			break
		}
		left -= n
		flush(w)
	}
	if wrote != nil {
		close(wrote)
	}
	if release != nil {
		<-release
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (f *fakeDocker) serveList(w http.ResponseWriter) {
	f.listN++
	if f.listFailAfter > 0 && f.listN >= f.listFailAfter {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	var items []map[string]any
	for _, c := range f.ctrs {
		ports := []map[string]any{}
		for _, p := range c.ports {
			ports = append(ports, map[string]any{
				"IP": p.HostIP, "PrivatePort": p.ContainerPort, "PublicPort": p.HostPort, "Type": "tcp",
			})
		}
		labels := map[string]string{"com.docker.compose.project": secret}
		for k, v := range c.labels {
			labels[k] = v
		}
		items = append(items, map[string]any{
			"Id": c.id, "Names": []string{"/" + c.name}, "Image": c.image, "State": c.state,
			"Labels": labels, "Ports": ports,
			"Env":         []string{"PASSWORD=" + secret},
			"HostConfig":  map[string]any{"Binds": []string{"/etc:/host"}},
			"Mounts":      []any{map[string]string{"Source": "/var/lib/docker"}},
			"GraphDriver": map[string]string{"Data": secret},
		})
	}
	json.NewEncoder(w).Encode(items)
}

func (f *fakeDocker) serveCreate(w http.ResponseWriter, r *http.Request, body []byte) {
	f.snapSlot("create")
	name := r.URL.Query().Get("name")
	if _, ok := f.ctrs[name]; ok {
		http.Error(w, "exists", http.StatusConflict)
		return
	}
	var raw struct {
		Image  string
		Labels map[string]string
	}
	if json.Unmarshal(body, &raw) != nil || name == "" {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	f.next++
	id := padHex(100 + f.next)
	c := &fakeCtr{
		id: id, name: name, state: "created", image: raw.Image, labels: raw.Labels,
		started: "0001-01-01T00:00:00Z", finished: "0001-01-01T00:00:00Z",
	}
	f.ctrs[name] = c
	f.byID[id] = c
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"Id": id})
}

func (f *fakeDocker) serveContainer(w http.ResponseWriter, r *http.Request, rest string) {
	id, op, _ := strings.Cut(rest, "/")
	c := f.byID[id]
	switch {
	case op == "json" && r.Method == http.MethodGet:
		if c == nil {
			http.NotFound(w, r)
			return
		}
		f.writeInspect(w, c)
	case op == "start" && r.Method == http.MethodPost:
		f.snapSlot("start")
		if c == nil {
			http.NotFound(w, r)
			return
		}
		if f.startFail {
			http.Error(w, "start failed", http.StatusInternalServerError)
			return
		}
		if c.state == "running" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.state = "running"
		w.WriteHeader(http.StatusNoContent)
	case op == "stop" && r.Method == http.MethodPost:
		if c == nil {
			http.NotFound(w, r)
			return
		}
		c.state = "exited"
		w.WriteHeader(http.StatusNoContent)
	case op == "rename" && r.Method == http.MethodPost:
		if c == nil {
			http.NotFound(w, r)
			return
		}
		if f.renameFail {
			http.Error(w, "rename failed", http.StatusInternalServerError)
			return
		}
		next := r.URL.Query().Get("name")
		delete(f.ctrs, c.name)
		c.name = next
		f.ctrs[next] = c
		f.renameN++
		if f.renameLand > 0 && f.renameN == f.renameLand {
			// Close and wait while this request is still the one the
			// client is blocked on. The waiter only cancels that client;
			// it does not take this lock, so the wait cannot deadlock it.
			if f.renameHit != nil {
				close(f.renameHit)
			}
			if f.renameWait != nil {
				<-f.renameWait
			}
			http.Error(w, "rename failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case op == "logs" && r.Method == http.MethodGet:
		if c == nil {
			http.NotFound(w, r)
			return
		}
		if f.logHuge {
			f.writeStall(w, maxLogStream+1, 'y', f.logWrote, f.logRelease)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.logBody)
	case op == "" && r.Method == http.MethodDelete:
		if c == nil {
			http.NotFound(w, r)
			return
		}
		delete(f.ctrs, c.name)
		delete(f.byID, c.id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}

func (f *fakeDocker) writeInspect(w http.ResponseWriter, c *fakeCtr) {
	ports := map[string]any{}
	for _, p := range c.ports {
		key := itoa(p.ContainerPort) + "/tcp"
		ports[key] = []map[string]string{{"HostIp": p.HostIP, "HostPort": itoa(p.HostPort)}}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"Id": c.id, "Name": "/" + c.name,
		"Config": map[string]any{
			"Image": c.image, "Labels": c.labels,
			"Env": []string{"PASSWORD=" + secret}, "Cmd": []string{"print-secret"},
			"Entrypoint": []string{"/bin/leak"},
		},
		"State": map[string]any{
			"Status": c.state, "ExitCode": c.exit, "StartedAt": c.started, "FinishedAt": c.finished,
		},
		"RestartCount":    c.restarts,
		"NetworkSettings": map[string]any{"Ports": ports},
		"HostConfig":      map[string]any{"Binds": []string{"/etc:/host"}},
		"Mounts":          []any{map[string]string{"Source": "/var/lib/docker"}},
		"GraphDriver":     map[string]string{"Data": secret},
	})
}

func (f *fakeDocker) serveVol(w http.ResponseWriter, name string) {
	v, ok := f.vols[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"Driver": v.driver, "Options": v.options, "Labels": v.labels})
}

func (f *fakeDocker) serveVolCreate(w http.ResponseWriter, body []byte) {
	var raw struct {
		Name    string
		Driver  string
		Labels  map[string]string
		Options map[string]string
	}
	if json.Unmarshal(body, &raw) != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	if f.volumeRace == "bind" {
		f.vols[raw.Name] = &fakeVol{driver: "local", options: map[string]string{"device": "/host", "o": "bind"}, labels: raw.Labels}
		http.Error(w, "exists", http.StatusConflict)
		return
	}
	if _, ok := f.vols[raw.Name]; ok || f.volumeRace == "good" {
		if f.vols[raw.Name] == nil {
			f.vols[raw.Name] = &fakeVol{driver: raw.Driver, options: raw.Options, labels: raw.Labels}
		}
		http.Error(w, "exists", http.StatusConflict)
		return
	}
	f.vols[raw.Name] = &fakeVol{driver: raw.Driver, options: raw.Options, labels: raw.Labels}
	w.WriteHeader(http.StatusCreated)
}

func (f *fakeDocker) serveNet(w http.ResponseWriter, name string) {
	n, ok := f.nets[name]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"Driver": n.driver, "Labels": n.labels,
		"Options": map[string]string{"com.docker.network.bridge.enable_icc": "true"},
	})
}

func (f *fakeDocker) serveNetCreate(w http.ResponseWriter, body []byte) {
	var raw struct {
		Name   string
		Driver string
		Labels map[string]string
	}
	if json.Unmarshal(body, &raw) != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	f.nets[raw.Name] = &fakeNet{driver: raw.Driver, labels: raw.Labels}
	w.WriteHeader(http.StatusCreated)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

type imgEnv struct {
	t    *testing.T
	fake *fakeDocker
	sys  Sys
	host string
}

func newImgEnv(t *testing.T, policy string) *imgEnv {
	t.Helper()
	dir := t.TempDir()
	fake, sock := startFake(t)
	mid := filepath.Join(dir, "machine-id")
	if err := os.WriteFile(mid, []byte("test-machine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host, ok := images.HostID("test-machine\n")
	if !ok {
		t.Fatal("host")
	}
	pol := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(pol, []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	return &imgEnv{t: t, fake: fake, host: host, sys: Sys{
		Root: filepath.Join(dir, "root"), Policy: pol, MachineID: mid, DockerSock: sock,
		UID: func(os.FileInfo) (uint32, bool) { return 0, true },
	}}
}

func (e *imgEnv) setPolicy(body string) {
	e.t.Helper()
	if err := os.WriteFile(e.sys.Policy, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *imgEnv) labels(entry string) map[string]string {
	return map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host,
		images.LabelProject: "mohiguide", images.LabelEntry: entry, images.LabelSpecHash: hashC,
	}
}

func (e *imgEnv) call(verb string, body []byte) []byte {
	e.t.Helper()
	return Handle(context.Background(), verb, body, e.sys)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func imgCode(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json %s: %v", raw, err)
	}
	if _, ok := m["ok"]; ok {
		t.Fatalf("program shape %s", raw)
	}
	s, _ := m["error"].(string)
	if s != "" && len(m) != 1 {
		t.Fatalf("error object %s", raw)
	}
	return s
}

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func forbidKeys(t *testing.T, raw []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			for k, child := range t {
				keys[k] = true
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	for _, k := range []string{"Env", "Cmd", "Entrypoint", "HostConfig", "Mounts", "GraphDriver", "Binds", "Config", "com.docker.compose.project"} {
		if keys[k] {
			t.Fatalf("forbidden key %s in %s", k, raw)
		}
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("secret leaked: %s", raw)
	}
}

func serviceSpec() images.ImageSpec {
	mem := 64
	return images.ImageSpec{
		Kind: "service", Image: "ghcr.io/joe-and-his-friends/mo-webadmin:1.2",
		Command: []string{"serve"}, Entrypoint: []string{"/bin/app"},
		Ports: []images.Port{{Host: 3031, Container: 3031}, {Host: 8080, Container: 80}},
		Env:   map[string]string{"NODE_ENV": "production"},
		Volumes: []images.Volume{
			{Name: "data", Target: "/var/www"},
			{Name: "ro", Target: "/etc/app", ReadOnly: true},
		},
		User: "app", MemoryMB: &mem,
	}
}

func serviceDeploy(hash string) images.DeployRequest {
	return images.DeployRequest{
		Project: "mohiguide", Entry: "web", Spec: serviceSpec(),
		Env: map[string]string{"A": "b", "NODE_ENV": "production"}, Digest: digestA, SpecHash: hash,
	}
}

func jobDeploy() images.DeployRequest {
	return images.DeployRequest{
		Project: "mohiguide", Entry: "job", Digest: digestA, SpecHash: hashC,
		Spec: images.ImageSpec{
			Kind: "job", Image: "ghcr.io/joe-and-his-friends/mo-job:1", Command: []string{"run"},
		},
	}
}

// snapSlot records the slot directory. The caller holds f.mu.
func (f *fakeDocker) snapSlot(op string) {
	if f.slotDir == "" {
		return
	}
	snap := slotSnap{op: op, files: map[string]string{}}
	entries, err := os.ReadDir(f.slotDir)
	if err == nil {
		for _, e := range entries {
			b, rerr := os.ReadFile(filepath.Join(f.slotDir, e.Name()))
			if rerr != nil {
				continue
			}
			snap.files[e.Name()] = string(b)
		}
	}
	f.slotSeen = append(f.slotSeen, snap)
}

func (f *fakeDocker) slotSnaps() []slotSnap {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]slotSnap, len(f.slotSeen))
	for i, s := range f.slotSeen {
		cp := slotSnap{op: s.op, files: map[string]string{}}
		for k, v := range s.files {
			cp.files[k] = v
		}
		out[i] = cp
	}
	return out
}

func (f *fakeDocker) body(method, pathSuffix string) []byte {
	for _, c := range f.Calls() {
		if c.Method == method && strings.HasSuffix(c.Path, pathSuffix) {
			return c.Body
		}
	}
	return nil
}

func TestGoldenCreateBody(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	got := e.fake.body(http.MethodPost, "/containers/create")
	want := `{
		"Image":"ghcr.io/joe-and-his-friends/mo-webadmin@` + digestA + `",
		"Cmd":["serve"],"Entrypoint":["/bin/app"],
		"Env":["A=b","NODE_ENV=production"],
		"Labels":{
			"dev.deploybot.entry":"web","dev.deploybot.host":"` + e.host + `",
			"dev.deploybot.owner":"bot-agent","dev.deploybot.project":"mohiguide",
			"dev.deploybot.spec_hash":"` + hashC + `"
		},
		"ExposedPorts":{"3031/tcp":{},"80/tcp":{}},
		"User":"app",
		"HostConfig":{
			"PortBindings":{
				"3031/tcp":[{"HostIp":"127.0.0.1","HostPort":"3031"}],
				"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]
			},
			"Mounts":[
				{"Type":"volume","Source":"mohiguide-data","Target":"/var/www","ReadOnly":false},
				{"Type":"volume","Source":"mohiguide-ro","Target":"/etc/app","ReadOnly":true}
			],
			"RestartPolicy":{"Name":"unless-stopped"},
			"NetworkMode":"deploybot-mohiguide","CgroupnsMode":"private","PidsLimit":1024,
			"Memory":67108864,"MemorySwap":67108864
		},
		"NetworkingConfig":{"EndpointsConfig":{"deploybot-mohiguide":{"Aliases":["web"]}}}
	}`
	jsonEqual(t, got, want)
	if strings.Contains(string(got), "0.0.0.0") || strings.Contains(string(raw), "production") {
		t.Fatal("public bind or env leaked")
	}
	vol := e.fake.body(http.MethodPost, "/volumes/create")
	jsonEqual(t, vol, `{
		"Name":"mohiguide-data","Driver":"local",
		"Labels":{"dev.deploybot.host":"`+e.host+`","dev.deploybot.owner":"bot-agent","dev.deploybot.project":"mohiguide"}
	}`)
	net := e.fake.body(http.MethodPost, "/networks/create")
	var nm map[string]any
	if json.Unmarshal(net, &nm) != nil {
		t.Fatal(string(net))
	}
	if _, ok := nm["Options"]; ok {
		t.Fatal("network options")
	}
	if _, ok := nm["IPAM"]; ok {
		t.Fatal("network IPAM")
	}
	if nm["Driver"] != "bridge" {
		t.Fatal(nm["Driver"])
	}

	e2 := newImgEnv(t, imagePolicy)
	raw = e2.call(images.VerbDeploy, mustJSON(t, jobDeploy()))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	got = e2.fake.body(http.MethodPost, "/containers/create")
	want = `{
		"Image":"ghcr.io/joe-and-his-friends/mo-job@` + digestA + `",
		"Cmd":["run"],
		"Labels":{
			"dev.deploybot.entry":"job","dev.deploybot.host":"` + e2.host + `",
			"dev.deploybot.owner":"bot-agent","dev.deploybot.project":"mohiguide",
			"dev.deploybot.spec_hash":"` + hashC + `"
		},
		"HostConfig":{
			"RestartPolicy":{"Name":"no"},
			"NetworkMode":"deploybot-mohiguide","CgroupnsMode":"private","PidsLimit":1024
		},
		"NetworkingConfig":{"EndpointsConfig":{"deploybot-mohiguide":{"Aliases":["job"]}}}
	}`
	jsonEqual(t, got, want)
}

func TestPublicAndGroupedPorts(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	spec := images.ImageSpec{
		Kind: "service", Image: "ghcr.io/joe-and-his-friends/app:1",
		Ports: []images.Port{{Host: 80, Container: 80, Public: true}},
	}
	req := images.DeployRequest{Project: "mohiguide", Entry: "web", Spec: spec, Digest: digestA, SpecHash: hashC}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != programs.ReasonPolicy {
		t.Fatal("public port without policy")
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatal("docker was called")
	}
	e.setPolicy(strings.Replace(imagePolicy, "allow_public_ports = false", "allow_public_ports = true", 1))
	spec.Ports = []images.Port{{Host: 8080, Container: 80}, {Host: 8081, Container: 80, Public: true}}
	req.Spec = spec
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != "" {
		t.Fatal("public allowed")
	}
	var body map[string]any
	if json.Unmarshal(e.fake.body(http.MethodPost, "/containers/create"), &body) != nil {
		t.Fatal("create")
	}
	hc := body["HostConfig"].(map[string]any)
	binds := hc["PortBindings"].(map[string]any)["80/tcp"].([]any)
	if len(binds) != 2 {
		t.Fatalf("binds %v", binds)
	}
	got := map[string]string{}
	for _, b := range binds {
		m := b.(map[string]any)
		got[m["HostPort"].(string)] = m["HostIp"].(string)
	}
	if got["8080"] != "127.0.0.1" || got["8081"] != "0.0.0.0" {
		t.Fatal(got)
	}
}

func TestPullQueryAndAuth(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	token := "tok+/?"
	req := images.PullRequest{
		Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1.2",
		RegistryAuth: &images.RegistryAuth{Host: "ghcr.io", Username: "bot", Token: token},
	}
	raw := e.call(images.VerbPull, mustJSON(t, req))
	if imgCode(t, raw) != "" || strings.Contains(string(raw), token) {
		t.Fatal(string(raw))
	}
	calls := e.fake.Calls()
	if len(calls) != 1 {
		t.Fatal(len(calls))
	}
	q, _ := url.ParseQuery(calls[0].Query)
	if len(q) != 2 || q.Get("fromImage") != "ghcr.io/joe-and-his-friends/app" || q.Get("tag") != "1.2" {
		t.Fatal(calls[0].Query)
	}
	payload := []byte(`{"username":"bot","password":"` + token + `","serveraddress":"ghcr.io"}`)
	hdr := calls[0].Header.Get("X-Registry-Auth")
	if hdr != base64.URLEncoding.EncodeToString(payload) || hdr == base64.StdEncoding.EncodeToString(payload) {
		t.Fatalf("auth header %s", hdr)
	}
	if calls[0].Header.Get("Authorization") != "" || strings.Contains(calls[0].Query, token) || strings.Contains(string(calls[0].Body), token) {
		t.Fatal("token escaped the header")
	}

	e = newImgEnv(t, imagePolicy)
	req.Ref = "ghcr.io/joe-and-his-friends/app:1.2@" + digestA
	req.RegistryAuth.Host = "docker.io"
	raw = e.call(images.VerbPull, mustJSON(t, req))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	c := e.fake.Calls()[0]
	q, _ = url.ParseQuery(c.Query)
	if len(q) != 1 || q.Get("fromImage") != "ghcr.io/joe-and-his-friends/app@"+digestA || q.Get("tag") != "" {
		t.Fatal(c.Query)
	}
	if c.Header.Get("X-Registry-Auth") != "" || strings.Contains(c.Query+string(c.Body), token) {
		t.Fatal("mismatched host sent the token")
	}

	e = newImgEnv(t, imagePolicy)
	bare := images.PullRequest{Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app"}
	if imgCode(t, e.call(images.VerbPull, mustJSON(t, bare))) != "" {
		t.Fatal("default tag")
	}
	q, _ = url.ParseQuery(e.fake.Calls()[0].Query)
	if q.Get("tag") != "latest" || len(q) != 2 {
		t.Fatal(e.fake.Calls()[0].Query)
	}
}

func TestPullRefusals(t *testing.T) {
	refs := []string{
		"quay.io/org/app:1",
		"ghcr.io/joe-and-his-friends/a/b:1",
		"user:pass@ghcr.io/joe-and-his-friends/app",
		"ghcr.io:443/joe-and-his-friends/app:1",
		"ghcr.io/Joe-and-his-friends/app:1",
		"ghcr.io/joe-and-his-friends/../app",
		"GHCR.IO/Joe/app",
	}
	for _, ref := range refs {
		e := newImgEnv(t, imagePolicy)
		req := images.PullRequest{Project: "mohiguide", Ref: ref}
		code := imgCode(t, e.call(images.VerbPull, mustJSON(t, req)))
		if code != programs.ReasonPolicy && code != programs.ReasonBadSpec {
			t.Fatalf("%s -> %s", ref, code)
		}
		if len(e.fake.Calls()) != 0 {
			t.Fatalf("%s reached docker", ref)
		}
	}
	e := newImgEnv(t, imagePolicy)
	for _, tok := range []string{"a\nb", "a\rb", "a\x00b"} {
		for _, set := range []func(*images.RegistryAuth){
			func(a *images.RegistryAuth) { a.Token = tok },
			func(a *images.RegistryAuth) { a.Username = tok },
			func(a *images.RegistryAuth) { a.Host = tok },
		} {
			auth := &images.RegistryAuth{Host: "ghcr.io", Username: "bot", Token: "ok"}
			set(auth)
			req := images.PullRequest{
				Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1", RegistryAuth: auth,
			}
			if imgCode(t, e.call(images.VerbPull, mustJSON(t, req))) != programs.ReasonBadSpec || len(e.fake.Calls()) != 0 {
				t.Fatalf("accepted %q", tok)
			}
		}
	}
	e.fake.pullErr = "denied for " + secret
	ok := images.PullRequest{Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1"}
	raw := e.call(images.VerbPull, mustJSON(t, ok))
	if imgCode(t, raw) != programs.ReasonImage || strings.Contains(string(raw), secret) || strings.Contains(string(raw), "denied") {
		t.Fatal(string(raw))
	}
	e.fake.pullErr = ""
	e.fake.pullCode = http.StatusUnauthorized
	if imgCode(t, e.call(images.VerbPull, mustJSON(t, ok))) != programs.ReasonImage {
		t.Fatal("401")
	}
	e.fake.pullCode = http.StatusBadGateway
	if imgCode(t, e.call(images.VerbPull, mustJSON(t, ok))) != programs.ReasonDocker {
		t.Fatal("502")
	}
}

func TestMalformedBeforeDocker(t *testing.T) {
	verbs := []string{
		images.VerbPull, images.VerbDeploy, images.VerbRollback, images.VerbRemove,
		images.VerbStart, images.VerbStop, images.VerbRestart, images.VerbStatus,
		images.VerbLogs, images.VerbList,
	}
	for _, verb := range verbs {
		e := newImgEnv(t, imagePolicy)
		valid := validBody(t, verb)
		var m map[string]any
		if json.Unmarshal(valid, &m) != nil {
			t.Fatal(verb)
		}
		m["zzz"] = true
		extra := mustJSON(t, m)
		bodies := [][]byte{
			[]byte("not-json"), []byte("[]"), []byte("null"), []byte("{}{}"),
			extra, bytesRepeat(programs.MaxFrame + 1),
		}
		for _, body := range bodies {
			if code := imgCode(t, e.call(verb, body)); code != programs.ReasonBadSpec {
				t.Fatalf("%s %s -> %s", verb, body, code)
			}
		}
		if len(e.fake.Calls()) != 0 {
			t.Fatalf("%s reached docker", verb)
		}
	}
}

func validBody(t *testing.T, verb string) []byte {
	t.Helper()
	switch verb {
	case images.VerbList:
		return []byte(`{}`)
	case images.VerbPull:
		return mustJSON(t, images.PullRequest{Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1"})
	case images.VerbDeploy:
		return mustJSON(t, jobDeploy())
	case images.VerbLogs:
		return mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 10})
	default:
		return mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
	}
}

func TestHostileNames(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	bodies := []images.DeployRequest{
		{Project: "../mohiguide", Entry: "web", Digest: digestA, SpecHash: hashC, Spec: jobDeploy().Spec},
		{Project: "mohiguide%2Fetc", Entry: "web", Digest: digestA, SpecHash: hashC, Spec: jobDeploy().Spec},
		{Project: "mohi guide", Entry: "web", Digest: digestA, SpecHash: hashC, Spec: jobDeploy().Spec},
		{Project: "mohiguide", Entry: "web-prev", Digest: digestA, SpecHash: hashC, Spec: jobDeploy().Spec},
		{Project: "mohiguide", Entry: "web-", Digest: digestA, SpecHash: hashC, Spec: jobDeploy().Spec},
		{Project: "mohiguide", Entry: "web?x", Digest: digestA, SpecHash: hashC, Spec: jobDeploy().Spec},
		{Project: "mohiguide", Entry: "web x", Digest: digestA, SpecHash: hashC, Spec: jobDeploy().Spec},
	}
	for _, req := range bodies {
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != programs.ReasonBadSpec {
			t.Fatalf("accepted %+v", req.Project+"/"+req.Entry)
		}
	}
	badVol := jobDeploy()
	badVol.Spec.Volumes = []images.Volume{{Name: "../x", Target: "/var/www"}}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, badVol))) != programs.ReasonBadSpec {
		t.Fatal("volume")
	}
	badVol.Spec.Volumes = []images.Volume{{Name: "a%2Fb", Target: "/var/www"}}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, badVol))) != programs.ReasonBadSpec {
		t.Fatal("encoded volume")
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatal("docker")
	}
	ok := e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))
	if imgCode(t, ok) != "" {
		t.Fatal(string(ok))
	}
	for _, c := range e.fake.Calls() {
		if strings.Contains(c.Path, "/containers/") && !strings.Contains(c.Path, "/containers/create") && !strings.Contains(c.Path, "/containers/json") {
			id := strings.TrimPrefix(c.Path, "/v1.47/containers/")
			id, _, _ = strings.Cut(id, "/")
			if len(id) != 64 || strings.ContainsAny(id, ".%? ") {
				t.Fatalf("path %s", c.Path)
			}
		}
	}
}

func TestOwnershipConflict(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.add("mohiguide-web", "running", "alpine:latest", map[string]string{"com.docker.compose.project": "mohiguide"})
	raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))
	if imgCode(t, raw) != programs.ReasonConflict {
		t.Fatal(string(raw))
	}
	for _, c := range e.fake.Calls() {
		if c.Method != http.MethodGet {
			t.Fatalf("mutated %s %s", c.Method, c.Path)
		}
	}
	if e.fake.ctrs["mohiguide-web"].state != "running" {
		t.Fatal("container was touched")
	}
	e = newImgEnv(t, imagePolicy)
	e.fake.add("mohiguide-web-prev", "exited", "alpine:latest", map[string]string{images.LabelOwner: "someone"})
	if imgCode(t, e.call(images.VerbRemove, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != programs.ReasonConflict {
		t.Fatal("prev")
	}
	for _, c := range e.fake.Calls() {
		if c.Method != http.MethodGet {
			t.Fatal(c.Method)
		}
	}
}

func TestVolumesAndNetwork(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.mu.Lock()
	e.fake.nets["deploybot-mohiguide"] = &fakeNet{driver: "bridge", labels: map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host, images.LabelProject: "mohiguide",
	}}
	e.fake.vols["mohiguide-data"] = &fakeVol{
		driver: "local", options: map[string]string{"device": "/host", "o": "bind"},
		labels: map[string]string{images.LabelProject: "mohiguide"},
	}
	e.fake.mu.Unlock()
	req := serviceDeploy(hashC)
	req.Spec.Volumes = []images.Volume{{Name: "data", Target: "/var/www"}}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != programs.ReasonConflict {
		t.Fatal("bind options")
	}
	for _, c := range e.fake.Calls() {
		if c.Method != http.MethodGet {
			t.Fatalf("mutated %s %s", c.Method, c.Path)
		}
	}

	e = newImgEnv(t, imagePolicy)
	e.fake.mu.Lock()
	e.fake.nets["deploybot-mohiguide"] = &fakeNet{driver: "bridge", labels: map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host, images.LabelProject: "mohiguide",
	}}
	e.fake.vols["mohiguide-data"] = &fakeVol{
		driver: "local", labels: map[string]string{images.LabelProject: "other"},
	}
	e.fake.mu.Unlock()
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != programs.ReasonConflict {
		t.Fatal("other project")
	}
	if e.fake.body(http.MethodPost, "/containers/create") != nil {
		t.Fatal("created on a foreign volume")
	}

	e = newImgEnv(t, imagePolicy)
	e.fake.mu.Lock()
	e.fake.nets["deploybot-mohiguide"] = &fakeNet{driver: "bridge", labels: map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host, images.LabelProject: "mohiguide",
	}}
	e.fake.vols["mohiguide-data"] = &fakeVol{
		driver: "nfs", labels: map[string]string{images.LabelProject: "mohiguide"},
	}
	e.fake.mu.Unlock()
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != programs.ReasonConflict {
		t.Fatal("nfs")
	}
	if e.fake.body(http.MethodPost, "/containers/create") != nil {
		t.Fatal("created on nfs")
	}

	e = newImgEnv(t, imagePolicy)
	e.fake.mu.Lock()
	e.fake.nets["deploybot-mohiguide"] = &fakeNet{driver: "overlay", labels: map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host, images.LabelProject: "mohiguide",
	}}
	e.fake.mu.Unlock()
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))) != programs.ReasonConflict {
		t.Fatal("driver")
	}
	e = newImgEnv(t, imagePolicy)
	e.fake.mu.Lock()
	e.fake.nets["deploybot-mohiguide"] = &fakeNet{driver: "bridge", labels: map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: "0123456789abcdef", images.LabelProject: "mohiguide",
	}}
	e.fake.mu.Unlock()
	if e.host == "0123456789abcdef" {
		t.Fatal("fixture host collided")
	}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))) != programs.ReasonConflict {
		t.Fatal("net labels")
	}

	for _, owner := range []struct {
		name   string
		labels map[string]string
	}{
		{"compose", map[string]string{
			images.LabelOwner: "compose", images.LabelHost: e.host, images.LabelProject: "mohiguide",
		}},
		{"absent", map[string]string{
			images.LabelHost: e.host, images.LabelProject: "mohiguide",
		}},
	} {
		e = newImgEnv(t, imagePolicy)
		owner.labels[images.LabelHost] = e.host
		e.fake.mu.Lock()
		e.fake.nets["deploybot-mohiguide"] = &fakeNet{driver: "bridge", labels: owner.labels}
		e.fake.mu.Unlock()
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))) != programs.ReasonConflict {
			t.Fatalf("net owner %s", owner.name)
		}
		if e.fake.body(http.MethodPost, "/containers/create") != nil {
			t.Fatalf("created on net owner %s", owner.name)
		}
	}

	e = newImgEnv(t, imagePolicy)
	e.fake.volumeRace = "bind"
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != programs.ReasonConflict {
		t.Fatal("race verify")
	}
	if e.fake.body(http.MethodPost, "/containers/create") != nil {
		t.Fatal("created after a bad volume")
	}
}

func TestDeployRollbackJobOrder(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	cur := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	prev := e.fake.add("mohiguide-web-prev", "running", "alpine:latest", e.labels("web"))
	raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	var id images.IDResponse
	if json.Unmarshal(raw, &id) != nil || len(id.ContainerID) != 64 {
		t.Fatal(string(raw))
	}
	want := []string{
		"GET /v1.47/containers/json?all=1",
		"POST /v1.47/containers/" + prev + "/stop?t=30",
		"DELETE /v1.47/containers/" + prev + "?v=1",
		"POST /v1.47/containers/" + cur + "/stop?t=30",
		"POST /v1.47/containers/" + cur + "/rename?name=mohiguide-web-prev",
		"GET /v1.47/networks/deploybot-mohiguide?",
		"POST /v1.47/networks/create?",
		"GET /v1.47/networks/deploybot-mohiguide?",
		"GET /v1.47/volumes/mohiguide-data?",
		"POST /v1.47/volumes/create?",
		"GET /v1.47/volumes/mohiguide-data?",
		"GET /v1.47/volumes/mohiguide-ro?",
		"POST /v1.47/volumes/create?",
		"GET /v1.47/volumes/mohiguide-ro?",
		"POST /v1.47/containers/create?name=mohiguide-web",
		"POST /v1.47/containers/" + id.ContainerID + "/start?",
	}
	assertCalls(t, e.fake, want)

	e = newImgEnv(t, imagePolicy)
	cur = e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	prev = e.fake.add("mohiguide-web-prev", "exited", "ghcr.io/joe-and-his-friends/app@"+digestB, e.labels("web"))
	raw = e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if json.Unmarshal(raw, &id) != nil || id.ContainerID != prev {
		t.Fatalf("id %s want %s", raw, prev)
	}
	assertCalls(t, e.fake, []string{
		"GET /v1.47/containers/json?all=1",
		"POST /v1.47/containers/" + cur + "/stop?t=30",
		"DELETE /v1.47/containers/" + cur + "?v=1",
		"POST /v1.47/containers/" + prev + "/rename?name=mohiguide-web",
		"POST /v1.47/containers/" + prev + "/start?",
	})

	e = newImgEnv(t, imagePolicy)
	e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	if imgCode(t, e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != programs.ReasonConflict {
		t.Fatal("missing prev")
	}
	if len(e.fake.Calls()) != 1 {
		t.Fatal(e.fake.Calls())
	}

	e = newImgEnv(t, imagePolicy)
	cur = e.fake.add("mohiguide-job", "running", "alpine:latest", e.labels("job"))
	prev = e.fake.add("mohiguide-job-prev", "exited", "alpine:latest", e.labels("job"))
	raw = e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	json.Unmarshal(raw, &id)
	assertCalls(t, e.fake, []string{
		"GET /v1.47/containers/json?all=1",
		"POST /v1.47/containers/" + cur + "/stop?t=30",
		"DELETE /v1.47/containers/" + cur + "?v=1",
		"GET /v1.47/networks/deploybot-mohiguide?",
		"POST /v1.47/networks/create?",
		"GET /v1.47/networks/deploybot-mohiguide?",
		"POST /v1.47/containers/create?name=mohiguide-job",
		"POST /v1.47/containers/" + id.ContainerID + "/start?",
	})
	for _, c := range e.fake.Calls() {
		if strings.Contains(c.Path, prev) || strings.Contains(c.Query, prev) {
			t.Fatal("job removed prev")
		}
	}
}

func assertCalls(t *testing.T, f *fakeDocker, want []string) {
	t.Helper()
	calls := f.Calls()
	if len(calls) != len(want) {
		var got []string
		for _, c := range calls {
			got = append(got, c.Method+" "+c.Path+"?"+c.Query)
		}
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for i, c := range calls {
		got := c.Method + " " + c.Path + "?" + c.Query
		if got != want[i] {
			t.Fatalf("[%d] %s\nwant %s", i, got, want[i])
		}
	}
}

func TestMaxContainersAndRemove(t *testing.T) {
	e := newImgEnv(t, strings.Replace(imagePolicy, "max_containers = 20", "max_containers = 1", 1))
	e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashD)))) != programs.ReasonPolicy {
		t.Fatal("cap")
	}
	if len(e.fake.Calls()) != 1 || e.fake.Calls()[0].Method != http.MethodGet {
		t.Fatal(e.fake.Calls())
	}
	if e.fake.ctrs["mohiguide-web"].state != "running" {
		t.Fatal("stopped before refusing")
	}

	e = newImgEnv(t, strings.Replace(imagePolicy, "max_containers = 20", "max_containers = 1", 1))
	cur := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	prev := e.fake.add("mohiguide-web-prev", "exited", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
	raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	if imgCode(t, raw) != "" || !strings.Contains(string(raw), prev) {
		t.Fatal(string(raw))
	}
	_ = cur

	e = newImgEnv(t, imagePolicy)
	id := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	kept := e.fake.add("mohiguide-web-prev", "exited", "alpine:latest", e.labels("web"))
	raw = e.call(images.VerbRemove, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	var act images.ActionResult
	if json.Unmarshal(raw, &act) != nil || act.Status.State != "missing" {
		t.Fatal(string(raw))
	}
	assertCalls(t, e.fake, []string{
		"GET /v1.47/containers/json?all=1",
		"POST /v1.47/containers/" + id + "/stop?t=30",
		"DELETE /v1.47/containers/" + id + "?v=1",
	})
	if _, ok := e.fake.byID[kept]; !ok {
		t.Fatal("prev removed")
	}
	raw = e.call(images.VerbRemove, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	if json.Unmarshal(raw, &act) != nil || act.Status.State != "missing" {
		t.Fatal(string(raw))
	}
}

func TestProjectionsAndLogs(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	c := e.fake.byID[e.fake.add("mohiguide-web", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))]
	c.restarts = 2
	c.exit = 7
	c.labels["dev.deploybot.token"] = secret
	c.started = "2026-10-15T12:00:00.5Z"
	c.ports = []portView{{HostIP: "127.0.0.1", HostPort: 3031, ContainerPort: 3031}}
	e.fake.add("other", "removing", "alpine:latest", nil)
	raw := e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	forbidKeys(t, raw)
	var st images.StatusResult
	if json.Unmarshal(raw, &st) != nil || st.SpecHash != hashC || st.Status.State != "running" || st.Status.RestartCount != 2 {
		t.Fatal(string(raw))
	}
	if st.Status.Digest == nil || *st.Status.Digest != digestA || st.Status.ExitCode != nil || st.Status.StartedAt == nil || st.Status.FinishedAt != nil {
		t.Fatal(string(raw))
	}
	if len(st.Status.Ports) != 1 || st.Status.Ports[0] != 3031 || st.Status.Container == nil || *st.Status.Container != "mohiguide-web" {
		t.Fatal(string(raw))
	}
	raw = e.call(images.VerbList, []byte(`{}`))
	forbidKeys(t, raw)
	var list images.ListResponse
	if json.Unmarshal(raw, &list) != nil || len(list.Items) != 2 || list.Truncated {
		t.Fatal(string(raw))
	}
	for _, item := range list.Items {
		if _, ok := item.Labels["com.docker.compose.project"]; ok {
			t.Fatal(item.Labels)
		}
		if item.Name == "other" {
			if item.State != "dead" || item.Digest != nil {
				t.Fatal(item)
			}
		}
		if item.Name == "mohiguide-web" {
			if item.Digest == nil || *item.Digest != digestA || item.Labels[images.LabelOwner] != images.OwnerValue {
				t.Fatal(item)
			}
			if _, ok := item.Labels["dev.deploybot.token"]; ok {
				t.Fatal(item.Labels)
			}
		}
	}
	e.fake.logBody = append(frame(1, "hello\n"), frame(2, "token="+secret+"\n")...)
	raw = e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 20}))
	var logs images.LogsResponse
	if json.Unmarshal(raw, &logs) != nil || logs.Truncated || logs.Container != "mohiguide-web" || len(logs.Lines) != 2 || logs.Lines[0] != "hello" || logs.Lines[1] != "token=[REDACTED]" {
		t.Fatal(string(raw))
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("log secret")
	}
	var logQuery string
	for _, call := range e.fake.Calls() {
		if strings.HasSuffix(call.Path, "/logs") {
			logQuery = call.Query
		}
	}
	if logQuery != "stderr=1&stdout=1&tail=20&timestamps=0" || strings.Contains(logQuery, "follow") {
		t.Fatal(logQuery)
	}
	c.state = "exited"
	c.exit = 3
	c.finished = "2026-10-15T12:05:00Z"
	raw = e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	if json.Unmarshal(raw, &st) != nil || st.Status.State != "exited" || st.Status.ExitCode == nil || *st.Status.ExitCode != 3 {
		t.Fatal(string(raw))
	}
	long := strings.Repeat("x", 3000)
	var bulk []byte
	for i := 0; i < 40; i++ {
		bulk = append(bulk, frame(1, strings.Repeat("y", 2000)+"\n")...)
	}
	bulk = append(bulk, frame(1, long+"\n")...)
	e.fake.logBody = bulk
	raw = e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 50}))
	if json.Unmarshal(raw, &logs) != nil || !logs.Truncated {
		t.Fatal(string(raw))
	}
	total := 0
	for _, line := range logs.Lines {
		if len([]rune(line)) > 2048 {
			t.Fatal(len(line))
		}
		total += len(line)
	}
	if total > 64*1024 || !strings.Contains(logs.Lines[len(logs.Lines)-1], "x") {
		t.Fatal(total)
	}
	miss := e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "absent", Lines: 5}))
	if json.Unmarshal(miss, &logs) != nil || logs.Container != "mohiguide-absent" || len(logs.Lines) != 0 {
		t.Fatal(string(miss))
	}
}

func frame(stream byte, payload string) []byte {
	b := make([]byte, 8+len(payload))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:8], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}

func TestEntryLockSerialises(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.block = make(chan struct{})
	e.fake.entered = make(chan struct{})
	release := sync.OnceFunc(func() { close(e.fake.block) })
	t.Cleanup(release)
	a := serviceDeploy(hashC)
	b := serviceDeploy(hashD)
	var rawA, rawB []byte
	doneA := make(chan struct{})
	go func() {
		rawA = e.call(images.VerbDeploy, mustJSON(t, a))
		close(doneA)
	}()
	select {
	case <-e.fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first deploy did not reach docker")
	}
	doneB := make(chan struct{})
	go func() {
		rawB = e.call(images.VerbDeploy, mustJSON(t, b))
		close(doneB)
	}()
	if n := callsWhileHeld(t, e.fake); n != 1 {
		t.Fatalf("calls %d while the entry lock was held", n)
	}
	lock, _ := images.LockPath("mohiguide", "web")
	path := filepath.Join(e.sys.Root, strings.TrimPrefix(lock, "/"))
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	f.Close()
	if err == nil {
		t.Fatal("entry lock was free")
	}
	release()
	select {
	case <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("deploy A")
	}
	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("deploy B")
	}
	if imgCode(t, rawA) != "" || imgCode(t, rawB) != "" {
		t.Fatalf("%s %s", rawA, rawB)
	}
	var creates []string
	seenStart := false
	listsAfter := 0
	for _, c := range e.fake.Calls() {
		if strings.HasSuffix(c.Path, "/start") {
			seenStart = true
		}
		if c.Path == "/v1.47/containers/json" && seenStart {
			listsAfter++
		}
		if strings.HasSuffix(c.Path, "/containers/create") {
			creates = append(creates, string(c.Body))
		}
	}
	if len(creates) != 2 || !strings.Contains(creates[0], hashC) || !strings.Contains(creates[1], hashD) || listsAfter != 1 {
		t.Fatalf("order creates %d lists-after %d", len(creates), listsAfter)
	}
}

func TestEngineRefusesRawTargets(t *testing.T) {
	f, sock := startFake(t)
	eng := newEngine(sock)
	ctx := context.Background()
	if eng.Remove(ctx, "../etc") == nil {
		t.Fatal("remove")
	}
	if _, err := eng.Create(ctx, "Bad_Name", []byte(`{"Image":"x"}`)); err == nil {
		t.Fatal("create")
	}
	if eng.VolumeCreate(ctx, "volname", "short", "mohiguide") == nil {
		t.Fatal("volume")
	}
	if _, _, err := eng.Pull(ctx, images.Reference{}, nil); err == nil {
		t.Fatal("pull")
	}
	if len(f.Calls()) != 0 {
		t.Fatal(f.Calls())
	}
	_, err := readCap(strings.NewReader(strings.Repeat("a", 8)), 7)
	if err == nil {
		t.Fatal("cap")
	}
}

func TestParseListSkipsHostile(t *testing.T) {
	raw := []byte(`[
		{"Id":"abc","Names":["/short"]},
		{"Id":"` + strings.Repeat("ab", 32) + `","Names":["/a/b"]},
		{"Id":"` + strings.Repeat("cd", 32) + `","Names":["/good"],"State":"running","Image":"alpine:latest"}
	]`)
	rows, err := parseList(raw)
	if err != nil || len(rows) != 1 || rows[0].Names[0] != "good" {
		t.Fatalf("%v %+v", err, rows)
	}
}

func TestStartStopRestartMissing(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	for _, verb := range []string{images.VerbStart, images.VerbStop, images.VerbRestart, images.VerbStatus} {
		raw := e.call(verb, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
		if strings.Contains(verb, "status") {
			var st images.StatusResult
			if json.Unmarshal(raw, &st) != nil || st.Status.State != "missing" || st.SpecHash != "" {
				t.Fatal(string(raw))
			}
			continue
		}
		var act images.ActionResult
		if json.Unmarshal(raw, &act) != nil || act.Status.State != "missing" {
			t.Fatal(verb, string(raw))
		}
	}
	for _, c := range e.fake.Calls() {
		if c.Method != http.MethodGet {
			t.Fatal(c.Method, c.Path)
		}
	}
	id := e.fake.add("mohiguide-web", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
	raw := e.call(images.VerbRestart, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	var act images.ActionResult
	if json.Unmarshal(raw, &act) != nil || act.Status.State != "running" {
		t.Fatal(string(raw))
	}
	var sawStop, sawStart bool
	for _, c := range e.fake.Calls() {
		if strings.Contains(c.Path, id+"/stop") && c.Query == "t=30" {
			sawStop = true
		}
		if strings.Contains(c.Path, id+"/start") {
			sawStart = true
		}
		if strings.Contains(c.Path, "restart") {
			t.Fatal("restart endpoint")
		}
	}
	if !sawStop || !sawStart {
		t.Fatal("restart was not stop+start")
	}
}

func TestOverlapDisablesImages(t *testing.T) {
	const head = "enabled = true\nimages = [\"ghcr.io/acme/*\"]\n"
	overlap := head + "projects = [\"acme\", \"acme-web\"]\n"
	e := newImgEnv(t, overlap)
	for _, verb := range []string{
		images.VerbList, images.VerbPull, images.VerbDeploy, images.VerbRollback,
		images.VerbRemove, images.VerbStart, images.VerbStop, images.VerbRestart,
		images.VerbStatus, images.VerbLogs,
	} {
		raw := e.call(verb, []byte(`{}`))
		if imgCode(t, raw) != programs.ReasonPolicy {
			t.Fatalf("%s %s calls %d", verb, raw, len(e.fake.Calls()))
		}
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatalf("engine %v", e.fake.Calls())
	}
	listed, reason, err := programs.DecodeList(e.call("list", []byte(`{}`)))
	if err != nil || reason != "" || len(listed) != 0 {
		t.Fatalf("list %q reason %s err %v", listed, reason, err)
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatal("program list dialed the engine")
	}

	for _, projects := range []string{`["acme", "acmeweb"]`, `["acme-web", "web"]`} {
		fine := newImgEnv(t, head+"projects = "+projects+"\n")
		raw := fine.call(images.VerbList, []byte(`{}`))
		if imgCode(t, raw) != "" || len(fine.fake.Calls()) == 0 {
			t.Fatalf("%s -> %s calls %d", projects, raw, len(fine.fake.Calls()))
		}
	}

	three := newImgEnv(t, head+"projects = [\"acme\", \"other\", \"acme-web\"]\n")
	if imgCode(t, three.call(images.VerbList, []byte(`{}`))) != programs.ReasonPolicy || len(three.fake.Calls()) != 0 {
		t.Fatal("three projects still called the engine")
	}
}

func TestImagesOffAndForeignProject(t *testing.T) {
	policies := []string{
		"enabled = false\nimages = [\"ghcr.io/joe-and-his-friends/*\"]\nprojects = [\"mohiguide\"]\n",
		"enabled = true\nimages = []\nprojects = [\"mohiguide\"]\n",
		"enabled = true\nimages = [\"ghcr.io/joe-and-his-friends/*\"]\nprojects = []\n",
	}
	pull := images.PullRequest{Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1"}
	for _, pol := range policies {
		e := newImgEnv(t, pol)
		if imgCode(t, e.call(images.VerbPull, mustJSON(t, pull))) != programs.ReasonPolicy {
			t.Fatalf("pull %q", pol)
		}
		if imgCode(t, e.call(images.VerbList, []byte(`{}`))) != programs.ReasonPolicy {
			t.Fatalf("list %q", pol)
		}
		if len(e.fake.Calls()) != 0 {
			t.Fatalf("dialed %q", pol)
		}
	}
	e := newImgEnv(t, imagePolicy)
	pull.Project = "other"
	if imgCode(t, e.call(images.VerbPull, mustJSON(t, pull))) != programs.ReasonPolicy || len(e.fake.Calls()) != 0 {
		t.Fatal("foreign pull")
	}
	req := jobDeploy()
	req.Project = "other"
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != programs.ReasonPolicy || len(e.fake.Calls()) != 0 {
		t.Fatal("foreign deploy")
	}
}

func TestPullDigestStream(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.pullID = digestB
	raw := e.call(images.VerbPull, mustJSON(t, images.PullRequest{
		Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1",
	}))
	var resp images.PullResponse
	if json.Unmarshal(raw, &resp) != nil || resp.Digest != digestA || resp.ImageID != digestB {
		t.Fatal(string(raw))
	}

	e = newImgEnv(t, imagePolicy)
	e.fake.omitDigest = true
	raw = e.call(images.VerbPull, mustJSON(t, images.PullRequest{
		Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app@" + digestA,
	}))
	if imgCode(t, raw) != programs.ReasonImage || strings.Contains(string(raw), digestA) {
		t.Fatal(string(raw))
	}

	e = newImgEnv(t, imagePolicy)
	e.fake.omitDigest = true
	if imgCode(t, e.call(images.VerbPull, mustJSON(t, images.PullRequest{
		Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1",
	}))) != programs.ReasonImage {
		t.Fatal("tag pull without a digest")
	}

	e = newImgEnv(t, imagePolicy)
	e.fake.badDigest = true
	raw = e.call(images.VerbPull, mustJSON(t, images.PullRequest{
		Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app@" + digestA,
	}))
	if imgCode(t, raw) != programs.ReasonImage || strings.Contains(string(raw), digestB) {
		t.Fatal(string(raw))
	}

	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		e = newImgEnv(t, imagePolicy)
		e.fake.pullCode = code
		if imgCode(t, e.call(images.VerbPull, mustJSON(t, images.PullRequest{
			Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1",
		}))) != programs.ReasonImage {
			t.Fatalf("status %d", code)
		}
	}
}

func TestExistingNetworkKeepsEngineOptions(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.mu.Lock()
	e.fake.nets["deploybot-mohiguide"] = &fakeNet{
		driver: "bridge",
		labels: map[string]string{
			images.LabelOwner: images.OwnerValue, images.LabelHost: e.host, images.LabelProject: "mohiguide",
		},
	}
	e.fake.mu.Unlock()
	raw := e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	for _, c := range e.fake.Calls() {
		if c.Path == "/v1.47/networks/create" {
			t.Fatal("recreated a network that already matched")
		}
	}
	var body map[string]any
	if json.Unmarshal(e.fake.body(http.MethodPost, "/containers/create"), &body) != nil {
		t.Fatal("create")
	}
	ep := body["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)["deploybot-mohiguide"].(map[string]any)
	aliases := ep["Aliases"].([]any)
	if len(aliases) != 1 || aliases[0] != "job" {
		t.Fatal(aliases)
	}
}

func TestResolvedEnvIsNotEchoed(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	req := jobDeploy()
	req.Env = map[string]string{"DB_PASSWORD": secret}
	raw := e.call(images.VerbDeploy, mustJSON(t, req))
	if imgCode(t, raw) != "" || strings.Contains(string(raw), secret) {
		t.Fatal(string(raw))
	}
	if !strings.Contains(string(e.fake.body(http.MethodPost, "/containers/create")), "DB_PASSWORD="+secret) {
		t.Fatal("create body dropped the resolved env")
	}

	e = newImgEnv(t, imagePolicy)
	req = jobDeploy()
	req.Spec.SecretEnv = map[string]string{"DB_PASSWORD": secret}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != programs.ReasonBadSpec || len(e.fake.Calls()) != 0 {
		t.Fatal("plaintext secret_env")
	}

	e = newImgEnv(t, imagePolicy)
	req = jobDeploy()
	req.Spec.SecretEnv = map[string]string{"DB_PASSWORD": "services.DB_PASSWORD"}
	raw = e.call(images.VerbDeploy, mustJSON(t, req))
	if imgCode(t, raw) != "" || strings.Contains(string(e.fake.body(http.MethodPost, "/containers/create")), "services.DB_PASSWORD") {
		t.Fatal("secret_env reference was sent to the engine")
	}
}

func TestLogLineBounds(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	for _, n := range []int{0, 2001, -1} {
		raw := e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: n}))
		if imgCode(t, raw) != programs.ReasonBadSpec {
			t.Fatalf("lines %d -> %s", n, raw)
		}
	}
	if len(e.fake.Calls()) != 0 {
		t.Fatal(e.fake.Calls())
	}
}

func TestDeployRefusesOutsideImages(t *testing.T) {
	refs := []string{
		"quay.io/org/app:1",
		"ghcr.io/joe-and-his-friends/a/b:1",
		"user:pass@ghcr.io/joe-and-his-friends/app",
		"ghcr.io:443/joe-and-his-friends/app:1",
		"ghcr.io/Joe-and-his-friends/app:1",
		"ghcr.io/joe-and-his-friends/../app",
		"quay.io/org/app@" + digestA,
	}
	for _, ref := range refs {
		e := newImgEnv(t, imagePolicy)
		req := jobDeploy()
		req.Spec.Image = ref
		code := imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req)))
		if code != programs.ReasonPolicy && code != programs.ReasonBadSpec {
			t.Fatalf("%s -> %s", ref, code)
		}
		if len(e.fake.Calls()) != 0 {
			t.Fatalf("%s reached docker", ref)
		}
	}
}

func TestOwnershipEachLabel(t *testing.T) {
	otherHost := strings.Repeat("0", 16)
	tweaks := []struct {
		name string
		fn   func(map[string]string)
	}{
		{"host", func(m map[string]string) { m[images.LabelHost] = otherHost }},
		{"project", func(m map[string]string) { m[images.LabelProject] = "other" }},
		{"entry", func(m map[string]string) { m[images.LabelEntry] = "api" }},
	}
	for _, tw := range tweaks {
		for _, prev := range []bool{false, true} {
			e := newImgEnv(t, imagePolicy)
			if otherHost == e.host {
				otherHost = strings.Repeat("f", 16)
			}
			name := "mohiguide-web"
			verb := images.VerbDeploy
			body := mustJSON(t, serviceDeploy(hashC))
			if prev {
				name = "mohiguide-web-prev"
				verb = images.VerbRemove
				body = mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
			}
			labels := e.labels("web")
			tw.fn(labels)
			e.fake.add(name, "running", "ghcr.io/joe-and-his-friends/app@"+digestA, labels)
			raw := e.call(verb, body)
			if imgCode(t, raw) != programs.ReasonConflict {
				t.Fatalf("%s prev=%v %s", tw.name, prev, raw)
			}
			for _, c := range e.fake.Calls() {
				if c.Method != http.MethodGet {
					t.Fatalf("%s mutated %s", tw.name, c.Path)
				}
			}
		}
	}
}

func TestCapCountsOtherContainers(t *testing.T) {
	pol := strings.Replace(imagePolicy, "max_containers = 20", "max_containers = 1", 1)
	e := newImgEnv(t, pol)
	e.fake.add("mohiguide-api", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("api"))
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))) != programs.ReasonPolicy {
		t.Fatal("other entry")
	}
	if len(e.fake.Calls()) != 1 || e.fake.Calls()[0].Method != http.MethodGet {
		t.Fatal(e.fake.Calls())
	}

	e = newImgEnv(t, pol)
	e.fake.add("otherproj-job", "exited", "ghcr.io/joe-and-his-friends/app@"+digestA, map[string]string{
		images.LabelOwner: images.OwnerValue, images.LabelHost: e.host,
		images.LabelProject: "otherproj", images.LabelEntry: "job",
	})
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))) != programs.ReasonPolicy {
		t.Fatal("other project")
	}
	if len(e.fake.Calls()) != 1 || e.fake.Calls()[0].Method != http.MethodGet {
		t.Fatal(e.fake.Calls())
	}

	e = newImgEnv(t, pol)
	e.fake.add("compose-web", "running", "alpine:latest", map[string]string{"com.docker.compose.project": "mohiguide"})
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))) != "" {
		t.Fatal("compose counted")
	}
}

func TestRollbackRechecksAllowlist(t *testing.T) {
	bad := []string{
		"alpine:latest",
		"ghcr.io/evil/app@" + digestA,
		"ghcr.io/joe-and-his-friends/app:1@" + digestA,
	}
	for _, image := range bad {
		e := newImgEnv(t, imagePolicy)
		e.fake.add("mohiguide-web", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
		e.fake.add("mohiguide-web-prev", "exited", image, e.labels("web"))
		raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
		if imgCode(t, raw) != programs.ReasonPolicy {
			t.Fatalf("%s -> %s", image, raw)
		}
		if len(e.fake.Calls()) != 1 || e.fake.Calls()[0].Method != http.MethodGet {
			t.Fatalf("%s touched docker", image)
		}
		if e.fake.ctrs["mohiguide-web"].state != "running" {
			t.Fatal("current was stopped")
		}
	}
}

func TestPullRedirectNotFollowed(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.pullRedirect = "http://evil.example/steal"
	token := "tok+/?"
	raw := e.call(images.VerbPull, mustJSON(t, images.PullRequest{
		Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1",
		RegistryAuth: &images.RegistryAuth{Host: "ghcr.io", Username: "bot", Token: token},
	}))
	if imgCode(t, raw) != programs.ReasonDocker || strings.Contains(string(raw), token) {
		t.Fatal(string(raw))
	}
	calls := e.fake.Calls()
	if len(calls) != 1 || calls[0].Header.Get("X-Registry-Auth") == "" {
		t.Fatalf("calls %d", len(calls))
	}
}

func TestPullStreamOverCap(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.pullHuge = true
	e.fake.pullWrote = make(chan struct{})
	e.fake.pullRelease = make(chan struct{})
	release := sync.OnceFunc(func() { close(e.fake.pullRelease) })
	t.Cleanup(release)
	done := make(chan []byte, 1)
	go func() {
		done <- e.call(images.VerbPull, mustJSON(t, images.PullRequest{
			Project: "mohiguide", Ref: "ghcr.io/joe-and-his-friends/app:1",
		}))
	}()
	select {
	case <-e.fake.pullWrote:
	case <-time.After(15 * time.Second):
		t.Fatal("pull did not hit the cap")
	}
	select {
	case raw := <-done:
		if imgCode(t, raw) != programs.ReasonDocker || strings.Contains(string(raw), "sha256:") {
			t.Fatal(string(raw))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client kept reading past the cap")
	}
	release()
}

func TestLogsTruncateInsteadOfFailing(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.add("mohiguide-web", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
	e.fake.logHuge = true
	e.fake.logWrote = make(chan struct{})
	e.fake.logRelease = make(chan struct{})
	release := sync.OnceFunc(func() { close(e.fake.logRelease) })
	t.Cleanup(release)
	done := make(chan []byte, 1)
	go func() {
		done <- e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 20}))
	}()
	select {
	case <-e.fake.logWrote:
	case <-time.After(15 * time.Second):
		t.Fatal("logs did not hit the cap")
	}
	var raw []byte
	select {
	case raw = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("client kept reading past the cap")
	}
	release()
	var logs images.LogsResponse
	if imgCode(t, raw) != "" || json.Unmarshal(raw, &logs) != nil || !logs.Truncated || len(logs.Lines) == 0 {
		t.Fatal(string(raw))
	}
	for _, line := range logs.Lines {
		if line == "" || strings.Trim(line, "y") != "" {
			t.Fatalf("kept %q", line)
		}
	}

	e.fake.logHuge = false
	e.fake.logBody = frame(1, "hello\n")
	raw = e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 20}))
	if json.Unmarshal(raw, &logs) != nil || logs.Truncated || len(logs.Lines) != 1 || logs.Lines[0] != "hello" {
		t.Fatal(string(raw))
	}

	e.fake.logBody = append(frame(1, "hello\n"), 1, 0, 0, 0)
	raw = e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 20}))
	if json.Unmarshal(raw, &logs) != nil || !logs.Truncated || len(logs.Lines) != 1 || logs.Lines[0] != "hello" {
		t.Fatal(string(raw))
	}
}

func TestPausedContainerIsStopped(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	id := e.fake.add("mohiguide-web", "paused", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
	raw := e.call(images.VerbStop, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	var act images.ActionResult
	if json.Unmarshal(raw, &act) != nil || act.Status.State != "exited" {
		t.Fatal(string(raw))
	}
	saw := false
	for _, c := range e.fake.Calls() {
		if c.Path == "/v1.47/containers/"+id+"/stop" && c.Query == "t=30" {
			saw = true
		}
	}
	if !saw {
		t.Fatal("paused container was not stopped")
	}
}

func TestCountLockSerialisesDeploys(t *testing.T) {
	e := newImgEnv(t, strings.Replace(imagePolicy, "max_containers = 20", "max_containers = 1", 1))
	e.fake.block = make(chan struct{})
	e.fake.entered = make(chan struct{})
	release := sync.OnceFunc(func() { close(e.fake.block) })
	t.Cleanup(release)
	a := jobDeploy()
	b := jobDeploy()
	b.Entry = "api"
	b.SpecHash = hashD
	var rawA, rawB []byte
	doneA := make(chan struct{})
	go func() {
		rawA = e.call(images.VerbDeploy, mustJSON(t, a))
		close(doneA)
	}()
	select {
	case <-e.fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first deploy did not reach docker")
	}
	doneB := make(chan struct{})
	go func() {
		rawB = e.call(images.VerbDeploy, mustJSON(t, b))
		close(doneB)
	}()
	if n := callsWhileHeld(t, e.fake); n != 1 {
		t.Fatalf("calls %d while the count lock was held", n)
	}
	path := filepath.Join(e.sys.Root, "run", "bot-agent", "image-count.lock")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	f.Close()
	if err == nil {
		t.Fatal("count lock was free")
	}
	release()
	select {
	case <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("deploy A")
	}
	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("deploy B")
	}
	codes := []string{imgCode(t, rawA), imgCode(t, rawB)}
	if (codes[0] == "") == (codes[1] == "") || (codes[0] != programs.ReasonPolicy && codes[1] != programs.ReasonPolicy) {
		t.Fatalf("%s %s", rawA, rawB)
	}
	creates := 0
	for _, c := range e.fake.Calls() {
		if strings.HasSuffix(c.Path, "/containers/create") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("creates %d", creates)
	}
}

func TestOversizedJSONDeploy(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	req := jobDeploy()
	body := mustJSON(t, req)
	// Trailing space is still one JSON value. The size check has to be what refuses it.
	body = append(body, bytes.Repeat([]byte(" "), images.MaxFrame)...)
	if len(body) <= images.MaxFrame {
		t.Fatalf("fixture %d", len(body))
	}
	if imgCode(t, e.call(images.VerbDeploy, body)) != programs.ReasonBadSpec || len(e.fake.Calls()) != 0 {
		t.Fatal("oversized deploy")
	}
}

// callsWhileHeld waits until a second Engine call shows up or the deadline
// passes. The caller holds a flock that should keep that call from starting.
func callsWhileHeld(t *testing.T, f *fakeDocker) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		n := len(f.Calls())
		if n > 1 || !time.Now().Before(deadline) {
			return n
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOwnershipOwnerOnly(t *testing.T) {
	tweaks := []struct {
		name string
		fn   func(map[string]string)
	}{
		{"compose", func(m map[string]string) { m[images.LabelOwner] = "compose" }},
		{"absent", func(m map[string]string) { delete(m, images.LabelOwner) }},
	}
	verbs := []struct {
		name  string
		verb  string
		entry string
		body  func() []byte
		logs  bool
	}{
		{"deploy", images.VerbDeploy, "web", func() []byte { return mustJSON(t, serviceDeploy(hashC)) }, false},
		{"job", images.VerbDeploy, "job", func() []byte { return mustJSON(t, jobDeploy()) }, false},
		{"remove", images.VerbRemove, "web", func() []byte {
			return mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
		}, false},
		{"start", images.VerbStart, "web", func() []byte {
			return mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
		}, false},
		{"stop", images.VerbStop, "web", func() []byte {
			return mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
		}, false},
		{"restart", images.VerbRestart, "web", func() []byte {
			return mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
		}, false},
		{"status", images.VerbStatus, "web", func() []byte {
			return mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
		}, false},
		{"logs", images.VerbLogs, "web", func() []byte {
			return mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 20})
		}, true},
	}
	for _, tw := range tweaks {
		for _, prev := range []bool{false, true} {
			for _, v := range verbs {
				e := newImgEnv(t, imagePolicy)
				labels := e.labels(v.entry)
				tw.fn(labels)
				name := "mohiguide-" + v.entry
				if prev {
					name += "-prev"
				}
				e.fake.add(name, "running", "ghcr.io/joe-and-his-friends/app@"+digestA, labels)
				if v.logs {
					e.fake.logBody = []byte(secret)
				}
				raw := e.call(v.verb, v.body())
				if imgCode(t, raw) != programs.ReasonConflict || strings.Contains(string(raw), secret) {
					t.Fatalf("%s prev=%v %s: %s", tw.name, prev, v.name, raw)
				}
				for _, c := range e.fake.Calls() {
					if c.Method != http.MethodGet {
						t.Fatalf("%s %s mutated %s", tw.name, v.name, c.Path)
					}
				}
				if e.fake.ctrs[name].state != "running" {
					t.Fatalf("%s %s touched the container", tw.name, v.name)
				}
			}
		}
	}
}

func TestCapAllowsReplacingOwned(t *testing.T) {
	pol := strings.Replace(imagePolicy, "max_containers = 20", "max_containers = 1", 1)
	e := newImgEnv(t, pol)
	id := e.fake.add("mohiguide-job", "exited", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("job"))
	raw := e.call(images.VerbDeploy, mustJSON(t, jobDeploy()))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	del, create := -1, -1
	for i, c := range e.fake.Calls() {
		if c.Method == http.MethodDelete && c.Path == "/v1.47/containers/"+id && c.Query == "v=1" {
			del = i
		}
		if strings.HasSuffix(c.Path, "/containers/create") {
			create = i
		}
	}
	if del < 0 || create < 0 || del > create {
		t.Fatalf("delete %d create %d", del, create)
	}

	pol = strings.Replace(imagePolicy, "max_containers = 20", "max_containers = 2", 1)
	e = newImgEnv(t, pol)
	e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	prev := e.fake.add("mohiguide-web-prev", "exited", "alpine:latest", e.labels("web"))
	raw = e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	saw := false
	for _, c := range e.fake.Calls() {
		if c.Method == http.MethodDelete && strings.Contains(c.Path, prev) && c.Query == "v=1" {
			saw = true
		}
	}
	if !saw || e.fake.body(http.MethodPost, "/containers/create") == nil {
		t.Fatal("service replace did not delete prev and create")
	}
}

func TestRestartingContainerIsStopped(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	id := e.fake.add("mohiguide-web", "restarting", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
	raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	stopAt, renameAt := -1, -1
	for i, c := range e.fake.Calls() {
		if c.Path == "/v1.47/containers/"+id+"/stop" && c.Query == "t=30" {
			stopAt = i
		}
		if strings.Contains(c.Path, "/rename") && strings.Contains(c.Query, "name=mohiguide-web-prev") {
			renameAt = i
		}
	}
	if stopAt < 0 || renameAt < 0 || stopAt > renameAt {
		t.Fatalf("stop %d rename %d", stopAt, renameAt)
	}

	e = newImgEnv(t, imagePolicy)
	id = e.fake.add("mohiguide-web", "restarting", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
	raw = e.call(images.VerbStop, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	var act images.ActionResult
	if json.Unmarshal(raw, &act) != nil || act.Status.State != "exited" {
		t.Fatal(string(raw))
	}
	saw := false
	for _, c := range e.fake.Calls() {
		if c.Path == "/v1.47/containers/"+id+"/stop" && c.Query == "t=30" {
			saw = true
		}
	}
	if !saw {
		t.Fatal("restarting container was not stopped")
	}
}

func TestEntryLockFilesStayCapped(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	if err := ensureRuntime(e.sys.Root); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < entryLockCap; i++ {
		entry := "e" + itoa(i)
		unlock, err := lockEntry(e.sys.Root, "mohiguide", entry)
		if err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
		unlock()
		rel, _ := images.LockPath("mohiguide", entry)
		if _, err := os.Stat(filepath.Join(e.sys.Root, strings.TrimPrefix(rel, "/"))); err != nil {
			t.Fatalf("lock file removed: %v", err)
		}
	}
	if _, err := lockEntry(e.sys.Root, "mohiguide", "over"); err != errLockCap {
		t.Fatalf("cap: %v", err)
	}
	rel, _ := images.LockPath("mohiguide", "over")
	if _, err := os.Stat(filepath.Join(e.sys.Root, strings.TrimPrefix(rel, "/"))); !os.IsNotExist(err) {
		t.Fatal("cap still created a file")
	}
	n := 0
	ents, err := os.ReadDir(filepath.Join(e.sys.Root, "run", "bot-agent"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		name := ent.Name()
		if name == "image-count.lock" || name == "image-lock-gate.lock" {
			continue
		}
		if strings.HasPrefix(name, "image-") && strings.HasSuffix(name, ".lock") {
			n++
		}
	}
	if n != entryLockCap {
		t.Fatalf("lock files %d", n)
	}
	before := len(e.fake.Calls())
	raw := e.call(images.VerbStart, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "over"}))
	if imgCode(t, raw) != programs.ReasonPolicy || len(e.fake.Calls()) != before {
		t.Fatalf("start past cap: %s calls %d", raw, len(e.fake.Calls())-before)
	}
	raw = e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "e0"}))
	var st images.StatusResult
	if json.Unmarshal(raw, &st) != nil || st.Status.State != "missing" {
		t.Fatal(string(raw))
	}
}
