package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// agent-protocol v1.9 rev 3b.

const pinnedPrev = "ghcr.io/joe-and-his-friends/app@" + digestB

func entryReq(entry string) images.EntryRequest {
	return images.EntryRequest{Project: "mohiguide", Entry: entry}
}

func hasName(e *imgEnv, name string) bool { return containerNames(e)[name] }

func TestLeftoverOldRemovedByEveryVerb(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		setup func(e *imgEnv)
		verb  string
		body  func(t *testing.T) []byte
	}{
		{"service deploy", "web", func(*imgEnv) {}, images.VerbDeploy, func(t *testing.T) []byte { return mustJSON(t, serviceDeploy(hashC)) }},
		{"job deploy", "job", func(*imgEnv) {}, images.VerbDeploy, func(t *testing.T) []byte { return mustJSON(t, jobDeploy()) }},
		{"rollback, current not running", "web", func(e *imgEnv) {
			e.fake.add("mohiguide-web", "exited", "alpine:latest", e.labels("web"))
			e.fake.add("mohiguide-web-prev", "exited", pinnedPrev, e.labels("web"))
		}, images.VerbRollback, func(t *testing.T) []byte { return mustJSON(t, entryReq("web")) }},
		{"rollback, current running", "web", func(e *imgEnv) {
			e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
			e.fake.add("mohiguide-web-prev", "exited", pinnedPrev, e.labels("web"))
		}, images.VerbRollback, func(t *testing.T) []byte { return mustJSON(t, entryReq("web")) }},
		{"remove", "web", func(e *imgEnv) {
			e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
		}, images.VerbRemove, func(t *testing.T) []byte { return mustJSON(t, entryReq("web")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			root := e.useStore(t)
			oldName := "mohiguide-" + tc.entry + "-old"
			e.fake.add(oldName, "exited", "alpine:latest", e.labels(tc.entry))
			// The parked original's files, as rollbackRunning leaves them.
			newSlot := "mohiguide-" + tc.entry + ".new"
			plantSlot(t, root, newSlot, map[string]string{"app.conf": "old"})
			tc.setup(e)
			if raw := e.call(tc.verb, tc.body(t)); imgCode(t, raw) != "" {
				t.Fatalf("%s", raw)
			}
			if hasName(e, oldName) {
				t.Fatalf("leftover kept: %v", containerNames(e))
			}
			if _, err := os.Lstat(filepath.Join(root, newSlot)); !os.IsNotExist(err) {
				t.Fatalf(".new slot kept: %v", err)
			}
		})
	}
}

// A full cap that includes the leftover still admits a deploy, because the
// removed -old is counted out.
func TestLeftoverOldFreesCapSlot(t *testing.T) {
	e := newImgEnv(t, strings.Replace(imagePolicy, "max_containers = 20", "max_containers = 2", 1))
	e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	e.fake.add("mohiguide-web-old", "exited", "alpine:latest", e.labels("web"))
	if raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC))); imgCode(t, raw) != "" {
		t.Fatalf("%s", raw)
	}
	names := containerNames(e)
	if names["mohiguide-web-old"] || !names["mohiguide-web"] || !names["mohiguide-web-prev"] {
		t.Fatalf("%v", names)
	}
}

func TestSquatterOldIsConflict(t *testing.T) {
	squatters := map[string]func(e *imgEnv) map[string]string{
		"wrong entry": func(e *imgEnv) map[string]string { return e.labels("api") },
		"wrong host": func(e *imgEnv) map[string]string {
			l := e.labels("web")
			l[images.LabelHost] = "0000000000000000"
			return l
		},
		"no labels": func(*imgEnv) map[string]string { return nil },
	}
	verbs := []struct {
		name  string
		setup func(e *imgEnv)
		verb  string
		body  func(t *testing.T) []byte
	}{
		{"deploy", func(*imgEnv) {}, images.VerbDeploy, func(t *testing.T) []byte { return mustJSON(t, serviceDeploy(hashC)) }},
		{"rollback", func(e *imgEnv) {
			e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
			e.fake.add("mohiguide-web-prev", "exited", pinnedPrev, e.labels("web"))
		}, images.VerbRollback, func(t *testing.T) []byte { return mustJSON(t, entryReq("web")) }},
		{"remove", func(e *imgEnv) {
			e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
		}, images.VerbRemove, func(t *testing.T) []byte { return mustJSON(t, entryReq("web")) }},
	}
	for sq, labels := range squatters {
		for _, v := range verbs {
			t.Run(sq+" "+v.name, func(t *testing.T) {
				e := newImgEnv(t, imagePolicy)
				id := e.fake.add("mohiguide-web-old", "running", "alpine:latest", labels(e))
				v.setup(e)
				namesBefore := containerNames(e)
				before := len(e.fake.Calls())
				raw := e.call(v.verb, v.body(t))
				if imgCode(t, raw) != programs.ReasonConflict {
					t.Fatalf("%s", raw)
				}
				if mutatedSince(e, before) || !sameNames(namesBefore, containerNames(e)) {
					t.Fatalf("touched: %v", containerNames(e))
				}
				if _, st := state(e, "mohiguide-web-old"); st != "running" || currentByID(e, id) == nil {
					t.Fatalf("squatter %s", st)
				}
			})
		}
	}
}

func currentByID(e *imgEnv, id string) *fakeCtr {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	return e.fake.byID[id]
}

func TestStopErrorStillRestarts(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	id := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	e.fake.mu.Lock()
	e.fake.stopFail = true
	e.fake.mu.Unlock()
	raw := e.call(images.VerbRestart, mustJSON(t, entryReq("web")))
	if imgCode(t, raw) != programs.ReasonDocker {
		t.Fatalf("%s", raw)
	}
	if gotID, st := state(e, "mohiguide-web"); gotID != id || st != "running" {
		t.Fatalf("container %s %s", gotID, st)
	}
}

func TestStopErrorRollbackRestoresOriginal(t *testing.T) {
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	e.fake.mu.Lock()
	e.fake.stopFail = true
	e.fake.mu.Unlock()
	before := len(e.fake.Calls())
	raw := e.call(images.VerbRollback, mustJSON(t, entryReq("web")))
	assertRestored(t, e, raw, before, orig, true)
	assertSlotsRestored(t, root)
}

func captureLog(e *imgEnv) func() string {
	var mu sync.Mutex
	var buf strings.Builder
	e.sys.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&buf, format+"\n", args...)
	}
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// A failed final remove after -prev started is still a successful
// rollback, is logged, and the leftover it leaves is cleared by the next
// verb together with its .new file slot.
func TestRollbackFinalRemoveFails(t *testing.T) {
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	prevID, _ := state(e, "mohiguide-web-prev")
	logs := captureLog(e)
	e.fake.mu.Lock()
	e.fake.removeFailID = orig
	e.fake.mu.Unlock()
	raw := e.call(images.VerbRollback, mustJSON(t, entryReq("web")))
	var id images.IDResponse
	if imgCode(t, raw) != "" || json.Unmarshal(raw, &id) != nil || id.ContainerID != prevID {
		t.Fatalf("%s", raw)
	}
	if gotID, st := state(e, "mohiguide-web"); gotID != prevID || st != "running" {
		t.Fatalf("current %s %s", gotID, st)
	}
	if !hasName(e, "mohiguide-web-old") {
		t.Fatal("no leftover to clear")
	}
	if !strings.Contains(logs(), "could not remove the old container") {
		t.Fatalf("log %q", logs())
	}
	newSlot := filepath.Join(root, "mohiguide-web.new")
	if _, err := os.Lstat(newSlot); err != nil {
		t.Fatalf(".new slot: %v", err)
	}
	e.fake.mu.Lock()
	e.fake.removeFailID = ""
	e.fake.mu.Unlock()
	if raw := e.call(images.VerbRemove, mustJSON(t, entryReq("web"))); imgCode(t, raw) != "" {
		t.Fatalf("remove %s", raw)
	}
	if hasName(e, "mohiguide-web-old") {
		t.Fatal("leftover kept")
	}
	if _, err := os.Lstat(newSlot); !os.IsNotExist(err) {
		t.Fatal(".new slot kept")
	}
}

func shrinkFinish(t *testing.T, finish time.Duration) {
	t.Helper()
	old := finishBudget
	finishBudget = finish
	t.Cleanup(func() { finishBudget = old })
}

// The stop uses up the whole finish budget; the recovery context still
// starts the original.
func TestRestartRecoversAfterFinishBudget(t *testing.T) {
	shrinkFinish(t, 100*time.Millisecond)
	e := newImgEnv(t, imagePolicy)
	id := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	e.fake.mu.Lock()
	e.fake.stopDelay = 400 * time.Millisecond
	e.fake.mu.Unlock()
	raw := e.call(images.VerbRestart, mustJSON(t, entryReq("web")))
	if imgCode(t, raw) != programs.ReasonDocker {
		t.Fatalf("%s", raw)
	}
	if gotID, st := state(e, "mohiguide-web"); gotID != id || st != "running" {
		t.Fatalf("container %s %s", gotID, st)
	}
}

func TestRollbackRecoversAfterFinishBudget(t *testing.T) {
	shrinkFinish(t, 100*time.Millisecond)
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	e.fake.mu.Lock()
	e.fake.stopDelay = 400 * time.Millisecond
	e.fake.mu.Unlock()
	before := len(e.fake.Calls())
	raw := e.call(images.VerbRollback, mustJSON(t, entryReq("web")))
	assertRestored(t, e, raw, before, orig, true)
	assertSlotsRestored(t, root)
}

// The supervisor's pattern: an action whose caller gives up mid-stop (it
// reports error) leaves the container running, so the next action works.
// Before rev 3 the first call left the container exited.
func TestActionsContinueAfterCallerTimeout(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	id := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	e.fake.mu.Lock()
	e.fake.stopDelay = 200 * time.Millisecond
	e.fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if raw := Handle(ctx, images.VerbRestart, mustJSON(t, entryReq("web")), e.sys); imgCode(t, raw) != programs.ReasonDocker {
		t.Fatalf("first %s", raw)
	}
	if _, st := state(e, "mohiguide-web"); st != "running" {
		t.Fatalf("after first action %s", st)
	}
	e.fake.mu.Lock()
	e.fake.stopDelay = 0
	e.fake.mu.Unlock()
	if raw := e.call(images.VerbRestart, mustJSON(t, entryReq("web"))); imgCode(t, raw) != "" {
		t.Fatalf("second %s", raw)
	}
	starts := 0
	for _, c := range e.fake.Calls() {
		if strings.HasSuffix(c.Path, "/"+id+"/start") {
			starts++
		}
	}
	if gotID, st := state(e, "mohiguide-web"); gotID != id || st != "running" || starts != 2 {
		t.Fatalf("container %s %s starts %d", gotID, st, starts)
	}
}

func TestLeftoverOldRunningIsStoppedAndRemoved(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	old := e.fake.add("mohiguide-web-old", "running", "alpine:latest", e.labels("web"))
	before := len(e.fake.Calls())
	if raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC))); imgCode(t, raw) != "" {
		t.Fatalf("%s", raw)
	}
	var stopped, deleted bool
	for _, c := range e.fake.Calls()[before:] {
		stopped = stopped || c.Path == "/v1.47/containers/"+old+"/stop"
		deleted = deleted || (c.Method == "DELETE" && c.Path == "/v1.47/containers/"+old)
	}
	if !stopped || !deleted || currentByID(e, old) != nil {
		t.Fatalf("stopped %v deleted %v", stopped, deleted)
	}
}

// A leftover that cannot be removed fails the verb before anything else.
func TestLeftoverOldRemoveFailureTouchesNothingElse(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	cur := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	old := e.fake.add("mohiguide-web-old", "exited", "alpine:latest", e.labels("web"))
	e.fake.mu.Lock()
	e.fake.removeFailID = old
	e.fake.mu.Unlock()
	before := len(e.fake.Calls())
	raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))
	if imgCode(t, raw) != programs.ReasonDocker {
		t.Fatalf("%s", raw)
	}
	for _, c := range e.fake.Calls()[before:] {
		if c.Method != "GET" && !strings.Contains(c.Path, old) {
			t.Fatalf("touched %s %s", c.Method, c.Path)
		}
	}
	if _, st := state(e, "mohiguide-web"); st != "running" || currentByID(e, cur) == nil || currentByID(e, old) == nil {
		t.Fatalf("state %s", st)
	}
}

func assertCleanLog(t *testing.T, log, root string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(log, w) {
			t.Fatalf("log %q lacks %q", log, w)
		}
	}
	if strings.Contains(log, "mohiguide") || strings.Contains(log, root) || strings.Contains(log, "/v1.47") {
		t.Fatalf("log has names or paths: %q", log)
	}
}

// The .new slot fails its safety check after -prev has started: the
// rollback still succeeds and the failed cleanup is logged.
func TestRollbackNewSlotRemoveFailureLogged(t *testing.T) {
	e, root := twoFileDeploys(t)
	prevID, _ := state(e, "mohiguide-web-prev")
	logs := captureLog(e)
	newSlot := filepath.Join(root, "mohiguide-web.new")
	e.fake.mu.Lock()
	e.fake.startHook = func(id string) {
		if id == prevID {
			_ = os.Chmod(newSlot, 0o755)
		}
	}
	e.fake.mu.Unlock()
	raw := e.call(images.VerbRollback, mustJSON(t, entryReq("web")))
	if imgCode(t, raw) != "" {
		t.Fatalf("%s", raw)
	}
	if gotID, st := state(e, "mohiguide-web"); gotID != prevID || st != "running" {
		t.Fatalf("current %s %s", gotID, st)
	}
	assertCleanLog(t, logs(), root, "rollback could not remove the old file slot")
}

// restoreOriginal cannot put the slots back because both targets are
// occupied: each failure is logged without names or paths.
func TestRestoreSlotFailuresLogged(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	root := e.useStore(t)
	cur := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	prev := e.fake.add("mohiguide-web-prev", "exited", pinnedPrev, e.labels("web"))
	for _, n := range []string{"mohiguide-web", "mohiguide-web-prev", "mohiguide-web.new"} {
		plantSlot(t, root, n, map[string]string{"app.conf": n})
	}
	logs := captureLog(e)
	st, code := openEntryStore(e.sys, false)
	if code != "" || st == nil {
		t.Fatalf("store %q", code)
	}
	defer closeStore(st)
	restoreOriginal(context.Background(), e.sys, newEngine(dockerSock(e.sys)), st, entryReq("web"), cur, prev, true, true)
	assertCleanLog(t, logs(), root,
		"rollback could not restore the prev file slot",
		"rollback could not restore the current file slot")
	if _, s := state(e, "mohiguide-web"); s != "running" {
		t.Fatalf("original %s", s)
	}
}

// Both rename-backs fail: each is logged, and the original is still
// started by id.
func TestRestoreRenameFailuresLogged(t *testing.T) {
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	prevID, _ := state(e, "mohiguide-web-prev")
	logs := captureLog(e)
	e.fake.mu.Lock()
	e.fake.startFailID = prevID
	// Renames 1 and 2 (park the original, -prev onto the name) work; the
	// restore's renames 3 and 4 fail.
	e.fake.renameFailFrom = e.fake.renameN + 3
	e.fake.mu.Unlock()
	raw := e.call(images.VerbRollback, mustJSON(t, entryReq("web")))
	if imgCode(t, raw) != programs.ReasonDocker {
		t.Fatalf("%s", raw)
	}
	assertCleanLog(t, logs(), root,
		"rollback could not rename the prev container back",
		"rollback could not rename the original container back")
	if c := currentByID(e, orig); c == nil || c.state != "running" {
		t.Fatalf("original %+v", c)
	}
}
