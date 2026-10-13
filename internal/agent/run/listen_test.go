package run

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoListenSocket fails if non-test agent code opens a listening socket.
// The fake server is the only listener, and it is test-only.
func TestNoListenSocket(t *testing.T) {
	mod := moduleRoot(t)
	roots := []string{
		filepath.Join(mod, "cmd", "bot-agent"),
		filepath.Join(mod, "internal", "agent"),
	}
	banned := []*regexp.Regexp{
		regexp.MustCompile(`\bnet\.Listen\b`),
		regexp.MustCompile(`\btls\.Listen\b`),
		regexp.MustCompile(`\bListenAndServe`),
		regexp.MustCompile(`\bnet\.ListenPacket\b`),
		regexp.MustCompile(`\bnet\.ListenUDP\b`),
		regexp.MustCompile(`\bnet\.ListenTCP\b`),
		regexp.MustCompile(`\.Listen\(`),
	}
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if strings.Contains(path, filepath.Join("internal", "agent", "fakeserver")) {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(body), "\n") {
				trim := strings.TrimSpace(line)
				if strings.HasPrefix(trim, "//") {
					continue
				}
				for _, re := range banned {
					if re.MatchString(line) {
						t.Errorf("%s:%d matches %s", path, i+1, re.String())
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir := callerDir()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod")
		}
		dir = parent
	}
}
