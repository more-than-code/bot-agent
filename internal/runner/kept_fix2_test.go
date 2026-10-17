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
	landNextRename(e, 1, nil, nil)
	before := len(e.fake.Calls())
	raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	assertRenameRefusal(t, e, raw, before)
	if slotFile(t, root, "mohiguide-web") != "v1" {
		t.Fatal("current slot")
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web-prev")); !os.IsNotExist(err) {
		t.Fatal("prev slot still present")
	}
}

func TestRollbackRenameAppliedOnCancel(t *testing.T) {
	e, root := twoFileDeploys(t)
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
	assertRenameRefusal(t, e, raw, before)
	if slotFile(t, root, "mohiguide-web") != "v1" {
		t.Fatal("current slot")
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web-prev")); !os.IsNotExist(err) {
		t.Fatal("prev slot still present")
	}
	lists := 0
	for _, c := range e.fake.Calls()[before:] {
		if c.Path == "/v1.47/containers/json" {
			lists++
		}
	}
	if lists < 2 {
		t.Fatalf("reconcile list %d", lists)
	}
}

func TestRollbackRenameAppliedListUnknown(t *testing.T) {
	e, root := twoFileDeploys(t)
	landNextRename(e, 1, nil, nil)
	e.fake.mu.Lock()
	e.fake.listFailAfter = e.fake.listN + 2
	e.fake.mu.Unlock()
	before := len(e.fake.Calls())
	raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	assertRenameRefusal(t, e, raw, before)
	// The list could not place the container. The slot stays where the
	// rollback had already moved it, which is the name the rename landed on.
	if slotFile(t, root, "mohiguide-web") != "v1" {
		t.Fatal("current slot")
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web-prev")); !os.IsNotExist(err) {
		t.Fatal("prev slot still present")
	}
}

func TestRollbackStartRenameAppliedMovesSlot(t *testing.T) {
	e, root := twoFileDeploys(t)
	e.fake.startFail = true
	landNextRename(e, 2, nil, nil)
	before := len(e.fake.Calls())
	raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	if imgCode(t, raw) != programs.ReasonDocker || bytes.Contains(raw, []byte("container_id")) {
		t.Fatalf("rollback %s", raw)
	}
	started := false
	for _, c := range e.fake.Calls()[before:] {
		if strings.Contains(c.Path, "/start") {
			started = true
		}
	}
	if !started {
		t.Fatal("start was not attempted")
	}
	names := containerNames(e)
	if names["mohiguide-web"] || !names["mohiguide-web-prev"] {
		t.Fatalf("containers %v", names)
	}
	if slotFile(t, root, "mohiguide-web-prev") != "v1" {
		t.Fatal("prev slot")
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web")); !os.IsNotExist(err) {
		t.Fatal("current slot still present")
	}
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

func assertRenameRefusal(t *testing.T, e *imgEnv, raw []byte, before int) {
	t.Helper()
	if imgCode(t, raw) != programs.ReasonDocker || bytes.Contains(raw, []byte("container_id")) {
		t.Fatalf("rollback %s", raw)
	}
	for _, c := range e.fake.Calls()[before:] {
		if strings.Contains(c.Path, "/start") {
			t.Fatal("started after a failed rename")
		}
	}
	names := containerNames(e)
	if !names["mohiguide-web"] || names["mohiguide-web-prev"] {
		t.Fatalf("containers %v", names)
	}
}

func slotFile(t *testing.T, root, name string) string {
	t.Helper()
	return readFile(t, filepath.Join(root, name, "app.conf"))
}
