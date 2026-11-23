package run

import (
	"context"
	"encoding/json"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/results"
)

// Detached tasks (agent-protocol v1.21 item 5). An image deploy or rollback,
// its health wait and its automatic rollback (a job run is an image deploy
// too) run on Run's context, not the session's, so they finish when the API
// connection drops or the session restarts. The final result is written to
// the results store before it is sent, and every stored result is sent
// again after each hello until the server answers task_result_ack.

// detachedKind reports the task kinds that outlive their session.
func detachedKind(kind string) bool {
	return kind == "image_deploy" || kind == "image_rollback"
}

// taskBase is the context a task runs on: Run's for a detached kind, the
// session's otherwise. An agent built without Run (some tests) has no life
// context and keeps the session's.
func (a *agent) taskBase(sess context.Context, kind string) context.Context {
	if detachedKind(kind) && a.life != nil {
		return a.life
	}
	return sess
}

// beginConn records a new acknowledged connection and its writer. It
// returns the connection's number.
func (a *agent) beginConn(w *writer) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connSeq++
	a.curW = w
	return a.connSeq
}

// endConn forgets w when it is still the current writer.
func (a *agent) endConn(w *writer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.curW == w {
		a.curW = nil
	}
}

// currentConn is the number of the connection a task starts on.
func (a *agent) currentConn() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connSeq
}

// maxTaskConn bounds the start records. A record is dropped when its
// result is acked, is not stored, or leaves the store; this cap only guards
// against a server that never acks while tasks keep arriving.
const maxTaskConn = 1000

// noteStarted remembers which connection a detached task started on, so a
// resend can say late. At the cap it records nothing: that task's resends
// then say late, the safe default.
func (a *agent) noteStarted(id string, conn uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.taskConn == nil {
		a.taskConn = map[string]uint64{}
	}
	if len(a.taskConn) >= maxTaskConn {
		return
	}
	a.taskConn[id] = conn
}

// forgetStarted drops a task's start record.
func (a *agent) forgetStarted(id string) {
	a.mu.Lock()
	delete(a.taskConn, id)
	a.mu.Unlock()
}

// stopping reports that Run's context has ended (the agent is stopping).
func (a *agent) stopping() bool {
	return a.life != nil && a.life.Err() != nil
}

// sendCurrent sends v on the current connection, if any. A failed send is
// fine: a stored result goes again after the next hello.
func (a *agent) sendCurrent(v any) {
	a.mu.Lock()
	w := a.curW
	a.mu.Unlock()
	if w == nil {
		return
	}
	ctx := a.life
	if ctx == nil {
		ctx = context.Background()
	}
	_ = w.Send(ctx, v)
}

// withLate is res with late set when its task started on a connection
// other than the current one. A result read back after a restart has no
// record and is always late.
func (a *agent) withLate(res protocol.TaskResult) protocol.TaskResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	started, ok := a.taskConn[res.TaskID]
	res.Late = !ok || started != a.connSeq
	return res
}

// keepDetached stores a detached task's final result: on disk when the
// results store works, else in memory like any other result.
// A result kept in memory is not stored, so its start record goes (it is
// replayed without late, as before v1.21).
func (a *agent) keepDetached(res protocol.TaskResult) {
	if a.results != nil {
		if err := a.results.Put(res); err == nil {
			return
		}
		a.opt.Log.Printf("bot-agent: could not store task_result id=%s; kept in memory", quoteLog(res.TaskID))
	}
	a.storeResult(res)
	a.forgetStarted(res.TaskID)
}

// storedResult is a detached result still waiting for its ack.
func (a *agent) storedResult(id string) (protocol.TaskResult, bool) {
	if a.results == nil {
		return protocol.TaskResult{}, false
	}
	res, ok := a.results.Get(id)
	if !ok {
		return protocol.TaskResult{}, false
	}
	return a.withLate(res), true
}

// replayStored sends every stored result after hello, oldest first.
func (a *agent) replayStored(ctx context.Context, w *writer) error {
	if a.results == nil {
		return nil
	}
	list := a.results.List()
	a.forgetUnstored(list)
	for _, res := range list {
		if err := w.Send(ctx, a.withLate(res)); err != nil {
			return err
		}
	}
	return nil
}

// forgetUnstored drops start records whose result is no longer stored (an
// older server never acks; the store's caps remove those files) and whose
// task is not running, so the map stays as small as the store.
func (a *agent) forgetUnstored(list []protocol.TaskResult) {
	keep := make(map[string]bool, len(list))
	for _, res := range list {
		keep[res.TaskID] = true
	}
	a.mu.Lock()
	ids := make([]string, 0, len(a.taskConn))
	for id := range a.taskConn {
		if !keep[id] {
			ids = append(ids, id)
		}
	}
	a.mu.Unlock()
	for _, id := range ids {
		if a.inflight.has(id) {
			continue
		}
		a.mu.Lock()
		delete(a.taskConn, id)
		a.mu.Unlock()
	}
}

// onResultAck is task_result_ack: delete the stored file. The in-memory
// copy of a result that could not be stored goes too.
func (a *agent) onResultAck(raw []byte) {
	var ack protocol.TaskResultAck
	if err := json.Unmarshal(raw, &ack); err != nil || ack.TaskID == "" {
		a.opt.Log.Printf("bot-agent: ignored invalid frame")
		return
	}
	if a.results != nil && results.ValidTaskID(ack.TaskID) {
		if err := a.results.Delete(ack.TaskID); err != nil {
			a.opt.Log.Printf("bot-agent: results delete")
		}
	}
	a.pending.ack(ack.TaskID)
	a.mu.Lock()
	delete(a.taskConn, ack.TaskID)
	a.mu.Unlock()
}
