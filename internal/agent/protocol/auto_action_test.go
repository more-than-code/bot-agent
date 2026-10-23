package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAutoAction(t *testing.T) {
	good := func() map[string]any {
		return map[string]any{
			"v": 1, "type": "auto_action", "id": "8d5f2b8e-0d6c-4b8e-9b8a-1f0c2d3e4f50",
			"t": "2026-10-23T10:00:00Z", "project": "acme", "entry": "web", "container": "acme-web",
			"action": "restart", "reason": "unhealthy for 45s", "result": "ok",
			"digest": "sha256:" + strings.Repeat("a", 64),
		}
	}
	cases := []struct {
		name string
		key  string
		val  any
		ok   bool
	}{
		{"valid", "", nil, true},
		{"null digest", "digest", nil, true},
		{"gave_up", "action", "gave_up", true},
		{"rollback", "action", "rollback", true},
		{"error result", "result", "error", true},
		{"reason 200 bytes", "reason", strings.Repeat("x", 200), true},
		{"reason 201 bytes", "reason", strings.Repeat("x", 201), false},
		{"reason empty", "reason", "", false},
		{"bad action", "action", "stop", false},
		{"bad result", "result", "failed", false},
		{"wrong container", "container", "acme-api", false},
		{"prev container", "container", "acme-web-prev", false},
		{"bad project", "project", "Acme", false},
		{"bad entry", "entry", "web_1", false},
		{"short digest", "digest", "sha256:abc", false},
		{"digest no prefix", "digest", strings.Repeat("a", 64), false},
		{"uuid upper", "id", "8D5F2B8E-0D6C-4B8E-9B8A-1F0C2D3E4F50", false},
		{"uuid short", "id", "8d5f2b8e", false},
		{"uuid empty", "id", "", false},
		{"bad time", "t", "yesterday", false},
		{"unknown key", "extra", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := good()
			if tc.key != "" {
				m[tc.key] = tc.val
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			err = ValidateAgent(raw)
			if (err == nil) != tc.ok {
				t.Fatalf("err %v, want ok=%v: %s", err, tc.ok, raw)
			}
		})
	}
}
