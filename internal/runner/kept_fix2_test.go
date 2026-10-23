package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestRollbackRenameAppliedKeepsSlot(t *testing.T) {
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	landNextRename(e, 1, nil, nil) // the original's park on -old lands, then errors
	before := len(e.fake.Calls())
	raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	assertRestored(t, e, raw, before, orig, true)
	assertSlotsRestored(t, root)
}

func TestRollbackRenameAppliedOnCancel(t *testing.T) {
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hit := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	landNextRename(e, 1, hit, release)
	go func() {
		<-hit
		cancel()
		finish()
	}()
	before := len(e.fake.Calls())
	raw := Handle(ctx, images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}), e.sys)
	if ctx.Err() == nil {
		t.Fatal("context still live")
	}
	// The caller is gone, but the restore still runs on its own context.
	assertRestored(t, e, raw, before, orig, true)
	assertSlotsRestored(t, root)
}

func TestRollbackRenameAppliedListUnknown(t *testing.T) {
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	landNextRename(e, 1, nil, nil)
	e.fake.mu.Lock()
	// loadPair and the leftover check list; the restore's list fails.
	e.fake.listFailAfter = e.fake.listN + 3
	e.fake.mu.Unlock()
	before := len(e.fake.Calls())
	raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	// Without a list the restore renames blind and still starts the original.
	assertRestored(t, e, raw, before, orig, true)
	assertSlotsRestored(t, root)
}

func TestRollbackStartRenameAppliedMovesSlot(t *testing.T) {
	e, root := twoFileDeploys(t)
	orig := currentID(e)
	e.fake.startFail = true
	landNextRename(e, 2, nil, nil) // -prev's rename onto the current name lands, then errors
	before := len(e.fake.Calls())
	raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	// Every start fails in this fake, so the original is put back but stays stopped.
	assertRestored(t, e, raw, before, orig, false)
	assertSlotsRestored(t, root)
}

func TestParkRenameAppliedMovesSlot(t *testing.T) {
	e, root := twoFileDeploys(t)
	landNextRename(e, 1, nil, nil)
	before := len(e.fake.Calls())
	raw := e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v3")))
	if imgCode(t, raw) != programs.ReasonDocker || bytes.Contains(raw, []byte("container_id")) {
		t.Fatalf("deploy %s", raw)
	}
	for _, c := range e.fake.Calls()[before:] {
		if strings.Contains(c.Path, "/containers/create") || strings.Contains(c.Path, "/start") {
			t.Fatalf("continued %s", c.Path)
		}
	}
	names := containerNames(e)
	if names["mohiguide-web"] || !names["mohiguide-web-prev"] {
		t.Fatalf("containers %v", names)
	}
	if slotFile(t, root, "mohiguide-web-prev") != "v2" {
		t.Fatal("prev slot")
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web")); !os.IsNotExist(err) {
		t.Fatal("current slot still present")
	}
}

func landNextRename(e *imgEnv, nth int, hit, wait chan struct{}) {
	e.fake.mu.Lock()
	e.fake.renameLand = e.fake.renameN + nth
	e.fake.renameHit = hit
	e.fake.renameWait = wait
	e.fake.mu.Unlock()
}

// assertRestored is a failed container_rollback under v1.9 rev 3: the
// refusal is docker_unavailable, the original is back on <name> and
// -prev on <name>-prev, and the original was started again. running is
// whether that start could succeed in the fake.
func assertRestored(t *testing.T, e *imgEnv, raw []byte, before int, origID string, running bool) {
	t.Helper()
	if imgCode(t, raw) != programs.ReasonDocker || bytes.Contains(raw, []byte("container_id")) {
		t.Fatalf("rollback %s", raw)
	}
	var origStarted bool
	for _, c := range e.fake.Calls()[before:] {
		if strings.HasSuffix(c.Path, "/"+origID+"/start") {
			origStarted = true
		}
	}
	if !origStarted {
		t.Fatal("the original was not started again")
	}
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	orig := e.fake.byID[origID]
	if orig == nil || orig.name != "mohiguide-web" {
		t.Fatalf("original %+v", orig)
	}
	if _, ok := e.fake.ctrs["mohiguide-web-prev"]; !ok {
		t.Fatalf("no -prev: %v", e.fake.ctrs)
	}
	if _, ok := e.fake.ctrs["mohiguide-web-old"]; ok {
		t.Fatal("-old left behind")
	}
	if running && orig.state != "running" {
		t.Fatalf("original %s", orig.state)
	}
}

func currentID(e *imgEnv) string {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	return e.fake.ctrs["mohiguide-web"].id
}

// assertSlotsRestored: the original's files on the current slot, -prev's on -prev.
func assertSlotsRestored(t *testing.T, root string) {
	t.Helper()
	if slotFile(t, root, "mohiguide-web") != "v2" || slotFile(t, root, "mohiguide-web-prev") != "v1" {
		t.Fatal("slots not restored")
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web.new")); !os.IsNotExist(err) {
		t.Fatal(".new slot left behind")
	}
}

func slotFile(t *testing.T, root, name string) string {
	t.Helper()
	return readFile(t, filepath.Join(root, name, "app.conf"))
}
