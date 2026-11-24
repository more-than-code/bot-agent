package tasks

import (
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// v1.25: docker_usage takes exactly {}. The policy gate is the helper's.
func TestDecideDockerUsage(t *testing.T) {
	host := "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	live := protocol.FormatTime(now.Add(time.Minute))
	gate := Gate{Policy: policy.Policy{Exists: true, Valid: true}}
	cases := []struct {
		name string
		task protocol.Task
		want string
	}{
		{"ok", task(host, "docker_usage", nil, live, []byte(`{}`)), ""},
		{"spaces", task(host, "docker_usage", nil, live, []byte(` { } `)), ""},
		{"key", task(host, "docker_usage", nil, live, []byte(`{"all":true}`)), "bad_spec"},
		{"null", task(host, "docker_usage", nil, live, []byte(`null`)), "bad_spec"},
		{"array", task(host, "docker_usage", nil, live, []byte(`[]`)), "bad_spec"},
		{"empty", task(host, "docker_usage", nil, live, nil), "bad_spec"},
		{"two", task(host, "docker_usage", nil, live, []byte(`{}{}`)), "bad_spec"},
		{"service", task(host, "docker_usage", protocol.Str("nginx"), live, []byte(`{}`)), "bad_spec"},
		{"expired", task(host, "docker_usage", nil, protocol.FormatTime(now.Add(-time.Minute)), []byte(`{}`)), "expired"},
	}
	for _, tc := range cases {
		if code, _ := Decide(tc.task, host, now, nil, gate); code != tc.want {
			t.Errorf("%s: %q want %q", tc.name, code, tc.want)
		}
	}
}
