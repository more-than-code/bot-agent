// Package fakeserver is the contract's server side for tests.
// It is not linked into the agent binary. It is the only package that listens.
package fakeserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// Server accepts one agent at a time on ws://127.0.0.1.
type Server struct {
	URL          string
	HostID       string
	CredentialID string
	Credential   string
	Config       protocol.Config
	Heartbeat    int

	// EnrollError and HelloError, when set, are sent instead of success.
	EnrollError string
	HelloError  string
	// HelloHostID, when set, replaces HostID in hello_ok.
	HelloHostID string
	// Now, when set, is the clock for hello_ok.server_time.
	Now func() time.Time
	// RedirectTo, when set, answers every request with 302 and does not upgrade.
	RedirectTo string

	mu           sync.Mutex
	frameWait    *sync.Cond
	frames       []Frame
	violations   []string
	conns        int
	pings        int
	redirectHits int
	dropOnce     int
	skipTaskAck  int
	ackMetrics   bool
	taskAcks     []string
	w            *wsWriter
	sentKind     map[string]string
	http         *http.Server
}

// Frame is one agent→server object.
type Frame struct {
	Type string
	Raw  json.RawMessage
}

type wsWriter struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (w *wsWriter) write(ctx context.Context, v any) error {
	b, err := protocol.Marshal(v)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return w.c.Write(wctx, websocket.MessageText, b)
}

// Start listens on 127.0.0.1 and stops when the test ends.
func Start(t *testing.T) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		URL:          "ws://" + ln.Addr().String() + "/agent/v1/ws",
		HostID:       "11111111-1111-1111-1111-111111111111",
		CredentialID: "22222222-2222-2222-2222-222222222222",
		Credential:   strings.Repeat("a", 43),
		Heartbeat:    20,
		Config: protocol.Config{
			MetricsIntervalSec: 5,
			ProcessIntervalSec: 30,
		},
		sentKind:   map[string]string{},
		ackMetrics: true,
	}
	s.frameWait = sync.NewCond(&s.mu)
	s.http = &http.Server{Handler: s}
	go func() { _ = s.http.Serve(ln) }()
	t.Cleanup(func() {
		_ = s.http.Close()
		_ = ln.Close()
	})
	return s
}

// DropNextMetrics closes the socket after the next metrics frame, without an ack.
func (s *Server) DropNextMetrics() {
	s.mu.Lock()
	s.dropOnce++
	s.mu.Unlock()
}

// SkipNextTaskAck closes the socket after the next task_result, without an ack.
func (s *Server) SkipNextTaskAck() {
	s.mu.Lock()
	s.skipTaskAck++
	s.mu.Unlock()
}

// SetAckMetrics turns metrics acknowledgements on or off.
// The default is on. Off leaves batches unacked so a test can drop the socket.
func (s *Server) SetAckMetrics(v bool) {
	s.mu.Lock()
	s.ackMetrics = v
	s.mu.Unlock()
}

// Pings is how many agent ping frames have been read.
func (s *Server) Pings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

// RedirectHits is how many 302 responses have been written.
func (s *Server) RedirectHits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.redirectHits
}

// TaskAcks are task ids whose task_ack was written.
func (s *Server) TaskAcks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.taskAcks...)
}

// CloseConn drops the current WebSocket, if any.
func (s *Server) CloseConn() {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()
	if w != nil {
		_ = w.c.Close(websocket.StatusGoingAway, "")
	}
}

// SendConfig writes one config_update on the current connection.
func (s *Server) SendConfig(cfg protocol.Config) error {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()
	if w == nil {
		return errors.New("no connection")
	}
	return w.write(context.Background(), protocol.ConfigUpdate{
		V: protocol.Version, Type: "config_update", Config: cfg,
	})
}

// Conns is how many WebSocket upgrades have completed.
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// Violations are contract failures in agent frames.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.violations...)
}

// Frames returns a copy of the frames received so far.
func (s *Server) Frames() []Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Frame(nil), s.frames...)
}

// WaitType waits until typ has been read n times. The deadline fails the wait;
// it is not a sleep used to decide that a frame was absent.
func (s *Server) WaitType(typ string, n int, d time.Duration) bool {
	if n <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline := time.Now().Add(d)
	for countFrames(s.frames, typ) < n {
		if !s.waitFrame(deadline) {
			return countFrames(s.frames, typ) >= n
		}
	}
	return true
}

func countFrames(fr []Frame, typ string) int {
	n := 0
	for _, f := range fr {
		if f.Type == typ {
			n++
		}
	}
	return n
}

// waitFrame waits for a read or for the deadline. Caller holds s.mu.
// Stop does not wait for the callback. A callback that then locks s.mu
// only broadcasts; WaitType checks the count again.
func (s *Server) waitFrame(deadline time.Time) bool {
	if !time.Now().Before(deadline) {
		return false
	}
	timer := time.AfterFunc(time.Until(deadline), func() {
		s.mu.Lock()
		s.frameWait.Broadcast()
		s.mu.Unlock()
	})
	s.frameWait.Wait()
	timer.Stop()
	return time.Now().Before(deadline)
}

// Wait polls until pred is true or the timeout passes.
func (s *Server) Wait(d time.Duration, pred func([]Frame) bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred(s.Frames()) {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return pred(s.Frames())
}

// SendTask delivers a task on the current connection.
func (s *Server) SendTask(task protocol.Task) error {
	s.mu.Lock()
	w := s.w
	if s.sentKind == nil {
		s.sentKind = map[string]string{}
	}
	s.sentKind[task.TaskID] = task.Kind
	s.mu.Unlock()
	if w == nil {
		return errors.New("no connection")
	}
	return w.write(context.Background(), task)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	redir := s.RedirectTo
	if redir != "" {
		s.redirectHits++
	}
	s.mu.Unlock()
	if redir != "" {
		http.Redirect(w, r, redir, http.StatusFound)
		return
	}
	if r.URL.Path != "/agent/v1/ws" {
		http.NotFound(w, r)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(protocol.MaxFrame)
	ww := &wsWriter{c: c}
	s.mu.Lock()
	s.conns++
	s.w = ww
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.w == ww {
			s.w = nil
		}
		s.mu.Unlock()
		_ = c.Close(websocket.StatusNormalClosure, "")
	}()
	auth := r.Header.Get("Authorization")
	if auth == "" {
		s.serveEnroll(ww)
		return
	}
	if auth != "Bearer "+s.Credential {
		_ = ww.write(r.Context(), protocol.ServerError{
			V: protocol.Version, Type: "error", ErrorCode: "unauthenticated", Message: "no",
		})
		return
	}
	s.serveSession(ww)
}

func (s *Server) serveEnroll(w *wsWriter) {
	ctx := context.Background()
	typ, _, err := s.read(ctx, w.c)
	if err != nil || typ != "enroll" {
		return
	}
	if s.EnrollError != "" {
		_ = w.write(ctx, protocol.ServerError{
			V: protocol.Version, Type: "error", ErrorCode: s.EnrollError, Message: "no",
		})
		return
	}
	_ = w.write(ctx, map[string]any{
		"v": protocol.Version, "type": "enrolled",
		"host_id": s.HostID, "credential_id": s.CredentialID, "credential": s.Credential,
	})
	_ = w.c.Close(websocket.StatusNormalClosure, "")
}

func (s *Server) serveSession(w *wsWriter) {
	ctx := context.Background()
	for {
		typ, raw, err := s.read(ctx, w.c)
		if err != nil {
			return
		}
		switch typ {
		case "hello":
			if s.HelloError != "" {
				_ = w.write(ctx, protocol.ServerError{
					V: protocol.Version, Type: "error", ErrorCode: s.HelloError, Message: "no",
				})
				return
			}
			hostID := s.HostID
			if s.HelloHostID != "" {
				hostID = s.HelloHostID
			}
			now := time.Now()
			if s.Now != nil {
				now = s.Now()
			}
			_ = w.write(ctx, protocol.HelloOK{
				V: protocol.Version, Type: "hello_ok", HostID: hostID,
				HeartbeatSec: s.Heartbeat, ServerTime: protocol.FormatTime(now),
				Config: s.Config,
			})
		case "ping":
			s.mu.Lock()
			s.pings++
			s.mu.Unlock()
			_ = w.write(ctx, protocol.Pong{V: protocol.Version, Type: "pong", TS: protocol.FormatTime(time.Now().UTC())})
		case "metrics":
			s.mu.Lock()
			drop := s.dropOnce > 0
			if drop {
				s.dropOnce--
			}
			ack := s.ackMetrics
			s.mu.Unlock()
			if drop {
				_ = w.c.Close(websocket.StatusGoingAway, "")
				return
			}
			if !ack {
				break
			}
			var m protocol.Metrics
			_ = json.Unmarshal(raw, &m)
			_ = w.write(ctx, protocol.MetricsAck{V: protocol.Version, Type: "metrics_ack", BatchID: m.BatchID})
		case "task_result":
			s.checkResult(raw)
			var head struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(raw, &head)
			s.mu.Lock()
			skip := s.skipTaskAck > 0
			if skip {
				s.skipTaskAck--
			}
			s.mu.Unlock()
			if skip {
				_ = w.c.Close(websocket.StatusGoingAway, "")
				return
			}
			if err := w.write(ctx, protocol.TaskAck{V: protocol.Version, Type: "task_ack", TaskID: head.TaskID}); err == nil {
				s.mu.Lock()
				s.taskAcks = append(s.taskAcks, head.TaskID)
				s.mu.Unlock()
			}
		default:
			// task_reject and services are recorded by read.
		}
	}
}

func (s *Server) read(ctx context.Context, c *websocket.Conn) (string, []byte, error) {
	kind, data, err := c.Read(ctx)
	if err != nil {
		return "", nil, err
	}
	if kind != websocket.MessageText {
		s.addViolation("binary frame")
		return "", data, errors.New("binary")
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		s.addViolation("json")
		return "", data, err
	}
	if err := protocol.ValidateAgent(data); err != nil {
		s.addViolation(env.Type + ": " + err.Error())
	}
	s.mu.Lock()
	s.frames = append(s.frames, Frame{Type: env.Type, Raw: append(json.RawMessage(nil), data...)})
	s.frameWait.Broadcast()
	s.mu.Unlock()
	return env.Type, data, nil
}

func (s *Server) checkResult(raw []byte) {
	var head struct {
		TaskID  string          `json:"task_id"`
		State   string          `json:"state"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return
	}
	if head.State != "succeeded" {
		return
	}
	s.mu.Lock()
	kind := s.sentKind[head.TaskID]
	s.mu.Unlock()
	var err error
	switch kind {
	case "host_snapshot":
		err = protocol.ValidateSnapshot(head.Payload)
	case "probe_health":
		err = protocol.ValidateProbeResult(head.Payload)
	}
	if err != nil {
		s.addViolation("payload: " + err.Error())
	}
}

func (s *Server) addViolation(msg string) {
	s.mu.Lock()
	s.violations = append(s.violations, msg)
	s.mu.Unlock()
}
