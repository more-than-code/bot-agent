package images

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseAndMatch(t *testing.T) {
	ref, err := Parse("alpine")
	if err != nil || ref.Name != "docker.io/library/alpine" || ref.Tag != "latest" || ref.Digest != "" {
		t.Fatalf("%+v %v", ref, err)
	}
	q, err := ref.PullQuery()
	if err != nil || q.Get("fromImage") != ref.Name || q.Get("tag") != "latest" || len(q) != 2 {
		t.Fatalf("%v", q)
	}
	legacy, err := Parse("index.docker.io/library/alpine:1")
	if err != nil || legacy.Name != "docker.io/library/alpine" || legacy.Tag != "1" {
		t.Fatalf("%+v %v", legacy, err)
	}
	folded, err := Parse("GHCR.IO/joe-and-his-friends/app")
	if err != nil || folded.Host != "ghcr.io" || folded.Tag != "latest" {
		t.Fatalf("%+v %v", folded, err)
	}
	if !Match(folded, []string{"ghcr.io/joe-and-his-friends/*"}) {
		t.Fatal("folded host did not match")
	}
	both, err := Parse("ghcr.io/joe-and-his-friends/app:1.2@" + digestA)
	if err != nil || both.Tag != "1.2" || both.Digest != digestA {
		t.Fatalf("%+v %v", both, err)
	}
	dq, err := both.PullQuery()
	if err != nil || len(dq) != 1 || dq.Get("fromImage") != both.Name+"@"+digestA || dq.Get("tag") != "" {
		t.Fatalf("digest query %v", dq)
	}
	pat := []string{"ghcr.io/joe-and-his-friends/*"}
	ok, err := Parse("ghcr.io/joe-and-his-friends/app:1")
	if err != nil || !Match(ok, pat) {
		t.Fatal("anchor match")
	}
	extra, err := Parse("ghcr.io/joe-and-his-friends/a/b:1")
	if err != nil || Match(extra, pat) {
		t.Fatal("extra component matched")
	}
	other, err := Parse("docker.io/library/alpine:1")
	if err != nil || Match(other, pat) {
		t.Fatal("other registry matched")
	}
	ported, err := Parse("ghcr.io:443/joe-and-his-friends/app:1")
	if err != nil || ported.Host != "ghcr.io:443" || Match(ported, pat) {
		t.Fatalf("port trick %+v %v", ported, err)
	}
	for _, raw := range []string{
		"ghcr.io/Joe-and-his-friends/app",
		"user:pass@ghcr.io/joe-and-his-friends/app",
		"ghcr.io/joe-and-his-friends/../app",
		"https://ghcr.io/joe-and-his-friends/app",
		"ghcr.io/joe-and-his-friends/app?x=1",
		"ghcr.io/joe-and-his-friends/a b",
		"ghcr.io/joe-and-his-friends/app@" + strings.ToUpper(digestA),
	} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if ValidPattern("*") || ValidPattern("*.io/a/*") || ValidPattern("GHCR.IO/a/*") || ValidPattern("ghcr.io/Joe/*") {
		t.Fatal("bad pattern accepted")
	}
	if !ValidPattern("ghcr.io/joe-and-his-friends/*") || !ValidPattern("docker.io/library/*") {
		t.Fatal("good pattern rejected")
	}
}

func TestNamesAndUser(t *testing.T) {
	if name, ok := ContainerName("mohiguide", "web"); !ok || name != "mohiguide-web" {
		t.Fatalf("%s %v", name, ok)
	}
	if _, ok := PrevName("mohiguide", "web"); !ok {
		t.Fatal("prev")
	}
	for _, entry := range []string{"web-prev", "web-", "../x", "web?x", "web x", "%2F", "Bad"} {
		if _, ok := ContainerName("mohiguide", entry); ok {
			t.Fatalf("entry accepted %s", entry)
		}
	}
	for _, project := range []string{"../mohiguide", "mohiguide%2Fetc", "Has Space", "A"} {
		if _, ok := ContainerName(project, "web"); ok {
			t.Fatalf("project accepted %s", project)
		}
	}
	if _, ok := VolumeName("mohiguide", "../x"); ok {
		t.Fatal("volume")
	}
	lock, ok := LockPath("mohiguide", "web")
	if !ok || lock != "/run/bot-agent/image-mohiguide-web.lock" || strings.Contains(lock, "..") {
		t.Fatal(lock)
	}
	id, ok := HostID("test-machine\n")
	if !ok || len(id) != 16 {
		t.Fatalf("host %s %v", id, ok)
	}
	if _, ok := HostID("a\nb"); ok {
		t.Fatal("multiline machine-id")
	}
	if _, ok := HostID(""); ok {
		t.Fatal("empty machine-id")
	}
	for _, u := range []string{"", "app", "app-1", "_svc", "1000", "1000:1000", "app:users"} {
		if !ValidUser(u) {
			t.Fatalf("user %s", u)
		}
	}
	for _, u := range []string{"root", "0", "0:1", "1:0", "app:root", "root:app", "01", "00", "4294967295", "app:group:extra", "Has Space"} {
		if ValidUser(u) {
			t.Fatalf("accepted user %s", u)
		}
	}
}

func TestSpecValidate(t *testing.T) {
	mem := 64
	s := ImageSpec{
		Kind: "service", Image: "ghcr.io/joe-and-his-friends/app:1",
		Ports:   []Port{{Host: 3031, Container: 80}, {Host: 3032, Container: 80}},
		Env:     map[string]string{"NODE_ENV": "production"},
		EnvSets: []string{"services"}, SecretEnv: map[string]string{"DB_PASSWORD": "services.DB_PASSWORD"},
		Volumes:  []Volume{{Name: "data", Target: "/var/www"}},
		MemoryMB: &mem,
	}
	if s.Validate() != "" || s.Restart != "unless-stopped" || s.HealthyAfterSec != 30 || s.TimeoutSec != 0 {
		t.Fatalf("%q restart %s healthy %d timeout %d", s.Validate(), s.Restart, s.HealthyAfterSec, s.TimeoutSec)
	}
	job := ImageSpec{Kind: "job", Image: "ghcr.io/joe-and-his-friends/app"}
	if job.Validate() != "" || job.Restart != "no" || job.TimeoutSec != 600 || job.HealthyAfterSec != 0 {
		t.Fatalf("job %+v", job)
	}
	forced := ImageSpec{Kind: "job", Image: "ghcr.io/joe-and-his-friends/app", Restart: "unless-stopped"}
	if forced.Validate() != "" || forced.Restart != "no" {
		t.Fatalf("job restart %+v", forced)
	}
	root := s
	root.Volumes = []Volume{{Name: "data", Target: "/"}}
	if root.Validate() != "" {
		t.Fatal("root target")
	}
	if ResolvedEnv(map[string]string{"DB_PASSWORD": "x"}) != "" {
		t.Fatal("resolved secret key")
	}
	bad := []struct {
		name string
		fn   func(*ImageSpec)
	}{
		{"kind", func(s *ImageSpec) { s.Kind = "task" }},
		{"image", func(s *ImageSpec) { s.Image = "GHCR.IO/Nope/App" }},
		{"timeout", func(s *ImageSpec) { s.TimeoutSec = 10 }},
		{"healthy", func(s *ImageSpec) { s.Kind = "job"; s.Restart = "no"; s.HealthyAfterSec = 30 }},
		{"memory", func(s *ImageSpec) { n := 15; s.MemoryMB = &n }},
		{"ports", func(s *ImageSpec) { s.Ports = []Port{{Host: 1, Container: 1}, {Host: 1, Container: 2}} }},
		{"target", func(s *ImageSpec) { s.Volumes = []Volume{{Name: "data", Target: "/var/../x"}} }},
		{"slash", func(s *ImageSpec) { s.Volumes = []Volume{{Name: "data", Target: "/var//x"}} }},
		{"env", func(s *ImageSpec) { s.Env = map[string]string{"PASSWORD": "x"} }},
		{"path", func(s *ImageSpec) { s.Env = map[string]string{"LD_PRELOAD": "x"} }},
		{"secret", func(s *ImageSpec) { s.SecretEnv = map[string]string{"A": "no-dot"} }},
		{"user", func(s *ImageSpec) { s.User = "root" }},
		{"nul", func(s *ImageSpec) { s.Command = []string{"a\x00b"} }},
	}
	for _, tc := range bad {
		cp := s
		cp.Ports = append([]Port(nil), s.Ports...)
		cp.Env = map[string]string{"NODE_ENV": "production"}
		cp.SecretEnv = map[string]string{"DB_PASSWORD": "services.DB_PASSWORD"}
		cp.Volumes = append([]Volume(nil), s.Volumes...)
		tc.fn(&cp)
		if cp.Validate() == "" {
			t.Fatalf("%s accepted", tc.name)
		}
	}
}

func TestHealthcheckValidate(t *testing.T) {
	good := func() ImageSpec {
		return ImageSpec{
			Kind: "service", Image: "ghcr.io/joe-and-his-friends/app:1",
			Healthcheck: &Healthcheck{
				Test: []string{"wget", "ok\n"}, IntervalSec: 30, TimeoutSec: 5, Retries: 3, StartPeriodSec: 0,
			},
		}
	}
	s := good()
	if s.Validate() != "" || s.Restart != "unless-stopped" || s.HealthyAfterSec != 30 || s.TimeoutSec != 0 {
		t.Fatalf("%q %+v", s.Validate(), s)
	}
	if s.Healthcheck.IntervalSec != 30 || s.Healthcheck.TimeoutSec != 5 || s.Healthcheck.Retries != 3 {
		t.Fatal("defaults were written onto healthcheck")
	}
	bare := ImageSpec{Kind: "service", Image: "ghcr.io/joe-and-his-friends/app:1"}
	if bare.Validate() != "" || bare.Healthcheck != nil {
		t.Fatal("nil healthcheck")
	}
	job := ImageSpec{
		Kind: "job", Image: "ghcr.io/joe-and-his-friends/app:1",
		Healthcheck: &Healthcheck{Test: []string{"echo", "secret-argv"}, IntervalSec: 0},
	}
	if got := job.Validate(); got != "healthcheck" || strings.Contains(got, "secret-argv") {
		t.Fatalf("job %q", got)
	}
	bad := []struct {
		field string
		fn    func(*Healthcheck)
	}{
		{"healthcheck", func(h *Healthcheck) { h.Test = nil }},
		{"healthcheck", func(h *Healthcheck) { h.Test = []string{} }},
		{"healthcheck", func(h *Healthcheck) { h.Test = []string{""} }},
		{"healthcheck", func(h *Healthcheck) { h.Test = []string{"a\x00b"} }},
		{"healthcheck", func(h *Healthcheck) { h.Test = []string{strings.Repeat("a", 1025)} }},
		{"healthcheck", func(h *Healthcheck) {
			h.Test = make([]string, 33)
			for i := range h.Test {
				h.Test[i] = "a"
			}
		}},
		{"interval_sec", func(h *Healthcheck) { h.IntervalSec = 0 }},
		{"interval_sec", func(h *Healthcheck) { h.IntervalSec = 4 }},
		{"interval_sec", func(h *Healthcheck) { h.IntervalSec = 301 }},
		{"timeout_sec", func(h *Healthcheck) { h.TimeoutSec = 0 }},
		{"timeout_sec", func(h *Healthcheck) { h.TimeoutSec = 61 }},
		{"timeout_sec", func(h *Healthcheck) { h.TimeoutSec = 30 }},
		{"retries", func(h *Healthcheck) { h.Retries = 0 }},
		{"retries", func(h *Healthcheck) { h.Retries = 11 }},
		{"start_period_sec", func(h *Healthcheck) { h.StartPeriodSec = -1 }},
		{"start_period_sec", func(h *Healthcheck) { h.StartPeriodSec = 601 }},
	}
	for _, tc := range bad {
		s := good()
		tc.fn(s.Healthcheck)
		if got := s.Validate(); got != tc.field {
			t.Fatalf("%s got %q", tc.field, got)
		}
	}
	s = good()
	s.TimeoutSec = 10
	if s.Validate() != "timeout_sec" {
		t.Fatal("service timeout")
	}
	var decoded ImageSpec
	raw := `{"kind":"service","image":"ghcr.io/joe-and-his-friends/app:1","healthcheck":{"test":["wget"],"interval_sec":30,"timeout_sec":5,"retries":3,"start_period_sec":0}}`
	if Decode([]byte(raw), &decoded) != nil || decoded.Healthcheck == nil || decoded.Healthcheck.Retries != 3 {
		t.Fatalf("%+v", decoded.Healthcheck)
	}
	if Decode([]byte(`{"kind":"service","image":"ghcr.io/joe-and-his-friends/app:1","healthcheck":{"test":["wget"],"extra":1}}`), &ImageSpec{}) == nil {
		t.Fatal("unknown healthcheck key")
	}
}

func TestLoggingValidate(t *testing.T) {
	good := func(kind string) ImageSpec {
		s := ImageSpec{
			Kind: kind, Image: "ghcr.io/joe-and-his-friends/app:1",
			Logging: &Logging{MaxSizeMB: 5, MaxFiles: 2},
		}
		if kind == "job" {
			s.TimeoutSec = 10
		}
		return s
	}
	for _, kind := range []string{"service", "job"} {
		s := good(kind)
		if s.Validate() != "" {
			t.Fatalf("%s %s", kind, s.Validate())
		}
		if s.Logging.MaxSizeMB != 5 || s.Logging.MaxFiles != 2 {
			t.Fatal("defaults were written onto logging")
		}
		n, k, ok := LogLimit(s)
		if !ok || n != 5 || k != 2 {
			t.Fatalf("limit %d %d %v", n, k, ok)
		}
	}
	bare := ImageSpec{Kind: "service", Image: "ghcr.io/joe-and-his-friends/app:1"}
	if bare.Validate() != "" || bare.Logging != nil {
		t.Fatal("nil logging")
	}
	n, k, ok := LogLimit(bare)
	if !ok || n != 10 || k != 3 {
		t.Fatalf("default %d %d %v", n, k, ok)
	}
	bad := []struct {
		field string
		fn    func(*Logging)
	}{
		{"logging.max_size_mb", func(l *Logging) { l.MaxSizeMB = 0 }},
		{"logging.max_size_mb", func(l *Logging) { l.MaxSizeMB = 101 }},
		{"logging.max_size_mb", func(l *Logging) { l.MaxSizeMB = -1 }},
		{"logging.max_files", func(l *Logging) { l.MaxFiles = 0 }},
		{"logging.max_files", func(l *Logging) { l.MaxFiles = 11 }},
	}
	for _, kind := range []string{"service", "job"} {
		for _, tc := range bad {
			s := good(kind)
			tc.fn(s.Logging)
			if got := s.Validate(); got != tc.field {
				t.Fatalf("%s %s got %q", kind, tc.field, got)
			}
			if _, _, ok := LogLimit(s); ok {
				t.Fatalf("%s %s accepted", kind, tc.field)
			}
		}
	}
	edges := []Logging{{MaxSizeMB: 1, MaxFiles: 1}, {MaxSizeMB: 100, MaxFiles: 10}}
	for _, l := range edges {
		s := good("service")
		s.Logging = &l
		if s.Validate() != "" {
			t.Fatalf("%+v", l)
		}
	}
	var decoded ImageSpec
	raw := `{"kind":"service","image":"ghcr.io/joe-and-his-friends/app:1","logging":{"max_size_mb":10,"max_files":3}}`
	if Decode([]byte(raw), &decoded) != nil || decoded.Logging == nil || decoded.Logging.MaxSizeMB != 10 || decoded.Logging.MaxFiles != 3 {
		t.Fatalf("%+v", decoded.Logging)
	}
	if Decode([]byte(`{"kind":"job","image":"ghcr.io/joe-and-his-friends/app:1","logging":null}`), &decoded) != nil || decoded.Logging != nil {
		t.Fatal("null logging")
	}
	for _, raw := range []string{
		`{"kind":"service","image":"ghcr.io/joe-and-his-friends/app:1","logging":{"max_size_mb":10,"max_files":3,"extra":1}}`,
		`{"kind":"service","image":"ghcr.io/joe-and-his-friends/app:1","logging":{"max_size_mb":10.5,"max_files":3}}`,
		`{"kind":"service","image":"ghcr.io/joe-and-his-friends/app:1","logging":{"max_size_mb":"10","max_files":3}}`,
		`{"kind":"service","image":"ghcr.io/joe-and-his-friends/app:1","logging":[]}`,
	} {
		if Decode([]byte(raw), &ImageSpec{}) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestMergedEnvLimit(t *testing.T) {
	env := map[string]string{}
	for i := 0; i < 8*256+64+64; i++ {
		env["E"+strconv.Itoa(i)] = "v"
	}
	if ResolvedEnv(env) != "" {
		t.Fatal("legal merge")
	}
	env["EXTRA"] = "v"
	if ResolvedEnv(env) == "" {
		t.Fatal("over merge")
	}
}

func TestWireDecode(t *testing.T) {
	var req PullRequest
	if Decode([]byte(`{"project":"mohiguide","ref":"alpine","extra":1}`), &req) == nil {
		t.Fatal("unknown field")
	}
	if Decode([]byte(`{}{}`), &ListRequest{}) == nil {
		t.Fatal("trailing")
	}
	if Decode([]byte(`[]`), &ListRequest{}) == nil || Decode([]byte(`null`), &ListRequest{}) == nil {
		t.Fatal("non-object")
	}
	raw := Encode(PullResponse{Digest: digestA, ImageID: digestA})
	if strings.Contains(string(raw), "\n") || !strings.Contains(string(raw), `"digest"`) {
		t.Fatal(string(raw))
	}
	errBody := string(EncodeError("conflict"))
	if errBody != `{"error":"conflict"}` {
		t.Fatal(errBody)
	}
}

const digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
