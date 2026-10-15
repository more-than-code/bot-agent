package programs

import (
	"os"
	"strings"
	"testing"
)

func TestAgentSkipsFilesystem(t *testing.T) {
	pol := joePolicy("/home/joe/workspace", false, []string{"/usr/bin/pnpm"}, 20)
	missing := "/home/joe/workspace/does-not-exist-fix1"
	_, code := Check("program_start", spec(t, startSpec{Dir: missing, Exec: "/usr/bin/pnpm"}), Gate{Policy: pol})
	if code != "" {
		t.Fatalf("agent stat %s", code)
	}
	root := t.TempDir()
	app := root + "/workspace/app"
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	bash := joePolicy(root+"/workspace", false, []string{"/bin/bash"}, 20)
	_, code = Check("program_start", spec(t, startSpec{Dir: app, Exec: "/bin/bash"}), Gate{Policy: bash})
	if code != "" {
		t.Fatalf("agent shell basename %s", code)
	}
	_, code = Check("program_start", spec(t, startSpec{Dir: app, Exec: "/bin/bash"}), Gate{Policy: bash, Files: true})
	if code != ReasonPolicy {
		t.Fatalf("helper shell basename %s", code)
	}
}

func TestFrameCapBeforeFieldRules(t *testing.T) {
	pol := joePolicy("/home/joe/workspace", false, []string{"/usr/bin/pnpm"}, 20)
	_, code := Check("program_start", []byte(strings.Repeat("a", MaxFrame+1)), Gate{Policy: pol, Files: true})
	if code != ReasonBadSpec {
		t.Fatalf("oversize %s", code)
	}
	args := make([]string, 32)
	for i := range args {
		args[i] = strings.Repeat("a", 4096)
	}
	raw := spec(t, startSpec{Dir: "/home/joe/workspace/app", Exec: "/usr/bin/pnpm", Args: args})
	if len(raw) <= MaxFrame {
		t.Fatalf("fixture len %d", len(raw))
	}
	_, code = Check("program_start", raw, Gate{Policy: pol, Files: true})
	if code != ReasonBadSpec {
		t.Fatalf("32x4096 %s", code)
	}
	_, code = Check("program_start", spec(t, startSpec{
		Dir: "/home/joe/workspace/app", Exec: "/usr/bin/pnpm", Args: strings.Split(strings.Repeat("a ", 32), " ")[:32],
	}), Gate{Policy: pol})
	if code != "" {
		t.Fatalf("32 short args %s", code)
	}
	_, code = Check("program_start", spec(t, startSpec{
		Dir: "/home/joe/workspace/app", Exec: "/usr/bin/pnpm", Args: strings.Split(strings.Repeat("a ", 33), " ")[:33],
	}), Gate{Policy: pol})
	if code != ReasonBadSpec {
		t.Fatalf("33 args %s", code)
	}
}

func TestEnvDenylistReasons(t *testing.T) {
	pol := joePolicy("/home/joe/workspace", false, []string{"/usr/bin/pnpm"}, 20)
	keys := []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "NODE_OPTIONS", "NODE_PATH", "BASH_ENV", "ENV", "IFS", "PYTHONPATH", "PYTHONSTARTUP", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB"}
	for _, key := range keys {
		_, code := Check("program_start", spec(t, startSpec{
			Dir: "/home/joe/workspace/app", Exec: "/usr/bin/pnpm", Env: map[string]string{key: "x"},
		}), Gate{Policy: pol})
		if code != ReasonBadSpec {
			t.Fatalf("agent %s %s", key, code)
		}
		_, code = Check("program_start", spec(t, startSpec{
			Dir: "/home/joe/workspace/app", Exec: "/usr/bin/pnpm", Env: map[string]string{key: "x"},
		}), Gate{Policy: pol, Files: true})
		if code != ReasonPolicy {
			t.Fatalf("helper %s %s", key, code)
		}
	}
	_, code := Check("program_start", spec(t, startSpec{
		Dir: "/home/joe/workspace/app", Exec: "/usr/bin/pnpm", Args: []string{"NODE_OPTIONS=--inspect"},
	}), Gate{Policy: pol})
	if code != ReasonBadSpec {
		t.Fatalf("agent hoisted %s", code)
	}
	_, code = Check("program_start", spec(t, startSpec{
		Dir: "/home/joe/workspace/app", Exec: "/usr/bin/pnpm", Args: []string{"NODE_OPTIONS=--inspect"},
	}), Gate{Policy: pol, Files: true})
	if code != ReasonPolicy {
		t.Fatalf("helper hoisted %s", code)
	}
}

func TestLinesBounds(t *testing.T) {
	pol := joePolicy("/home/joe/workspace", false, []string{"/usr/bin/pnpm"}, 20)
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{`{"project":"tour-agent","entry":"web","lines":0}`, ReasonBadSpec},
		{`{"project":"tour-agent","entry":"web","lines":1}`, ""},
		{`{"project":"tour-agent","entry":"web","lines":1000}`, ""},
		{`{"project":"tour-agent","entry":"web","lines":1001}`, ReasonBadSpec},
		{`{"project":"tour-agent","entry":"web","lines":1.5}`, ReasonBadSpec},
	} {
		_, code := Check("program_logs", []byte(tc.raw), Gate{Policy: pol, Files: true})
		if code != tc.want {
			t.Fatalf("%s got %s", tc.raw, code)
		}
	}
}
