package run

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/discover"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/redact"
)

const (
	// startWait is the contract's start/restart observation window.
	startWait = 10 * time.Second
	startPoll = 200 * time.Millisecond
	tailLines = 50
)

func (a *agent) now() time.Time {
	if a.opt.Now != nil {
		return a.opt.Now()
	}
	return time.Now()
}

func (a *agent) sleep(ctx context.Context, d time.Duration) error {
	fn := a.opt.Sleep
	if fn == nil {
		fn = sleepCtx
	}
	return fn(ctx, d)
}

func (a *agent) helloPrograms() (*protocol.Programs, []string) {
	caps := protocol.Capabilities()
	if !a.policy.Exists {
		return nil, caps
	}
	p := &protocol.Programs{
		RunAs:      []string{},
		PolicyHash: a.policy.Hash,
	}
	if a.policy.Valid {
		p.AllowShell = a.policy.AllowShell
		if len(a.policy.RunAs) > 0 {
			p.RunAs = append([]string{}, a.policy.RunAs...)
		}
		p.Enabled = a.policy.Enabled
	}
	p.Images = a.policy.ImagesEnabled()
	p.Docker = a.dockerUp()
	p.Files = true
	if p.Enabled {
		caps = append(caps, "programs")
	}
	return p, caps
}

func (a *agent) runProgram(ctx context.Context, task protocol.Task) protocol.TaskResult {
	tmo := time.Duration(task.TimeoutSec) * time.Second
	if tmo <= 0 {
		tmo = 15 * time.Second
	}
	if tmo > 60*time.Second {
		tmo = 60 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, tmo)
	defer cancel()
	verb, ok := programs.Verb(task.Kind)
	if !ok {
		return a.helperFail(task, programs.ReasonHelper)
	}
	raw, err := a.call(tctx, verb, task.Spec)
	if err != nil {
		if tctx.Err() != nil {
			return timedOut(task)
		}
		return a.helperFail(task, programs.ReasonHelper)
	}
	reason, passed, err := peekOK(raw)
	if err != nil {
		return a.helperFail(task, programs.ReasonHelper)
	}
	if !passed {
		return a.helperFail(task, reason)
	}
	switch task.Kind {
	case "program_logs":
		return a.finishLogs(task, raw)
	case "program_start", "program_restart":
		st, reason, err := programs.DecodeStatus(raw)
		if err != nil {
			return a.helperFail(task, programs.ReasonHelper)
		}
		if reason != "" {
			return a.helperFail(task, reason)
		}
		// fresh is false when this call did not move ActiveEnterTimestampMonotonic
		// or systemctl exited non-zero. A unit that was already active is not success.
		if !programs.StatusFresh(raw) {
			return a.startFailed(tctx, task, st)
		}
		return a.waitActive(tctx, task)
	default:
		return a.finishStatus(tctx, task, raw)
	}
}

func (a *agent) waitActive(ctx context.Context, task protocol.Task) protocol.TaskResult {
	project, entry, ok := identity(task.Spec)
	if !ok {
		return a.helperFail(task, programs.ReasonHelper)
	}
	start := a.now()
	for {
		if ctx.Err() != nil {
			return timedOut(task)
		}
		st, reason, err := a.fetchStatus(ctx, project, entry)
		if err != nil {
			if ctx.Err() != nil {
				return timedOut(task)
			}
			return a.helperFail(task, programs.ReasonHelper)
		}
		if reason != "" {
			return a.helperFail(task, reason)
		}
		elapsed := a.now().Sub(start)
		switch st.ActiveState {
		case "active":
			return a.successStatus(ctx, task, st)
		case "failed":
			return a.startFailed(ctx, task, st)
		case "activating":
			if elapsed >= startWait {
				return a.successStatus(ctx, task, st)
			}
		default:
			if elapsed >= startWait {
				return a.startFailed(ctx, task, st)
			}
		}
		if err := a.sleep(ctx, startPoll); err != nil {
			return timedOut(task)
		}
	}
}

func (a *agent) finishStatus(ctx context.Context, task protocol.Task, raw []byte) protocol.TaskResult {
	st, reason, err := programs.DecodeStatus(raw)
	if err != nil {
		return a.helperFail(task, programs.ReasonHelper)
	}
	if reason != "" {
		return a.helperFail(task, reason)
	}
	return a.successStatus(ctx, task, st)
}

func (a *agent) successStatus(ctx context.Context, task protocol.Task, st programs.Status) protocol.TaskResult {
	project, entry, ok := identity(task.Spec)
	ports := []int{}
	if ok {
		if st.Unit == "" {
			if name, good := programs.UnitName(project, entry); good {
				st.Unit = name
			}
		}
		if p, _, err := a.sockets(ctx, project, entry); err == nil {
			ports = p
		}
	}
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "succeeded", Payload: statusPayload(st, ports),
	}
}

func (a *agent) startFailed(ctx context.Context, task protocol.Task, st programs.Status) protocol.TaskResult {
	project, entry, ok := identity(task.Spec)
	ports := []int{}
	lines := []string{}
	if ok {
		if st.Unit == "" {
			if name, good := programs.UnitName(project, entry); good {
				st.Unit = name
			}
		}
		if p, _, err := a.sockets(ctx, project, entry); err == nil {
			ports = p
		}
		lines = a.tail(ctx, project, entry)
	}
	payload := statusPayload(st, ports)
	payload["lines"] = lines
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "failed", ErrorCode: protocol.Str("start_failed"), Payload: payload,
	}
}

func (a *agent) finishLogs(task protocol.Task, raw []byte) protocol.TaskResult {
	batch, reason, err := programs.DecodeLogs(raw)
	if err != nil {
		return a.helperFail(task, programs.ReasonHelper)
	}
	if reason != "" {
		return a.helperFail(task, reason)
	}
	lines, cut := redact.Journal(batch.Lines)
	if lines == nil {
		lines = []string{}
	}
	project, entry, _ := identity(task.Spec)
	unit, _ := programs.UnitName(project, entry)
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "succeeded",
		Payload: map[string]any{
			"unit": unit, "lines": lines, "truncated": cut || batch.Truncated,
		},
	}
}

func (a *agent) tail(ctx context.Context, project, entry string) []string {
	body, err := json.Marshal(struct {
		Project string `json:"project"`
		Entry   string `json:"entry"`
		Lines   int    `json:"lines"`
	}{project, entry, tailLines})
	if err != nil {
		return []string{}
	}
	raw, err := a.call(ctx, "logs", body)
	if err != nil {
		return []string{}
	}
	batch, reason, err := programs.DecodeLogs(raw)
	if err != nil || reason != "" {
		return []string{}
	}
	lines, _ := redact.Journal(batch.Lines)
	if lines == nil {
		return []string{}
	}
	return lines
}

func (a *agent) fetchStatus(ctx context.Context, project, entry string) (programs.Status, string, error) {
	raw, err := a.call(ctx, "status", refBody(project, entry))
	if err != nil {
		return programs.Status{}, "", err
	}
	return programs.DecodeStatus(raw)
}

func (a *agent) sockets(ctx context.Context, project, entry string) ([]int, *string, error) {
	raw, err := a.call(ctx, "sockets", refBody(project, entry))
	if err != nil {
		return nil, nil, err
	}
	ports, user, reason, err := programs.DecodeSockets(raw)
	if err != nil || reason != "" {
		return nil, nil, errors.New("sockets")
	}
	if ports == nil {
		ports = []int{}
	}
	return ports, user, nil
}

func (a *agent) listUnits(ctx context.Context) ([]programs.UnitInfo, error) {
	raw, err := a.call(ctx, "list", []byte("{}"))
	if err != nil {
		return nil, err
	}
	units, reason, err := programs.DecodeList(raw)
	if err != nil || reason != "" {
		return nil, errors.New("list")
	}
	return units, nil
}

func (a *agent) ownedPrograms(ctx context.Context) ([]discover.Owned, error) {
	units, err := a.listUnits(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]discover.Owned, 0, len(units))
	for _, u := range units {
		if u.ActiveState != "active" && u.ActiveState != "activating" {
			continue
		}
		o := discover.Owned{
			Unit: u.Unit, Project: u.Project, Entry: u.Entry, Ports: []int{},
		}
		if ports, user, err := a.sockets(ctx, u.Project, u.Entry); err == nil {
			o.Ports = ports
			o.User = user
		}
		out = append(out, o)
	}
	return out, nil
}

func (a *agent) call(ctx context.Context, verb string, body []byte) ([]byte, error) {
	if a.opt.Helper == nil || !programs.KnownVerb(verb) || len(body) > programs.FrameLimit(verb) {
		return nil, errHelper
	}
	return a.opt.Helper.Call(ctx, verb, body)
}

func (a *agent) helperFail(task protocol.Task, reason string) protocol.TaskResult {
	payloadReason := programs.ReasonHelper
	if reason != programs.ReasonHelper {
		payloadReason = programs.FixedReason(reason)
	}
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "failed", ErrorCode: protocol.Str(programs.ReasonPolicy),
		Payload: map[string]any{"reason": payloadReason},
	}
}

func timedOut(task protocol.Task) protocol.TaskResult {
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "timed_out", ErrorCode: protocol.Str("timed_out"), Payload: map[string]any{},
	}
}

func statusPayload(st programs.Status, ports []int) map[string]any {
	if ports == nil {
		ports = []int{}
	}
	var pid any
	if st.MainPID != nil {
		pid = *st.MainPID
	}
	var started any
	if st.StartedAt != nil {
		started = *st.StartedAt
	}
	return map[string]any{
		"unit": st.Unit, "active_state": st.ActiveState, "sub_state": st.SubState,
		"main_pid": pid, "started_at": started, "ports": ports,
	}
}

func identity(spec json.RawMessage) (string, string, bool) {
	var m struct {
		Project string `json:"project"`
		Entry   string `json:"entry"`
	}
	if json.Unmarshal(spec, &m) != nil || m.Project == "" || m.Entry == "" {
		return "", "", false
	}
	return m.Project, m.Entry, true
}

func refBody(project, entry string) []byte {
	b, err := json.Marshal(struct {
		Project string `json:"project"`
		Entry   string `json:"entry"`
	}{project, entry})
	if err != nil {
		return []byte("{}")
	}
	return b
}

func peekOK(raw []byte) (string, bool, error) {
	var wrap struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return "", false, err
	}
	if !wrap.OK {
		if wrap.Reason == "" {
			return "", false, errors.New("helper")
		}
		return wrap.Reason, false, nil
	}
	return "", true, nil
}
