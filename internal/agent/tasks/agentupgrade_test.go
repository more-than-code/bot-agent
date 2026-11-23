package tasks

import (
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// v1.20 item 4: forbidden unless the hello had agent_upgrade true; then
// only {"version":"X.Y.Z"} with no service is accepted.
func TestDecideAgentUpgrade(t *testing.T) {
	host := "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	live := protocol.FormatTime(now.Add(time.Minute))
	on := Gate{Policy: policy.Policy{Exists: true, Valid: true, AgentUpgrade: true}}
	off := Gate{Policy: policy.Policy{Exists: true, Valid: true}}
	invalid := Gate{Policy: policy.Policy{Exists: true, AgentUpgrade: true}}
	good := []byte(`{"version":"0.2.16"}`)
	cases := []struct {
		name string
		task protocol.Task
		gate Gate
		want string
	}{
		{"off", task(host, "agent_upgrade", nil, live, good), off, "forbidden"},
		{"no policy", task(host, "agent_upgrade", nil, live, good), Gate{}, "forbidden"},
		{"invalid policy", task(host, "agent_upgrade", nil, live, good), invalid, "forbidden"},
		{"off beats bad spec", task(host, "agent_upgrade", nil, live, []byte(`{}`)), off, "forbidden"},
		{"service", task(host, "agent_upgrade", protocol.Str("nginx"), live, good), on, "bad_spec"},
		{"bad version", task(host, "agent_upgrade", nil, live, []byte(`{"version":"latest"}`)), on, "bad_spec"},
		{"extra key", task(host, "agent_upgrade", nil, live, []byte(`{"version":"0.2.16","url":"x"}`)), on, "bad_spec"},
		{"wrong host first", task("other", "agent_upgrade", nil, live, good), on, "wrong_host"},
		{"ok", task(host, "agent_upgrade", nil, live, good), on, ""},
	}
	for _, tc := range cases {
		if code, _ := Decide(tc.task, host, now, nil, tc.gate); code != tc.want {
			t.Errorf("%s: %q want %q", tc.name, code, tc.want)
		}
	}
}
