package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// TestSlotAndCapRefusalBytes is the v1.5.2 rule 4 reproducer. A symlink in
// the live slot and a full max_containers cap must be the single-key image
// refusal, not a program envelope and not docker_unavailable.
func TestSlotAndCapRefusalBytes(t *testing.T) {
	t.Run("slot symlink", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		root := e.useStore(t)
		slot := filepath.Join(root, "mohiguide-web")
		if err := os.Symlink(root, slot); err != nil {
			t.Fatal(err)
		}
		raw := e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))
		if string(raw) != `{"error":"`+programs.ReasonBadSpec+`"}` {
			t.Fatalf("%s", raw)
		}
		if created(e) {
			t.Fatal("container created")
		}
		if fi, err := os.Lstat(slot); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("slot symlink removed")
		}
	})

	t.Run("rollback slot symlink", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		root := e.useStore(t)
		slot := filepath.Join(root, "mohiguide-web")
		if err := os.Symlink(root, slot); err != nil {
			t.Fatal(err)
		}
		e.fake.add("mohiguide-web-prev", "exited", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
		raw := e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
		if string(raw) != `{"error":"`+programs.ReasonBadSpec+`"}` {
			t.Fatalf("%s", raw)
		}
		if _, ok := e.fake.ctrs["mohiguide-web-prev"]; !ok {
			t.Fatal("prev container removed")
		}
		if fi, err := os.Lstat(slot); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("slot symlink removed")
		}
	})

	t.Run("cap", func(t *testing.T) {
		e := newImgEnv(t, strings.Replace(imagePolicy, "max_containers = 20", "max_containers = 1", 1))
		e.fake.add("mohiguide-web", "running", "alpine:latest", e.labels("web"))
		raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashD)))
		if string(raw) != `{"error":"`+programs.ReasonPolicy+`"}` {
			t.Fatalf("%s", raw)
		}
		if len(e.fake.Calls()) != 1 || e.fake.Calls()[0].Method != "GET" {
			t.Fatal(e.fake.Calls())
		}
		if e.fake.ctrs["mohiguide-web"].state != "running" {
			t.Fatal("stopped before refusing")
		}
	})
}

func TestContainerStatusPrev(t *testing.T) {
	entry := images.EntryRequest{Project: "mohiguide", Entry: "web"}

	t.Run("absent", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		raw := e.call(images.VerbStatus, mustJSON(t, entry))
		forbidKeys(t, raw)
		if !strings.Contains(string(raw), `"prev":null`) {
			t.Fatalf("%s", raw)
		}
		var st images.StatusResult
		if json.Unmarshal(raw, &st) != nil || st.Prev != nil || st.Status.State != "missing" {
			t.Fatal(string(raw))
		}
	})

	t.Run("labels only", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		prevLabels := e.labels("web")
		prevLabels[images.LabelFilesHash] = hashD
		prevLabels["dev.deploybot.token"] = secret
		e.fake.add("mohiguide-web", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
		e.fake.add("mohiguide-web-prev", "exited", "ghcr.io/joe-and-his-friends/app@"+digestB, prevLabels)
		raw := e.call(images.VerbStatus, mustJSON(t, entry))
		forbidKeys(t, raw)
		var body map[string]any
		if json.Unmarshal(raw, &body) != nil {
			t.Fatal(string(raw))
		}
		prev, ok := body["prev"].(map[string]any)
		if !ok || len(prev) != 3 || prev["digest"] != digestB || prev["spec_hash"] != hashC || prev["files_hash"] != hashD {
			t.Fatalf("%s", raw)
		}
		for _, k := range []string{"state", "ports", "env", "mounts", "Env", "Mounts"} {
			if _, leak := prev[k]; leak {
				t.Fatalf("prev leaked %s", k)
			}
		}
		var st images.StatusResult
		if json.Unmarshal(raw, &st) != nil || st.Status.State != "running" || st.Status.Digest == nil || *st.Status.Digest != digestA {
			t.Fatal(string(raw))
		}
		if st.SpecHash != hashC || st.FilesHash != "" || st.Prev == nil || st.Prev.Digest == nil || *st.Prev.Digest != digestB {
			t.Fatal(string(raw))
		}
	})

	t.Run("prev without current", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		e.fake.add("mohiguide-web-prev", "exited", "ghcr.io/joe-and-his-friends/app@"+digestB, e.labels("web"))
		raw := e.call(images.VerbStatus, mustJSON(t, entry))
		forbidKeys(t, raw)
		var st images.StatusResult
		if json.Unmarshal(raw, &st) != nil || st.Status.State != "missing" || st.SpecHash != "" || st.FilesHash != "" {
			t.Fatal(string(raw))
		}
		if st.Prev == nil || st.Prev.Digest == nil || *st.Prev.Digest != digestB || st.Prev.SpecHash != hashC || st.Prev.FilesHash != "" {
			t.Fatal(string(raw))
		}
	})

	t.Run("unpinned", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		e.fake.add("mohiguide-web-prev", "exited", "alpine:latest", e.labels("web"))
		raw := e.call(images.VerbStatus, mustJSON(t, entry))
		forbidKeys(t, raw)
		var st images.StatusResult
		if json.Unmarshal(raw, &st) != nil || st.Prev == nil || st.Prev.Digest != nil || st.Prev.SpecHash != hashC {
			t.Fatal(string(raw))
		}
		if !strings.Contains(string(raw), `"digest":null`) {
			t.Fatal(string(raw))
		}
	})
}

func TestKeptSlotRestored(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	root := e.useStore(t)
	e.fake.slotDir = filepath.Join(root, "mohiguide-web")
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != "" {
		t.Fatal("v1")
	}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v2")))) != "" {
		t.Fatal("v2")
	}
	if got := readFile(t, filepath.Join(root, "mohiguide-web", "app.conf")); got != "v2" {
		t.Fatalf("current %s", got)
	}
	raw := e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	forbidKeys(t, raw)
	var st images.StatusResult
	wantPrev := images.FilesHash(fileDeploy("v1").Files)
	wantCur := images.FilesHash(fileDeploy("v2").Files)
	if json.Unmarshal(raw, &st) != nil || st.Prev == nil || st.Prev.Digest == nil || *st.Prev.Digest != digestA || st.Prev.FilesHash != wantPrev {
		t.Fatal(string(raw))
	}
	if st.Status.Digest == nil || *st.Status.Digest != digestA || st.FilesHash != wantCur {
		t.Fatal(string(raw))
	}
	if imgCode(t, e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != "" {
		t.Fatal("rollback")
	}
	if got := readFile(t, filepath.Join(root, "mohiguide-web", "app.conf")); got != "v1" {
		t.Fatalf("rolled %s", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web-prev")); !os.IsNotExist(err) {
		t.Fatal("prev slot kept")
	}
	raw = e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	if json.Unmarshal(raw, &st) != nil || st.Prev != nil || !strings.Contains(string(raw), `"prev":null`) {
		t.Fatal(string(raw))
	}
	if got := readFile(t, filepath.Join(root, "mohiguide-web", "app.conf")); got != "v1" {
		t.Fatalf("still %s", got)
	}
}
