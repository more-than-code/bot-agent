package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/runner"
)

// upgrade-run is root only and takes exactly one checked version.
func TestUpgradeRunArgs(t *testing.T) {
	var out bytes.Buffer
	if code := run(1000, []string{"upgrade-run", "0.2.16"}, strings.NewReader(""), &out, runner.Sys{}, nil); code != 0 || !strings.Contains(out.String(), `"policy_denied"`) {
		t.Fatalf("non-root: %d %s", code, out.String())
	}
	for _, v := range []string{"", "latest", "0.2.16;id", "../0.2.16"} {
		out.Reset()
		sys := runner.Sys{Root: t.TempDir()}
		if code := run(0, []string{"upgrade-run", v}, strings.NewReader(""), &out, sys, nil); code != 2 {
			t.Fatalf("%q: exit %d %s", v, code, out.String())
		}
	}
	out.Reset()
	if code := run(0, []string{"upgrade-run"}, strings.NewReader(""), &out, runner.Sys{}, nil); code != 0 || !strings.Contains(out.String(), `"invalid_verb"`) {
		t.Fatalf("no version: %d %s", code, out.String())
	}
}
