package run

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// hostFilesStatusTimeout bounds one host_file_status helper call. The
// session loop waits for it, as for the discovery calls.
const hostFilesStatusTimeout = discoverHelperTimeout

// runHostFile forwards an accepted host_file_apply task (agent-protocol
// v1.18 item 4, rev 1a A4/A5) to the helper with the 600 s budget, whatever
// the frame's timeout_sec says. outcome succeeded or
// unchanged is state succeeded; every other outcome is state failed with
// error_code the outcome. The payload is always {outcome, stage, reason,
// took_ms[, sha256]}. A helper refusal or transport failure is state
// failed with error_code bad_spec, policy_denied, io_error or
// helper_unavailable and a payload without outcome.
func (a *agent) runHostFile(ctx context.Context, task protocol.Task) protocol.TaskResult {
	// The spec holds the file bytes, secret ones included. Zero it when the
	// task is done, as runProgram does.
	defer clear(task.Spec)
	a.setHelperKind(task.Kind)
	defer a.setHelperKind("")
	defer a.hostFilesWake.signal()
	secret := a.hostFileSecret(task.Spec)
	start := a.opt.Now()
	tctx, cancel := context.WithTimeout(ctx, hostfiles.TaskTimeout)
	defer cancel()
	raw, err := a.call(tctx, hostfiles.VerbApply, task.Spec)
	took := a.opt.Now().Sub(start).Milliseconds()
	if err != nil {
		if tctx.Err() != nil {
			return timedOut(task)
		}
		return hostFileFailed(task, programs.ReasonHelper, took)
	}
	if code, refused := helperRefusal(raw); refused {
		a.logHelper(hostfiles.VerbApply, code)
		return hostFileFailed(task, code, took)
	}
	var r hostfiles.ApplyResult
	if hostfiles.DecodeStrict(raw, &r) != nil || !hostfiles.ValidResult(r) {
		a.logHelper(hostfiles.VerbApply, "decode")
		return hostFileFailed(task, programs.ReasonHelper, took)
	}
	payload := map[string]any{
		"outcome": r.Outcome, "stage": r.Stage, "reason": r.Reason, "took_ms": r.TookMS,
	}
	// The agent drops the hash of a secret file itself (it knows from the
	// policy), whatever the helper sent.
	if r.SHA256 != "" && !secret {
		payload["sha256"] = r.SHA256
	}
	res := protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "succeeded", Payload: payload,
	}
	if r.Outcome != hostfiles.OutcomeSucceeded && r.Outcome != hostfiles.OutcomeUnchanged {
		res.State = "failed"
		res.ErrorCode = protocol.Str(r.Outcome)
	}
	return res
}

// hostFileSecret reports whether the spec names a secret file. An
// undecodable spec counts as secret, so nothing is relayed for it.
func (a *agent) hostFileSecret(spec []byte) bool {
	var req struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(spec, &req) != nil {
		return true
	}
	f, ok := a.policy.HostFile(req.Name)
	return !ok || f.Secret
}

// helperRefusal reads the helper's {"ok":false,"reason":code} shape.
func helperRefusal(raw []byte) (string, bool) {
	var m struct {
		OK     *bool  `json:"ok"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &m) != nil || m.OK == nil || *m.OK {
		return "", false
	}
	return programs.FixedReason(m.Reason), true
}

func hostFileFailed(task protocol.Task, code string, took int64) protocol.TaskResult {
	if took < 0 {
		took = 0
	}
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "failed", ErrorCode: protocol.Str(code),
		// No outcome: payload.outcome is only ever one of the contract's
		// seven. The API maps these error codes to a generic error.
		Payload: map[string]any{"stage": "", "reason": "", "took_ms": took},
	}
}

// hostFilesStatus asks the helper for host_file_status. ok is false when
// the policy has no host files or the helper did not answer.
func (a *agent) hostFilesStatus(ctx context.Context) (protocol.HostFilesStatus, bool) {
	if !a.policy.Valid || len(a.policy.HostFiles) == 0 || a.opt.Helper == nil {
		return protocol.HostFilesStatus{}, false
	}
	hctx, cancel := context.WithTimeout(ctx, hostFilesStatusTimeout)
	defer cancel()
	raw, err := a.call(hctx, hostfiles.VerbStatus, []byte(`{}`))
	if err != nil {
		return protocol.HostFilesStatus{}, false
	}
	if code, refused := helperRefusal(raw); refused {
		a.logHelper(hostfiles.VerbStatus, code)
		return protocol.HostFilesStatus{}, false
	}
	var s hostfiles.StatusResult
	if hostfiles.DecodeStrict(raw, &s) != nil || !hostfiles.ValidStatus(s) {
		a.logHelper(hostfiles.VerbStatus, "decode")
		return protocol.HostFilesStatus{}, false
	}
	// A secret file never carries size or sha256, whatever the helper sent;
	// a name the policy does not know is treated the same.
	for i := range s.Files {
		if f, ok := a.policy.HostFile(s.Files[i].Name); !ok || f.Secret {
			s.Files[i].Size = nil
			s.Files[i].SHA256 = ""
		}
	}
	return protocol.HostFilesStatus{V: protocol.Version, Type: hostfiles.FrameStatus, Files: s.Files}, true
}

// sendHostFiles sends one host_files_status frame when there is one.
// A helper failure skips the frame; only a send error ends the session.
func (a *agent) sendHostFiles(ctx context.Context, w *writer) error {
	f, ok := a.hostFilesStatus(ctx)
	if !ok {
		return nil
	}
	return w.Send(ctx, f)
}

// wakeSignal is a one-slot wake-up channel.
type wakeSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

func (s *wakeSignal) wake() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch == nil {
		s.ch = make(chan struct{}, 1)
	}
	return s.ch
}

func (s *wakeSignal) signal() {
	ch := s.wake()
	select {
	case ch <- struct{}{}:
	default:
	}
}
