package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
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
	// A program request just over 64 KiB. The bytes are a valid list body,
	// so a missing size check would return ok. The frame is far under the
	// 2 MiB socket cap.
	prog := paddedObject(programs.MaxFrame + 1)
	var outProg bytes.Buffer
	code = run(0, []string{"serve-conn"}, bytes.NewReader(verbFrame("list", prog)), &outProg, sys, peer)
	if code != 0 || !strings.Contains(outProg.String(), `"reason":"`+programs.ReasonBadSpec+`"`) || strings.Contains(outProg.String(), `"ok":true`) {
		t.Fatalf("program oversize exit %d %s", code, outProg.String())
	}
	if len(*calls) != 0 {
		t.Fatal("oversize program called systemctl")
	}

	// An image request just over 2 MiB. The wrapper fits in the socket slack,
	// so the socket cap is not what refuses it. Images are off: without the
	// size check the answer would be policy_denied.
	img := paddedObject(images.MaxFrame + 1)
	frame := verbFrame("container_list", img)
	if len(frame) >= programs.SocketLimit() || len(img) != images.MaxFrame+1 {
		t.Fatalf("frame %d request %d socket %d", len(frame), len(img), programs.SocketLimit())
	}
	var outImg bytes.Buffer
	code = run(0, []string{"serve-conn"}, bytes.NewReader(frame), &outImg, sys, peer)
	if code != 0 || !strings.Contains(outImg.String(), `"error":"`+programs.ReasonBadSpec+`"`) || strings.Contains(outImg.String(), programs.ReasonPolicy) {
		t.Fatalf("image oversize exit %d %s", code, outImg.String())
	}

	// An image request at 1.5 MiB is inside the image cap. container_list
	// against an empty engine is the acceptance. The full 1.5 MiB deploy is
	// TestDeployAcceptsOneAndAHalfMiB.
	sys.DockerSock = emptyDocker(t)
	if err := os.WriteFile(sys.Policy, []byte("enabled = true\nimages = [\"ghcr.io/example/*\"]\nprojects = [\"mohiguide\"]\nmax_containers = 20\nallow_public_ports = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mid := paddedObject(3 * 1024 * 1024 / 2)
	if len(mid) <= programs.MaxFrame || len(mid) >= images.MaxFrame {
		t.Fatalf("1.5 MiB size %d", len(mid))
	}
	var outMid bytes.Buffer
	code = run(0, []string{"serve-conn"}, bytes.NewReader(verbFrame("container_list", mid)), &outMid, sys, peer)
	if code != 0 || strings.Contains(outMid.String(), `"error"`) || !strings.Contains(outMid.String(), `"items"`) {
		t.Fatalf("1.5 MiB exit %d %s", code, outMid.String())
	}
}

func paddedObject(n int) []byte {
	// Spaces sit inside the object so they are part of the raw JSON value.
	// A decoder still accepts it as an empty object.
	if n < 2 {
		panic("n")
	}
	b := bytes.Repeat([]byte(" "), n)
	b[0] = '{'
	b[n-1] = '}'
	return b
}

func verbFrame(verb string, request []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"verb":"`)
	buf.WriteString(verb)
	buf.WriteString(`","request":`)
	buf.Write(request)
	buf.WriteByte('}')
	buf.WriteByte('\n')
	return buf.Bytes()
}

func emptyDocker(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ba-dock-")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.47/containers/json" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte("[]"))
			return
		}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		os.RemoveAll(dir)
	})
	return sock
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

func TestRefusalStderrLine(t *testing.T) {
	const marker = "REQMARKER9secret"
	peer := func() (int, bool) { return 900, true }

	t.Run("image", func(t *testing.T) {
		sys, calls := enabledSys(t)
		body := []byte(`{"project":"mohiguide","entry":"web","note":"` + marker + `"}`)
		var out bytes.Buffer
		got := withStderr(t, func() {
			code := run(0, []string{"container_deploy"}, bytes.NewReader(body), &out, sys, nil)
			if code != 0 || !strings.Contains(out.String(), `"error":"policy_denied"`) {
				t.Fatalf("exit %d %s", code, out.String())
			}
		})
		if got != "bot-agent-runner: refuse container_deploy policy_denied\n" {
			t.Fatalf("stderr %q", got)
		}
		if strings.Contains(got, marker) || strings.Contains(got, "mohiguide") || strings.Contains(out.String(), "refuse") {
			t.Fatalf("leaked stderr %q stdout %s", got, out.String())
		}
		if len(*calls) != 0 {
			t.Fatal(calls)
		}
	})

	t.Run("serve-conn", func(t *testing.T) {
		sys, calls := enabledSys(t)
		body := []byte(`{"project":"mohiguide","entry":"web","note":"` + marker + `"}`)
		var out bytes.Buffer
		got := withStderr(t, func() {
			code := run(0, []string{"serve-conn"}, bytes.NewReader(verbFrame("container_deploy", body)), &out, sys, peer)
			if code != 0 || !strings.Contains(out.String(), `"error":"policy_denied"`) {
				t.Fatalf("exit %d %s", code, out.String())
			}
		})
		if got != "bot-agent-runner: refuse container_deploy policy_denied\n" {
			t.Fatalf("stderr %q", got)
		}
		if strings.Contains(got, marker) || strings.Contains(got, "mohiguide") {
			t.Fatalf("request on stderr %q", got)
		}
		if len(*calls) != 0 {
			t.Fatal(calls)
		}
	})

	t.Run("program", func(t *testing.T) {
		sys, calls := enabledSys(t)
		body := []byte(`{"note":"` + marker + `"}`)
		var out bytes.Buffer
		got := withStderr(t, func() {
			code := run(0, []string{"start"}, bytes.NewReader(body), &out, sys, nil)
			if code != 0 || !strings.Contains(out.String(), `"reason":"bad_spec"`) {
				t.Fatalf("exit %d %s", code, out.String())
			}
		})
		if got != "bot-agent-runner: refuse start bad_spec\n" {
			t.Fatalf("stderr %q", got)
		}
		if strings.Contains(got, marker) || strings.Contains(out.String(), marker) {
			t.Fatalf("marker stderr %q stdout %s", got, out.String())
		}
		if len(*calls) != 0 {
			t.Fatal(calls)
		}
	})

	t.Run("argv hidden", func(t *testing.T) {
		sys, _ := enabledSys(t)
		var out bytes.Buffer
		got := withStderr(t, func() {
			code := run(1000, []string{"/tmp/" + marker}, strings.NewReader(`{}`), &out, sys, nil)
			if code != 0 || !strings.Contains(out.String(), `"reason":"policy_denied"`) {
				t.Fatalf("exit %d %s", code, out.String())
			}
		})
		if got != "bot-agent-runner: refuse - policy_denied\n" {
			t.Fatalf("stderr %q", got)
		}
		if strings.Contains(got, marker) || strings.Contains(got, "/tmp") {
			t.Fatalf("argv on stderr %q", got)
		}
	})
}

func TestOverlapPolicyLine(t *testing.T) {
	const marker = "REQMARKER9secret"
	peer := func() (int, bool) { return 900, true }
	setProjects := func(t *testing.T, sys runner.Sys, projects string) {
		t.Helper()
		body := "enabled = true\nimages = [\"ghcr.io/acme/*\"]\nprojects = " + projects + "\n"
		if err := os.WriteFile(sys.Policy, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("image", func(t *testing.T) {
		sys, calls := enabledSys(t)
		setProjects(t, sys, `["acme", "acme-web"]`)
		body := []byte(`{"note":"` + marker + `","path":"` + sys.Policy + `"}`)
		var out bytes.Buffer
		got := withStderr(t, func() {
			code := run(0, []string{"container_list"}, bytes.NewReader(body), &out, sys, nil)
			if code != 0 || !strings.Contains(out.String(), `"error":"policy_denied"`) {
				t.Fatalf("exit %d %s", code, out.String())
			}
		})
		want := "bot-agent-runner: policy: overlapping projects acme acme-web\n" +
			"bot-agent-runner: refuse container_list policy_denied\n"
		if got != want {
			t.Fatalf("stderr %q", got)
		}
		if strings.Contains(got, marker) || strings.Contains(got, sys.Policy) || strings.Count(got, "overlapping projects") != 1 {
			t.Fatalf("stderr %q", got)
		}
		if len(*calls) != 0 {
			t.Fatal(calls)
		}
	})

	t.Run("program", func(t *testing.T) {
		sys, calls := enabledSys(t)
		setProjects(t, sys, `["acme-web", "acme"]`)
		var out bytes.Buffer
		got := withStderr(t, func() {
			code := run(0, []string{"list"}, strings.NewReader(`{}`), &out, sys, nil)
			if code != 0 || !strings.Contains(out.String(), `"ok":true`) {
				t.Fatalf("exit %d %s", code, out.String())
			}
		})
		if got != "bot-agent-runner: policy: overlapping projects acme-web acme\n" {
			t.Fatalf("stderr %q stdout %s", got, out.String())
		}
		if strings.Contains(got, marker) || strings.Contains(out.String(), "acme") {
			t.Fatalf("stderr %q stdout %s", got, out.String())
		}
		if len(*calls) != 0 {
			t.Fatal(calls)
		}
	})

	t.Run("serve-conn", func(t *testing.T) {
		sys, calls := enabledSys(t)
		setProjects(t, sys, `["web", "acme", "acme-web"]`)
		var out bytes.Buffer
		got := withStderr(t, func() {
			code := run(0, []string{"serve-conn"}, bytes.NewReader(verbFrame("list", []byte(`{}`))), &out, sys, peer)
			if code != 0 || !strings.Contains(out.String(), `"ok":true`) {
				t.Fatalf("exit %d %s", code, out.String())
			}
		})
		if got != "bot-agent-runner: policy: overlapping projects acme acme-web\n" {
			t.Fatalf("stderr %q stdout %s", got, out.String())
		}
		if strings.Contains(got, sys.Policy) || strings.Count(got, "\n") != 1 {
			t.Fatalf("stderr %q", got)
		}
		if len(*calls) != 0 {
			t.Fatal(calls)
		}
	})

	t.Run("neighbours", func(t *testing.T) {
		for _, projects := range []string{`["acme", "acmeweb"]`, `["acme-web", "web"]`} {
			sys, _ := enabledSys(t)
			setProjects(t, sys, projects)
			var out bytes.Buffer
			got := withStderr(t, func() {
				code := run(0, []string{"serve-conn"}, bytes.NewReader(verbFrame("list", []byte(`{}`))), &out, sys, peer)
				if code != 0 || !strings.Contains(out.String(), `"ok":true`) {
					t.Fatalf("exit %d %s", code, out.String())
				}
			})
			if got != "" || strings.Contains(got, "overlapping") {
				t.Fatalf("%s stderr %q", projects, got)
			}
		}
	})
}

func withStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
