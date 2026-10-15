package run

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func shortSock(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "ba-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func TestSocketHelperRoundTrip(t *testing.T) {
	path := shortSock(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 256)
		n, _ := c.Read(buf)
		got <- string(buf[:n])
		_, _ = c.Write([]byte("{\"ok\":false,\"reason\":\"policy_denied\"}\n"))
	}()
	h := socketHelper{path: path, dialTimeout: time.Second}
	raw, err := h.Call(context.Background(), "status", []byte(`{"project":"tour-agent","entry":"web"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"ok":false,"reason":"policy_denied"}` {
		t.Fatalf("%s", raw)
	}
	select {
	case line := <-got:
		if !strings.Contains(line, `"verb":"status"`) || !strings.Contains(line, `"entry":"web"`) || !strings.HasSuffix(line, "\n") {
			t.Fatalf("frame %q", line)
		}
		if strings.Contains(line, "sudo") {
			t.Fatal(line)
		}
	case <-time.After(time.Second):
		t.Fatal("helper was not dialed")
	}
}

func TestSocketHelperDialTimeout(t *testing.T) {
	h := socketHelper{
		path:        defaultSocketPath,
		dialTimeout: 40 * time.Millisecond,
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != defaultSocketPath || network != "unix" {
				t.Errorf("dial %s %s", network, addr)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	raw, err := h.Call(parent, "list", []byte(`{}`))
	elapsed := time.Since(start)
	if err == nil || raw != nil {
		t.Fatalf("err %v raw %q", err, raw)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("dial waited %s", elapsed)
	}
}

func TestSocketHelperRejectsOversizedAndEmpty(t *testing.T) {
	path := shortSock(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte(strings.Repeat("x", programs.MaxHelperOut+1)))
	}()
	h := socketHelper{path: path, dialTimeout: time.Second}
	raw, err := h.Call(context.Background(), "list", []byte(`{}`))
	if err == nil || raw != nil || strings.Contains(err.Error(), "xxx") {
		t.Fatalf("err %v raw %d", err, len(raw))
	}
}

func TestSocketHelperDoesNotDialUnknownVerb(t *testing.T) {
	h := socketHelper{
		path: defaultSocketPath,
		dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("dialed")
			return nil, errHelper
		},
	}
	raw, err := h.Call(context.Background(), "reboot", []byte(`{}`))
	if err == nil || raw != nil {
		t.Fatalf("err %v", err)
	}
}

func TestSocketHelperCancelledContext(t *testing.T) {
	h := socketHelper{
		path:        defaultSocketPath,
		dialTimeout: time.Minute,
		dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		_, _ = h.Call(ctx, "status", []byte(`{}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled helper call hung")
	}
}

func TestSocketHelperCallOutlivesDialTimeout(t *testing.T) {
	path := shortSock(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 256)
		_, _ = c.Read(buf)
		time.Sleep(120 * time.Millisecond)
		_, _ = c.Write([]byte("{\"ok\":false,\"reason\":\"policy_denied\"}\n"))
	}()
	h := socketHelper{path: path, dialTimeout: 40 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := h.Call(ctx, "status", []byte(`{"project":"tour-agent","entry":"web"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"ok":false,"reason":"policy_denied"}` {
		t.Fatalf("%s", raw)
	}
}

func TestSocketHelperCopyError(t *testing.T) {
	h := socketHelper{
		path:        defaultSocketPath,
		dialTimeout: time.Second,
		dial: func(context.Context, string, string) (net.Conn, error) {
			return &scriptConn{r: bytes.NewReader([]byte(`{"ok":true}`)), err: io.ErrUnexpectedEOF}, nil
		},
	}
	raw, err := h.Call(context.Background(), "status", []byte(`{"project":"tour-agent","entry":"web"}`))
	if err == nil || raw != nil {
		t.Fatalf("err %v raw %q", err, raw)
	}
}

type scriptConn struct {
	r   *bytes.Reader
	err error
}

func (c *scriptConn) Read(p []byte) (int, error) {
	if c.r.Len() == 0 {
		return 0, c.err
	}
	n, _ := c.r.Read(p)
	if c.r.Len() == 0 {
		return n, c.err
	}
	return n, nil
}

func (c *scriptConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *scriptConn) Close() error                     { return nil }
func (c *scriptConn) LocalAddr() net.Addr              { return nil }
func (c *scriptConn) RemoteAddr() net.Addr             { return nil }
func (c *scriptConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptConn) SetWriteDeadline(time.Time) error { return nil }

func TestAcceptStdoutDropsOversize(t *testing.T) {
	_, err := acceptStdout(programs.MaxHelperOut+1, []byte("SENTINEL-OUT"))
	if err == nil || strings.Contains(err.Error(), "SENTINEL-OUT") {
		t.Fatal(err)
	}
	if _, err := acceptStdout(0, nil); err == nil {
		t.Fatal("empty")
	}
}
