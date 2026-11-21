package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

const (
	systemctlBin  = "/usr/bin/systemctl"
	journalctlBin = "/usr/bin/journalctl"
	systemdRunBin = "/usr/bin/systemd-run"
)

const maxCmdOut = 1 << 20

// RealSys is the production side effect set. Tests do not use it.
func RealSys() Sys {
	return Sys{
		Systemctl:  command(systemctlBin),
		Journalctl: command(journalctlBin),
		RunUnit:    runUnitAt(systemdRunBin),
		Proc:       "/proc",
		Sysfs:      "/sys",
		Passwd:     policy.DefaultPasswd,
		Group:      policy.DefaultGroup,
		Policy:     policy.DefaultPath,
		Logf:       log.Printf,
	}
}

// command runs bin with args as an argument vector. LANG=C. Stderr is discarded.
// Stdout is returned even when the process exits non-zero: systemctl is-active
// prints the state and then exits 3.
func command(bin string) func(context.Context, []string) (string, error) {
	return func(ctx context.Context, args []string) (string, error) {
		// A relative name would search PATH. /usr/local/bin is not trusted here.
		if !filepath.IsAbs(bin) {
			return "", errors.New("bin")
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = []string{"LANG=C", "PATH=/usr/bin:/bin"}
		cmd.Stdin = nil
		var stdout bytes.Buffer
		cmd.Stdout = &capWriter{buf: &stdout, max: maxCmdOut}
		cmd.Stderr = io.Discard
		err := cmd.Run()
		return stdout.String(), err
	}
}

type capWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	room := w.max - w.buf.Len()
	if room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		if _, err := w.buf.Write(p); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// runUnitAt runs bin (production: /usr/bin/systemd-run) with args
// (agent-protocol v1.18 host files). stdin is handed over as a file
// descriptor, which --pipe passes on to the transient unit; nil is
// /dev/null. stdout is discarded. At most MaxStderr+1 bytes of stderr are
// kept, so the caller can tell an oversized stderr from one line. The
// environment is fixed. WaitDelay stops a child that keeps the stderr pipe
// open from holding Wait after a kill. A kill, a signal or the context
// ending is -1 with an error.
func runUnitAt(bin string) func(context.Context, []string, *os.File) (int, []byte, error) {
	return func(ctx context.Context, args []string, stdin *os.File) (int, []byte, error) {
		if !filepath.IsAbs(bin) {
			return -1, nil, errors.New("bin")
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = []string{"LANG=C", "PATH=/usr/bin:/bin"}
		if stdin != nil {
			cmd.Stdin = stdin
		}
		cmd.Stdout = io.Discard
		var stderr bytes.Buffer
		cmd.Stderr = &capWriter{buf: &stderr, max: hostfiles.MaxStderr + 1}
		cmd.WaitDelay = 2 * time.Second
		err := cmd.Run()
		if err == nil {
			return 0, stderr.Bytes(), nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil && ee.ExitCode() >= 0 {
			return ee.ExitCode(), stderr.Bytes(), nil
		}
		return -1, stderr.Bytes(), err
	}
}
