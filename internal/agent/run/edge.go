package run

import (
	"context"
	"encoding/json"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/edge"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

const (
	// edgeDefaultTimeout covers a pull of the Caddy image and the start
	// wait when the frame sets no timeout. maxEdgeTimeout caps the frame.
	edgeDefaultTimeout = 120 * time.Second
	maxEdgeTimeout     = 600 * time.Second
)

// runEdge runs an accepted edge task (agent-protocol v1.15 item 4) through
// the helper. A success is state succeeded with the helper's result object.
// A refusal is state failed, error_code the refusal code, and payload
// {"outcome": code, "detail": text}.
func (a *agent) runEdge(ctx context.Context, task protocol.Task) protocol.TaskResult {
	a.setHelperKind(task.Kind)
	defer a.setHelperKind("")
	if task.Kind == edge.KindApply && !edgeOn(a.policy) {
		return edgeFailed(task, edge.ReasonDisabled, "")
	}
	verb, ok := edge.VerbFor(task.Kind)
	if !ok {
		return edgeFailed(task, edge.ReasonHelper, "")
	}
	tmo := time.Duration(task.TimeoutSec) * time.Second
	if tmo <= 0 {
		tmo = edgeDefaultTimeout
	}
	if tmo > maxEdgeTimeout {
		tmo = maxEdgeTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, tmo)
	defer cancel()
	raw, err := a.call(tctx, verb, task.Spec)
	if err != nil {
		if tctx.Err() != nil {
			return timedOut(task)
		}
		return edgeFailed(task, edge.ReasonHelper, "")
	}
	if code, detail, refused := edgeRefusal(raw); refused {
		a.logHelper(verb, code)
		return edgeFailed(task, code, detail)
	}
	payload, ok := edgePayload(task.Kind, raw)
	if !ok {
		a.logHelper(verb, "decode")
		return edgeFailed(task, edge.ReasonHelper, "")
	}
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "succeeded", Payload: payload,
	}
}

// edgeOn is the hello edge.enabled flag: the policy key, and a release
// whose Caddy digest is not the placeholder.
func edgeOn(p policy.Policy) bool {
	return p.EdgeEnabled() && edge.ImagePinned()
}

// edgeRefusal reads {"error": code[, "detail": text]}. The code is
// allowlisted and the detail sanitised again.
func edgeRefusal(raw []byte) (string, string, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", "", false
	}
	if _, ok := m["error"]; !ok {
		return "", "", false
	}
	var r edge.Refusal
	if err := images.Decode(raw, &r); err != nil || r.Error == "" {
		return edge.ReasonHelper, "", true
	}
	return edge.FixedReason(r.Error), edge.Detail(r.Detail), true
}

// edgePayload decodes the helper's result strictly and checks its enums.
func edgePayload(kind string, raw []byte) (map[string]any, bool) {
	switch kind {
	case edge.KindApply:
		var r edge.ApplyResult
		if images.Decode(raw, &r) != nil {
			return nil, false
		}
		switch r.Outcome {
		case edge.OutcomeApplied, edge.OutcomeUnchanged, edge.OutcomeStarted:
		default:
			return nil, false
		}
		return map[string]any{"outcome": r.Outcome, "config_hash": r.ConfigHash, "image": r.Image}, true
	case edge.KindStop:
		var r edge.StopResult
		if images.Decode(raw, &r) != nil || (r.Outcome != edge.OutcomeStopped && r.Outcome != edge.OutcomeAbsent) {
			return nil, false
		}
		return map[string]any{"outcome": r.Outcome}, true
	case edge.KindStatus:
		var r edge.StatusResult
		if images.Decode(raw, &r) != nil {
			return nil, false
		}
		switch r.State {
		case edge.StateRunning, edge.StateStopped, edge.StateAbsent:
		default:
			return nil, false
		}
		var started any
		if r.StartedAt != nil {
			started = *r.StartedAt
		}
		return map[string]any{
			"state": r.State, "image": r.Image, "config_hash": r.ConfigHash, "started_at": started,
		}, true
	default:
		return nil, false
	}
}

func edgeFailed(task protocol.Task, code, detail string) protocol.TaskResult {
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "failed", ErrorCode: protocol.Str(code),
		Payload: map[string]any{"outcome": code, "detail": detail},
	}
}
