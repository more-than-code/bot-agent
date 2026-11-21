//go:build hostfile_testhelper

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runCheck(t *testing.T, input string, extra ...string) (int, string, []string) {
	t.Helper()
	var stderr bytes.Buffer
	var reports []string
	args := append([]string{"policy", "check", "--file", "-"}, extra...)
	code := run(args, strings.NewReader(input), &stderr, func(s string) { reports = append(reports, s) })
	return code, stderr.String(), reports
}

func TestCheckContract(t *testing.T) {
	for in, want := range map[string]struct {
		code   int
		stderr string
	}{
		"a = 1\n":                            {0, ""},
		"testhelper_check = \"bad_repo\"\n":  {2, "bad_repo\n"},
		"testhelper_check = \"exit5\"\n":     {5, ""},
		"testhelper_check = \"two_lines\"\n": {2, "bad_one\nbad_two\n"},
	} {
		code, stderr, reports := runCheck(t, in)
		if code != want.code || stderr != want.stderr || len(reports) != 0 {
			t.Fatalf("%q: %d %q %v", in, code, stderr, reports)
		}
	}
	code, stderr, _ := runCheck(t, "testhelper_check = \"noise\"\n")
	if code != 2 || len(stderr) < 2048 {
		t.Fatalf("noise %d %d", code, len(stderr))
	}
}

func TestCheckReports(t *testing.T) {
	in := strings.Repeat("k = \"v\"\n", 8192) // 64 KiB
	sum := sha256.Sum256([]byte(in))
	h := hex.EncodeToString(sum[:])
	code, stderr, reports := runCheck(t, in, "--report")
	if code != 0 || stderr != "" || len(reports) != 1 || reports[0] != "stdin sha256="+h+" bytes="+strconv.Itoa(len(in)) {
		t.Fatalf("%d %q %v", code, stderr, reports)
	}
	code, stderr, _ = runCheck(t, in, "--report-reason")
	want := "bad_n65536_" + h[:52] + "\n"
	if code != 2 || stderr != want || len(strings.TrimSpace(stderr)) > 68 {
		t.Fatalf("%d %q", code, stderr)
	}
	if strings.Contains(stderr, "k = ") {
		t.Fatal("content printed")
	}
}

func TestHealth(t *testing.T) {
	dir := t.TempDir()
	status := filepath.Join(dir, "status")
	noSleep := func(time.Duration) {}
	h := func(args ...string) (int, string) {
		var stderr bytes.Buffer
		return health(append(args, "--status", status), &stderr, noSleep), stderr.String()
	}
	if code, out := h("--wait", "3s"); code != 3 || out != "not_running\n" {
		t.Fatalf("missing: %d %q", code, out)
	}
	if err := os.WriteFile(status, []byte("health=not_connected\nidle=busy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := h("--wait", "3s"); code != 3 || out != "not_connected\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, _ := h("--idle"); code != 4 {
		t.Fatalf("busy %d", code)
	}
	if err := os.WriteFile(status, []byte("health=ok\nidle=idle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := h("--wait", "3s"); code != 0 {
		t.Fatalf("ok %d", code)
	}
	if code, _ := h("--idle"); code != 0 {
		t.Fatalf("idle %d", code)
	}
	if code, _ := h(); code != 1 {
		t.Fatalf("usage %d", code)
	}
}

// C6: the probe reports uid, write access and network.
func TestProbe(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	in := "testhelper_check = \"probe\"\ntesthelper_probe_dir = \"" + dir + "\"\ntesthelper_probe_addr = \"" + ln.Addr().String() + "\"\n"
	code, stderr, _ := runCheck(t, in)
	want := fmt.Sprintf("bad_u%d_w1_n1\n", os.Getuid())
	if code != 2 || stderr != want {
		t.Fatalf("%d %q want %q", code, stderr, want)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatal("probe file left")
	}
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	in = "testhelper_check = \"probe\"\ntesthelper_probe_dir = \"" + ro + "\"\ntesthelper_probe_addr = \"" + addr + "\"\n"
	code, stderr, _ = runCheck(t, in)
	if os.Geteuid() != 0 && (code != 2 || stderr != fmt.Sprintf("bad_u%d_w0_n0\n", os.Getuid())) {
		t.Fatalf("%d %q", code, stderr)
	}
}
