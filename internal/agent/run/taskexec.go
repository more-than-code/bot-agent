package run

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/tasks"
)

const (
	maxRunningTasks   = 4
	maxWaitingTasks   = 16
	maxPendingResults = 100
)

func (a *agent) handleTask(ctx context.Context, w *writer, raw []byte) {
	var task protocol.Task
	if err := json.Unmarshal(raw, &task); err != nil || task.V != protocol.Version || task.TaskID == "" {
		a.opt.Log.Printf("bot-agent: ignored invalid frame")
		return
	}
	if res, ok := a.pending.get(task.TaskID); ok {
		_ = w.Send(ctx, res)
		return
	}
	// Cheap checks run here, before any goroutine. A task already running or
	// waiting is left to that goroutine so a second frame cannot reject it.
	if code, _ := a.classify(task); code != "" {
		if a.inflight.has(task.TaskID) {
			return
		}
		a.sendReject(ctx, w, task.TaskID, code)
		return
	}
	a.taskMu.Lock()
	if a.taskRunning+a.taskWaiting >= maxRunningTasks+maxWaitingTasks {
		a.taskMu.Unlock()
		if a.inflight.has(task.TaskID) {
			return
		}
		a.sendReject(ctx, w, task.TaskID, "busy")
		return
	}
	if !a.inflight.start(task.TaskID) {
		a.taskMu.Unlock()
		return
	}
	a.taskWaiting++
	a.taskMu.Unlock()
	go a.runAdmitted(ctx, w, task)
}

func (a *agent) runAdmitted(ctx context.Context, w *writer, task protocol.Task) {
	defer a.inflight.done(task.TaskID)
	select {
	case <-ctx.Done():
		a.taskMu.Lock()
		a.taskWaiting--
		a.taskMu.Unlock()
		return
	case a.taskSlots <- struct{}{}:
	}
	a.taskMu.Lock()
	a.taskWaiting--
	a.taskRunning++
	a.taskMu.Unlock()
	defer func() {
		<-a.taskSlots
		a.taskMu.Lock()
		a.taskRunning--
		a.taskMu.Unlock()
	}()
	if a.opt.OnTaskStart != nil {
		a.opt.OnTaskStart()
	}
	if rej, res, ok := a.execute(ctx, task); !ok {
		_ = w.Send(ctx, rej)
	} else {
		a.storeResult(res)
		_ = w.Send(ctx, res)
	}
}

func (a *agent) sendReject(ctx context.Context, w *writer, id, code string) {
	a.opt.Log.Printf("bot-agent: task_reject code=%s", quoteLog(code))
	_ = w.Send(ctx, protocol.TaskReject{
		V: protocol.Version, Type: "task_reject", TaskID: id, ErrorCode: code,
	})
}

func (a *agent) classify(task protocol.Task) (string, tasks.ProbeSpec) {
	a.mu.Lock()
	services := append([]protocol.Discovered(nil), a.services...)
	offset := a.offset
	a.mu.Unlock()
	return tasks.Decide(task, a.cfg.HostID, a.opt.Now().Add(offset), services)
}

func (a *agent) execute(ctx context.Context, task protocol.Task) (protocol.TaskReject, protocol.TaskResult, bool) {
	code, spec := a.classify(task)
	if code != "" {
		a.opt.Log.Printf("bot-agent: task_reject code=%s", quoteLog(code))
		return protocol.TaskReject{
			V: protocol.Version, Type: "task_reject", TaskID: task.TaskID, ErrorCode: code,
		}, protocol.TaskResult{}, false
	}
	tmo := time.Duration(task.TimeoutSec) * time.Second
	if tmo <= 0 {
		tmo = 15 * time.Second
	}
	if tmo > 60*time.Second {
		tmo = 60 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, tmo)
	defer cancel()
	now := a.opt.Now()
	_, view, _ := a.col.SampleHost(now)
	_, procs, _ := a.col.SampleProcs(now, a.serviceFor)
	res := tasks.Run(tctx, task, spec, tasks.Env{Now: now, Host: view, Procs: procs, Timeout: tmo})
	if tctx.Err() != nil && res.State == "succeeded" && task.Kind == "host_snapshot" {
		res = protocol.TaskResult{
			V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
			State: "timed_out", ErrorCode: protocol.Str("timed_out"), Payload: map[string]any{},
		}
	}
	return protocol.TaskReject{}, res, true
}

func (a *agent) storeResult(res protocol.TaskResult) {
	if dropped := a.pending.put(res); dropped != "" {
		a.opt.Log.Printf("bot-agent: drop unacked task_result id=%s", quoteLog(dropped))
	}
}

type pendingTasks struct {
	mu    sync.Mutex
	m     map[string]protocol.TaskResult
	order []string
}

// put stores a result. A repeat of the same task id replaces the body and
// does not count as a new result. Past 100 new results, the oldest is dropped.
func (p *pendingTasks) put(r protocol.TaskResult) (dropped string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]protocol.TaskResult{}
	}
	if _, ok := p.m[r.TaskID]; ok {
		p.m[r.TaskID] = r
		return ""
	}
	if len(p.order) >= maxPendingResults {
		dropped = p.order[0]
		p.order = append([]string(nil), p.order[1:]...)
		delete(p.m, dropped)
	}
	p.order = append(p.order, r.TaskID)
	p.m[r.TaskID] = r
	return dropped
}

func (p *pendingTasks) get(id string) (protocol.TaskResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.m[id]
	return r, ok
}

func (p *pendingTasks) ack(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.m[id]; !ok {
		return
	}
	delete(p.m, id)
	for i, cur := range p.order {
		if cur == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
}

func (p *pendingTasks) list() []protocol.TaskResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]protocol.TaskResult, 0, len(p.m))
	for _, r := range p.m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out
}

type inflight struct {
	mu sync.Mutex
	m  map[string]bool
}

func (f *inflight) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[id]
}

func (f *inflight) start(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m[id] {
		return false
	}
	if f.m == nil {
		f.m = map[string]bool{}
	}
	f.m[id] = true
	return true
}

func (f *inflight) done(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, id)
}
