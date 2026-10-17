package run

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/probe"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// swapDigest is the kept container's digest. It is not imgDigest, which is
// what a pull on this fake returns, and not otherDigest, which is the running
// container. A result of swapDigest can only have come from prev.
func swapDigest() string {
	return "sha256:" + strings.Repeat("c", 64)
}

func TestSwapDifferentCurrentDigest(t *testing.T) {
	y := swapDigest()
	a, h, clock, _ := imageRun(t)
	s := &diffSwap{
		cur:   otherDigest,
		prev:  &images.PrevView{Digest: protocolStr(y), SpecHash: imgHash},
		after: runningStatus(0),
	}
	h.fn = s.call
	res := mustImage(t, a, "image_rollback", rollbackAt(y, 5, 0), 120)
	m := assertDeploy(t, res, "succeeded", "succeeded")
	if dig(m) != y || dig(m) == otherDigest || dig(m) == imgDigest || s.pulls != 0 || clock.sleeps() != 3 {
		t.Fatalf("digest %s pulls %d sleeps %d calls %v", dig(m), s.pulls, clock.sleeps(), h.Calls())
	}
	if s.firstState != "running" || s.firstDigest != otherDigest {
		t.Fatalf("current %s %s", s.firstState, s.firstDigest)
	}
	verbOrder(t, h, images.VerbStatus, images.VerbRollback,
		images.VerbStatus, images.VerbStatus, images.VerbStatus, images.VerbStatus, images.VerbLogs)
}

func TestSwapHealthPollFailure(t *testing.T) {
	y := swapDigest()
	for _, tc := range []struct {
		name string
		err  bool
		code string
	}{
		{"transport", true, ""},
		{"docker_unavailable", false, "docker_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, h, clock, _ := imageRun(t)
			s := &diffSwap{
				cur:      otherDigest,
				prev:     &images.PrevView{Digest: protocolStr(y), SpecHash: imgHash},
				after:    runningStatus(0),
				waitErr:  tc.err,
				waitCode: tc.code,
			}
			h.fn = s.call
			res := mustImage(t, a, "image_rollback", rollbackAt(y, 5, 0), 120)
			m := assertDeploy(t, res, "failed", "failed")
			if dig(m) != y || res.ErrorCode == nil || *res.ErrorCode != "failed" || s.pulls != 0 || clock.sleeps() != 0 {
				t.Fatalf("%v pulls %d sleeps %d", m, s.pulls, clock.sleeps())
			}
			verbOrder(t, h, images.VerbStatus, images.VerbRollback, images.VerbStatus, images.VerbLogs)
		})
	}
}

func TestSwapHealthProbe(t *testing.T) {
	y := swapDigest()
	pass := func(t *testing.T, protocolName, path string, port int, min, max int) {
		t.Helper()
		a, h, clock, _ := imageRun(t)
		s := &diffSwap{
			cur:   otherDigest,
			prev:  &images.PrevView{Digest: protocolStr(y), SpecHash: imgHash},
			after: runningStatus(0),
		}
		h.fn = s.call
		chk := protocol.HealthCheck{
			ID: "up", Service: "web", Protocol: protocolName, Port: port, Path: path, IntervalSec: 60,
			ExpectStatusMin: min, ExpectStatusMax: max,
		}
		a.configured = []protocol.HealthCheck{chk}
		res := mustImage(t, a, "image_rollback", rollbackAt(y, 30, port), 120)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		if dig(m) != y || clock.sleeps() != 0 || s.pulls != 0 {
			t.Fatalf("digest %s sleeps %d pulls %d", dig(m), clock.sleeps(), s.pulls)
		}
		verbOrder(t, h, images.VerbStatus, images.VerbRollback, images.VerbStatus, images.VerbLogs)
	}

	t.Run("tcp pass", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port
		pass(t, "tcp", "", port, 0, 0)
	})

	t.Run("http pass", func(t *testing.T) {
		port := httpProbe(t, http.StatusOK)
		pass(t, "http", "/health", port, 200, 299)
	})

	// healthy_after_sec is 5, the shortest service spec, and the deadline
	// is 8s. waitStable would call this running container healthy on the
	// poll where it has been up for 6s. A failing probe must still finish
	// failed, and it must reach the target.
	fail := func(t *testing.T, protocolName, path string, port int, min, max int, hits func() int) {
		t.Helper()
		a, h, clock, _ := imageRun(t)
		start := clock.now()
		s := &diffSwap{
			cur:   otherDigest,
			prev:  &images.PrevView{Digest: protocolStr(y), SpecHash: imgHash},
			after: runningStatus(0),
		}
		h.fn = s.call
		a.configured = []protocol.HealthCheck{{
			ID: "up", Service: "web", Protocol: protocolName, Port: port, Path: path, IntervalSec: 60,
			ExpectStatusMin: min, ExpectStatusMax: max,
		}}
		res := mustImage(t, a, "image_rollback", rollbackAt(y, 5, port), 8)
		m := assertDeploy(t, res, "failed", "failed")
		if dig(m) != y || clock.sleeps() != 4 || !clock.now().Equal(start.Add(8*time.Second)) || s.pulls != 0 || hits() != 4 {
			t.Fatalf("digest %s sleeps %d hits %d now %s calls %v", dig(m), clock.sleeps(), hits(), clock.now(), h.Calls())
		}
		verbOrder(t, h, images.VerbStatus, images.VerbRollback, images.VerbStatus, images.VerbStatus, images.VerbStatus, images.VerbStatus, images.VerbLogs)
	}

	t.Run("tcp fail", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		var hits atomic.Int32
		probe.DialObserve = func(addr string) {
			if addr == net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) {
				hits.Add(1)
			}
		}
		t.Cleanup(func() { probe.DialObserve = nil })
		fail(t, "tcp", "", port, 0, 0, func() int { return int(hits.Load()) })
	})

	t.Run("http fail", func(t *testing.T) {
		port, hits := countingHTTP(t, http.StatusInternalServerError)
		fail(t, "http", "/health", port, 200, 299, func() int { return int(hits.Load()) })
	})
}

func countingHTTP(t *testing.T, status int) (int, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port, &hits
}

func TestHelperCallLog(t *testing.T) {
	const marker = "LOGMARKER9secret"
	spec := []byte(`{"project":"acme","entry":"web"}`)

	t.Run("refusal", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		var buf bytes.Buffer
		a.opt.Log = stdLog(&buf)
		h.fn = func(_ string, body []byte) ([]byte, error) {
			if !strings.Contains(string(body), "acme") || !strings.Contains(string(body), "web") {
				return nil, errors.New("request missing")
			}
			return []byte(`{"error":"policy_denied"}`), nil
		}
		res := mustImage(t, a, "image_start", append([]byte(nil), spec...), 60)
		assertStatus(t, res, "forbidden_spec", "failed")
		want := "bot-agent: helper kind=\"image_start\" verb=\"container_start\" reason=\"policy_denied\"\n"
		if buf.String() != want {
			t.Fatalf("log %q", buf.String())
		}
		if strings.Contains(buf.String(), "acme") || strings.Contains(buf.String(), "web") || strings.Contains(buf.String(), "project") {
			t.Fatalf("request in log %s", buf.String())
		}
	})

	t.Run("transport", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		var buf bytes.Buffer
		a.opt.Log = stdLog(&buf)
		h.fn = func(string, []byte) ([]byte, error) {
			return nil, errors.New(marker)
		}
		res := mustImage(t, a, "image_start", append([]byte(nil), spec...), 60)
		assertStatus(t, res, "failed", "failed")
		want := "bot-agent: helper kind=\"image_start\" verb=\"container_start\" reason=\"transport\"\n"
		if buf.String() != want || strings.Contains(buf.String(), marker) || strings.Contains(buf.String(), "acme") {
			t.Fatalf("log %q", buf.String())
		}
	})

	t.Run("decode", func(t *testing.T) {
		a, h, _, _ := imageRun(t)
		var buf bytes.Buffer
		a.opt.Log = stdLog(&buf)
		h.fn = func(string, []byte) ([]byte, error) {
			return []byte(`{"leak":"` + marker + `"}`), nil
		}
		res := mustImage(t, a, "image_start", append([]byte(nil), spec...), 60)
		assertStatus(t, res, "failed", "failed")
		want := "bot-agent: helper kind=\"image_start\" verb=\"container_start\" reason=\"decode\"\n"
		if buf.String() != want || strings.Contains(buf.String(), marker) || strings.Contains(buf.String(), "leak") || strings.Contains(buf.String(), "acme") {
			t.Fatalf("log %q", buf.String())
		}
	})
}

func rollbackAt(digest string, healthy, port int) []byte {
	ports := ""
	if port > 0 {
		ports = fmt.Sprintf(`,"ports":[{"host":%d,"container":%d,"public":false}]`, port, port)
	}
	return []byte(fmt.Sprintf(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1"%s,"healthy_after_sec":%d},"env":{},"registry_auth":null,"force":false,"spec_hash":"%s","digest":"%s"}`, ports, healthy, imgHash, digest))
}

// diffSwap is a service rollback whose running container digest is not prev.
// After container_rollback, waitErr or waitCode fails the next status poll.
type diffSwap struct {
	cur         string
	prev        *images.PrevView
	after       images.ImageStatus
	rolled      bool
	waitErr     bool
	waitCode    string
	pulls       int
	firstState  string
	firstDigest string
}

func (s *diffSwap) call(verb string, body []byte) ([]byte, error) {
	switch verb {
	case images.VerbStatus:
		if s.rolled {
			if s.waitErr {
				return nil, errors.New("down")
			}
			if s.waitCode != "" {
				return []byte(`{"error":"` + s.waitCode + `"}`), nil
			}
			return images.Encode(images.StatusResult{Status: s.after, SpecHash: imgHash}), nil
		}
		st := runningStatus(0)
		st.Digest = protocolStr(s.cur)
		s.firstState = st.State
		if st.Digest != nil {
			s.firstDigest = *st.Digest
		}
		return images.Encode(images.StatusResult{Status: st, SpecHash: imgHash, Prev: s.prev}), nil
	case images.VerbRollback:
		if err := readRollback(body); err != nil {
			return nil, err
		}
		s.rolled = true
		return images.Encode(images.IDResponse{ContainerID: "prev"}), nil
	case images.VerbPull:
		s.pulls++
		return images.Encode(images.PullResponse{Digest: imgDigest, ImageID: "sha256:image"}), nil
	case images.VerbDeploy:
		return images.Encode(images.IDResponse{ContainerID: "c1"}), nil
	case images.VerbLogs:
		return images.Encode(images.LogsResponse{Container: "acme-web", Lines: []string{}, Truncated: false}), nil
	default:
		return nil, errors.New(verb)
	}
}
