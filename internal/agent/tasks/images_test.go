package tasks

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestImageCheckOrder(t *testing.T) {
	host := "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	live := protocol.FormatTime(now.Add(time.Minute))
	off := policy.Policy{}
	on := policy.Policy{
		Valid: true, Enabled: true,
		Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
	}
	good := deploySpec(false, "")
	cases := []struct {
		name string
		pol  policy.Policy
		task protocol.Task
		want string
	}{
		{"wrong host", on, task("other", "image_deploy", nil, live, good), "wrong_host"},
		{"expired", on, task(host, "image_deploy", nil, protocol.FormatTime(now), good), "expired"},
		{"unknown kind", on, task(host, "image_exec", nil, live, []byte(`{}`)), "forbidden"},
		{"service set", off, task(host, "image_deploy", protocol.Str("web"), live, good), "bad_spec"},
		{"unknown key", off, task(host, "image_deploy", nil, live, []byte(`{"project":"acme","entry":"web","nope":1}`)), "bad_spec"},
		{"digest key on deploy", off, task(host, "image_deploy", nil, live, withDigest(good)), "bad_spec"},
		{"auth extra key", off, task(host, "image_start", nil, live, []byte(`{"project":"acme","entry":"web","spec":{}}`)), "bad_spec"},
		{"missing project", off, task(host, "image_start", nil, live, []byte(`{"entry":"web"}`)), "bad_spec"},
		{"valid shape images off", off, task(host, "image_deploy", nil, live, good), "policy_denied"},
		{"wrong project beats bad digest", on, task(host, "image_rollback", nil, live, []byte(`{"project":"nope","entry":"web","spec":{"kind":"service","image":"not an image"},"spec_hash":"zz","digest":"nope"}`)), "policy_denied"},
		{"bad digest", on, task(host, "image_rollback", nil, live, rollbackSpec("sha256:abcd")), "bad_spec"},
		{"bad kind before pattern", on, task(host, "image_deploy", nil, live, deployImage("pod", "nginx")), "bad_spec"},
		{"pattern miss", on, task(host, "image_deploy", nil, live, deployImage("service", "nginx")), "policy_denied"},
		{"entry prev", on, task(host, "image_start", nil, live, []byte(`{"project":"acme","entry":"web-prev"}`)), "bad_spec"},
		{"logs missing lines", off, task(host, "image_logs", nil, live, []byte(`{"project":"acme","entry":"web"}`)), "bad_spec"},
		{"logs zero images off", off, task(host, "image_logs", nil, live, []byte(`{"project":"acme","entry":"web","lines":0}`)), "policy_denied"},
		{"logs zero", on, task(host, "image_logs", nil, live, []byte(`{"project":"acme","entry":"web","lines":0}`)), "bad_spec"},
		{"logs 2001", on, task(host, "image_logs", nil, live, []byte(`{"project":"acme","entry":"web","lines":2001}`)), "bad_spec"},
		{"logs 2000", on, task(host, "image_logs", nil, live, []byte(`{"project":"acme","entry":"web","lines":2000}`)), ""},
		{"start ok", on, task(host, "image_start", nil, live, []byte(`{"project":"acme","entry":"web"}`)), ""},
		{"deploy ok", on, task(host, "image_deploy", nil, live, good), ""},
		{"rollback ok", on, task(host, "image_rollback", nil, live, rollbackSpec(imgDigest)), ""},
		{"rollback omits digest", on, task(host, "image_rollback", nil, live, deploySpec(false, "")), "bad_spec"},
		{"rollback null digest", on, task(host, "image_rollback", nil, live, nullDigestSpec()), "bad_spec"},
		{"auth controls", on, task(host, "image_deploy", nil, live, authToken("abc\ndef")), "bad_spec"},
		{"oversize", on, task(host, "image_deploy", nil, live, oversizeSpec()), "bad_spec"},
		{"secret key missing", on, task(host, "image_deploy", nil, live, withSecretKeys(good, `["NOPE"]`)), "bad_spec"},
		{"secret key on rollback", on, task(host, "image_rollback", nil, live, withSecretKeys(rollbackSpec(imgDigest), `["DB_URL"]`)), ""},
		{"secret keys empty", on, task(host, "image_deploy", nil, live, withSecretKeys(good, `[]`)), ""},
		{"over helper frame off", off, task(host, "image_deploy", nil, live, wideSpec()), "policy_denied"},
		{"over helper frame", on, task(host, "image_deploy", nil, live, wideSpec()), ""},
	}
	wide := wideSpec()
	if len(wide) <= programs.MaxFrame || len(wide) >= protocol.MaxFrame {
		t.Fatalf("wide spec len %d", len(wide))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := Decide(tc.task, host, now, nil, Gate{Policy: tc.pol})
			if code != tc.want {
				t.Fatalf("got %s", code)
			}
		})
	}
}

const (
	imgDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	imgHash   = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func deploySpec(force bool, token string) []byte {
	if token == "" {
		token = "ghp_tokenvalue"
	}
	forceJSON := "false"
	if force {
		forceJSON = "true"
	}
	return []byte(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","ports":[{"host":3031,"container":3031,"public":false}],"env":{"NODE_ENV":"production"},"secret_env":{"DB_URL":"services.DB_URL"},"healthy_after_sec":5},"env":{"NODE_ENV":"production","DB_URL":"sek\"ret"},"registry_auth":{"host":"ghcr.io","username":"bot","token":"` + token + `"},"force":` + forceJSON + `,"spec_hash":"` + imgHash + `"}`)
}

func deployImage(kind, image string) []byte {
	return []byte(`{"project":"acme","entry":"web","spec":{"kind":"` + kind + `","image":"` + image + `","healthy_after_sec":5},"env":{},"registry_auth":null,"force":false,"spec_hash":"` + imgHash + `"}`)
}

func rollbackSpec(digest string) []byte {
	body := deploySpec(false, "")
	body = body[:len(body)-1]
	return append(body, []byte(`,"digest":"`+digest+`"}`)...)
}

func withSecretKeys(spec []byte, keys string) []byte {
	return []byte(strings.TrimSuffix(string(spec), "}") + `,"secret_keys":` + keys + `}`)
}

func withDigest(spec []byte) []byte {
	return []byte(strings.TrimSuffix(string(spec), "}") + `,"digest":"` + imgDigest + `"}`)
}

func authToken(token string) []byte {
	return []byte(strings.Replace(string(deploySpec(false, "x")), `"token":"x"`, `"token":"`+token+`"`, 1))
}

func oversizeSpec() []byte {
	return []byte(strings.Repeat("x", protocol.MaxFrame+1))
}

func nullDigestSpec() []byte {
	body := deploySpec(false, "")
	return []byte(strings.TrimSuffix(string(body), "}") + `,"digest":null}`)
}

func wideSpec() []byte {
	var b strings.Builder
	b.WriteString(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","healthy_after_sec":5},"env":{`)
	val := strings.Repeat("a", 4096)
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"K%02d":"%s"`, i, val)
	}
	b.WriteString(`},"registry_auth":null,"force":false,"spec_hash":"` + imgHash + `"}`)
	return []byte(b.String())
}

func TestSecretValuesFollowsKeys(t *testing.T) {
	host := "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	live := protocol.FormatTime(now.Add(time.Minute))
	pol := policy.Policy{
		Valid: true, Enabled: true,
		Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
	}
	const (
		plain = "mohiguide"
		key   = "s3cret-value-1"
		token = "registry-token-1"
		file  = "file-secret-value"
	)
	body := `{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1","healthy_after_sec":5,"files":[{"file":"k","target":"/etc/k"}]},"env":{"DB_NAME":"` + plain + `","API_KEY":"` + key + `"},"registry_auth":{"host":"ghcr.io","username":"bot","token":"` + token + `"},"force":false,"spec_hash":"` + imgHash + `","files":[{"name":"k","target":"/etc/k","content":"` + file + `","secret":true}]}`
	parse := func(spec []byte) ImageReq {
		t.Helper()
		req, code := ParseImage(task(host, "image_deploy", nil, live, spec), pol)
		if code != "" {
			t.Fatalf("parse %s", code)
		}
		return req
	}
	has := func(vals []string, want string) bool {
		for _, v := range vals {
			if v == want {
				return true
			}
		}
		return false
	}
	absent := parse([]byte(body))
	if !has(absent.SecretValues(), plain) || !has(absent.SecretValues(), key) || !has(absent.SecretValues(), token) || !has(absent.SecretValues(), file) {
		t.Fatalf("absent %q", absent.SecretValues())
	}
	listed := parse(withSecretKeys([]byte(body), `["API_KEY"]`))
	got := listed.SecretValues()
	if has(got, plain) || !has(got, key) || !has(got, token) || !has(got, file) {
		t.Fatalf("listed %q", got)
	}
	empty := parse(withSecretKeys([]byte(body), `[]`))
	got = empty.SecretValues()
	if has(got, plain) || has(got, key) || !has(got, token) || !has(got, file) {
		t.Fatalf("empty %q", got)
	}
	if _, code := ParseImage(task(host, "image_deploy", nil, live, withSecretKeys([]byte(body), `["NOPE"]`)), pol); code != "bad_spec" {
		t.Fatalf("missing key %s", code)
	}
}
