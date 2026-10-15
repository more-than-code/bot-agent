package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/runner"
)

func TestRunRefusesNonRoot(t *testing.T) {
	sys, calls := enabledSys(t)
	var out bytes.Buffer
	code := run(1000, []string{"list"}, strings.NewReader(`{}`), &out, sys, nil)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), `"reason":"policy_denied"`) || strings.Contains(out.String(), "root") {
		t.Fatalf("%s", out.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("non-root ran systemctl %v", *calls)
	}
	var root bytes.Buffer
	code = run(0, []string{"list"}, strings.NewReader(`{}`), &root, sys, nil)
	if code != 0 || !strings.Contains(root.String(), `"ok":true`) {
		t.Fatalf("root list exit %d %s", code, root.String())
	}
}

func TestRunRejectsUnknownVerb(t *testing.T) {
	var out bytes.Buffer
	code := run(0, []string{"shell"}, strings.NewReader(`{}`), &out, runner.Sys{}, nil)
	if code != 0 || !strings.Contains(out.String(), programs.ReasonVerb) {
		t.Fatalf("exit %d %s", code, out.String())
	}
}

func TestRunRejectsOversizeFrame(t *testing.T) {
	sys, calls := enabledSys(t)
	body := bytes.Repeat([]byte("a"), programs.MaxFrame+1)
	var out bytes.Buffer
	code := run(0, []string{"start"}, bytes.NewReader(body), &out, sys, nil)
	if code != 0 || !strings.Contains(out.String(), programs.ReasonBadSpec) {
		t.Fatalf("exit %d %s", code, out.String())
	}
	if len(*calls) != 0 {
		t.Fatal("oversize frame called systemctl")
	}
}

func TestServeConnRefusesForeignPeer(t *testing.T) {
	sys, calls := enabledSys(t)
	frame := `{"verb":"start","request":{"project":"demo","entry":"web","dir":"/tmp/x","exec":"/usr/bin/pnpm","args":[],"shell":false,"run_as":"joe","env":{},"restart":"no"}}` + "\n"
	var out bytes.Buffer
	code := run(0, []string{"serve-conn"}, strings.NewReader(frame), &out, sys, func() (int, bool) {
		return 1, true
	})
	if code != 0 || !strings.Contains(out.String(), `"reason":"policy_denied"`) {
		t.Fatalf("exit %d %s", code, out.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("foreign peer ran %v", *calls)
	}
	if _, err := os.Stat(filepath.Join(sys.Root, "run", "systemd", "system", "deploybot-demo-web.service")); !os.IsNotExist(err) {
		t.Fatal("foreign peer wrote a unit")
	}
}

func TestServeConnOneRequestAndOversize(t *testing.T) {
	sys, calls := enabledSys(t)
	peer := func() (int, bool) { return 900, true }
	var out bytes.Buffer
	in := "{\"verb\":\"list\",\"request\":{}}\n{\"verb\":\"start\",\"request\":{}}\n"
	code := run(0, []string{"serve-conn"}, strings.NewReader(in), &out, sys, peer)
	if code != 0 {
		t.Fatalf("exit %d %s", code, out.String())
	}
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), `"ok":true`) {
		t.Fatalf("responses %q", out.String())
	}
	if strings.Contains(out.String(), "start") {
		t.Fatal(out.String())
	}
	*calls = nil
	big := bytes.Repeat([]byte("b"), programs.MaxFrame+1)
	var out2 bytes.Buffer
	code = run(0, []string{"serve-conn"}, bytes.NewReader(append(big, '\n')), &out2, sys, peer)
	if code != 0 || !strings.Contains(out2.String(), programs.ReasonBadSpec) {
		t.Fatalf("exit %d %s", code, out2.String())
	}
	if len(*calls) != 0 {
		t.Fatal("oversize connection called systemctl")
	}
}

func TestServeConnCancelStopsSystemctl(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	sys, _ := enabledSys(t)
	sys.Policy = writeCancelPolicy(t, dir, app)
	sys.Root = filepath.Join(dir, "root")
	started := make(chan struct{})
	var reloads int
	sys.Systemctl = func(ctx context.Context, args []string) (string, error) {
		if len(args) > 0 && args[0] == "daemon-reload" {
			reloads++
			if reloads == 1 {
				close(started)
				<-ctx.Done()
				return "", ctx.Err()
			}
		}
		return "", nil
	}
	tmp, err := os.CreateTemp("/tmp", "ba-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	sock := tmp.Name()
	tmp.Close()
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer os.Remove(sock)
	done := make(chan int, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- 2
			return
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			done <- 2
			return
		}
		f, err := uc.File()
		if err != nil {
			done <- 2
			return
		}
		c.Close()
		defer f.Close()
		done <- run(0, []string{"serve-conn"}, f, &bytes.Buffer{}, sys, func() (int, bool) {
			return 900, true
		})
	}()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	frame := `{"verb":"start","request":{"project":"demo","entry":"web","dir":"` + app + `","exec":"/usr/bin/pnpm","args":[],"shell":false,"run_as":"joe","env":{},"restart":"no"}}` + "\n"
	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon-reload was not reached")
	}
	conn.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closed client did not cancel the helper")
	}
	if _, err := os.Stat(filepath.Join(sys.Root, "run", "systemd", "system", "deploybot-demo-web.service")); !os.IsNotExist(err) {
		t.Fatal("cancelled start left the unit file")
	}
}

func writeCancelPolicy(t *testing.T, dir, app string) string {
	t.Helper()
	path := filepath.Join(dir, "runner.toml")
	body := "enabled = true\nrun_as = [\"joe\"]\ndirs = [\"" + filepath.Dir(app) + "\"]\nexec_allow = [\"/usr/bin/pnpm\"]\nallow_shell = false\nmax_programs = 20\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func enabledSys(t *testing.T) (runner.Sys, *[][]string) {
	t.Helper()
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/bash\nbot-agent:x:900:900:bot:/var/lib/bot-agent:/usr/sbin/nologin\njoe:x:1000:1000:Joe:/home/joe:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("bot-agent:x:900:\njoe:x:1000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(dir, "runner.toml")
	body := "enabled = true\nrun_as = [\"joe\"]\ndirs = [\"/home/joe/workspace\"]\nexec_allow = [\"/usr/bin/pnpm\"]\nallow_shell = false\nmax_programs = 20\n"
	if err := os.WriteFile(policyPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	sys := runner.Sys{
		Policy: policyPath,
		Passwd: passwd,
		Group:  group,
		Root:   filepath.Join(dir, "root"),
		UID:    func(os.FileInfo) (uint32, bool) { return 0, true },
		Systemctl: func(context.Context, []string) (string, error) {
			calls = append(calls, []string{"called"})
			return "inactive\n", nil
		},
	}
	return sys, &calls
}
