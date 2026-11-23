// Package agentupgrade is the shared wire model of the v1.20 agent upgrade
// (docs/contracts/agent-protocol-v1.md "v1.20 agent upgrade from the
// server"): the task kind, the helper verbs, the result object and the
// version rules. The agent and the helper both import it.
package agentupgrade

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Kind is the task kind; VerbStart and VerbResult are the helper verbs;
	// Frame is the agent → API frame type.
	Kind       = "agent_upgrade"
	VerbStart  = "agent_upgrade"
	VerbResult = "agent_upgrade_result"
	Frame      = "agent_upgrade_result"

	// TaskTimeout is the agent's budget for the agent_upgrade task.
	TaskTimeout = 120 * time.Second
	// PollEvery and PollFor bound the agent's agent_upgrade_result polling
	// after an accepted task (item 3).
	PollEvery = 10 * time.Second
	PollFor   = 15 * time.Minute

	// Unit is the transient upgrade unit (item 5).
	Unit = "bot-agent-upgrade"
	// RunnerPath is the helper binary that runs `upgrade-run`.
	RunnerPath = "/usr/local/libexec/bot-agent-runner"
	// RunSubcommand is the helper subcommand the unit runs.
	RunSubcommand = "upgrade-run"
	// RuntimeMax is the unit's RuntimeMaxSec.
	RuntimeMax = 900
	// CoolDown is how long a failed or rolled-back result for a version
	// keeps the same version from starting again (item 5).
	CoolDown = 10 * time.Minute
	// LockName is the flock upgrade-run holds in /run/bot-agent for its
	// whole run; the helper's mutating verbs answer busy while it is held.
	LockName = "upgrade.lock"

	// Helper refusal codes added by v1.20 (the others are the programs ones).
	ReasonBusy     = "busy"
	ReasonNotNewer = "not_newer"

	// Outcomes.
	OutcomeSucceeded  = "succeeded"
	OutcomeRolledBack = "rolled_back"
	OutcomeFailed     = "failed"

	// Reasons.
	ReasonDownload       = "download_failed"
	ReasonSignature      = "signature_invalid"
	ReasonBundle         = "bundle_invalid"
	ReasonVersion        = "version_mismatch"
	ReasonInstall        = "install_failed"
	ReasonNotReconnected = "not_reconnected"
)

// versionPattern is the contract's version rule.
var versionPattern = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)

// ValidVersion reports whether v is X.Y.Z with 1-4 digits per part.
func ValidVersion(v string) bool {
	return versionPattern.MatchString(v)
}

// Newer reports whether a is a strictly higher X.Y.Z than b. Both must be
// valid versions; anything else is false.
func Newer(a, b string) bool {
	if !ValidVersion(a) || !ValidVersion(b) {
		return false
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// Request is the task spec and the agent_upgrade helper request.
type Request struct {
	Version string `json:"version"`
}

// ParseRequest decodes {"version":"X.Y.Z"} with no other key.
func ParseRequest(raw []byte) (Request, bool) {
	var req Request
	if !decodeStrict(raw, &req) || !ValidVersion(req.Version) {
		return Request{}, false
	}
	return req, true
}

// ParseEmpty accepts only {}.
func ParseEmpty(raw []byte) bool {
	var empty struct{}
	return decodeStrict(raw, &empty)
}

func decodeStrict(raw []byte, dest any) bool {
	if len(bytes.TrimSpace(raw)) == 0 || len(raw) > 4096 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return false
	}
	return !dec.More()
}

// Result is hello.last_upgrade, the frame's result and result.json.
type Result struct {
	Version string `json:"version"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
}

// Valid reports whether r uses the contract's tokens: a valid version, an
// allowed outcome and reason, and an RFC 3339 time.
func (r Result) Valid() bool {
	if !ValidVersion(r.Version) {
		return false
	}
	switch r.Outcome {
	case OutcomeSucceeded, OutcomeRolledBack, OutcomeFailed:
	default:
		return false
	}
	switch r.Reason {
	case "", ReasonDownload, ReasonSignature, ReasonBundle, ReasonVersion, ReasonInstall, ReasonNotReconnected:
	default:
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, r.At)
	return err == nil
}

// DecodeResult reads result.json. Anything that is not one valid Result
// object is false.
func DecodeResult(raw []byte) (Result, bool) {
	var r Result
	if !decodeStrict(raw, &r) || !r.Valid() {
		return Result{}, false
	}
	return r, true
}

// StartReply is the helper's agent_upgrade success reply.
type StartReply struct {
	OK    bool   `json:"ok"`
	State string `json:"state"`
}

// StateAccepted is the only success state.
const StateAccepted = "accepted"

// ResultReply is the helper's agent_upgrade_result reply. Running is
// helper-internal (agent and helper ship together): true while an
// upgrade-run holds its lock, so the agent's poller knows when to stop.
type ResultReply struct {
	OK      bool    `json:"ok"`
	Result  *Result `json:"result"`
	Running bool    `json:"running"`
}

// Encode marshals a reply without HTML escaping or a trailing newline.
func Encode(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte(`{"ok":false,"reason":"io_error"}`)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// DecodeStartReply reads an accepted reply.
func DecodeStartReply(raw []byte) bool {
	var r StartReply
	return decodeStrict(raw, &r) && r.OK && r.State == StateAccepted
}

// DecodeResultReply reads an agent_upgrade_result reply. ok is false when
// the bytes are not that reply; a nil result is the helper's null.
func DecodeResultReply(raw []byte) (ResultReply, bool) {
	var r ResultReply
	if !decodeStrict(raw, &r) || !r.OK {
		return ResultReply{}, false
	}
	if r.Result != nil && !r.Result.Valid() {
		return ResultReply{}, false
	}
	return r, true
}
