package runner

import (
	"context"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// agent-protocol v1.9 rev 3: once the helper has stopped a running
// container, it finishes the restart or rollback even when the caller's
// context ends first.

func TestFinishBudgetCoversStopGrace(t *testing.T) {
	if finishBudget <= time.Duration(images.StopGraceSec)*time.Second {
		t.Fatalf("finish budget %s does not cover the %ds stop grace", finishBudget, images.StopGraceSec)
	}
}

func shortCaller(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func state(e *imgEnv, name string) (string, string) {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	c := e.fake.ctrs[name]
	if c == nil {
		return "", ""
	}
	return c.id, c.state
}

func TestRestartCompletesAfterCallerTimeout(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	id := e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
	e.fake.mu.Lock()
	e.fake.stopDelay = 300 * time.Millisecond
	e.fake.mu.Unlock()
	ctx := shortCaller(t, 50*time.Millisecond)
	raw := Handle(ctx, images.VerbRestart, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}), e.sys)
	if ctx.Err() == nil {
		t.Fatal("caller context still live")
	}
	if imgCode(t, raw) != programs.ReasonDocker {
		t.Fatalf("restart %s", raw)
	}
	if gotID, st := state(e, "mohiguide-web"); gotID != id || st != "running" {
		t.Fatalf("container %s %s", gotID, st)
	}
}

func TestRollbackCompletesAfterCallerTimeout(t *testing.T) {
	e, root := twoFileDeploys(t)
	prevID, _ := state(e, "mohiguide-web-prev")
	e.fake.mu.Lock()
	e.fake.stopDelay = 300 * time.Millisecond
	e.fake.mu.Unlock()
	ctx := shortCaller(t, 50*time.Millisecond)
	raw := Handle(ctx, images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}), e.sys)
	if ctx.Err() == nil {
		t.Fatal("caller context still live")
	}
	// The caller timed out, so the call is an error, but -prev now runs on
	// the current name with its own files, and nothing else is left over.
	if imgCode(t, raw) != programs.ReasonDocker {
		t.Fatalf("rollback %s", raw)
	}
	if gotID, st := state(e, "mohiguide-web"); gotID != prevID || st != "running" {
		t.Fatalf("current %s %s", gotID, st)
	}
	names := containerNames(e)
	if names["mohiguide-web-prev"] || names["mohiguide-web-old"] {
		t.Fatalf("containers %v", names)
	}
	if slotFile(t, root, "mohiguide-web") != "v1" {
		t.Fatal("current slot")
	}
}

func TestRollbackPrevStartFailureRestartsOriginal(t *testing.T) {
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	prevID, _ := state(e, "mohiguide-web-prev")
	e.fake.mu.Lock()
	e.fake.startFailID = prevID
	e.fake.mu.Unlock()
	before := len(e.fake.Calls())
	raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	assertRestored(t, e, raw, before, orig, true)
	assertSlotsRestored(t, root)
	if _, st := state(e, "mohiguide-web-prev"); st == "running" {
		t.Fatal("-prev running")
	}
}

func TestRollbackPrevStartFailureAfterCallerTimeout(t *testing.T) {
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	prevID, _ := state(e, "mohiguide-web-prev")
	e.fake.mu.Lock()
	e.fake.startFailID = prevID
	e.fake.stopDelay = 300 * time.Millisecond
	e.fake.mu.Unlock()
	before := len(e.fake.Calls())
	ctx := shortCaller(t, 50*time.Millisecond)
	raw := Handle(ctx, images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}), e.sys)
	assertRestored(t, e, raw, before, orig, true)
	assertSlotsRestored(t, root)
}

func TestRestartNotRunningUnchanged(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	id := e.fake.add("mohiguide-web", "exited", "alpine:latest", e.labels("web"))
	raw := e.call(images.VerbRestart, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	if imgCode(t, raw) != "" {
		t.Fatalf("restart %s", raw)
	}
	if gotID, st := state(e, "mohiguide-web"); gotID != id || st != "running" {
		t.Fatalf("container %s %s", gotID, st)
	}
}
