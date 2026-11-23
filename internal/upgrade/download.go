package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

// BaseURL is the fixed release location (item 6.1). Only the version and
// the three file names are appended.
const BaseURL = "https://github.com/more-than-code/bot-agent/releases/download/"

// Download limits (item 6.1).
const (
	MaxFile      = 64 << 20
	FileTimeout  = 120 * time.Second
	MaxRedirects = 5
)

// AllowedHosts are the only hosts a download may start at or redirect to.
var AllowedHosts = []string{"github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}

// HTTPFetcher downloads release files over HTTPS. Production uses
// NewHTTPFetcher; tests replace Base, Hosts and the client's transport.
type HTTPFetcher struct {
	Base    string
	Hosts   []string
	Client  *http.Client
	MaxSize int64
	Timeout time.Duration
}

// NewHTTPFetcher is the production fetcher. proxy is the request proxy
// function (from the agent's env file), or nil for none.
func NewHTTPFetcher(proxy func(*http.Request) (*url.URL, error)) *HTTPFetcher {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = proxy
	f := &HTTPFetcher{Base: BaseURL, Hosts: AllowedHosts, MaxSize: MaxFile, Timeout: FileTimeout}
	f.Client = &http.Client{Transport: tr}
	return f
}

// ProxyFunc is the transport proxy for agent_upgrade_https_proxy: nil when
// it is empty, else that URL for every request (NO_PROXY is empty).
func ProxyFunc(raw string) (func(*http.Request) (*url.URL, error), error) {
	if raw == "" {
		return nil, nil
	}
	if !policy.ValidUpgradeProxy(raw) {
		return nil, errors.New("bad proxy")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return http.ProxyURL(u), nil
}

// FileURL builds the URL of one release file. version must be valid and
// name one of the three release names; anything else is an error.
func FileURL(base, version, name string) (string, error) {
	if !agentupgrade.ValidVersion(version) {
		return "", errors.New("bad version")
	}
	switch name {
	case sumsName, sigName, tarballName(version):
	default:
		return "", errors.New("bad file name")
	}
	return base + "v" + version + "/" + name, nil
}

// Fetch downloads one file: HTTPS only, allowed hosts only (also after a
// redirect), at most MaxRedirects redirects, MaxSize bytes and Timeout.
func (f *HTTPFetcher) Fetch(ctx context.Context, version, name string) ([]byte, error) {
	raw, err := FileURL(f.Base, version, name)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := f.allowed(u); err != nil {
		return nil, err
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = FileTimeout
	}
	max := f.MaxSize
	if max <= 0 {
		max = MaxFile
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := *f.Client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > MaxRedirects {
			return errors.New("too many redirects")
		}
		return f.allowed(req.URL)
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "bot-agent-runner")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed", name)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", name, resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, fmt.Errorf("%s: too large", name)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read failed", name)
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("%s: too large", name)
	}
	return body, nil
}

// allowed accepts an https URL without userinfo whose host is listed,
// on the default port. An exact host:port entry (a test server) also
// matches.
func (f *HTTPFetcher) allowed(u *url.URL) error {
	if u.Scheme != "https" {
		return errors.New("not https")
	}
	if u.User != nil {
		return errors.New("userinfo in url")
	}
	for _, h := range f.Hosts {
		if u.Host == h {
			return nil
		}
		if u.Hostname() == h && (u.Port() == "" || u.Port() == "443") {
			return nil
		}
	}
	return fmt.Errorf("host %s not allowed", u.Hostname())
}
