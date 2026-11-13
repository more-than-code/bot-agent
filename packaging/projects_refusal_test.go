package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The v1.12 installer refusal (agent-protocol v1.12 item 7).
const projectsRefusal = `runner.toml allows programs but lists no projects; add projects = ["<slug>"] (from v0.2.8 programs run only for listed projects)`

const programsOn = "enabled = true\nrun_as = [\"joe\"]\ndirs = [\"/home/joe/workspace\"]\nexec_allow = [\"/usr/bin/pnpm\"]\nmax_programs = 20\n"

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertRefused(t *testing.T, res cmdResult, label string) {
	t.Helper()
	if res.code != 1 || !strings.Contains(res.stderr, projectsRefusal) || !strings.Contains(res.stdout, "nothing changed") {
		res.fail(t, "", label+": not refused")
	}
}

func TestInstallerRefusesProgramsWithoutProjects(t *testing.T) {
	t.Parallel()
	bundle := makeBundle(t, "0.2.0")
	cfg := writeCfg(t, programsOn)
	for _, args := range [][]string{
		{"check", "--runner", "--runner-config", cfg},
		{"install", "--runner", "--runner-config", cfg},
		{"install", "--dry-run", "--runner", "--runner-config", cfg},
	} {
		rg := newRig(t)
		before := treeSig(t, rg.root)
		assertRefused(t, runBundle(t, bundle, rg, "", args...), strings.Join(args, " "))
		if treeSig(t, rg.root) != before {
			t.Fatalf("%v changed the root", args)
		}
	}
}

// Programs are not on, or projects are listed: no refusal.
func TestInstallerProjectsRefusalOnlyWhenProgramsOn(t *testing.T) {
	t.Parallel()
	bundle := makeBundle(t, "0.2.0")
	for name, body := range map[string]string{
		"disabled":       strings.Replace(programsOn, "enabled = true", "enabled = false", 1),
		"no run_as":      strings.Replace(programsOn, `run_as = ["joe"]`, "run_as = [ ]", 1),
		"max zero":       strings.Replace(programsOn, "max_programs = 20", "max_programs = 0", 1),
		"no max":         strings.Replace(programsOn, "max_programs = 20\n", "", 1),
		"projects":       programsOn + "projects = [\"home-lab\"]\n",
		"images, listed": "enabled = true\nimages = [\"ghcr.io/acme/*\"]\nprojects = [\"acme\"]\n",
	} {
		rg := newRig(t)
		res := runBundle(t, bundle, rg, "", "check", "--runner", "--runner-config", writeCfg(t, body))
		if res.code != 0 || strings.Contains(res.combined(), "lists no projects") {
			res.fail(t, "", name)
		}
	}
}

// Without the runner the refusal does not apply: programs need the helper.
func TestInstallerProjectsRefusalNeedsRunner(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	policy := filepath.Join(rg.root, "etc", "bot-agent", "runner.toml")
	if err := os.MkdirAll(filepath.Dir(policy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, []byte(programsOn), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := runBundle(t, bundle, rg, "", "check"); res.code != 0 {
		res.fail(t, "", "check without runner")
	}
	assertRefused(t, runBundle(t, bundle, rg, "", "check", "--runner"), "check --runner")
}

// Acceptance criterion 5: upgrade --runner exits 1 and changes nothing;
// after the projects line is added, it upgrades.
func TestInstallerUpgradeRefusesProgramsWithoutProjects(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	policy := filepath.Join(rg.root, "etc", "bot-agent", "runner.toml")
	if err := os.WriteFile(policy, []byte(programsOn), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeSig(t, rg.root)
	rg.truncateLog(t)
	res := runBundle(t, next, rg, "", "upgrade", "--runner")
	assertRefused(t, res, "upgrade --runner")
	if treeSig(t, rg.root) != before {
		t.Fatal("refused upgrade changed the root")
	}
	assertNoSystemctlChange(t, res, "refused upgrade")
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.0" {
		t.Fatal("binary replaced")
	}
	if err := os.WriteFile(policy, []byte(programsOn+"projects = [\"home-lab\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = runBundle(t, next, rg, "", "upgrade", "--runner")
	if res.code != 0 {
		res.fail(t, "", "upgrade after adding projects")
	}
	if binVersion(t, filepath.Join(rg.root, "usr/local/bin/bot-agent")) != "0.2.1" {
		t.Fatal("version after upgrade")
	}
}

// C7f: with the helper already installed, a plain `upgrade` (no --runner)
// is refused the same way, and so is an explicit empty projects list.
func TestInstallerUpgradeRefusesWithInstalledHelper(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	old := makeBundle(t, "0.2.0")
	next := makeBundle(t, "0.2.1")
	mustInstall(t, old, rg, writeOKConfig(t))
	plantEnv(t, rg)
	policy := filepath.Join(rg.root, "etc", "bot-agent", "runner.toml")
	for _, body := range []string{programsOn, programsOn + "projects = []\n"} {
		if err := os.WriteFile(policy, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		before := treeSig(t, rg.root)
		rg.truncateLog(t)
		res := runBundle(t, next, rg, "", "upgrade")
		assertRefused(t, res, "upgrade with installed helper")
		assertNoSystemctlChange(t, res, "refused upgrade")
		if treeSig(t, rg.root) != before {
			t.Fatal("refused upgrade changed the root")
		}
	}
}

func TestInstallerRefusesExplicitEmptyProjects(t *testing.T) {
	t.Parallel()
	rg := newRig(t)
	bundle := makeBundle(t, "0.2.0")
	cfg := writeCfg(t, programsOn+"projects = []\n")
	assertRefused(t, runBundle(t, bundle, rg, "", "check", "--runner", "--runner-config", cfg), "projects = []")
}
