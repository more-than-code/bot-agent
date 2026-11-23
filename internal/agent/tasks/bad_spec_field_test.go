package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// agent-protocol v1.23: a bad_spec image task names the key that failed,
// from fixed key names only.
func TestBadSpecField(t *testing.T) {
	host := "11111111-1111-4111-8111-111111111111"
	live := protocol.FormatTime(time.Now().Add(time.Minute))
	on := policy.Policy{
		Valid: true, Enabled: true,
		Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
	}
	good := string(deploySpec(false, ""))
	swap := func(old, new string) []byte {
		if !strings.Contains(good, old) {
			t.Fatalf("fixture has no %q", old)
		}
		return []byte(strings.Replace(good, old, new, 1))
	}
	cases := []struct {
		name string
		kind string
		svc  *string
		spec []byte
		want string
	}{
		{"service set", "image_deploy", protocol.Str("web"), []byte(good), "service"},
		{"secret_env ref", "image_deploy", nil, swap(`"secret_env":{"DB_URL":"services.DB_URL"}`, `"secret_env":{"DB_URL":"not a ref"}`), "spec.secret_env"},
		{"secret_env type", "image_deploy", nil, swap(`"secret_env":{"DB_URL":"services.DB_URL"}`, `"secret_env":["DB_URL"]`), "spec.secret_env"},
		{"spec ports", "image_deploy", nil, swap(`"host":3031`, `"host":0`), "spec.ports"},
		{"healthy_after", "image_deploy", nil, swap(`"healthy_after_sec":5`, `"healthy_after_sec":1`), "spec.healthy_after_sec"},
		{"task env", "image_deploy", nil, swap(`"env":{"NODE_ENV":"production","DB_URL"`, `"env":{"bad key":"x","DB_URL"`), "env"},
		{"registry auth", "image_deploy", nil, swap(`"username":"bot"`, `"username":""`), "registry_auth"},
		{"spec hash", "image_deploy", nil, swap(imgHash, "zz"), "spec_hash"},
		{"missing spec_hash", "image_deploy", nil, swap(`,"spec_hash":"`+imgHash+`"`, ""), "spec_hash"},
		{"env_hash", "image_deploy", nil, withEnvHash([]byte(good), `"nope"`), "env_hash"},
		{"secret keys", "image_deploy", nil, withSecretKeys([]byte(good), `["MISSING"]`), "secret_keys"},
		{"digest", "image_rollback", nil, rollbackSpec("sha256:abcd"), "digest"},
		{"logs lines", "image_logs", nil, []byte(`{"project":"acme","entry":"web","lines":0}`), "lines"},
		{"entry", "image_start", nil, []byte(`{"project":"acme","entry":"Web"}`), "entry"},
		// Unknown keys and map keys come from the task: they are not echoed.
		{"unknown key", "image_deploy", nil, swap(`"force":false`, `"force":false,"sekret_name":1`), ""},
		{"map key in type error", "image_deploy", nil, swap(`"env":{"NODE_ENV":"production","DB_URL"`, `"env":{"NODE_ENV":7,"DB_URL"`), "env"},
		{"not json", "image_deploy", nil, []byte(`{`), ""},
		{"accepted", "image_deploy", nil, []byte(good), ""},
		{"policy denied", "image_deploy", nil, swap(`"project":"acme"`, `"project":"other"`), ""},
	}
	for _, tc := range cases {
		task := task(host, tc.kind, tc.svc, live, tc.spec)
		code, _ := Decide(task, host, time.Now(), nil, Gate{Policy: on})
		if tc.want != "" && code != "bad_spec" {
			t.Fatalf("%s: code %q", tc.name, code)
		}
		if got := BadSpecField(task, on); got != tc.want {
			t.Fatalf("%s: field %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestValidRejectField(t *testing.T) {
	for _, ok := range []string{"env", "spec.secret_env", "spec.healthcheck.interval_sec", "spec.health_policy.on_unhealthy"} {
		if !protocol.ValidRejectField(ok) {
			t.Fatalf("refused %q", ok)
		}
	}
	for _, bad := range []string{"", "Env", "spec.", "a.b.c.d", "spec.DB_URL", "spec secret", strings.Repeat("a", 65)} {
		if protocol.ValidRejectField(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
