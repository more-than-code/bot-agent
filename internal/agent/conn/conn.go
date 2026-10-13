// Package conn is the agent WebSocket dialer, backoff, and frame IO.
package conn

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

const (
	backoffMin = time.Second
	backoffMax = 30 * time.Second
)

// ErrForbidden stops reconnects until the process is restarted.
var ErrForbidden = errors.New("forbidden")

// ErrUnauthenticated asks the caller to back off to 30s and keep trying.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrReconnect means enrollment finished and the caller should dial again.
var ErrReconnect = errors.New("reconnect")

// Backoff is 1s, doubling, capped at 30s. Take applies ±20% jitter.
type Backoff struct {
	next time.Duration
}

// NewBackoff starts at 1s.
func NewBackoff() Backoff { return Backoff{next: backoffMin} }

// Reset runs after hello_ok so the next failure waits about 1s.
func (b *Backoff) Reset() { b.next = backoffMin }

// Set replaces the next delay. Values above 30s are capped.
func (b *Backoff) Set(d time.Duration) {
	if d < backoffMin {
		d = backoffMin
	}
	if d > backoffMax {
		d = backoffMax
	}
	b.next = d
}

// Next is the delay that the following Take will jitter, before Take advances it.
func (b *Backoff) Next() time.Duration { return b.next }

// Take returns the jittered delay and advances the sequence.
func (b *Backoff) Take() time.Duration {
	d := jitter(b.next)
	n := b.next * 2
	if n > backoffMax || b.next >= backoffMax {
		n = backoffMax
	}
	b.next = n
	return d
}

func jitter(d time.Duration) time.Duration {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return d
	}
	u := float64(binary.BigEndian.Uint64(buf[:])) / float64(math.MaxUint64)
	factor := 0.8 + 0.4*u
	return time.Duration(float64(d) * factor)
}

// Dial opens a text WebSocket. bearer is sent only when non-empty.
func Dial(ctx context.Context, rawURL, bearer string) (*websocket.Conn, error) {
	hdr := http.Header{}
	if bearer != "" {
		hdr.Set("Authorization", "Bearer "+bearer)
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// A 3xx is a failed dial. CheckRedirect stops the client from replaying
	// the handshake, including the bearer, onto another host. The clone keeps
	// the default Proxy (ProxyFromEnvironment).
	c, _, err := websocket.Dial(dctx, rawURL, &websocket.DialOptions{
		HTTPHeader: hdr,
		HTTPClient: dialHTTPClient(),
	})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(protocol.MaxFrame)
	return c, nil
}

// WriteJSON writes one text frame.
func WriteJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := protocol.Marshal(v)
	if err != nil {
		return err
	}
	return WriteRaw(ctx, c, b)
}

// WriteRaw writes an already-encoded frame.
func WriteRaw(ctx context.Context, c *websocket.Conn, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

// ReadFrame returns the type and the raw JSON object.
func ReadFrame(ctx context.Context, c *websocket.Conn) (string, []byte, error) {
	kind, data, err := c.Read(ctx)
	if err != nil {
		return "", nil, err
	}
	if kind != websocket.MessageText {
		return "", nil, errors.New("binary frame")
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "", data, err
	}
	if env.V != protocol.Version || env.Type == "" {
		return env.Type, data, errors.New("envelope")
	}
	return env.Type, data, nil
}

func dialHTTPClient() *http.Client {
	transport := http.DefaultTransport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: transport,
	}
}

// Safe redacts known secrets from an error string before it is logged.
func Safe(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	for _, sec := range secrets {
		if len(sec) < 8 {
			continue
		}
		s = strings.ReplaceAll(s, sec, "[REDACTED]")
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
