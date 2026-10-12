// Command render-telegraf writes telegraf/<host>.conf from service manifests.
// It does not install Telegraf or talk to a node.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/more-than-code/bot-agent/internal/render"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if slices.Contains(argv, "--check") {
		problems, err := render.Check(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if len(problems) > 0 {
			fmt.Fprintln(os.Stderr, strings.Join(problems, "\n"))
			return 1
		}
		fmt.Println("telegraf snippets match the manifests")
		return 0
	}
	n, err := render.WriteAll(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("wrote %d files in %s\n", n, filepath.Join(root, "telegraf"))
	return 0
}

// repoRoot is the module directory that holds nodes.json. go run records
// the source path, which is the same anchor render_telegraf.py used (__file__).
// A trimmed or moved binary falls back to the working directory.
func repoRoot() (string, error) {
	if root, ok := rootFromCaller(); ok {
		return root, nil
	}
	if root, ok := rootFromCWD(); ok {
		return root, nil
	}
	return "", errors.New("nodes.json: not found")
}

func rootFromCaller() (string, bool) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	return cleanRoot(filepath.Join(filepath.Dir(file), "..", ".."))
}

func rootFromCWD() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if root, ok := cleanRoot(dir); ok {
			return root, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func cleanRoot(dir string) (string, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	nodes, err := os.Stat(filepath.Join(abs, "nodes.json"))
	if err != nil || nodes.IsDir() {
		return "", false
	}
	services, err := os.Stat(filepath.Join(abs, "services"))
	if err != nil || !services.IsDir() {
		return "", false
	}
	return abs, true
}
