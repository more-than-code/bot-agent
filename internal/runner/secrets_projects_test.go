package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

const ghToken = "ghp_SECRETvalue0123456789"

func (h *harness) startSecrets(t *testing.T, env map[string]string, keys []string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(h.startEnv(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil, env), &m); err != nil {
		t.Fatal(err)
	}
	m["secret_keys"] = keys
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// v1.11 items 2–3: the helper accepts a listed secret key, writes the env file
// as before (0600) and keeps secret_keys in the stored spec.
func TestStartWithSecretKeysStoresSpec(t *testing.T) {
	h := newHarness(t, true, false, 20)
	env := map[string]string{"GH_TOKEN": ghToken, "PORT": "3031"}
	raw := Handle(context.Background(), "start", h.startSecrets(t, env, []string{"GH_TOKEN"}), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if strings.Contains(string(raw), ghToken) {
		t.Fatal("value in reply")
	}
	unit := "deploybot-demo-web.service"
	envBody, err := os.ReadFile(envFile(h.root, unit))
	if err != nil || !strings.Contains(string(envBody), "GH_TOKEN=\""+ghToken+"\"") {
		t.Fatalf("env file %q %v", envBody, err)
	}
	assertMode(t, envFile(h.root, unit), 0o600)
	assertMode(t, specFile(h.root, unit), 0o600)
	b, err := os.ReadFile(specFile(h.root, unit))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		SecretKeys []string `json:"secret_keys"`
	}
	if err := json.Unmarshal(b, &stored); err != nil || !reflect.DeepEqual(stored.SecretKeys, []string{"GH_TOKEN"}) {
		t.Fatalf("stored %s", b)
	}
	if bytes, _ := os.ReadFile(unitFile(h.root, unit)); strings.Contains(string(bytes), ghToken) {
		t.Fatal("value in unit file")
	}
}

// A start without secret_keys stores the spec without the key (v1.9 bytes).
func TestStartWithoutSecretKeysOmitsField(t *testing.T) {
	h := newHarness(t, true, false, 20)
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	b, err := os.ReadFile(specFile(h.root, "deploybot-demo-web.service"))
	if err != nil || strings.Contains(string(b), "secret_keys") {
		t.Fatalf("stored %s %v", b, err)
	}
}

func TestHelperRefusesUnlistedSecretKey(t *testing.T) {
	h := newHarness(t, true, false, 20)
	cases := []struct {
		env  map[string]string
		keys []string
		want string
	}{
		{map[string]string{"GH_TOKEN": ghToken}, []string{}, programs.ReasonBadSpec},
		{map[string]string{"PORT": "1"}, []string{"GH_TOKEN"}, programs.ReasonBadSpec},
		{map[string]string{"NODE_OPTIONS": "--require x"}, []string{"NODE_OPTIONS"}, programs.ReasonPolicy},
	}
	for _, tc := range cases {
		raw := Handle(context.Background(), "start", h.startSecrets(t, tc.env, tc.keys), h.sys())
		if got := refusalReason(t, raw); got != tc.want {
			t.Fatalf("%v %v: %s", tc.env, tc.keys, raw)
		}
	}
	if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); !os.IsNotExist(err) {
		t.Fatal("a refused start wrote a unit")
	}
}

// v1.11 item 2: the restart re-check of the stored spec accepts its secret_keys.
func TestRestartRecheckAcceptsStoredSecretKeys(t *testing.T) {
	h := newHarness(t, true, false, 20)
	env := map[string]string{"GH_TOKEN": ghToken}
	if raw := Handle(context.Background(), "start", h.startSecrets(t, env, []string{"GH_TOKEN"}), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	h.ctl.calls = nil
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if !h.ctl.called("restart") || h.ctl.called("disable") {
		t.Fatalf("calls %v", h.ctl.calls)
	}
	if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); err != nil {
		t.Fatal("unit removed")
	}
}

// v1.11 item 4: the helper redacts the stored spec's listed values (raw,
// base64, JSON-escaped) as the preview; unlisted env values are not touched.
func TestLogsRedactStoredSecrets(t *testing.T) {
	h := newHarness(t, true, false, 20)
	env := map[string]string{"GH_TOKEN": ghToken, "PORT": "3031-visible"}
	if raw := Handle(context.Background(), "start", h.startSecrets(t, env, []string{"GH_TOKEN"}), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	h.ctl.journal = "using " + ghToken + "\n" +
		"b64 " + base64.StdEncoding.EncodeToString([]byte(ghToken)) + "\n" +
		"port 3031-visible\n"
	raw := Handle(context.Background(), "logs", h.logs(t, "demo", "web", 10), h.sys())
	batch, reason, err := programs.DecodeLogs(raw)
	if err != nil || reason != "" {
		t.Fatal(string(raw), err)
	}
	joined := strings.Join(batch.Lines, "\n")
	if strings.Contains(joined, ghToken) || strings.Contains(joined, base64.StdEncoding.EncodeToString([]byte(ghToken))) {
		t.Fatalf("value leaked: %s", joined)
	}
	if strings.Count(joined, "ghp_…") != 2 || !strings.Contains(joined, "3031-visible") {
		t.Fatalf("lines %q", batch.Lines)
	}
}

// A stored spec that cannot be parsed makes logs fail closed.
func TestLogsUnreadableSpecFailsClosed(t *testing.T) {
	h := newHarness(t, true, false, 20)
	if raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if err := os.WriteFile(specFile(h.root, "deploybot-demo-web.service"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ctl.journal = "line\n"
	raw := Handle(context.Background(), "logs", h.logs(t, "demo", "web", 10), h.sys())
	if refusalReason(t, raw) != programs.ReasonIO {
		t.Fatal(string(raw))
	}
}

// v1.12 items 1–3: a project taken out of projects refuses start and restart
// with policy_denied; the restart keeps the unit (no disable, stop or
// removal); stop, status and logs still work.
func TestProjectRemovedRestartKeepsUnit(t *testing.T) {
	h := newHarness(t, true, false, 20)
	if raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	unit := "deploybot-demo-web.service"
	h.ctl.state[unit] = "active"
	// The owner drops "demo" from projects.
	b, err := os.ReadFile(h.policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.policy, []byte(strings.Replace(string(b), `"demo", `, "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	h.ctl.calls = nil
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatal(string(raw))
	}
	if len(systemctlVerbs(h.ctl)) != 0 {
		t.Fatalf("restart touched systemd: %v", h.ctl.calls)
	}
	for _, p := range []string{unitFile(h.root, unit), envFile(h.root, unit), specFile(h.root, unit)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("removed %s", p)
		}
	}
	if raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys()); refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatalf("start %s", raw)
	}
	if raw := Handle(context.Background(), "status", h.ref(t, "demo", "web"), h.sys()); refusalReason(t, raw) != "" {
		t.Fatalf("status %s", raw)
	}
	h.ctl.journal = "hello\n"
	if raw := Handle(context.Background(), "logs", h.logs(t, "demo", "web", 10), h.sys()); refusalReason(t, raw) != "" {
		t.Fatalf("logs %s", raw)
	}
	if raw := Handle(context.Background(), "list", []byte("{}"), h.sys()); refusalReason(t, raw) != "" || !strings.Contains(string(raw), unit) {
		t.Fatalf("list %s", raw)
	}
	raw = Handle(context.Background(), "stop", h.ref(t, "demo", "web"), h.sys())
	if st := mustStatus(t, raw); st.ActiveState != "inactive" {
		t.Fatalf("stop %s", raw)
	}
	if _, err := os.Stat(unitFile(h.root, unit)); !os.IsNotExist(err) {
		t.Fatal("stop left the unit")
	}
}

// v1.12 item 1: overlapping projects turn programs off at the helper too.
func TestOverlapRefusesProgramStart(t *testing.T) {
	h := newHarness(t, true, false, 20)
	b, err := os.ReadFile(h.policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.policy, []byte(strings.Replace(string(b), `"mohiguide"`, `"demo-x"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy {
		t.Fatal(string(raw))
	}
}

func readStored(t *testing.T, h *harness) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(specFile(h.root, "deploybot-demo-web.service"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func logsJoined(t *testing.T, h *harness, journal string) string {
	t.Helper()
	h.ctl.journal = journal
	raw := Handle(context.Background(), "logs", h.logs(t, "demo", "web", 10), h.sys())
	batch, reason, err := programs.DecodeLogs(raw)
	if err != nil || reason != "" {
		t.Fatal(string(raw), err)
	}
	return strings.Join(batch.Lines, "\n")
}

// C7a and S1: a Run with a changed value rewrites the env file and the stored
// spec, keeps the old value as retired, and logs still redact it. A restart
// keeps the retired set, and the re-check ignores the helper-only key.
func TestRunWithChangedValueRetiresOld(t *testing.T) {
	h := newHarness(t, true, false, 20)
	const newToken = "ghp_NEWvalue98765432100"
	for _, v := range []string{ghToken, newToken} {
		raw := Handle(context.Background(), "start", h.startSecrets(t, map[string]string{"GH_TOKEN": v}, []string{"GH_TOKEN"}), h.sys())
		if refusalReason(t, raw) != "" {
			t.Fatal(string(raw))
		}
	}
	unit := "deploybot-demo-web.service"
	envBody, err := os.ReadFile(envFile(h.root, unit))
	if err != nil || !strings.Contains(string(envBody), newToken) || strings.Contains(string(envBody), ghToken) {
		t.Fatalf("env file %q", envBody)
	}
	m := readStored(t, h)
	if string(m["retired_secrets"]) != `["`+ghToken+`"]` || !strings.Contains(string(m["env"]), newToken) {
		t.Fatalf("stored %s %s", m["retired_secrets"], m["env"])
	}
	assertMode(t, specFile(h.root, unit), 0o600)
	joined := logsJoined(t, h, "old "+ghToken+"\nnew "+newToken+"\n")
	if strings.Contains(joined, ghToken) || strings.Contains(joined, newToken) || strings.Count(joined, "ghp_…") != 2 {
		t.Fatalf("lines %q", joined)
	}
	// Restart re-checks the stored spec (retired key dropped) and keeps the set.
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if got := string(readStored(t, h)["retired_secrets"]); got != `["`+ghToken+`"]` {
		t.Fatalf("after restart %s", got)
	}
	// A value that is current again leaves the retired set.
	if raw := Handle(context.Background(), "start", h.startSecrets(t, map[string]string{"GH_TOKEN": ghToken}, []string{"GH_TOKEN"}), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if got := string(readStored(t, h)["retired_secrets"]); got != `["`+newToken+`"]` {
		t.Fatalf("swap back %s", got)
	}
}

// The retired set keeps the newest 64 values.
func TestRetiredSetCapped(t *testing.T) {
	h := newHarness(t, true, false, 20)
	for i := 0; i < maxRetired+5; i++ {
		v := "ghp_value_" + strings.Repeat("x", 4) + string(rune('A'+i/26)) + string(rune('a'+i%26))
		if raw := Handle(context.Background(), "start", h.startSecrets(t, map[string]string{"GH_TOKEN": v}, []string{"GH_TOKEN"}), h.sys()); refusalReason(t, raw) != "" {
			t.Fatal(string(raw))
		}
	}
	var retired []string
	if err := json.Unmarshal(readStored(t, h)["retired_secrets"], &retired); err != nil {
		t.Fatal(err)
	}
	if len(retired) != maxRetired || retired[0] != "ghp_value_xxxxAe" || retired[maxRetired-1] != "ghp_value_xxxxCp" {
		t.Fatalf("len %d first %s last %s", len(retired), retired[0], retired[len(retired)-1])
	}
}

// C6: the JSON-escaped form of a value with `"` and `\` is redacted.
func TestLogsRedactJSONEscapedSecret(t *testing.T) {
	h := newHarness(t, true, false, 20)
	const v = `pa"ss\word-123456`
	if raw := Handle(context.Background(), "start", h.startSecrets(t, map[string]string{"DB_PASSWORD": v}, []string{"DB_PASSWORD"}), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	escaped, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Trim(string(escaped), `"`)
	joined := logsJoined(t, h, `{"msg":"connect","dsn":"`+body+`"}`+"\n")
	if strings.Contains(joined, body) || strings.Contains(joined, `pa\"ss`) {
		t.Fatalf("escaped value leaked: %q", joined)
	}
}

// S2: an owned unit with no stored spec fails closed for logs.
func TestLogsMissingSpecFailsClosed(t *testing.T) {
	h := newHarness(t, true, false, 20)
	if raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if err := os.Remove(specFile(h.root, "deploybot-demo-web.service")); err != nil {
		t.Fatal(err)
	}
	h.ctl.journal = "line\n"
	h.ctl.calls = nil
	raw := Handle(context.Background(), "logs", h.logs(t, "demo", "web", 10), h.sys())
	if refusalReason(t, raw) != programs.ReasonIO {
		t.Fatal(string(raw))
	}
}

// S3: a capped journal is cut at the last line end, so no partial line is kept.
func TestLogsCapCutsAtLineEnd(t *testing.T) {
	h := newHarness(t, true, false, 20)
	if raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	full := strings.Repeat("a", 1000) + "\n"
	n := maxJournalRead / len(full)
	journal := strings.Repeat(full, n) + "PARTIAL" + strings.Repeat("b", 4096) + "\n"
	joined := logsJoined(t, h, journal)
	if strings.Contains(joined, "PARTIAL") {
		t.Fatal("partial line kept")
	}
}

// C7b: a restart refused because projects overlap keeps the unit.
func TestOverlapRestartKeepsUnit(t *testing.T) {
	h := newHarness(t, true, false, 20)
	if raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	b, err := os.ReadFile(h.policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.policy, []byte(strings.Replace(string(b), `"mohiguide"`, `"demo-x"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	h.ctl.calls = nil
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy || len(systemctlVerbs(h.ctl)) != 0 {
		t.Fatalf("%s calls %v", raw, h.ctl.calls)
	}
	if _, err := os.Stat(unitFile(h.root, "deploybot-demo-web.service")); err != nil {
		t.Fatal("unit removed")
	}
}

// S4: the restart re-check uses Handle's policy snapshot, and a stored spec
// whose project that snapshot does not allow is refused without removing the
// unit. The request's project is listed; the stored one is not.
func TestRecheckStoredProjectNotAllowedKeepsUnit(t *testing.T) {
	h := newHarness(t, true, false, 20)
	if raw := Handle(context.Background(), "start", h.start(t, "demo", "web", h.app, "/usr/bin/pnpm", false, nil), h.sys()); refusalReason(t, raw) != "" {
		t.Fatal(string(raw))
	}
	unit := "deploybot-demo-web.service"
	path := specFile(h.root, unit)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), `"project":"demo"`, `"project":"other"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ctl.calls = nil
	raw := Handle(context.Background(), "restart", h.ref(t, "demo", "web"), h.sys())
	if refusalReason(t, raw) != programs.ReasonPolicy || len(systemctlVerbs(h.ctl)) != 0 {
		t.Fatalf("%s calls %v", raw, h.ctl.calls)
	}
	for _, p := range []string{unitFile(h.root, unit), envFile(h.root, unit), path} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("removed %s", p)
		}
	}
}
