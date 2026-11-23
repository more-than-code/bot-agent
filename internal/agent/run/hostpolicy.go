package run

import (
	"context"
	"encoding/json"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/hostpolicy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// hostPolicyTimeout is the helper call budget for host_policy_apply.
const hostPolicyTimeout = 30 * time.Second

// runHostPolicy forwards a host_policy_apply task to the helper
// (agent-protocol v1.22 item 2). Success is state succeeded with
// {"state":"accepted","new_hash":...}: the transient unit then restarts
// this agent, and the server learns the outcome from the next hello's
// policy_hash. A refusal is state failed with policy_denied, bad_spec,
// conflict or busy; anything else is helper_unavailable.
func (a *agent) runHostPolicy(ctx context.Context, task protocol.Task) protocol.TaskResult {
	a.setHelperKind(task.Kind)
	defer a.setHelperKind("")
	req, ok := hostpolicy.ParseRequest(task.Spec)
	if !ok {
		return policyFailed(task, programs.ReasonBadSpec)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return policyFailed(task, programs.ReasonHelper)
	}
	tctx, cancel := context.WithTimeout(ctx, hostPolicyTimeout)
	defer cancel()
	raw, err := a.call(tctx, hostpolicy.Verb, body)
	if err != nil {
		if tctx.Err() != nil {
			return timedOut(task)
		}
		return policyFailed(task, programs.ReasonHelper)
	}
	if code, refused := helperRefusal(raw); refused {
		a.logHelper(hostpolicy.Verb, code)
		switch code {
		case programs.ReasonPolicy, programs.ReasonBadSpec, hostpolicy.ReasonConflict, hostpolicy.ReasonBusy:
			return policyFailed(task, code)
		default:
			return policyFailed(task, programs.ReasonHelper)
		}
	}
	reply, ok := hostpolicy.DecodeStartReply(raw)
	if !ok {
		a.logHelper(hostpolicy.Verb, "decode")
		return policyFailed(task, programs.ReasonHelper)
	}
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "succeeded", Payload: map[string]any{"state": hostpolicy.StateAccepted, "new_hash": reply.NewHash},
	}
}

func policyFailed(task protocol.Task, code string) protocol.TaskResult {
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "failed", ErrorCode: protocol.Str(code), Payload: map[string]any{},
	}
}
