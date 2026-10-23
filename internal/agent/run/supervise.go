package run

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// Health failure supervisor (agent-protocol v1.9 item 4).
const (
	superviseEvery  = 15 * time.Second
	actionWindow    = time.Hour
	maxAutoPending  = 100
	autoActionGave  = "gave_up"
	autoReasonBytes = 200
)

// A supervisor action is a 60 s helper call plus a 5 s status read on its
// own context, 65 s in all (agent-protocol v1.9 rev 3). The call must
// outlast the helper's stop grace (images.StopGraceSec) so a container that
// ignores SIGTERM is still restarted inside the call; the helper also
// finishes a started stop on its own if the call ends first. They are
// variables only so tests can shrink them.
var (
	actionCallTimeout   = 60 * time.Second
	actionStatusTimeout = 5 * time.Second
)

// entryLocks is the per-(project, entry) lock shared by image tasks and the
// supervisor. Tasks take the shared side, so tasks for one entry still run
// side by side as before. The supervisor only tries the exclusive side: it
// never waits, and it skips an entry while any image task holds the lock.
// A task waits for a running supervisor action only within its own context
// (agent-protocol v1.9 rev 2), which sync.RWMutex cannot do.
type entryLocks struct {
	mu sync.Mutex
	m  map[string]*entryLock
}

func (l *entryLocks) get(project, entry string) *entryLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*entryLock{}
	}
	key := project + "/" + entry
	lk, ok := l.m[key]
	if !ok {
		lk = &entryLock{}
		l.m[key] = lk
	}
	return lk
}

// entryLock is a shared/exclusive lock whose shared side can be cancelled.
// free is closed when the exclusive holder unlocks.
type entryLock struct {
	mu      sync.Mutex
	readers int
	writer  bool
	free    chan struct{}
}

// rLock takes the shared side, waiting for an exclusive holder until ctx ends.
func (l *entryLock) rLock(ctx context.Context) error {
	for {
		l.mu.Lock()
		if !l.writer {
			l.readers++
			l.mu.Unlock()
			return nil
		}
		ch := l.free
		l.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (l *entryLock) rUnlock() {
	l.mu.Lock()
	l.readers--
	l.mu.Unlock()
}

// tryLock takes the exclusive side only when nobody holds the lock.
func (l *entryLock) tryLock() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.writer || l.readers > 0 {
		return false
	}
	l.writer = true
	l.free = make(chan struct{})
	return true
}

func (l *entryLock) unlock() {
	l.mu.Lock()
	l.writer = false
	close(l.free)
	l.mu.Unlock()
}

// superEntry is the in-memory supervisor state for one entry. It is lost on
// agent restart and cleared by every image_deploy or image_rollback task.
type superEntry struct {
	since      time.Time   // first unhealthy poll of the current streak; zero is none
	actions    []time.Time // automatic actions in the last hour
	rolledBack bool        // an automatic rollback happened since the last task
	gaveUp     bool        // gave_up was reported; ignore until the next task
}

type supervisor struct {
	mu sync.Mutex
	m  map[string]*superEntry
}

func (s *supervisor) entry(key string) *superEntry {
	if s.m == nil {
		s.m = map[string]*superEntry{}
	}
	e, ok := s.m[key]
	if !ok {
		e = &superEntry{}
		s.m[key] = e
	}
	return e
}

// reset forgets everything about one entry. Image deploy and rollback tasks
// call it when they finish.
func (s *supervisor) reset(project, entry string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, project+"/"+entry)
}

// endStreak clears the unhealthy streak of one entry, or of every entry
// not in keep when key is "".
func (s *supervisor) endStreak(key string, keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		if e, ok := s.m[key]; ok {
			e.since = time.Time{}
		}
		return
	}
	for k, e := range s.m {
		if !keep[k] {
			e.since = time.Time{}
		}
	}
}

// decide records one unhealthy poll and returns the action to take: restart,
// rollback, gave_up, or "" for nothing. The caller holds the entry lock.
func (s *supervisor) decide(key string, now time.Time, pol images.ContainerPolicy, hasPrev bool) (string, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(key)
	if e.gaveUp {
		return "", 0
	}
	if e.since.IsZero() {
		e.since = now
	}
	for len(e.actions) > 0 && now.Sub(e.actions[0]) >= actionWindow {
		e.actions = e.actions[1:]
	}
	long := now.Sub(e.since)
	if long < time.Duration(pol.UnhealthyAfterSec)*time.Second {
		return "", 0
	}
	blocked := len(e.actions) >= pol.MaxActionsPerHour
	if pol.OnUnhealthy == images.UnhealthyRollback && (e.rolledBack || !hasPrev) {
		blocked = true
	}
	if blocked {
		e.gaveUp = true
		return autoActionGave, long
	}
	e.actions = append(e.actions, now)
	e.since = time.Time{}
	if pol.OnUnhealthy == images.UnhealthyRollback {
		e.rolledBack = true
	}
	return pol.OnUnhealthy, long
}

func (a *agent) supervise(ctx context.Context) {
	t := time.NewTicker(a.opt.withDefaults().SuperviseEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.superviseOnce(ctx)
		}
	}
}

// superviseOnce is one poll. A failed list ends every streak: an unhealthy
// streak must be seen on every poll.
func (a *agent) superviseOnce(ctx context.Context) {
	// v1.9 rev 2: without a host label the agent cannot tell its own
	// containers from another host's, so it does nothing.
	host := a.machineHost()
	if host == "" {
		return
	}
	items, _, err := a.listContainers(ctx)
	if err != nil {
		a.super.endStreak("", nil)
		return
	}
	allow := map[string]bool{}
	for _, p := range a.policy.Projects {
		allow[p] = true
	}
	seen := map[string]bool{}
	for _, it := range items {
		project, entry, ok := supervised(it, host, allow)
		if !ok {
			continue
		}
		key := project + "/" + entry
		seen[key] = true
		if it.State != "running" || healthOf(it) != "unhealthy" {
			a.super.endStreak(key, nil)
			continue
		}
		a.superviseEntry(ctx, it, items, project, entry, host)
	}
	a.super.endStreak("", seen)
}

// supervised is an owned current container with an on_unhealthy action.
// -prev and -new slots are skipped by the name check.
func supervised(it images.ContainerInfo, host string, allow map[string]bool) (string, string, bool) {
	if it.Policy == nil || it.Policy.OnUnhealthy == images.UnhealthyNone {
		return "", "", false
	}
	project, entry := it.Labels[images.LabelProject], it.Labels[images.LabelEntry]
	if !allow[project] || !images.ValidProject(project) || !images.ValidEntry(entry) {
		return "", "", false
	}
	if !images.OwnsLabels(it.Labels, project, entry, host) {
		return "", "", false
	}
	want, ok := images.ContainerName(project, entry)
	if !ok || strings.TrimPrefix(it.Name, "/") != want {
		return "", "", false
	}
	return project, entry, true
}

// findPrev is the entry's kept container: the <project>-<entry>-prev name
// with the same owner, project, entry and host labels supervised() checks.
// A container that only squats on the name is not -prev.
func findPrev(items []images.ContainerInfo, project, entry, host string) (images.ContainerInfo, bool) {
	name, ok := images.PrevName(project, entry)
	if !ok {
		return images.ContainerInfo{}, false
	}
	for _, it := range items {
		if strings.TrimPrefix(it.Name, "/") != name {
			continue
		}
		if images.OwnsLabels(it.Labels, project, entry, host) {
			return it, true
		}
	}
	return images.ContainerInfo{}, false
}

func (a *agent) superviseEntry(ctx context.Context, it images.ContainerInfo, items []images.ContainerInfo, project, entry, host string) {
	lk := a.entries.get(project, entry)
	if !lk.tryLock() {
		return
	}
	defer lk.unlock()
	prev, hasPrev := findPrev(items, project, entry, host)
	now := a.now()
	action, long := a.super.decide(project+"/"+entry, now, *it.Policy, hasPrev)
	if action == "" {
		return
	}
	name := strings.TrimPrefix(it.Name, "/")
	result := "ok"
	var digest *string
	if action == autoActionGave {
		digest = validDigest(it.Digest)
	} else {
		// The digest the action should leave running, from this poll:
		// the current container for restart, -prev for rollback. It is
		// used when the status read after the action cannot say.
		known := validDigest(it.Digest)
		if action == images.UnhealthyRollback {
			known = validDigest(prev.Digest)
		}
		result, digest = a.autoAct(ctx, action, project, entry, known)
	}
	reason := fmt.Sprintf("unhealthy for %ds", int(long/time.Second))
	if len(reason) > autoReasonBytes {
		reason = reason[:autoReasonBytes]
	}
	if dropped := a.autoq.add(protocol.AutoAction{
		V: protocol.Version, Type: "auto_action", ID: uuid.NewString(),
		T: protocol.FormatTime(now), Project: project, Entry: entry, Container: name,
		Action: action, Reason: reason, Result: result, Digest: digest,
	}); dropped != "" {
		a.opt.Log.Printf("bot-agent: drop unacked auto_action id=%s", quoteLog(dropped))
	}
	a.opt.Log.Printf("bot-agent: auto_action action=%s project=%s entry=%s result=%s", quoteLog(action), quoteLog(project), quoteLog(entry), quoteLog(result))
}

// autoAct runs one restart or rollback. A failed helper call is result
// error with a null digest and no status read. Otherwise the digest is the
// running container's, read with its own context after the call; known
// when that read fails or the running container has no digest; null when
// the read shows nothing running.
func (a *agent) autoAct(ctx context.Context, action, project, entry string, known *string) (string, *string) {
	verb := images.VerbRestart
	var dest any = &images.ActionResult{}
	if action == images.UnhealthyRollback {
		verb = images.VerbRollback
		dest = &images.IDResponse{}
	}
	body := images.Encode(images.EntryRequest{Project: project, Entry: entry})
	cctx, cancel := context.WithTimeout(ctx, actionCallTimeout)
	code, err := a.callImage(cctx, verb, body, dest)
	cancel()
	if err != nil || code != "" {
		return "error", nil
	}
	sctx, cancel := context.WithTimeout(ctx, actionStatusTimeout)
	defer cancel()
	resp, code, err := a.imageStatus(sctx, project, entry)
	if err != nil || code != "" {
		return "ok", known
	}
	if resp.Status.State != "running" {
		// The read worked and nothing is running.
		return "ok", nil
	}
	if d := validDigest(resp.Status.Digest); d != nil {
		return "ok", d
	}
	return "ok", known
}

func validDigest(d *string) *string {
	if d == nil || !images.ValidDigest(*d) {
		return nil
	}
	v := *d
	return &v
}

// autoQueue holds auto_action frames until auto_action_ack. Past 100 the
// oldest is dropped. wake is signalled on every add so a connected session
// sends the frame without waiting for a reconnect.
type autoQueue struct {
	mu    sync.Mutex
	items []protocol.AutoAction
	ch    chan struct{}
}

func (q *autoQueue) wake() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ch == nil {
		q.ch = make(chan struct{}, 1)
	}
	return q.ch
}

// add queues f and returns the id of a dropped frame, or "".
func (q *autoQueue) add(f protocol.AutoAction) string {
	q.mu.Lock()
	dropped := ""
	if len(q.items) >= maxAutoPending {
		dropped = q.items[0].ID
		q.items = append([]protocol.AutoAction(nil), q.items[1:]...)
	}
	q.items = append(q.items, f)
	if q.ch == nil {
		q.ch = make(chan struct{}, 1)
	}
	ch := q.ch
	q.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
	return dropped
}

func (q *autoQueue) ack(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, f := range q.items {
		if f.ID == id {
			q.items = append(q.items[:i:i], q.items[i+1:]...)
			return
		}
	}
}

func (q *autoQueue) list() []protocol.AutoAction {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]protocol.AutoAction(nil), q.items...)
}

// sendAuto sends every queued frame this connection has not sent yet.
// sent is per connection, so a reconnect resends everything unacked.
func (a *agent) sendAuto(ctx context.Context, w *writer, sent map[string]bool) error {
	for _, f := range a.autoq.list() {
		if sent[f.ID] {
			continue
		}
		if err := w.Send(ctx, f); err != nil {
			return err
		}
		sent[f.ID] = true
	}
	return nil
}

func (a *agent) onAutoAck(raw []byte) {
	var ack protocol.AutoActionAck
	if err := json.Unmarshal(raw, &ack); err != nil || ack.ID == "" {
		a.opt.Log.Printf("bot-agent: ignored invalid frame")
		return
	}
	a.autoq.ack(ack.ID)
}
