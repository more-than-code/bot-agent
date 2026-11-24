package run

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/dockerusage"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

const (
	// dockerUsageDefault is the budget when the frame sets none (the API
	// sends 45 s); maxDockerUsage is the generic 60 s task clamp.
	dockerUsageDefault = 45 * time.Second
	maxDockerUsage     = 60 * time.Second
)

// runDockerUsage answers a docker_usage task (agent-protocol v1.25) with
// the helper's docker_df summary. Success is state succeeded with
// {"outcome":"ok","summary":…}; anything else is state failed with
// {"outcome":"failed","reason":code} and error_code the same code.
func (a *agent) runDockerUsage(ctx context.Context, task protocol.Task) protocol.TaskResult {
	a.setHelperKind(task.Kind)
	defer a.setHelperKind("")
	// The helper's gate, checked here first so a host without the helper
	// gets the same answer.
	if !a.policy.ImagesEnabled() && !a.policy.EdgeEnabled() {
		return dockerUsageFailed(task, dockerusage.ReasonDisabled)
	}
	tctx, cancel := context.WithTimeout(ctx, dockerUsageTimeout(task.TimeoutSec))
	defer cancel()
	raw, err := a.call(tctx, dockerusage.Verb, []byte(`{}`))
	if err != nil {
		if errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return dockerUsageFailed(task, dockerusage.ReasonTimeout)
		}
		return dockerUsageFailed(task, dockerusage.ReasonUnavailable)
	}
	if code, refused := dockerUsageRefusal(raw); refused {
		a.logHelper(dockerusage.Verb, code)
		return dockerUsageFailed(task, code)
	}
	summary, ok := dockerusage.DecodeReply(raw)
	if !ok {
		a.logHelper(dockerusage.Verb, "decode")
		return dockerUsageFailed(task, dockerusage.ReasonUnavailable)
	}
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "succeeded", Payload: map[string]any{"outcome": "ok", "summary": summary},
	}
}

// dockerUsageRefusal reads the helper's {"error": code}. Only the v1.25
// codes are kept; any other refusal, or a malformed one, is
// docker_unavailable. A reply without "error" is not a refusal.
func dockerUsageRefusal(raw []byte) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "", false
	}
	if _, ok := m["error"]; !ok {
		return "", false
	}
	var r struct {
		Error string `json:"error"`
	}
	if images.Decode(raw, &r) != nil {
		return dockerusage.ReasonUnavailable, true
	}
	switch r.Error {
	case dockerusage.ReasonDisabled, dockerusage.ReasonTimeout:
		return r.Error, true
	default:
		return dockerusage.ReasonUnavailable, true
	}
}

func dockerUsageTimeout(sec int) time.Duration {
	tmo := time.Duration(sec) * time.Second
	if tmo <= 0 {
		tmo = dockerUsageDefault
	}
	if tmo > maxDockerUsage {
		tmo = maxDockerUsage
	}
	return tmo
}

func dockerUsageFailed(task protocol.Task, code string) protocol.TaskResult {
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "failed", ErrorCode: protocol.Str(code),
		Payload: map[string]any{"outcome": "failed", "reason": code},
	}
}
