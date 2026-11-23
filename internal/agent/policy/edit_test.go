package policy

import (
	"strings"
	"testing"
)

// Rewrite replaces only the three managed lines, keeps comments, order and
// every other key, and appends a key the file lacks.
func TestRewriteKeepsOtherLines(t *testing.T) {
	in := "# host policy\nenabled = true\nimages = [\"ghcr.io/a/*\"]\n# projects below\nprojects = [\"acme\"]\nconsole_policy = true\nmax_containers = 5\n"
	out := string(Rewrite([]byte(in), Managed{Edge: true, Images: []string{"ghcr.io/a/*", "ghcr.io/b/web"}, Projects: []string{"acme", "beta"}}))
	want := "# host policy\nenabled = true\nimages = [\"ghcr.io/a/*\", \"ghcr.io/b/web\"]\n# projects below\nprojects = [\"acme\", \"beta\"]\nconsole_policy = true\nmax_containers = 5\nedge = true\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	p := FromBytes([]byte(out), "/nonexistent", "/nonexistent", nil)
	if !p.Valid || !p.ConsoleManaged() || !p.Edge || len(p.Images) != 2 || len(p.Projects) != 2 {
		t.Fatalf("%+v", p)
	}
}

func TestRewriteEmptyListsAndNoTrailingNewline(t *testing.T) {
	out := string(Rewrite([]byte("console_policy = true\nedge = true"), Managed{Images: []string{}, Projects: []string{}}))
	if out != "console_policy = true\nedge = false\nimages = []\nprojects = []\n" {
		t.Fatalf("%q", out)
	}
}

// A commented-out key is not a key: it stays and the real one is appended.
func TestRewriteIgnoresComments(t *testing.T) {
	out := string(Rewrite([]byte("# edge = true\nconsole_policy = true\n"), Managed{Edge: true, Images: []string{}, Projects: []string{}}))
	if !strings.HasPrefix(out, "# edge = true\n") || !strings.Contains(out, "\nedge = true\n") {
		t.Fatalf("%q", out)
	}
}

func TestConsolePolicyKey(t *testing.T) {
	for in, want := range map[string]bool{
		"console_policy = true\n":                true,
		"console_policy = false\n":               false,
		"":                                       false,
		"console_policy = true\nbogus = 1\n":     false,
		"console_policy = true\nimages = [\"\"]": false,
	} {
		p := FromBytes([]byte(in), "/nonexistent", "/nonexistent", nil)
		if p.ConsoleManaged() != want {
			t.Fatalf("%q: %+v", in, p)
		}
	}
	if p := FromBytes([]byte("console_policy = yes\n"), "", "", nil); p.Valid || p.Reason != "parse" {
		t.Fatalf("%+v", p)
	}
}
