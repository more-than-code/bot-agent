package policy

import (
	"reflect"
	"strings"
	"testing"
)

// agent-protocol v1.26 item 1: host_env_command and host_env_healthcheck.

const pinFiles = `host_env_files = ["deploybot/api=/etc/deploybot/api.env", "deploybot/migrate=/etc/deploybot/api.env"]` + "\n" + apiImages

func TestHostEnvPinsValid(t *testing.T) {
	f := newHostEnvFixture(t)
	p := f.load(pinFiles+
		`host_env_command = ["deploybot/api=api", "deploybot/migrate=api --migrate"]`+"\n"+
		`host_env_healthcheck = ["deploybot/api=/usr/local/bin/deploybot-server health http://127.0.0.1:8190/health"]`+"\n", rootOwner, false)
	if !p.Valid {
		t.Fatalf("reason %s %s", p.Reason, p.ReasonEntry)
	}
	api, _ := p.HostEnv("deploybot", "api")
	if !reflect.DeepEqual(api.Command, []string{"api"}) ||
		!reflect.DeepEqual(api.Healthcheck, []string{"/usr/local/bin/deploybot-server", "health", "http://127.0.0.1:8190/health"}) {
		t.Fatalf("%+v", api)
	}
	mig, _ := p.HostEnv("deploybot", "migrate")
	if !reflect.DeepEqual(mig.Command, []string{"api", "--migrate"}) || mig.Healthcheck != nil {
		t.Fatalf("%+v", mig)
	}
	// Absent keys: nil pins, the image's own.
	p = f.load(pinFiles, rootOwner, false)
	api, _ = p.HostEnv("deploybot", "api")
	if !p.Valid || api.Command != nil || api.Healthcheck != nil {
		t.Fatalf("%+v", p)
	}
	// The agent (lines only) parses the pins too.
	p = f.load(pinFiles+`host_env_command = ["deploybot/api=api"]`+"\n", nil, true)
	api, _ = p.HostEnv("deploybot", "api")
	if !p.Valid || !reflect.DeepEqual(api.Command, []string{"api"}) {
		t.Fatalf("%+v", p)
	}
}

func TestHostEnvPinsInvalid(t *testing.T) {
	seventeen := "deploybot/api=a" + strings.Repeat(" x", 17)
	cases := []struct {
		name, lines, reason, entry string
	}{
		{"no file line", `host_env_command = ["deploybot/web=api"]`, ReasonHostEnvFile, "deploybot/web"},
		{"healthcheck no file line", `host_env_healthcheck = ["deploybot/web=/bin/true"]`, ReasonHostEnvFile, "deploybot/web"},
		{"duplicate", `host_env_command = ["deploybot/api=api", "deploybot/api=mcp"]`, ReasonHostEnvFile, "deploybot/api"},
		{"empty argv", `host_env_command = ["deploybot/api="]`, ReasonHostEnvFile, "deploybot/api"},
		{"double space", `host_env_command = ["deploybot/api=api  x"]`, ReasonHostEnvFile, "deploybot/api"},
		{"trailing space", `host_env_command = ["deploybot/api=api "]`, ReasonHostEnvFile, "deploybot/api"},
		{"too many args", `host_env_command = ["` + seventeen + `"]`, ReasonHostEnvFile, "deploybot/api"},
		{"long arg", `host_env_command = ["deploybot/api=` + strings.Repeat("a", 257) + `"]`, ReasonHostEnvFile, "deploybot/api"},
		{"quote", `host_env_command = ["deploybot/api=sh -c 'env'"]`, ReasonHostEnvFile, "deploybot/api"},
		{"backslash", `host_env_command = ["deploybot/api=a\\b"]`, ReasonHostEnvFile, "deploybot/api"},
		{"tab", "host_env_command = [\"deploybot/api=a\tb\"]", ReasonHostEnvFile, "deploybot/api"},
		{"relative healthcheck", `host_env_healthcheck = ["deploybot/api=deploybot-server health"]`, ReasonHostEnvFile, "deploybot/api"},
		{"bad names", `host_env_command = ["Deploy Bot/api=api"]`, ReasonHostEnvFile, ""},
		{"no equals", `host_env_command = ["deploybot/api"]`, "parse", ""},
		{"not an array", `host_env_command = "deploybot/api=api"`, "parse", ""},
		{"key twice", "host_env_command = [\"deploybot/api=api\"]\nhost_env_command = [\"deploybot/api=api\"]", "parse", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newHostEnvFixture(t)
			p := f.load(pinFiles+c.lines+"\n", rootOwner, false)
			if p.Valid || p.Reason != c.reason || p.ReasonEntry != c.entry {
				t.Fatalf("valid=%v reason=%q entry=%q", p.Valid, p.Reason, p.ReasonEntry)
			}
		})
	}
	// Pins without any host_env_files line are a typo too.
	f := newHostEnvFixture(t)
	p := f.load(`host_env_command = ["deploybot/api=api"]`+"\n", rootOwner, false)
	if p.Valid || p.Reason != ReasonHostEnvFile || p.ReasonEntry != "deploybot/api" {
		t.Fatalf("%+v", p)
	}
}

func TestHostEnvPinKeysAreKnown(t *testing.T) {
	for _, k := range []string{"host_env_command", "host_env_healthcheck"} {
		if !knownKey(k) {
			t.Fatal(k)
		}
	}
}

// The limits are inclusive: a program and 16 arguments, 256-byte
// arguments. An empty array is no pin.
func TestHostEnvPinsBoundaries(t *testing.T) {
	f := newHostEnvFixture(t)
	seventeen := "a" + strings.Repeat(" x", 16)
	p := f.load(pinFiles+`host_env_command = ["deploybot/api=`+seventeen+`", "deploybot/migrate=`+strings.Repeat("b", 256)+`"]`+"\n", rootOwner, false)
	api, _ := p.HostEnv("deploybot", "api")
	mig, _ := p.HostEnv("deploybot", "migrate")
	if !p.Valid || len(api.Command) != 17 || len(mig.Command[0]) != 256 {
		t.Fatalf("%+v", p)
	}
	p = f.load(pinFiles+"host_env_command = []\nhost_env_healthcheck = []\n", rootOwner, false)
	api, _ = p.HostEnv("deploybot", "api")
	if !p.Valid || api.Command != nil || api.Healthcheck != nil {
		t.Fatalf("empty arrays: %+v", p)
	}
	p = f.load("host_env_command = []\n", rootOwner, false)
	if !p.Valid || len(p.HostEnvFiles) != 0 {
		t.Fatalf("empty array alone: %+v", p)
	}
	for name, argv := range map[string]string{"DEL": "a\x7fb", "non-ASCII": "café"} {
		p = f.load(pinFiles+`host_env_command = ["deploybot/api=`+argv+`"]`+"\n", rootOwner, false)
		if p.Valid || p.Reason != ReasonHostEnvFile || p.ReasonEntry != "deploybot/api" {
			t.Fatalf("%s: %+v", name, p)
		}
	}
	items := make([]string, 33)
	for i := range items {
		items[i] = `"deploybot/api=api"`
	}
	p = f.load(pinFiles+"host_env_command = ["+strings.Join(items, ", ")+"]\n", rootOwner, false)
	if p.Valid || p.Reason != "parse" {
		t.Fatalf("33 items: %+v", p)
	}
}

// The v1.22 console rewrite changes edge, images and projects only: the
// pins survive byte for byte and still load.
func TestRewriteKeepsHostEnvPins(t *testing.T) {
	pins := `host_env_command = ["deploybot/api=api"]` + "\n" +
		`host_env_healthcheck = ["deploybot/api=/usr/local/bin/deploybot-server health http://127.0.0.1:8190/health"]` + "\n"
	in := hostEnvBase + "console_policy = true\n" + pinFiles + pins
	out := string(Rewrite([]byte(in), Managed{Edge: true, Images: []string{"ghcr.io/acme/*"}, Projects: []string{"deploybot", "other"}}))
	if !strings.Contains(out, pinFiles+pins) {
		t.Fatalf("pins changed:\n%s", out)
	}
	p := FromBytes([]byte(out), "", "", nil)
	api, _ := p.HostEnv("deploybot", "api")
	if !p.Valid || !reflect.DeepEqual(api.Command, []string{"api"}) || len(api.Healthcheck) != 3 {
		t.Fatalf("reason %s %+v", p.Reason, api)
	}
}
