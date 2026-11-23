// Package edge is the shared vocabulary of the per-host edge
// (agent-protocol v1.15): task kinds, helper verbs, names, labels, the
// pinned Caddy image and the request checks both the agent and the root
// helper run. It does not read the filesystem or talk to Docker.
package edge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Task kinds (server → agent) and helper verbs.
const (
	KindApply  = "edge_apply"
	KindStop   = "edge_stop"
	KindStatus = "edge_status"

	VerbApply  = "edge.apply"
	VerbStop   = "edge.stop"
	VerbStatus = "edge.status"
)

// Names (item 3). The edge has no project.
const (
	ContainerName = "deploybot-edge"
	VolumeName    = "deploybot-edge-data"
	// Dir is the config directory on the host, root 0700. It is bind-mounted
	// at MountTarget. A test root never changes this value in a create body.
	Dir         = "/var/lib/bot-agent-runner/edge"
	MountTarget = "/run/edge"
	DataTarget  = "/data"
	ConfigFile  = "config.json"
	AdminSocket = "admin.sock"
	// AdminListen is the only admin address a config may set. Caddy's admin
	// API must never listen on TCP: the container shares the host network.
	AdminListen = "unix//run/edge/admin.sock"
	// ConfigPath is the file Caddy is started with, inside the container.
	ConfigPath = MountTarget + "/" + ConfigFile

	// LabelKind value and the config hash label. The owner and host labels
	// are the images package's. The config_hash label is informational: it
	// is the hash at create time, and a later /load does not change it.
	// The applied config is config.json; status and unchanged read that.
	KindValue       = "edge"
	LabelConfigHash = "dev.deploybot.config_hash"

	// MaxConfig bounds the config object in edge_apply.
	MaxConfig = 512 * 1024
)

// PinnedImage is the Caddy image this agent release runs as the edge: the
// official multi-arch image docker.io/library/caddy:2.11.9, pinned by its
// OCI index digest (looked up on Docker Hub 2026-11-17; linux amd64, arm v6
// and v7, arm64 and others). Bump it with a new agent release, never at
// runtime. If it were the all-zero placeholder, the edge would stay off:
// hello says enabled false and the helper refuses edge_disabled before any
// Docker call, and TestReleaseGateEdgeImageDigestNotPlaceholder fails.
const PinnedImage = "docker.io/library/caddy@sha256:f971688b2aaf1a26d256de552f17fb13f64fc5ed2dee4cf40c7f9d70cea0e12d"

const placeholderDigest = "0000000000000000000000000000000000000000000000000000000000000000"

// Image is the image the agent and helper use. It is a variable only so
// tests in other packages can stand in a non-placeholder digest; production
// code never assigns it.
var Image = PinnedImage

// ImagePinned reports whether Image carries a real digest. The edge stays
// off while it does not.
func ImagePinned() bool {
	return !strings.HasSuffix(Image, "@sha256:"+placeholderDigest)
}

// Refusal codes (item 4). The image-verb codes conflict, bad_spec,
// docker_unavailable and image_unavailable are reused as well.
const (
	ReasonDisabled      = "edge_disabled"
	ReasonPortInUse     = "port_in_use"
	ReasonInvalidConfig = "invalid_config"
	ReasonBadSpec       = "bad_spec"
	ReasonConflict      = "conflict"
	ReasonDocker        = "docker_unavailable"
	ReasonImage         = "image_unavailable"
	ReasonIO            = "io_error"
	ReasonHelper        = "helper_unavailable"
	// ReasonBusy is a mutation refused while an agent upgrade runs
	// (agent-protocol v1.20 item 5).
	ReasonBusy = "busy"
)

// Outcomes and states (item 4).
const (
	OutcomeApplied   = "applied"
	OutcomeUnchanged = "unchanged"
	OutcomeStarted   = "started"
	OutcomeStopped   = "stopped"
	OutcomeAbsent    = "absent"

	StateRunning = "running"
	StateStopped = "stopped"
	StateAbsent  = "absent"
)

// MaxDetail bounds a refusal detail (a Caddy error line or a port holder).
const MaxDetail = 300

var hashRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

// IsKind reports the three edge task kinds.
func IsKind(kind string) bool {
	return kind == KindApply || kind == KindStop || kind == KindStatus
}

// IsVerb reports the three edge helper verbs.
func IsVerb(verb string) bool {
	return verb == VerbApply || verb == VerbStop || verb == VerbStatus
}

// VerbFor is the helper verb of a task kind.
func VerbFor(kind string) (string, bool) {
	switch kind {
	case KindApply:
		return VerbApply, true
	case KindStop:
		return VerbStop, true
	case KindStatus:
		return VerbStatus, true
	default:
		return "", false
	}
}

// ApplyRequest is the edge_apply spec and the edge.apply request. Config is
// the exact bytes of the config object; config_hash is their SHA-256.
type ApplyRequest struct {
	Config     json.RawMessage `json:"config"`
	ConfigHash string          `json:"config_hash"`
}

// ApplyResult is a successful edge.apply.
type ApplyResult struct {
	Outcome    string `json:"outcome"`
	ConfigHash string `json:"config_hash"`
	Image      string `json:"image"`
}

// StopResult is a successful edge.stop.
type StopResult struct {
	Outcome string `json:"outcome"`
}

// StatusResult is a successful edge.status. Image and ConfigHash are ""
// when unknown; StartedAt is null unless the edge is running.
type StatusResult struct {
	State      string  `json:"state"`
	Image      string  `json:"image"`
	ConfigHash string  `json:"config_hash"`
	StartedAt  *string `json:"started_at"`
}

// Refusal is the helper's edge refusal. Detail is a sanitised single line
// (Caddy's first error line, or the port holder) and is omitted when empty.
type Refusal struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

// HashConfig is the hex SHA-256 of the config bytes.
func HashConfig(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ErrSpec is any edge request that fails the checks. Callers map it to
// bad_spec and do not echo the input.
var ErrSpec = errors.New("edge spec")

// ParseApply runs the edge_apply checks, shared by the agent and the helper:
//   - one JSON object with exactly config and config_hash;
//   - config is a JSON object of at most MaxConfig bytes, in compact form
//     (so the bytes survive the agent-to-helper hop unchanged);
//   - config_hash is 64 lowercase hex and equals the SHA-256 of those bytes;
//   - config passes the strict schema in schema.go.
//
// Every refusal is a *SpecError (errors.Is ErrSpec) naming the offending
// path: "request" for the frame, "config_hash", or a path in the config.
func ParseApply(raw []byte) (ApplyRequest, error) {
	var req ApplyRequest
	if err := decodeStrict(raw, &req); err != nil {
		return ApplyRequest{}, bad("request")
	}
	cfg := []byte(req.Config)
	if len(cfg) == 0 || len(cfg) > MaxConfig || cfg[0] != '{' {
		return ApplyRequest{}, bad("config")
	}
	var compact bytes.Buffer
	if json.Compact(&compact, cfg) != nil || !bytes.Equal(compact.Bytes(), cfg) {
		return ApplyRequest{}, bad("config (not compact)")
	}
	if !hashRE.MatchString(req.ConfigHash) || HashConfig(cfg) != req.ConfigHash {
		return ApplyRequest{}, bad("config_hash")
	}
	if err := checkConfig(cfg); err != nil {
		return ApplyRequest{}, err
	}
	return req, nil
}

// ParseEmpty is the edge_stop and edge_status spec: exactly {}.
func ParseEmpty(raw []byte) error {
	var empty struct{}
	if err := decodeStrict(raw, &empty); err != nil {
		return ErrSpec
	}
	return nil
}

func decodeStrict(raw []byte, dest any) error {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || trim[0] != '{' {
		return ErrSpec
	}
	dec := json.NewDecoder(bytes.NewReader(trim))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return ErrSpec
	}
	return nil
}

// FixedReason keeps an allowlisted edge refusal code. Anything else is
// helper_unavailable, so a helper reply cannot put free text in error_code.
func FixedReason(code string) string {
	switch code {
	case ReasonDisabled, ReasonPortInUse, ReasonInvalidConfig, ReasonBadSpec,
		ReasonConflict, ReasonDocker, ReasonImage, ReasonIO, ReasonBusy:
		return code
	default:
		return ReasonHelper
	}
}

// Detail makes s safe to return and log: its first non-empty line, valid
// UTF-8, control characters replaced, at most MaxDetail bytes.
func Detail(s string) string {
	line := ""
	for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	line = strings.ToValidUTF8(line, "?")
	line = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, line)
	line = strings.TrimSpace(line)
	if len(line) <= MaxDetail {
		return line
	}
	cut := MaxDetail
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut]
}
