package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
)

// errLoad is a config Caddy refused. The detail is Caddy's error text.
var errLoad = errors.New("caddy load")

const maxAdminBody = 64 << 10

// caddyAdmin is the edge's admin API over its unix socket. It has two
// requests and no way to send a caller-supplied path, header or query.
// Caddy accepts Host 127.0.0.1 on a unix admin socket.
type caddyAdmin struct {
	http *http.Client
}

func newCaddyAdmin(sock string) *caddyAdmin {
	tr := &http.Transport{
		DisableCompression: true,
		DisableKeepAlives:  true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &caddyAdmin{http: &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// ready reports whether the admin API answers GET /config/ with 200.
func (c *caddyAdmin) ready(ctx context.Context) bool {
	code, _, err := c.do(ctx, http.MethodGet, "/config/", nil)
	return err == nil && code == http.StatusOK
}

// load posts a whole config to /load. Caddy keeps the running config when
// it refuses a new one. A 4xx is errLoad with Caddy's error text; anything
// else that is not 200 is errEngine.
func (c *caddyAdmin) load(ctx context.Context, cfg []byte) (string, error) {
	code, body, err := c.do(ctx, http.MethodPost, "/load", cfg)
	if err != nil {
		return "", errEngine
	}
	if code == http.StatusOK {
		return "", nil
	}
	if code >= 400 && code < 500 {
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &msg) == nil && msg.Error != "" {
			return msg.Error, errLoad
		}
		return string(body), errLoad
	}
	return "", errEngine
}

func (c *caddyAdmin) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1"+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header = http.Header{}
	req.Header.Set("User-Agent", "bot-agent-runner")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	buf, _, err := readLimit(resp.Body, maxAdminBody)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, buf, nil
}
