package protocol

import (
	"fmt"
	"strings"
	"testing"
)

func hostFilesHello(items string) []byte {
	return []byte(`{"v":1,"type":"hello","host_id":"11111111-1111-4111-8111-111111111111","credential_id":"22222222-2222-4222-8222-222222222222","agent_version":"0.2.13","os":"linux","arch":"arm64","capabilities":["metrics","probe"],"programs":{"enabled":false,"run_as":[],"allow_shell":false,"policy_hash":"` + strings.Repeat("ab", 32) + `","images":false,"docker":false,"projects":[],"shared_networks":[],"host_files":[` + items + `]},"services":[],"services_complete":true}`)
}

func hostFileItem(name, path, unit string) string {
	return fmt.Sprintf(`{"name":%q,"path":%q,"secret":false,"unit":%q,"health":true}`, name, path, unit)
}

// v1.18 item 3 bounds on hello.programs.host_files.
func TestHelloHostFilesBounds(t *testing.T) {
	var items []string
	for i := 0; i < 9; i++ {
		items = append(items, hostFileItem(fmt.Sprintf("f%d", i), fmt.Sprintf("/etc/app/f%d", i), "app.service"))
	}
	if err := ValidateAgent(hostFilesHello(strings.Join(items[:8], ","))); err != nil {
		t.Fatalf("8: %v", err)
	}
	if err := ValidateAgent(hostFilesHello(strings.Join(items, ","))); err == nil {
		t.Fatal("9 files accepted")
	}
	for _, bad := range []string{
		hostFileItem("Bad", "/etc/app/a", "app.service"),
		hostFileItem("a", "/etc/ssh/a", "app.service"),
		hostFileItem("a", "/etc/app/sub/a", "app.service"),
		hostFileItem("a", "/etc/app/a", "sshd.service"),
		hostFileItem("a", "/etc/app/a", "app"),
	} {
		if err := ValidateAgent(hostFilesHello(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// v1.18 item 5 frame checks.
func TestHostFilesStatusFrameChecks(t *testing.T) {
	sum := strings.Repeat("a", 64)
	ok := `{"v":1,"type":"host_files_status","files":[{"name":"a","exists":true,"owner_ok":true,"matches_applied":null,"size":3,"sha256":"` + sum + `"}]}`
	if err := ValidateAgent([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"v":1,"type":"host_files_status","files":[{"name":"a","exists":true,"owner_ok":true,"matches_applied":null,"sha256":"` + sum + `"}]}`,
		`{"v":1,"type":"host_files_status","files":[{"name":"a","exists":true,"owner_ok":false,"reason":"weird","matches_applied":null}]}`,
		`{"v":1,"type":"host_files_status","files":[{"name":"a","exists":true,"owner_ok":true,"matches_applied":null,"content":"x"}]}`,
		`{"v":1,"type":"host_files_status","files":null}`,
	} {
		if err := ValidateAgent([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
