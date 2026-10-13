package probe

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPSuccessMismatchAndRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		case "/bad":
			w.WriteHeader(http.StatusInternalServerError)
		case "/go":
			http.Redirect(w, r, "/health", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := portOf(t, srv.Listener.Addr().String())
	ctx := context.Background()
	ok := Do(ctx, Spec{Protocol: "http", Port: port, Path: "/health", ExpectMin: 200, ExpectMax: 299, Timeout: time.Second})
	if ok.Result != "success" || ok.HTTPStatus == nil || *ok.HTTPStatus != 200 || ok.LatencyMS < 0 {
		t.Fatalf("%+v", ok)
	}
	bad := Do(ctx, Spec{Protocol: "http", Port: port, Path: "/bad", ExpectMin: 200, ExpectMax: 299, Timeout: time.Second})
	if bad.Result != "status_mismatch" || bad.HTTPStatus == nil || *bad.HTTPStatus != 500 {
		t.Fatalf("%+v", bad)
	}
	red := Do(ctx, Spec{Protocol: "http", Port: port, Path: "/go", ExpectMin: 200, ExpectMax: 299, Timeout: time.Second})
	if red.Result != "status_mismatch" || red.HTTPStatus == nil || *red.HTTPStatus != http.StatusFound {
		t.Fatalf("redirect followed: %+v", red)
	}
	tcp := Do(ctx, Spec{Protocol: "tcp", Port: port, Timeout: time.Second})
	if tcp.Result != "success" || tcp.HTTPStatus != nil {
		t.Fatalf("%+v", tcp)
	}
}

func TestRefusedAndTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, ln.Addr().String())
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	ref := Do(context.Background(), Spec{Protocol: "tcp", Port: port, Timeout: time.Second})
	if ref.Result != "connection_refused" {
		t.Fatalf("%+v", ref)
	}
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	go func() {
		c, err := hold.Accept()
		if err != nil {
			return
		}
		time.Sleep(2 * time.Second)
		_ = c.Close()
	}()
	timed := Do(context.Background(), Spec{
		Protocol: "http", Port: portOf(t, hold.Addr().String()), Path: "/",
		ExpectMin: 200, ExpectMax: 299, Timeout: 200 * time.Millisecond,
	})
	if timed.Result != "timeout" {
		t.Fatalf("%+v", timed)
	}
	if Do(context.Background(), Spec{Protocol: "udp", Port: 1}).Result != "error" {
		t.Fatal("udp")
	}
}

func TestValidPathAndPinnedDial(t *testing.T) {
	ok := []string{"", "/", "/health", "/" + strings.Repeat("a", 511)}
	bad := []string{
		"@evil.example/",
		"//evil.example/",
		"/a?b",
		"/a#b",
		"/a b",
		"/a\\b",
		"/a@b",
		"/a\nb",
		"/" + strings.Repeat("a", 512),
	}
	for _, p := range ok {
		if !ValidPath(p) {
			t.Fatalf("rejected %q", p)
		}
	}
	for _, p := range bad {
		if ValidPath(p) {
			t.Fatalf("accepted %q", p)
		}
	}
	u := requestURL(80, "/health")
	if u.Host != "127.0.0.1:80" || u.String() != "http://127.0.0.1:80/health" || strings.Contains(u.String(), "@") {
		t.Fatalf("url %s", u)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tcpLn := ln.(*net.TCPListener)
	port := portOf(t, ln.Addr().String())
	dial := pinnedDial(port, time.Second)
	if _, err := dial(context.Background(), "tcp", "evil.example:80"); err == nil {
		t.Fatal("foreign host dialed")
	}
	if _, err := dial(context.Background(), "tcp", "localhost:80"); err == nil {
		t.Fatal("localhost dialed")
	}
	_ = tcpLn.SetDeadline(time.Now().Add(80 * time.Millisecond))
	if c, err := ln.Accept(); err == nil {
		_ = c.Close()
		t.Fatal("foreign dial connected")
	}
	conn, err := dial(context.Background(), "tcp", "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	_ = tcpLn.SetDeadline(time.Now().Add(time.Second))
	got, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = got.Close()

	res := Do(context.Background(), Spec{
		Protocol: "http", Port: port, Path: "@evil.example/",
		ExpectMin: 200, ExpectMax: 299, Timeout: time.Second,
	})
	if res.Result != "error" {
		t.Fatalf("bad path %+v", res)
	}
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
