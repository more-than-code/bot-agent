package programs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// withSecretKeys adds "secret_keys" to a start spec (agent-protocol v1.11).
// A nil keys argument writes JSON null.
func withSecretKeys(t *testing.T, raw []byte, keys []string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["secret_keys"] = keys
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func secretsFixture(t *testing.T) (string, startSpec) {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "workspace", "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "workspace"), startSpec{Dir: app, Exec: "/usr/bin/pnpm"}
}

func TestSecretKeysRules(t *testing.T) {
	ws, base := secretsFixture(t)
	pol := joePolicy(ws, false, []string{"/usr/bin/pnpm"}, 20)
	cases := []struct {
		name string
		env  map[string]string
		keys []string
		want string
	}{
		{"listed secret-looking key", map[string]string{"GH_TOKEN": "ghp_value123", "PORT": "1"}, []string{"GH_TOKEN"}, ""},
		{"listed plain key", map[string]string{"PORT": "3031"}, []string{"PORT"}, ""},
		{"empty list", map[string]string{"PORT": "3031"}, []string{}, ""},
		{"unlisted secret-looking key", map[string]string{"GH_TOKEN": "ghp_value123"}, []string{}, ReasonBadSpec},
		{"other secret-looking key unlisted", map[string]string{"GH_TOKEN": "ghp_value123", "DB_PASSWORD": "pw123456"}, []string{"GH_TOKEN"}, ReasonBadSpec},
		{"listed key missing from env", map[string]string{"PORT": "1"}, []string{"GH_TOKEN"}, ReasonBadSpec},
		{"listed denylisted key", map[string]string{"NODE_OPTIONS": "--require x"}, []string{"NODE_OPTIONS"}, ReasonBadSpec},
		{"listed LD_ key", map[string]string{"LD_PRELOAD": "/x.so"}, []string{"LD_PRELOAD"}, ReasonBadSpec},
		{"listed PATH", map[string]string{"PATH": "/bin"}, []string{"PATH"}, ReasonBadSpec},
		{"listed HOME", map[string]string{"HOME": "/tmp"}, []string{"HOME"}, ReasonBadSpec},
		{"listed newline value", map[string]string{"GH_TOKEN": "a\nb"}, []string{"GH_TOKEN"}, ReasonBadSpec},
		{"listed long value", map[string]string{"GH_TOKEN": strings.Repeat("a", 4097)}, []string{"GH_TOKEN"}, ReasonBadSpec},
		{"listed value at the limit", map[string]string{"GH_TOKEN": strings.Repeat("a", 4096)}, []string{"GH_TOKEN"}, ""},
		{"listed bad key grammar", map[string]string{"gh_token": "ghp_value123"}, []string{"gh_token"}, ReasonBadSpec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.Env = tc.env
			_, code := Check("program_start", withSecretKeys(t, spec(t, s), tc.keys), Gate{Policy: pol})
			if code != tc.want {
				t.Fatalf("got %q want %q", code, tc.want)
			}
		})
	}
}

// The helper (Files true) applies the same rules; the denylist is policy_denied there.
func TestSecretKeysHelperPath(t *testing.T) {
	ws, base := secretsFixture(t)
	pol := joePolicy(ws, false, []string{"/usr/bin/pnpm"}, 20)
	s := base
	s.Env = map[string]string{"GH_TOKEN": "ghp_value123", "PORT": "1"}
	res, code := Check("program_start", withSecretKeys(t, spec(t, s), []string{"GH_TOKEN", "PORT", "GH_TOKEN"}), Gate{Policy: pol, Files: true})
	if code != "" {
		t.Fatalf("code %s", code)
	}
	if !reflect.DeepEqual(res.SecretKeys, []string{"GH_TOKEN", "PORT"}) {
		t.Fatalf("keys %v", res.SecretKeys)
	}
	if got := res.SecretValues(); !reflect.DeepEqual(got, []string{"ghp_value123", "1"}) {
		t.Fatalf("values %v", got)
	}
	s.Env = map[string]string{"GH_TOKEN": "ghp_value123"}
	if _, code := Check("program_start", withSecretKeys(t, spec(t, s), []string{}), Gate{Policy: pol, Files: true}); code != ReasonBadSpec {
		t.Fatalf("unlisted on helper: %s", code)
	}
	s.Env = map[string]string{"NODE_OPTIONS": "--require x"}
	if _, code := Check("program_start", withSecretKeys(t, spec(t, s), []string{"NODE_OPTIONS"}), Gate{Policy: pol, Files: true}); code != ReasonPolicy {
		t.Fatalf("denylist on helper: %s", code)
	}
}

// Without secret_keys (or with null) the frame and checks are exactly v1.9.
func TestSecretKeysAbsentOrNull(t *testing.T) {
	ws, base := secretsFixture(t)
	pol := joePolicy(ws, false, []string{"/usr/bin/pnpm"}, 20)
	s := base
	s.Env = map[string]string{"PORT": "1"}
	res, code := Check("program_start", spec(t, s), Gate{Policy: pol})
	if code != "" || res.SecretKeys != nil {
		t.Fatalf("absent: %s %v", code, res.SecretKeys)
	}
	res, code = Check("program_start", withSecretKeys(t, spec(t, s), nil), Gate{Policy: pol})
	if code != "" || res.SecretKeys != nil {
		t.Fatalf("null: %s %v", code, res.SecretKeys)
	}
	s.Env = map[string]string{"GH_TOKEN": "ghp_value123"}
	if _, code := Check("program_start", spec(t, s), Gate{Policy: pol}); code != ReasonBadSpec {
		t.Fatalf("secret key without list: %s", code)
	}
	// secret_keys is a start field only.
	ref := []byte(`{"project":"tour-agent","entry":"tra-backend","secret_keys":[]}`)
	if _, code := Check("program_restart", ref, Gate{Policy: pol}); code != ReasonBadSpec {
		t.Fatalf("restart with secret_keys: %s", code)
	}
}

func TestStartSecrets(t *testing.T) {
	_, base := secretsFixture(t)
	s := base
	s.Env = map[string]string{"GH_TOKEN": "ghp_value123", "PORT": "1"}
	raw := withSecretKeys(t, spec(t, s), []string{"GH_TOKEN"})
	if got := StartSecrets("program_start", raw); !reflect.DeepEqual(got, []string{"ghp_value123"}) {
		t.Fatalf("%v", got)
	}
	if got := StartSecrets("program_restart", raw); len(got) != 0 {
		t.Fatalf("restart %v", got)
	}
	if got := StartSecrets("program_start", spec(t, s)); len(got) != 0 {
		t.Fatalf("no list %v", got)
	}
}

func TestProjectGate(t *testing.T) {
	ws, base := secretsFixture(t)
	listed := joePolicy(ws, false, []string{"/usr/bin/pnpm"}, 20)
	unlisted := joePolicy(ws, false, []string{"/usr/bin/pnpm"}, 20)
	unlisted.Projects = []string{"home-lab"}
	empty := joePolicy(ws, false, []string{"/usr/bin/pnpm"}, 20)
	empty.Projects = []string{}
	overlap := joePolicy(ws, false, []string{"/usr/bin/pnpm"}, 20)
	overlap.Projects = []string{"tour", "tour-agent"}
	ref := []byte(`{"project":"tour-agent","entry":"tra-backend"}`)
	logs := []byte(`{"project":"tour-agent","entry":"tra-backend","lines":10}`)
	start := spec(t, base)
	cases := []struct {
		name string
		kind string
		body []byte
		want map[string]string // policy name → code
	}{
		{"start", "program_start", start, map[string]string{"listed": "", "unlisted": ReasonPolicy, "empty": ReasonPolicy, "overlap": ReasonPolicy}},
		{"restart", "program_restart", ref, map[string]string{"listed": "", "unlisted": ReasonPolicy, "empty": ReasonPolicy, "overlap": ReasonPolicy}},
		{"stop", "program_stop", ref, map[string]string{"listed": "", "unlisted": "", "empty": "", "overlap": ""}},
		{"status", "program_status", ref, map[string]string{"listed": "", "unlisted": "", "empty": "", "overlap": ""}},
		{"logs", "program_logs", logs, map[string]string{"listed": "", "unlisted": "", "empty": "", "overlap": ""}},
	}
	pols := map[string]Gate{
		"listed": {Policy: listed}, "unlisted": {Policy: unlisted}, "empty": {Policy: empty}, "overlap": {Policy: overlap},
	}
	for _, tc := range cases {
		for name, gate := range pols {
			for _, files := range []bool{false, true} {
				g := gate
				g.Files = files
				if _, code := Check(tc.kind, tc.body, g); code != tc.want[name] {
					t.Errorf("%s %s files=%t: got %q want %q", tc.name, name, files, code, tc.want[name])
				}
			}
		}
	}
	// v1.12 item 1: a bad slug is policy_denied on start and restart, in the
	// agent and the helper. Stop, status and logs keep bad_spec.
	badStart := spec(t, startSpec{Project: "Bad_Slug", Dir: base.Dir, Exec: "/usr/bin/pnpm"})
	for _, files := range []bool{false, true} {
		g := Gate{Policy: listed, Files: files}
		if _, code := Check("program_start", badStart, g); code != ReasonPolicy {
			t.Fatalf("bad slug start files=%t: %s", files, code)
		}
		if _, code := Check("program_restart", []byte(`{"project":"Bad","entry":"x"}`), g); code != ReasonPolicy {
			t.Fatalf("bad slug restart files=%t: %s", files, code)
		}
		for _, kind := range []string{"program_stop", "program_status"} {
			if _, code := Check(kind, []byte(`{"project":"Bad","entry":"x"}`), g); code != ReasonBadSpec {
				t.Fatalf("bad slug %s files=%t: %s", kind, files, code)
			}
		}
		if _, code := Check("program_logs", []byte(`{"project":"Bad","entry":"x","lines":5}`), g); code != ReasonBadSpec {
			t.Fatalf("bad slug logs files=%t: %s", files, code)
		}
	}
}

// secret_keys must be an array of strings (C7e).
func TestSecretKeysShape(t *testing.T) {
	ws, base := secretsFixture(t)
	pol := joePolicy(ws, false, []string{"/usr/bin/pnpm"}, 20)
	s := base
	s.Env = map[string]string{"GH_TOKEN": "ghp_value123"}
	raw := string(spec(t, s))
	for _, bad := range []string{`"GH_TOKEN"`, `{"GH_TOKEN":true}`, `[1]`, `["GH_TOKEN",null]`, `true`} {
		body := strings.TrimSuffix(raw, "}") + `,"secret_keys":` + bad + "}"
		for _, files := range []bool{false, true} {
			if _, code := Check("program_start", []byte(body), Gate{Policy: pol, Files: files}); code != ReasonBadSpec {
				t.Fatalf("%s files=%t: %s", bad, files, code)
			}
		}
	}
}
