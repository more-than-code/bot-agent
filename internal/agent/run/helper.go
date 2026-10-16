package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

const (
	defaultSocketPath = "/run/bot-agent-runner.sock"
	// helperDialTimeout bounds only the connect. The accepted connection
	// uses the caller's deadline (the task timeout, or the 5s discovery cap).
	helperDialTimeout = 5 * time.Second
	// discoverHelperTimeout bounds one helper call made while building hello
	// or a services frame. It is not a protocol field.
	discoverHelperTimeout = 5 * time.Second
)

// errHelper is the only error Call returns for a transport failure.
// It carries no helper payload, path, or env value.
var errHelper = errors.New("helper unavailable")

// Helper runs one helper verb. Production dials the runner socket.
// Tests substitute a fake.
type Helper interface {
	Call(ctx context.Context, verb string, body []byte) ([]byte, error)
}

// socketHelper dials the socket-activated helper. One newline-terminated
// request, one JSON response. The process environment is not sent.
type socketHelper struct {
	path        string
	dialTimeout time.Duration
	dial        func(context.Context, string, string) (net.Conn, error)
}

func (s socketHelper) Call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	limit := programs.FrameLimit(verb)
	if !programs.KnownVerb(verb) || len(body) > limit {
		return nil, errHelper
	}
	frame, err := marshalFrame(verb, body)
	if err != nil || len(frame) > limit+images.SocketSlack {
		return nil, errHelper
	}
	timeout := s.dialTimeout
	if timeout <= 0 {
		timeout = helperDialTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dial := s.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(dialCtx, "unix", s.path)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errHelper
	}
	defer conn.Close()
	// The dial budget is not the call budget. start and restart run under
	// the task's timeout_sec, which is already this context's deadline.
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errHelper
	}
	stdout := capBuf{limit: stdoutLimit(verb)}
	if _, err := io.Copy(&stdout, conn); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errHelper
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	out, err := acceptLimited(stdout.n, stdout.b, stdout.limit)
	if err != nil {
		return nil, errHelper
	}
	return out, nil
}

func marshalFrame(verb string, body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, errHelper
	}
	return json.Marshal(struct {
		Verb    string          `json:"verb"`
		Request json.RawMessage `json:"request"`
	}{Verb: verb, Request: json.RawMessage(body)})
}

func stdoutLimit(verb string) int {
	if verb == images.VerbList {
		return programs.MaxListOut
	}
	return programs.MaxHelperOut
}

// capBuf keeps at most limit+1 bytes and counts the rest.
// A count past the cap is a transport failure, not a truncated JSON object.
type capBuf struct {
	b     []byte
	n     int
	limit int
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.n += len(p)
	if c.limit > 0 && len(c.b) <= c.limit {
		room := c.limit + 1 - len(c.b)
		if room > len(p) {
			room = len(p)
		}
		if room > 0 {
			c.b = append(c.b, p[:room]...)
		}
	}
	return len(p), nil
}

func acceptStdout(n int, buf []byte) ([]byte, error) {
	return acceptLimited(n, buf, programs.MaxHelperOut)
}

func acceptLimited(n int, buf []byte, limit int) ([]byte, error) {
	if n == 0 || n > limit {
		return nil, errHelper
	}
	out := bytes.TrimSpace(buf)
	if len(out) == 0 {
		return nil, errHelper
	}
	return out, nil
}
