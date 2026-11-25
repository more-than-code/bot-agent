//go:build hostfile_testhelper

// Command hostfile-testhelper is a stand-in for a host-file worker (such as
// dd-worker) in the agent-protocol v1.18 live check (spec
// docs/spec-host-config-files.md §5 item 14). It is a test tool: it is not
// part of a release bundle and Deploybot never runs it on its own. It builds
// only with -tags hostfile_testhelper.
//
//	hostfile-testhelper policy check --file - [--report] [--report-reason]
//	hostfile-testhelper secrets check --file - [--report] [--report-reason]
//	hostfile-testhelper health --wait <dur> [--status FILE]
//	hostfile-testhelper health --idle [--status FILE]
//	hostfile-testhelper serve
//
// check reads stdin and follows the DD contract: exit 0 valid; exit 2 with
// one stderr line bad_<reason>; exit 1 usage or I/O. A line
// `testhelper_check = "<value>"` in the content drives it: a bad_* token
// exits 2 with that line, "exit5" exits 5, "two_lines" prints two lines,
// "noise" prints 2 KiB, "sleep" sleeps 60 s, and "probe" checks the
// sandbox (spec §5 item 9): it tries O_CREAT in testhelper_probe_dir
// (default /etc/hostfile-test) and a TCP connect to testhelper_probe_addr
// (default 1.1.1.1:443, 2 s), then exits 2 with bad_u<uid>_w<0|1>_n<0|1>
// (w1: the create worked, n1: the connect worked). --report writes
// "stdin sha256=<hex> bytes=<n>" to syslog (journalctl -t
// hostfile-testhelper); --report-reason instead exits 2 with
// bad_n<bytes>_<first 52 hex of the sha256>, so the hash shows up in the
// apply result. Content bytes are never printed.
//
// health reads the status file (default /var/lib/hostfile-test/status):
// lines health=<ok|not_connected|policy_invalid|not_running> and
// idle=<idle|busy>. --wait polls once a second until health=ok (exit 0)
// or the wait ends (exit 3 with the health value as one stderr line; a
// missing file is not_running). --idle exits 4 when idle=busy, else 0.
//
// serve sleeps until it is stopped: the stand-in unit's ExecStart.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/syslog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultStatus = "/var/lib/hostfile-test/status"
	maxInput      = 1 << 20
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stderr, journal))
}

// journal writes one line to syslog. A failure is ignored: the report is
// a diagnostic, and the check result must not depend on it.
func journal(line string) {
	w, err := syslog.New(syslog.LOG_INFO|syslog.LOG_USER, "hostfile-testhelper")
	if err != nil {
		return
	}
	defer w.Close()
	_ = w.Info(line)
}

func run(args []string, stdin io.Reader, stderr io.Writer, report func(string)) int {
	if len(args) == 0 {
		return usage(stderr)
	}
	switch args[0] {
	case "policy", "secrets":
		return check(args[1:], stdin, stderr, report)
	case "health":
		return health(args[1:], stderr, time.Sleep)
	case "serve":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		return 0
	default:
		return usage(stderr)
	}
}

func usage(stderr io.Writer) int {
	fmt.Fprintln(stderr, "usage")
	return 1
}

func check(args []string, stdin io.Reader, stderr io.Writer, report func(string)) int {
	if len(args) < 3 || args[0] != "check" || args[1] != "--file" || args[2] != "-" {
		return usage(stderr)
	}
	doReport, doReason := false, false
	for _, a := range args[3:] {
		switch a {
		case "--report":
			doReport = true
		case "--report-reason":
			doReason = true
		default:
			return usage(stderr)
		}
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxInput+1))
	if err != nil || len(data) > maxInput {
		fmt.Fprintln(stderr, "io")
		return 1
	}
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	if doReport && report != nil {
		report(fmt.Sprintf("stdin sha256=%s bytes=%d", hexSum, len(data)))
	}
	if doReason {
		fmt.Fprintf(stderr, "bad_n%d_%s\n", len(data), hexSum[:52])
		return 2
	}
	switch v := directive(data, "testhelper_check"); {
	case v == "":
		return 0
	case strings.HasPrefix(v, "bad_"):
		fmt.Fprintln(stderr, v)
		return 2
	case v == "exit5":
		return 5
	case v == "two_lines":
		fmt.Fprintln(stderr, "bad_one")
		fmt.Fprintln(stderr, "bad_two")
		return 2
	case v == "noise":
		fmt.Fprintln(stderr, strings.Repeat("x", 2048))
		return 2
	case v == "sleep":
		time.Sleep(60 * time.Second)
		return 0
	case v == "probe":
		fmt.Fprintln(stderr, probe(directive(data, "testhelper_probe_dir"), directive(data, "testhelper_probe_addr")))
		return 2
	default:
		return 0
	}
}

// probe reports the uid, whether a file can be created in dir and whether
// a TCP connect to addr works. A created file is removed again.
func probe(dir, addr string) string {
	if dir == "" {
		dir = "/etc/hostfile-test"
	}
	if addr == "" {
		addr = "1.1.1.1:443"
	}
	w := 0
	name := filepath.Join(dir, fmt.Sprintf(".probe-%d", os.Getpid()))
	if f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
		w = 1
		f.Close()
		os.Remove(name)
	}
	n := 0
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		n = 1
		c.Close()
	}
	return fmt.Sprintf("bad_u%d_w%d_n%d", os.Getuid(), w, n)
}

// directive is the value of a `<key> = "<value>"` line.
func directive(data []byte, key string) string {
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), maxInput)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}

func health(args []string, stderr io.Writer, sleep func(time.Duration)) int {
	status := defaultStatus
	var wait time.Duration
	idle := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--idle":
			idle = true
		case "--wait":
			if i+1 >= len(args) {
				return usage(stderr)
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d <= 0 {
				return usage(stderr)
			}
			wait = d
			i++
		case "--status":
			if i+1 >= len(args) {
				return usage(stderr)
			}
			status = args[i+1]
			i++
		default:
			return usage(stderr)
		}
	}
	if idle == (wait > 0) {
		return usage(stderr)
	}
	if idle {
		if readStatus(status)["idle"] == "busy" {
			return 4
		}
		return 0
	}
	steps := int(wait / time.Second)
	if steps < 1 {
		steps = 1
	}
	last := "not_running"
	for i := 0; i < steps; i++ {
		h := readStatus(status)["health"]
		if h == "ok" {
			return 0
		}
		if h == "not_connected" || h == "policy_invalid" || h == "not_running" {
			last = h
		}
		sleep(time.Second)
	}
	fmt.Fprintln(stderr, last)
	return 3
}

func readStatus(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
