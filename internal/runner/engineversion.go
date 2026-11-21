package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/redact"
)

// apiVersion is an Engine API version such as 1.43 (agent-protocol v1.17).
type apiVersion struct {
	major, minor int
}

var (
	// apiMax is the version the helper is written to. A newer daemon is
	// spoken to at apiMax.
	apiMax = apiVersion{1, 47}
	// apiMin is the floor: HostConfig.CgroupnsMode needs 1.41 (Docker 20.10).
	apiMin = apiVersion{1, 41}
	// apiNetworks is the first version that takes more than one network
	// endpoint at create and refuses a duplicate network name (Docker 25).
	apiNetworks = apiVersion{1, 44}

	apiVersionRE = regexp.MustCompile(`^([0-9]{1,3})\.([0-9]{1,3})$`)
)

func parseAPIVersion(s string) (apiVersion, bool) {
	m := apiVersionRE.FindStringSubmatch(s)
	if m == nil {
		return apiVersion{}, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return apiVersion{major, minor}, true
}

func (v apiVersion) String() string {
	return strconv.Itoa(v.major) + "." + strconv.Itoa(v.minor)
}

func (v apiVersion) less(w apiVersion) bool {
	if v.major != w.major {
		return v.major < w.major
	}
	return v.minor < w.minor
}

// maxEngineMessage is the longest Engine message the helper logs.
const maxEngineMessage = 300

// minScrubbed is the shortest secret value a logged Engine message may be
// redacted for. With a shorter one in the request, a redacted message could
// still carry it in a form exact redaction misses, so it is withheld.
const minScrubbed = 6

// engineMessageWithheld replaces the Engine message in that case.
const engineMessageWithheld = "message withheld"

// pingTimeout is the longest the version ping may take, so a daemon that
// accepts the connection but never answers cannot hold the helper (or the
// installer's docker_check) until the caller's own deadline. A variable
// so tests can shorten it.
var pingTimeout = 5 * time.Second

// negotiate asks the daemon for its API version once per engineClient,
// which is once per helper run: GET /_ping (unversioned) and its
// API-Version header. The client then speaks the lower of that and apiMax.
// A daemon below apiMin, or one that does not answer the ping, is errEngine
// (docker_unavailable). A missing or unreadable header is taken as apiMin.
// The ping has its own deadline, pingTimeout or ctx's if that is sooner.
func (e *engineClient) negotiate(ctx context.Context) error {
	if e.pinged {
		return e.verErr
	}
	e.pinged = true
	e.verErr = errEngine
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		return errEngine
	}
	req.Header = http.Header{}
	req.Header.Set("User-Agent", "bot-agent-runner")
	resp, err := e.http.Do(req)
	if err != nil {
		e.logLine("bot-agent-runner: docker: GET /_ping: no answer")
		return errEngine
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.logLine("bot-agent-runner: docker: GET /_ping: status %d", resp.StatusCode)
		return errEngine
	}
	v, ok := parseAPIVersion(resp.Header.Get("API-Version"))
	if !ok {
		v = apiMin
		e.assumed = true
		e.logLine("bot-agent-runner: docker: no usable API-Version header; assuming %s", apiMin)
	}
	e.daemon = v
	if v.less(apiMin) {
		e.logLine("bot-agent-runner: docker API %s is older than the minimum %s (Docker 20.10)", v, apiMin)
		return errEngine
	}
	e.ver = v
	if apiMax.less(v) {
		e.ver = apiMax
	}
	e.verErr = nil
	return nil
}

// APIVersion is the negotiated version, negotiating first if needed.
func (e *engineClient) APIVersion(ctx context.Context) (apiVersion, error) {
	if err := e.negotiate(ctx); err != nil {
		return apiVersion{}, err
	}
	return e.ver, nil
}

func (e *engineClient) logLine(format string, args ...any) {
	if e.logf != nil {
		e.logf(format, args...)
	}
}

// loggedStatus reports whether an Engine answer is a failure worth a
// journal line. A 404 on a GET or DELETE is a lookup's answer, and a 409
// on a volume or network create is a lost race the caller accepts.
func loggedStatus(method, path string, code int) bool {
	if code < 400 {
		return false
	}
	if code == http.StatusNotFound && (method == http.MethodGet || method == http.MethodDelete) {
		return false
	}
	if code == http.StatusConflict && (path == "/volumes/create" || path == "/networks/create") {
		return false
	}
	return true
}

// logFailure is the agent-protocol v1.17 item 4 line: method, path (never
// the query), status, the Engine message cut to 300 bytes, and the
// negotiated version. Request bodies and headers are never logged; the
// request's secret values are cut from the message.
func (e *engineClient) logFailure(method, path string, code int, body []byte) {
	if e.logf == nil {
		return
	}
	if code == 0 {
		e.logf("bot-agent-runner: docker: %s %s: no answer (API %s)", method, path, e.ver)
		return
	}
	e.logf("bot-agent-runner: docker: %s %s: status %d (API %s): %s", method, path, code, e.ver, engineMessage(body, e.scrub))
}

// engineMessage is the "message" of an Engine error body, redacted, cut to
// maxEngineMessage bytes and quoted so it stays on one line. When a scrub
// value is shorter than minScrubbed bytes the message is withheld.
func engineMessage(body []byte, scrub []string) string {
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.Message == "" {
		return `""`
	}
	for _, v := range scrub {
		if len(v) < minScrubbed {
			return engineMessageWithheld
		}
	}
	s := redact.NewExact(scrub...).Text(msg.Message)
	if len(s) > maxEngineMessage {
		s = s[:maxEngineMessage]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return strconv.Quote(s)
}

// createEnvValues is the env values of a container create body, so an
// Engine message that echoes one is not logged with it.
func createEnvValues(body []byte) []string {
	var raw struct {
		Env []string `json:"Env"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return nil
	}
	out := make([]string, 0, len(raw.Env))
	for _, kv := range raw.Env {
		if _, v, ok := strings.Cut(kv, "="); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// needNetworks refuses a deploy with shared networks on a daemon below
// apiNetworks (agent-protocol v1.17 item 2). It runs before any change.
func needNetworks(ctx context.Context, sys Sys, eng engine) string {
	v, err := eng.APIVersion(ctx)
	if err != nil {
		return engReason(err)
	}
	if v.less(apiNetworks) {
		if sys.Logf != nil {
			sys.Logf("bot-agent-runner: shared networks need Docker 25 (API %s); this host speaks API %s", apiNetworks, v)
		}
		return programs.ReasonNetworksDocker
	}
	return ""
}

// DockerCheck is the docker_check answer (agent-protocol v1.17 item 5).
// API is the daemon's version, empty when unknown or when the ping had no
// usable API-Version header (the helper then assumes apiMin). Reason is one
// of the dockerCheck* phrases.
type DockerCheck struct {
	Present    bool   `json:"present"`
	API        string `json:"api"`
	OK         bool   `json:"ok"`
	NetworksOK bool   `json:"networks_ok"`
	Reason     string `json:"reason"`
}

const (
	dockerCheckOK      = "ok"
	dockerCheckAssumed = "ok, API assumed 1.41"
	dockerCheckAbsent  = "absent"
	dockerCheckSilent  = "not answering"
	dockerCheckTooOld  = "too old"
	dockerCheckBadSpec = "bad request"
)

// doDockerCheck is the installer's Docker check. It is not gated by the
// runner policy and changes nothing: one ping, and a stat of the socket.
func doDockerCheck(ctx context.Context, in []byte, sys Sys) []byte {
	if images.Decode(in, &images.ListRequest{}) != nil {
		return images.Encode(DockerCheck{Reason: dockerCheckBadSpec})
	}
	sock := dockerSock(sys)
	if _, err := os.Stat(sock); err != nil {
		return images.Encode(DockerCheck{Reason: dockerCheckAbsent})
	}
	eng := newEngine(sock, sys.Logf)
	err := eng.negotiate(ctx)
	out := DockerCheck{Present: true}
	if eng.daemon != (apiVersion{}) && !eng.assumed {
		out.API = eng.daemon.String()
	}
	switch {
	case err == nil:
		out.OK = true
		out.NetworksOK = !eng.ver.less(apiNetworks)
		out.Reason = dockerCheckOK
		if eng.assumed {
			out.Reason = dockerCheckAssumed
		}
	case eng.daemon != (apiVersion{}):
		out.Reason = dockerCheckTooOld
	default:
		out.Reason = dockerCheckSilent
	}
	return images.Encode(out)
}
