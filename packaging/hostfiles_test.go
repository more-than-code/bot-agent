package packaging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hostFilesDropin = "etc/systemd/system/bot-agent-runner@.service.d/host-files.conf"

// hostFilesCheck is the real policy check output for two files in
// /etc/dd-worker and one in /etc/app (agent-protocol v1.18 item 2). The
// same golden file is checked against bot-agent policy check in
// cmd/bot-agent (TestPolicyCheckHostFilesGolden).
func hostFilesCheck(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hostfiles-policy-check.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const hostFilesNone = "valid\nenabled=false\nprograms=false\nimages=false\nprojects=0\nmax_containers=20\n" +
	"allow_public_ports=false\nimage_patterns=0\nprojects_overlap=false\nshared_networks=0\nedge=false\nhost_files=0\nagent_upgrade=false\nagent_upgrade_https_proxy=unset\n"

const hostFilesWant = "# Written by install.sh from host_files in /etc/bot-agent/runner.toml (agent-protocol v1.18).\n" +
	"# Rerun install.sh upgrade --runner after changing host_files.\n" +
	"[Service]\n" +
	"ReadWritePaths=-/etc/dd-worker\n" +
	"ReadWritePaths=-/etc/app\n"

func setPolicyCheck(t *testing.T, rg *rig, out string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rg.stubs, "policy-check"), []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Spec §5 item 2: with host files the drop-in lists exactly their
// directories; without, it is removed; check reports both.
func TestInstallerHostFilesDropin(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	setPolicyCheck(t, rg, hostFilesCheck(t))
	res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", writeOKConfig(t))
	if res.code != 0 {
		res.fail(t, "", "install")
	}
	drop := filepath.Join(rg.root, hostFilesDropin)
	got, err := os.ReadFile(drop)
	if err != nil {
		res.fail(t, "", "no drop-in")
	}
	if string(got) != hostFilesWant {
		t.Fatalf("drop-in:\n%s", got)
	}
	assertPerm(t, drop, 0o644)
	if !strings.Contains(res.stdout, "host files: helper may write /etc/dd-worker\n") ||
		!strings.Contains(res.stdout, "host files: helper may write /etc/app\n") {
		res.fail(t, "", "dirs not reported")
	}
	reloads := strings.Count(res.log, "systemctl daemon-reload")
	if reloads < 2 {
		res.fail(t, "", "drop-in write did not daemon-reload")
	}

	// check: in sync.
	res = runBundle(t, bundle, rg, "", "check", "--runner")
	if res.code != 0 || !strings.Contains(res.stdout, "host files: 3; drop-in /etc/systemd/system/bot-agent-runner@.service.d/host-files.conf: in sync") {
		res.fail(t, "", "check in sync")
	}

	// Same version upgrade: unchanged, no reload.
	rg.truncateLog(t)
	res = runBundle(t, bundle, rg, "", "upgrade")
	if res.code != 0 || !strings.Contains(res.stdout, "unchanged: /etc/systemd/system/bot-agent-runner@.service.d/host-files.conf") {
		res.fail(t, "", "upgrade unchanged")
	}
	if strings.Contains(res.log, "daemon-reload") {
		res.fail(t, "", "reload without a change")
	}

	// A hand edit is reported by check and repaired by upgrade.
	if err := os.WriteFile(drop, []byte("[Service]\nReadWritePaths=-/etc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = runBundle(t, bundle, rg, "", "check", "--runner")
	if res.code != 0 || !strings.Contains(res.stdout, "host-files.conf: differs") {
		res.fail(t, "", "check differs")
	}
	res = runBundle(t, bundle, rg, "", "upgrade")
	if got, _ := os.ReadFile(drop); res.code != 0 || string(got) != hostFilesWant {
		res.fail(t, "", "upgrade did not repair the drop-in")
	}

	// Keys removed: the drop-in goes away.
	setPolicyCheck(t, rg, hostFilesNone)
	res = runBundle(t, bundle, rg, "", "check", "--runner")
	if res.code != 0 || !strings.Contains(res.stdout, "host-files.conf: present but not needed") {
		res.fail(t, "", "check stale")
	}
	rg.truncateLog(t)
	res = runBundle(t, bundle, rg, "", "upgrade")
	if res.code != 0 || !strings.Contains(res.stdout, "deleted: /etc/systemd/system/bot-agent-runner@.service.d/host-files.conf") {
		res.fail(t, "", "upgrade did not delete")
	}
	assertGone(t, drop)
	assertGone(t, filepath.Dir(drop))
	if !strings.Contains(res.log, "systemctl daemon-reload") {
		res.fail(t, "", "delete without reload")
	}
	res = runBundle(t, bundle, rg, "", "check", "--runner")
	if res.code != 0 || !strings.Contains(res.stdout, "host files: 0\n") {
		res.fail(t, "", "check none")
	}
}

func TestInstallerHostFilesDropinNoneNotWritten(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	assertGone(t, filepath.Join(rg.root, hostFilesDropin))
	assertGone(t, filepath.Dir(filepath.Join(rg.root, hostFilesDropin)))
}

func TestInstallerHostFilesDropinDryRunAndRemove(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	setPolicyCheck(t, rg, hostFilesCheck(t))
	res := runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", writeOKConfig(t), "--dry-run")
	if res.code != 0 || !strings.Contains(res.stdout, "dry-run: write /etc/systemd/system/bot-agent-runner@.service.d/host-files.conf mode 0644 owner root:root") ||
		!strings.Contains(res.stdout, "dry-run: ReadWritePaths=-/etc/dd-worker") {
		res.fail(t, "", "dry-run plan")
	}
	assertGone(t, filepath.Join(rg.root, hostFilesDropin))
	res = runBundle(t, bundle, rg, "", "install", "--runner", "--runner-config", writeOKConfig(t))
	if res.code != 0 {
		res.fail(t, "", "install")
	}
	assertExists(t, filepath.Join(rg.root, hostFilesDropin))
	res = runBundle(t, bundle, rg, "", "remove")
	if res.code != 0 {
		res.fail(t, "", "remove")
	}
	assertGone(t, filepath.Join(rg.root, hostFilesDropin))
	assertGone(t, filepath.Dir(filepath.Join(rg.root, hostFilesDropin)))
}

func TestInstallerHostFilesDropinRefusesSymlink(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	mustInstall(t, bundle, rg, writeOKConfig(t))
	dir := filepath.Dir(filepath.Join(rg.root, hostFilesDropin))
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	setPolicyCheck(t, rg, hostFilesCheck(t))
	res := runBundle(t, bundle, rg, "", "upgrade")
	if res.code == 0 || !strings.Contains(res.combined(), "refusing symlink") {
		res.fail(t, "", "symlinked drop-in dir accepted")
	}
}

// C6: the live-check helper builds only with its tag, and no bundle or
// build script names it.
func TestHostfileTesthelperNotShipped(t *testing.T) {
	main, err := os.ReadFile(filepath.Join("..", "cmd", "hostfile-testhelper", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(main), "//go:build hostfile_testhelper\n") {
		t.Fatal("cmd/hostfile-testhelper/main.go lost its build tag")
	}
	self := filepath.Join("..", "packaging", "hostfiles_test.go")
	err = filepath.WalkDir("..", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "hostfile-testhelper", "docs", ".claude", ".agents", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || p == self || strings.HasSuffix(p, ".md") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte("hostfile-testhelper")) || bytes.Contains(b, []byte("hostfile_testhelper")) {
			t.Errorf("%s names the live-check helper", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
