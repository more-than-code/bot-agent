// Package hostpolicy is the shared wire model of the v1.21 console host
// policy (docs/contracts/agent-protocol-v1.md "v1.21 host policy from the
// console"): the task kind, the helper verb, the request, the reply and the
// result file. The agent and the helper both import it.
//
// A host whose root set console_policy = true in runner.toml lets org
// owners and admins change three keys from the console: edge, images and
// projects. The helper checks and stages the new file; a transient unit
// writes it, restarts bot-agent, waits for a hello and puts the previous
// file back when none comes.
package hostpolicy

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

const (
	// Kind is the task kind; Verb is the helper verb.
	Kind = "host_policy_apply"
	Verb = "host_policy_apply"

	// Unit is the transient unit that writes the file and restarts the agent.
	Unit = "bot-agent-policy"
	// RunSubcommand is the helper subcommand the unit runs.
	RunSubcommand = "policy-run"
	// RuntimeMax is the unit's RuntimeMaxSec.
	RuntimeMax = 300

	// RestartDelay lets the agent send the accepted task result before
	// policy-run restarts it.
	RestartDelay = 3 * time.Second
	// HelloWait is how long policy-run waits for a hello after a restart,
	// the same as the v1.20 installer's marker wait.
	HelloWait = 90 * time.Second

	// MaxImages bounds the images list a request may set.
	MaxImages = 64
	// MaxProjects bounds the projects list (the policy's own limit).
	MaxProjects = 32

	// StateAccepted is the only success state of the verb.
	StateAccepted = "accepted"

	// Refusal codes added by v1.21 (the others are the programs ones:
	// policy_denied, bad_spec, io_error).
	ReasonConflict = "conflict"
	ReasonBusy     = "busy"

	// Outcomes and reasons of result.json.
	OutcomeSucceeded  = "succeeded"
	OutcomeRolledBack = "rolled_back"
	OutcomeFailed     = "failed"

	ReasonNotReconnected = "not_reconnected"
	ReasonChanged        = "changed"
	ReasonWrite          = "write_failed"
	ReasonRestart        = "restart_failed"
)

// Request is the task spec and the helper request.
type Request struct {
	BaseHash string   `json:"base_hash"`
	Edge     bool     `json:"edge"`
	Images   []string `json:"images"`
	Projects []string `json:"projects"`
}

// ParseRequest decodes the spec with no other key: base_hash is the hex
// SHA-256 of the file the change was made against, images are valid
// patterns (at most MaxImages), projects valid slugs (at most MaxProjects),
// and neither list repeats an item.
func ParseRequest(raw []byte) (Request, bool) {
	var req Request
	if !decodeStrict(raw, &req) || !isHex64(req.BaseHash) || req.Images == nil || req.Projects == nil {
		return Request{}, false
	}
	if len(req.Images) > MaxImages || len(req.Projects) > MaxProjects {
		return Request{}, false
	}
	seen := map[string]bool{}
	for _, p := range req.Images {
		if !images.ValidPattern(p) || seen["i"+p] {
			return Request{}, false
		}
		seen["i"+p] = true
	}
	for _, p := range req.Projects {
		if !images.ValidProject(p) || seen["p"+p] {
			return Request{}, false
		}
		seen["p"+p] = true
	}
	return req, true
}

// StartReply is the helper's success reply. NewHash is the SHA-256 of the
// staged file: the server knows the change is live when a hello carries it
// as programs.policy_hash.
type StartReply struct {
	OK      bool   `json:"ok"`
	State   string `json:"state"`
	NewHash string `json:"new_hash"`
}

// DecodeStartReply reads an accepted reply.
func DecodeStartReply(raw []byte) (StartReply, bool) {
	var r StartReply
	if !decodeStrict(raw, &r) || !r.OK || r.State != StateAccepted || !isHex64(r.NewHash) {
		return StartReply{}, false
	}
	return r, true
}

// Result is result.json, written by policy-run for the host's root and the
// journal. The server reads the outcome from hello policy_hash instead.
type Result struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Hash    string `json:"hash"`
	At      string `json:"at"`
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

func decodeStrict(raw []byte, dest any) bool {
	if len(bytes.TrimSpace(raw)) == 0 || len(raw) > 16*1024 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return false
	}
	return !dec.More()
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
