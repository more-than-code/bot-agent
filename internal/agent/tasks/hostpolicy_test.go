package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// v1.22 item 2: forbidden unless the hello had console_policy true.
func TestDecideHostPolicy(t *testing.T) {
	host := "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	live := protocol.FormatTime(now.Add(time.Minute))
	on := Gate{Policy: policy.Policy{Exists: true, Valid: true, ConsolePolicy: true}}
	off := Gate{Policy: policy.Policy{Exists: true, Valid: true}}
	good := []byte(`{"base_hash":"` + strings.Repeat("a", 64) + `","edge":true,"images":[],"projects":["acme"]}`)
	cases := []struct {
		name string
		task protocol.Task
		gate Gate
		want string
	}{
		{"off", task(host, "host_policy_apply", nil, live, good), off, "forbidden"},
		{"invalid", task(host, "host_policy_apply", nil, live, good), Gate{Policy: policy.Policy{Exists: true, ConsolePolicy: true}}, "forbidden"},
		{"service", task(host, "host_policy_apply", protocol.Str("nginx"), live, good), on, "bad_spec"},
		{"bad spec", task(host, "host_policy_apply", nil, live, []byte(`{"edge":true}`)), on, "bad_spec"},
		{"ok", task(host, "host_policy_apply", nil, live, good), on, ""},
	}
	for _, tc := range cases {
		if code, _ := Decide(tc.task, host, now, nil, tc.gate); code != tc.want {
			t.Errorf("%s: %q want %q", tc.name, code, tc.want)
		}
	}
}
