package protocol

import (
	"fmt"
	"strings"
	"testing"
)

// v1.12 item 5: hello.programs.projects holds at most 32 slugs.
func TestHelloProjectsCap(t *testing.T) {
	frame := func(n int) []byte {
		xs := make([]string, n)
		for i := range xs {
			xs[i] = fmt.Sprintf("%q", fmt.Sprintf("p%d", i))
		}
		return []byte(`{"v":1,"type":"hello","host_id":"11111111-1111-4111-8111-111111111111","credential_id":"22222222-2222-4222-8222-222222222222","agent_version":"0.2.7","os":"linux","arch":"arm64","capabilities":["metrics","probe"],"programs":{"enabled":false,"run_as":[],"allow_shell":false,"policy_hash":"` + strings.Repeat("ab", 32) + `","images":false,"docker":false,"secrets":true,"projects":[` + strings.Join(xs, ",") + `]},"services":[],"services_complete":true}`)
	}
	if err := ValidateAgent(frame(32)); err != nil {
		t.Fatalf("32: %v", err)
	}
	if err := ValidateAgent(frame(33)); err == nil {
		t.Fatal("33 projects accepted")
	}
}
