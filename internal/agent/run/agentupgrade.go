package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// helloMarker is the file the installer waits for after a restart
// (agent-protocol v1.20 item 8): /var/lib/bot-agent/last_hello.
const helloMarker = "last_hello"

// runAgentUpgrade forwards an accepted agent_upgrade task to the helper
// (v1.20 item 4). Success is state succeeded with {"state":"accepted"};
// the upgrade runs on after the task ends, and the agent then polls the
// helper's result. A refusal is state failed with policy_denied, busy,
// not_newer or bad_spec; anything else is helper_unavailable.
func (a *agent) runAgentUpgrade(ctx context.Context, task protocol.Task) protocol.TaskResult {
	a.setHelperKind(task.Kind)
	defer a.setHelperKind("")
	req, ok := agentupgrade.ParseRequest(task.Spec)
	if !ok {
		return upgradeFailed(task, programs.ReasonBadSpec)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return upgradeFailed(task, programs.ReasonHelper)
	}
	tctx, cancel := context.WithTimeout(ctx, upgradeTimeout(task.TimeoutSec))
	defer cancel()
	raw, err := a.call(tctx, agentupgrade.VerbStart, body)
	if err != nil {
		if tctx.Err() != nil {
			return timedOut(task)
		}
		return upgradeFailed(task, programs.ReasonHelper)
	}
	if code, refused := helperRefusal(raw); refused {
		a.logHelper(agentupgrade.VerbStart, code)
		switch code {
		case programs.ReasonPolicy, programs.ReasonBadSpec, agentupgrade.ReasonBusy, agentupgrade.ReasonNotNewer:
			return upgradeFailed(task, code)
		default:
			return upgradeFailed(task, programs.ReasonHelper)
		}
	}
	if !agentupgrade.DecodeStartReply(raw) {
		a.logHelper(agentupgrade.VerbStart, "decode")
		return upgradeFailed(task, programs.ReasonHelper)
	}
	a.startUpgradePoll()
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "succeeded", Payload: map[string]any{"state": agentupgrade.StateAccepted},
	}
}

// upgradeTimeout is the task's timeout_sec, at most 120 s (and 120 s when
// it is not set). The generic 60 s clamp does not apply to this kind.
func upgradeTimeout(sec int) time.Duration {
	d := time.Duration(sec) * time.Second
	if d <= 0 || d > agentupgrade.TaskTimeout {
		return agentupgrade.TaskTimeout
	}
	return d
}

func upgradeFailed(task protocol.Task, code string) protocol.TaskResult {
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "failed", ErrorCode: protocol.Str(code), Payload: map[string]any{},
	}
}

// readUpgradeReply asks the helper for result.json and whether an
// upgrade is running. ok is false when there is no answer. Only a host that
// opted in (agent_upgrade = true) can have a result, so no other host's
// helper is asked.
func (a *agent) readUpgradeReply(ctx context.Context) (agentupgrade.ResultReply, bool) {
	if !a.policy.AgentUpgradeEnabled() || a.opt.Helper == nil {
		return agentupgrade.ResultReply{}, false
	}
	hctx, cancel := context.WithTimeout(ctx, discoverHelperTimeout)
	defer cancel()
	raw, err := a.call(hctx, agentupgrade.VerbResult, []byte(`{}`))
	if err != nil {
		return agentupgrade.ResultReply{}, false
	}
	return agentupgrade.DecodeResultReply(raw)
}

// readUpgradeResult is the result part of readUpgradeReply; nil means none.
func (a *agent) readUpgradeResult(ctx context.Context) *agentupgrade.Result {
	r, ok := a.readUpgradeReply(ctx)
	if !ok {
		return nil
	}
	return r.Result
}

// upgradeSnapshot is the last result and its sequence number, for hello.
func (a *agent) upgradeSnapshot() (*agentupgrade.Result, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastUpgrade == nil {
		return nil, a.upgradeSeq
	}
	r := *a.lastUpgrade
	return &r, a.upgradeSeq
}

// markUpgradeSent records that seq reached the server (in hello or the frame).
func (a *agent) markUpgradeSent(seq uint64) {
	a.mu.Lock()
	if seq > a.upgradeSent {
		a.upgradeSent = seq
	}
	a.mu.Unlock()
}

// noteUpgradeResult stores r when it differs from the last one and wakes
// the session to send the frame. It reports whether r was new.
func (a *agent) noteUpgradeResult(r *agentupgrade.Result) bool {
	if r == nil {
		return false
	}
	a.mu.Lock()
	if a.lastUpgrade != nil && *a.lastUpgrade == *r {
		a.mu.Unlock()
		return false
	}
	c := *r
	a.lastUpgrade = &c
	a.upgradeSeq++
	a.mu.Unlock()
	a.upgradeWake.signal()
	return true
}

// sendUpgradeResult sends the frame once for a result not yet sent.
func (a *agent) sendUpgradeResult(ctx context.Context, w *writer) error {
	r, seq := a.upgradeSnapshot()
	a.mu.Lock()
	sent := a.upgradeSent
	a.mu.Unlock()
	if r == nil || seq <= sent {
		return nil
	}
	if err := w.Send(ctx, protocol.AgentUpgradeResult{V: protocol.Version, Type: agentupgrade.Frame, Result: *r}); err != nil {
		return err
	}
	a.markUpgradeSent(seq)
	return nil
}

// startUpgradePoll polls the helper's result every PollEvery for up to
// PollFor (v1.20 item 3). It runs at agent start on an opted-in host and
// again after an accepted task, which restarts the window. The result file
// is written after the installer finishes, so the new (or restored) agent
// usually starts before it exists. A result that differs from the last one
// in hello or a frame is sent. The poll stops early once no upgrade runs
// and a new result has been seen and sent. One poller runs at a time.
func (a *agent) startUpgradePoll() {
	every, window := a.opt.UpgradePollEvery, a.opt.UpgradePollFor
	if every <= 0 {
		every = agentupgrade.PollEvery
	}
	if window <= 0 {
		window = agentupgrade.PollFor
	}
	a.mu.Lock()
	a.pollUntil = time.Now().Add(window)
	a.pollSawNew = false
	if a.polling {
		a.mu.Unlock()
		return
	}
	a.polling = true
	a.mu.Unlock()
	ctx := a.life
	if ctx == nil {
		ctx = context.Background()
	}
	go a.pollUpgrade(ctx, every)
}

func (a *agent) pollUpgrade(ctx context.Context, every time.Duration) {
	for {
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			a.mu.Lock()
			a.polling = false
			a.mu.Unlock()
			return
		case <-t.C:
		}
		reply, ok := a.readUpgradeReply(ctx)
		if ok && a.noteUpgradeResult(reply.Result) {
			a.mu.Lock()
			a.pollSawNew = true
			a.mu.Unlock()
		}
		a.mu.Lock()
		done := time.Now().After(a.pollUntil) ||
			(ok && !reply.Running && a.pollSawNew && a.upgradeSent >= a.upgradeSeq)
		if done {
			a.polling = false
		}
		a.mu.Unlock()
		if done {
			return
		}
	}
}

// writeHelloMarker touches StateDir/last_hello (0600) after an
// acknowledged hello. A failure is logged once; the session goes on.
func (a *agent) writeHelloMarker() {
	path := filepath.Join(a.cfg.StateDir, helloMarker)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		now := time.Now()
		err = os.Chtimes(path, now, now)
	}
	if err != nil && a.markerOnce.CompareAndSwap(false, true) {
		a.opt.Log.Printf("bot-agent: could not write the hello marker")
	}
}
