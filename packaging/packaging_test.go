package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

func TestRunnerSocketUnits(t *testing.T) {
	if _, err := os.Stat("sudoers.d"); !os.IsNotExist(err) {
		t.Fatal("sudoers.d is still packaged")
	}
	sock, err := os.ReadFile("bot-agent-runner.socket")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"ListenStream=/run/bot-agent-runner.sock\n",
		"SocketUser=root\n",
		"SocketGroup=bot-agent\n",
		"SocketMode=0660\n",
		"Accept=yes\n",
	} {
		if !strings.Contains(string(sock), line) {
			t.Fatalf("socket missing %q", line)
		}
	}
	unit, err := os.ReadFile("bot-agent-runner@.service")
	if err != nil {
		t.Fatal(err)
	}
	text := string(unit)
	for _, line := range []string{
		"User=root\n",
		"StandardInput=socket\n",
		"StandardOutput=socket\n",
		"ExecStart=/usr/local/libexec/bot-agent-runner serve-conn\n",
		"RuntimeMaxSec=900\n",
		"ProtectHome=read-only\n",
		"PrivateTmp=yes\n",
		"ProtectKernelModules=yes\n",
		"RestrictAddressFamilies=AF_UNIX\n",
		"ProtectSystem=strict\n",
		"PrivateDevices=yes\n",
		"NoNewPrivileges=yes\n",
		"ReadWritePaths=/run/systemd/system -/run/bot-agent -/var/run/docker.sock\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("helper unit missing %q", line)
		}
	}
	if !strings.Contains(text, "ProtectHome=yes") && !strings.Contains(text, "CapabilityBoundingSet") {
		t.Fatal("helper unit does not say why stronger sandboxing is omitted")
	}
	if strings.Contains(text, "sudo") {
		t.Fatal("helper unit mentions sudo")
	}
	tmp, err := os.ReadFile(filepath.Join("tmpfiles.d", "bot-agent-runner.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"d /run/bot-agent 0700 root root -\n",
		"d /run/bot-agent/env 0700 root root -\n",
	} {
		if !strings.Contains(string(tmp), line) {
			t.Fatalf("tmpfiles missing %q", line)
		}
	}
}

func TestServiceGainsNoCapability(t *testing.T) {
	b, err := os.ReadFile("bot-agent.service")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, "CapabilityBoundingSet=\n") || !strings.Contains(text, "AmbientCapabilities=\n") {
		t.Fatal("capability lines are not empty")
	}
	if strings.Contains(text, "CAP_") {
		t.Fatal("service names a capability")
	}
	if !strings.Contains(text, "InaccessiblePaths=-/run/docker.sock -/var/run/docker.sock\n") {
		t.Fatal("docker sockets are not hidden with the ignore-if-missing prefix")
	}
}

func TestExamplePolicyIsDisabled(t *testing.T) {
	src, err := os.ReadFile("runner.toml.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "container access equals root") {
		t.Fatal("example does not warn that container access equals root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte("joe:x:1000:1000:Joe:/home/joe:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("joe:x:1000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Load(policy.Files{
		Policy: path, Passwd: passwd, Group: group,
		UID: func(os.FileInfo) (uint32, bool) { return 0, true },
	})
	if !pol.Valid || pol.Enabled || pol.AllowShell || pol.MaxPrograms != 20 {
		t.Fatalf("valid %v enabled %v shell %v max %d reason %s", pol.Valid, pol.Enabled, pol.AllowShell, pol.MaxPrograms, pol.Reason)
	}
	if len(pol.RunAs) != 1 || pol.RunAs[0] != "joe" || len(pol.Dirs) != 1 || len(pol.ExecAllow) != 3 {
		t.Fatalf("%+v", pol)
	}
	if pol.ImagesEnabled() || len(pol.Images) != 0 || len(pol.Projects) != 0 || pol.AllowPublicPorts {
		t.Fatalf("images %+v", pol)
	}
}
