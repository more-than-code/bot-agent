// Package probe dials 127.0.0.1 only. Callers pass a port the service owns.
package probe

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxBody    = 64 << 10
	maxPathLen = 512
)

// ValidPath reports whether a probe path is safe to put on a loopback URL.
// An empty path means "/". A usable path starts with a single "/", is at
// most 512 bytes, and contains no "@", "\", "#", "?", space, or control byte.
func ValidPath(p string) bool {
	if p == "" {
		return true
	}
	if len(p) > maxPathLen || p[0] != '/' || strings.HasPrefix(p, "//") {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7f || c == ' ' || c == '@' || c == '\\' || c == '#' || c == '?' {
			return false
		}
	}
	return true
}

// requestURL builds http://127.0.0.1:<port><path>. The path is a Path field,
// never concatenated, so "@evil" cannot move the host.
func requestURL(port int, path string) *url.URL {
	if path == "" {
		path = "/"
	}
	return &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path:   path,
	}
}

// Result is the health outcome. HTTPStatus is nil for TCP and for errors
// that never produced a response.
type Result struct {
	Result     string
	HTTPStatus *int
	LatencyMS  float64
}

// Spec is one probe. Timeout is already capped by the caller.
type Spec struct {
	Protocol  string
	Port      int
	Path      string
	ExpectMin int
	ExpectMax int
	Timeout   time.Duration
}

// Do runs one probe against 127.0.0.1. Redirects are not followed.
// At most 64 KiB of an HTTP body is read, and it is discarded.
func Do(ctx context.Context, spec Spec) Result {
	if spec.Timeout <= 0 {
		spec.Timeout = 10 * time.Second
	}
	start := time.Now()
	switch spec.Protocol {
	case "tcp":
		return finish(start, dialTCP(ctx, spec))
	case "http":
		return finish(start, doHTTP(ctx, spec))
	default:
		return Result{Result: "error"}
	}
}

func finish(start time.Time, res Result) Result {
	res.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	if res.LatencyMS < 0 {
		res.LatencyMS = 0
	}
	return res
}

// DialObserve, when set, reports the loopback address a TCP probe is about
// to dial. Tests use it. Production leaves it nil. It cannot change the address.
var DialObserve func(addr string)

func dialTCP(ctx context.Context, spec Spec) Result {
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	var d net.Dialer
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.Port))
	if DialObserve != nil {
		DialObserve(addr)
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{Result: classify(err)}
	}
	_ = conn.Close()
	return Result{Result: "success"}
}

func doHTTP(ctx context.Context, spec Spec) Result {
	if !ValidPath(spec.Path) {
		return Result{Result: "error"}
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL(spec.Port, spec.Path).String(), nil)
	if err != nil {
		return Result{Result: "error"}
	}
	client := &http.Client{
		Timeout: spec.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       pinnedDial(spec.Port, spec.Timeout),
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Result: classify(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	status := resp.StatusCode
	result := "status_mismatch"
	if status >= spec.ExpectMin && status <= spec.ExpectMax {
		result = "success"
	}
	return Result{Result: result, HTTPStatus: &status}
}

// pinnedDial ignores the address the transport asked for and dials
// 127.0.0.1:<port>. Any other host fails before a socket is opened.
func pinnedDial(port int, timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || host != "127.0.0.1" {
			return nil, errors.New("probe host")
		}
		var d net.Dialer
		d.Timeout = timeout
		return d.DialContext(ctx, "tcp", target)
	}
}

func classify(err error) string {
	if err == nil {
		return "success"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	return "error"
}
