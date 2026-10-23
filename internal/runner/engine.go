package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

const (
	engineAPI     = "/v1.47"
	maxEngineJSON = 4 << 20
	maxPullStream = 8 << 20
	maxLogStream  = 1 << 20
)

var (
	errEngine   = errors.New("engine")
	errImage    = errors.New("image")
	errNotFound = errors.New("missing")
	errBadSpec  = errors.New("spec")

	engineIDRE   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	engineNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)
	hostIDRE     = regexp.MustCompile(`^[a-f0-9]{16}$`)
)

// engine is the only Docker surface the verbs can reach. There is no method
// that takes a caller-supplied path, query, header, or raw body.
type engine interface {
	Pull(ctx context.Context, ref images.Reference, auth *images.RegistryAuth) (string, string, error)
	List(ctx context.Context) ([]listed, error)
	Inspect(ctx context.Context, id string) (inspected, error)
	Create(ctx context.Context, name string, body []byte) (string, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Rename(ctx context.Context, id, name string) error
	Remove(ctx context.Context, id string) error
	Logs(ctx context.Context, id string, lines int) ([]byte, bool, error)
	VolumeInspect(ctx context.Context, name string) (volumeInfo, error)
	VolumeCreate(ctx context.Context, name, host, project string) error
	NetworkInspect(ctx context.Context, name string) (networkInfo, error)
	NetworkCreate(ctx context.Context, name, host, project string) error
}

type listed struct {
	ID     string
	Names  []string
	State  string
	Image  string
	Labels map[string]string
	Ports  []portView
	// Health is healthy, unhealthy, starting, or nil. It is not the Engine
	// state. nil means the list row did not say.
	Health *string
}

type inspected struct {
	ID           string
	Name         string
	State        string
	ExitCode     int
	RestartCount int
	StartedAt    string
	FinishedAt   string
	Image        string
	Labels       map[string]string
	Ports        []portView
}

type portView struct {
	HostIP        string
	HostPort      int
	ContainerPort int
}

type volumeInfo struct {
	Driver  string
	Options map[string]string
	Labels  map[string]string
}

type networkInfo struct {
	Driver string
	Labels map[string]string
}

type engineClient struct {
	http *http.Client
}

func newEngine(sock string) *engineClient {
	tr := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &engineClient{http: &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (e *engineClient) Pull(ctx context.Context, ref images.Reference, auth *images.RegistryAuth) (string, string, error) {
	q, err := ref.PullQuery()
	if err != nil {
		return "", "", errBadSpec
	}
	hdr := http.Header{}
	if auth != nil {
		if badCredential(auth) {
			return "", "", errBadSpec
		}
		if auth.Host == ref.Host {
			val, err := registryHeader(auth, ref.Host)
			if err != nil {
				return "", "", errBadSpec
			}
			hdr.Set("X-Registry-Auth", val)
		}
	}
	code, body, err := e.do(ctx, http.MethodPost, "/images/create", q, hdr, nil, maxPullStream)
	if err != nil {
		return "", "", errEngine
	}
	if code == http.StatusNotFound || code == http.StatusUnauthorized || code == http.StatusForbidden {
		return "", "", errImage
	}
	if code != http.StatusOK {
		return "", "", errEngine
	}
	return parsePull(body, ref.Digest)
}

func (e *engineClient) List(ctx context.Context) ([]listed, error) {
	q := url.Values{}
	q.Set("all", "1")
	code, body, err := e.do(ctx, http.MethodGet, "/containers/json", q, nil, nil, maxEngineJSON)
	if err != nil || code != http.StatusOK {
		return nil, errEngine
	}
	return parseList(body)
}

func (e *engineClient) Inspect(ctx context.Context, id string) (inspected, error) {
	p, err := containerPath(id, "/json")
	if err != nil {
		return inspected{}, err
	}
	code, body, err := e.do(ctx, http.MethodGet, p, nil, nil, nil, maxEngineJSON)
	if err != nil {
		return inspected{}, errEngine
	}
	if code == http.StatusNotFound {
		return inspected{}, errNotFound
	}
	if code != http.StatusOK {
		return inspected{}, errEngine
	}
	return parseInspect(body)
}

func (e *engineClient) Create(ctx context.Context, name string, body []byte) (string, error) {
	if !engineNameRE.MatchString(name) || len(body) == 0 || len(body) > maxEngineJSON {
		return "", errEngine
	}
	q := url.Values{}
	q.Set("name", name)
	code, buf, err := e.do(ctx, http.MethodPost, "/containers/create", q, nil, body, maxEngineJSON)
	if err != nil {
		return "", errEngine
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return "", errEngine
	}
	var resp struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(buf, &resp) != nil || !engineIDRE.MatchString(resp.ID) {
		return "", errEngine
	}
	return resp.ID, nil
}

func (e *engineClient) Start(ctx context.Context, id string) error {
	return e.emptyAction(ctx, http.MethodPost, id, "/start", nil, http.StatusNoContent, http.StatusNotModified)
}

func (e *engineClient) Stop(ctx context.Context, id string) error {
	q := url.Values{}
	q.Set("t", strconv.Itoa(images.StopGraceSec))
	return e.emptyAction(ctx, http.MethodPost, id, "/stop", q, http.StatusNoContent, http.StatusNotModified)
}

func (e *engineClient) Rename(ctx context.Context, id, name string) error {
	if !engineNameRE.MatchString(name) {
		return errEngine
	}
	q := url.Values{}
	q.Set("name", name)
	return e.emptyAction(ctx, http.MethodPost, id, "/rename", q, http.StatusNoContent)
}

func (e *engineClient) Remove(ctx context.Context, id string) error {
	q := url.Values{}
	q.Set("v", "1")
	return e.emptyAction(ctx, http.MethodDelete, id, "", q, http.StatusNoContent, http.StatusNotFound)
}

func (e *engineClient) Logs(ctx context.Context, id string, lines int) ([]byte, bool, error) {
	if lines < 1 || lines > 2000 {
		return nil, false, errEngine
	}
	p, err := containerPath(id, "/logs")
	if err != nil {
		return nil, false, err
	}
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("tail", strconv.Itoa(lines))
	q.Set("timestamps", "0")
	code, body, cut, err := e.exchange(ctx, http.MethodGet, p, q, nil, nil, maxLogStream, false)
	if err != nil || code != http.StatusOK {
		return nil, false, errEngine
	}
	return body, cut, nil
}

func (e *engineClient) VolumeInspect(ctx context.Context, name string) (volumeInfo, error) {
	p, err := namedPath("/volumes/", name)
	if err != nil {
		return volumeInfo{}, err
	}
	code, body, err := e.do(ctx, http.MethodGet, p, nil, nil, nil, maxEngineJSON)
	if err != nil {
		return volumeInfo{}, errEngine
	}
	if code == http.StatusNotFound {
		return volumeInfo{}, errNotFound
	}
	if code != http.StatusOK {
		return volumeInfo{}, errEngine
	}
	var raw struct {
		Driver  string            `json:"Driver"`
		Options map[string]string `json:"Options"`
		Labels  map[string]string `json:"Labels"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return volumeInfo{}, errEngine
	}
	return volumeInfo{Driver: raw.Driver, Options: raw.Options, Labels: raw.Labels}, nil
}

func (e *engineClient) VolumeCreate(ctx context.Context, name, host, project string) error {
	return e.createNamed(ctx, "/volumes/create", name, "local", host, project)
}

func (e *engineClient) NetworkInspect(ctx context.Context, name string) (networkInfo, error) {
	p, err := namedPath("/networks/", name)
	if err != nil {
		return networkInfo{}, err
	}
	code, body, err := e.do(ctx, http.MethodGet, p, nil, nil, nil, maxEngineJSON)
	if err != nil {
		return networkInfo{}, errEngine
	}
	if code == http.StatusNotFound {
		return networkInfo{}, errNotFound
	}
	if code != http.StatusOK {
		return networkInfo{}, errEngine
	}
	var raw struct {
		Driver string            `json:"Driver"`
		Labels map[string]string `json:"Labels"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return networkInfo{}, errEngine
	}
	return networkInfo{Driver: raw.Driver, Labels: raw.Labels}, nil
}

func (e *engineClient) NetworkCreate(ctx context.Context, name, host, project string) error {
	return e.createNamed(ctx, "/networks/create", name, "bridge", host, project)
}

func (e *engineClient) createNamed(ctx context.Context, path, name, driver, host, project string) error {
	if !engineNameRE.MatchString(name) || !hostIDRE.MatchString(host) || !images.ValidProject(project) {
		return errEngine
	}
	body, err := marshalJSON(struct {
		Name   string            `json:"Name"`
		Driver string            `json:"Driver"`
		Labels map[string]string `json:"Labels"`
	}{
		Name: name, Driver: driver,
		Labels: scopeLabels(host, project),
	})
	if err != nil {
		return errEngine
	}
	code, _, err := e.do(ctx, http.MethodPost, path, nil, nil, body, maxEngineJSON)
	if err != nil {
		return errEngine
	}
	// 409: create lost a race. The caller inspects whatever is there now.
	if code == http.StatusConflict || code == http.StatusOK || code == http.StatusCreated {
		return nil
	}
	return errEngine
}

func scopeLabels(host, project string) map[string]string {
	return map[string]string{
		images.LabelOwner:   images.OwnerValue,
		images.LabelHost:    host,
		images.LabelProject: project,
	}
}

func (e *engineClient) emptyAction(ctx context.Context, method, id, suffix string, q url.Values, ok ...int) error {
	p, err := containerPath(id, suffix)
	if err != nil {
		return err
	}
	code, _, err := e.do(ctx, method, p, q, nil, nil, maxEngineJSON)
	if err != nil {
		return errEngine
	}
	for _, want := range ok {
		if code == want {
			return nil
		}
	}
	return errEngine
}

func (e *engineClient) do(ctx context.Context, method, path string, q url.Values, hdr http.Header, body []byte, limit int) (int, []byte, error) {
	code, buf, _, err := e.exchange(ctx, method, path, q, hdr, body, limit, true)
	return code, buf, err
}

// exchange builds one Engine request. strict rejects a body larger than limit.
// Logs pass strict false and keep the prefix.
func (e *engineClient) exchange(ctx context.Context, method, path string, q url.Values, hdr http.Header, body []byte, limit int, strict bool) (int, []byte, bool, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "?") || strings.Contains(path, "..") {
		return 0, nil, false, errEngine
	}
	u := &url.URL{Scheme: "http", Host: "docker", Path: engineAPI + path}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return 0, nil, false, errEngine
	}
	// Replace the header map so nothing but the fields below can be sent.
	req.Header = http.Header{}
	req.Header.Set("User-Agent", "bot-agent-runner")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if hdr != nil {
		if v := hdr.Get("X-Registry-Auth"); v != "" {
			req.Header.Set("X-Registry-Auth", v)
		}
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return 0, nil, false, errEngine
	}
	defer resp.Body.Close()
	buf, cut, err := readLimit(resp.Body, limit)
	if err != nil || (cut && strict) {
		return 0, nil, cut, errEngine
	}
	return resp.StatusCode, buf, cut, nil
}

func containerPath(id, suffix string) (string, error) {
	if !engineIDRE.MatchString(id) {
		return "", errEngine
	}
	return "/containers/" + url.PathEscape(id) + suffix, nil
}

func namedPath(prefix, name string) (string, error) {
	if !engineNameRE.MatchString(name) {
		return "", errEngine
	}
	return prefix + url.PathEscape(name), nil
}

func readCap(r io.Reader, n int) ([]byte, error) {
	b, cut, err := readLimit(r, n)
	if err != nil || cut {
		return nil, errEngine
	}
	return b, nil
}

func readLimit(r io.Reader, n int) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(n)+1))
	if err != nil {
		return nil, false, errEngine
	}
	if len(b) > n {
		return b[:n], true, nil
	}
	return b, false, nil
}

func badCredential(a *images.RegistryAuth) bool {
	if a == nil || a.Host == "" || a.Username == "" || a.Token == "" {
		return true
	}
	if len(a.Host) > 255 || len(a.Username) > 255 || len(a.Token) > 4096 {
		return true
	}
	for _, s := range []string{a.Host, a.Username, a.Token} {
		if strings.ContainsAny(s, "\r\n\x00") {
			return true
		}
	}
	return false
}

func registryHeader(a *images.RegistryAuth, host string) (string, error) {
	raw, err := marshalJSON(struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}{Username: a.Username, Password: a.Token, ServerAddress: host})
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(raw), nil
}

type pullLine struct {
	Status      string `json:"status"`
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
	Aux struct {
		ID     string `json:"ID"`
		Digest string `json:"Digest"`
	} `json:"aux"`
}

func parsePull(body []byte, want string) (string, string, error) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64), 1<<20)
	got := ""
	id := ""
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg pullLine
		if json.Unmarshal(line, &msg) != nil {
			return "", "", errEngine
		}
		if msg.Error != "" || msg.ErrorDetail.Message != "" {
			return "", "", errImage
		}
		if d, ok := digestStatus(msg.Status); ok {
			got = d
		}
		if images.ValidDigest(msg.Aux.Digest) {
			got = msg.Aux.Digest
		}
		if images.ValidDigest(msg.Aux.ID) {
			id = msg.Aux.ID
		}
	}
	if sc.Err() != nil {
		return "", "", errEngine
	}
	if want != "" && got != "" && got != want {
		return "", "", errImage
	}
	if !images.ValidDigest(got) {
		return "", "", errImage
	}
	if id == "" {
		id = got
	}
	return got, id, nil
}

func digestStatus(status string) (string, bool) {
	const prefix = "Digest: "
	if !strings.HasPrefix(status, prefix) {
		return "", false
	}
	d := strings.TrimSpace(status[len(prefix):])
	if !images.ValidDigest(d) {
		return "", false
	}
	return d, true
}

func parseList(body []byte) ([]listed, error) {
	var raw []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		State  string            `json:"State"`
		Status string            `json:"Status"`
		Labels map[string]string `json:"Labels"`
		// Health is a raw value. API v1.47 does not document this list field.
		// API v1.52 adds an object; a non-object must not fail the list.
		Health json.RawMessage `json:"Health"`
		Ports  []struct {
			IP          string `json:"IP"`
			PrivatePort int    `json:"PrivatePort"`
			PublicPort  int    `json:"PublicPort"`
			Type        string `json:"Type"`
		} `json:"Ports"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return nil, errEngine
	}
	out := make([]listed, 0, len(raw))
	for _, item := range raw {
		if !engineIDRE.MatchString(item.ID) {
			continue
		}
		names := plainNames(item.Names)
		if len(names) == 0 {
			continue
		}
		row := listed{
			ID: item.ID, Names: names, State: item.State, Image: item.Image, Labels: item.Labels,
			Health: containerHealth(item.Health, item.Status),
		}
		for _, p := range item.Ports {
			if p.Type != "" && p.Type != "tcp" {
				continue
			}
			if p.PublicPort < 1 || p.PublicPort > 65535 || p.PrivatePort < 1 || p.PrivatePort > 65535 {
				continue
			}
			if p.IP != "" && net.ParseIP(p.IP) == nil {
				continue
			}
			row.Ports = append(row.Ports, portView{HostIP: p.IP, HostPort: p.PublicPort, ContainerPort: p.PrivatePort})
		}
		out = append(out, row)
	}
	return out, nil
}

func parseInspect(body []byte) (inspected, error) {
	var raw struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Status     string `json:"Status"`
			ExitCode   int    `json:"ExitCode"`
			StartedAt  string `json:"StartedAt"`
			FinishedAt string `json:"FinishedAt"`
		} `json:"State"`
		RestartCount    int `json:"RestartCount"`
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if json.Unmarshal(body, &raw) != nil || !engineIDRE.MatchString(raw.ID) {
		return inspected{}, errEngine
	}
	out := inspected{
		ID: raw.ID, Name: strings.TrimPrefix(raw.Name, "/"), State: raw.State.Status,
		ExitCode: raw.State.ExitCode, RestartCount: raw.RestartCount,
		StartedAt: raw.State.StartedAt, FinishedAt: raw.State.FinishedAt,
		Image: raw.Config.Image, Labels: raw.Config.Labels,
	}
	for key, binds := range raw.NetworkSettings.Ports {
		cp, ok := containerPort(key)
		if !ok {
			continue
		}
		for _, b := range binds {
			hp, err := strconv.Atoi(b.HostPort)
			if err != nil || hp < 1 || hp > 65535 {
				continue
			}
			if b.HostIP != "" && net.ParseIP(b.HostIP) == nil {
				continue
			}
			out.Ports = append(out.Ports, portView{HostIP: b.HostIP, HostPort: hp, ContainerPort: cp})
		}
	}
	return out, nil
}

func containerPort(key string) (int, bool) {
	num, proto, ok := strings.Cut(key, "/")
	if !ok || (proto != "tcp" && proto != "") {
		return 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// containerHealth reads one list row. When Health is a JSON object, its
// Status is the answer and the text suffix is not consulted: none, empty,
// unknown, or a bad object is null. That object is the API v1.52 list
// field; /v1.47 omits it. Anything else uses the Status suffix Docker's
// health.String() appends: "(healthy)", "(unhealthy)", "(health: starting)".
func containerHealth(raw json.RawMessage, status string) *string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		// encoding/json matches field names case-insensitively. The Engine
		// key is Status, so look that key up and do not accept status.
		var obj map[string]json.RawMessage
		if json.Unmarshal(trimmed, &obj) != nil {
			return nil
		}
		rawStatus, ok := obj["Status"]
		if !ok {
			return nil
		}
		var s string
		if json.Unmarshal(rawStatus, &s) != nil {
			return nil
		}
		return healthCode(s)
	}
	return healthFromStatus(status)
}

func healthCode(s string) *string {
	switch s {
	case "healthy", "unhealthy", "starting":
		v := s
		return &v
	default:
		return nil
	}
}

func healthFromStatus(status string) *string {
	switch {
	case strings.HasSuffix(status, "(unhealthy)"):
		return healthCode("unhealthy")
	case strings.HasSuffix(status, "(healthy)"):
		return healthCode("healthy")
	case strings.HasSuffix(status, "(health: starting)"):
		return healthCode("starting")
	default:
		return nil
	}
}

func plainNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimPrefix(n, "/")
		if n == "" || len(n) > 255 || strings.ContainsAny(n, "/\r\n\x00") {
			continue
		}
		out = append(out, n)
	}
	return out
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
