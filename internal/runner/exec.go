package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os/exec"
	"path/filepath"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

const (
	systemctlBin  = "/usr/bin/systemctl"
	journalctlBin = "/usr/bin/journalctl"
)

const maxCmdOut = 1 << 20

// RealSys is the production side effect set. Tests do not use it.
func RealSys() Sys {
	return Sys{
		Systemctl:  command(systemctlBin),
		Journalctl: command(journalctlBin),
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
