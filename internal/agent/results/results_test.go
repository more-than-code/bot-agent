package results

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func result(id string) protocol.TaskResult {
	return protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: id, State: "succeeded",
		Payload: map[string]any{"outcome": "succeeded"},
	}
}

func setMod(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(s.path(id), at, at); err != nil {
		t.Fatal(err)
	}
}

func TestPutGetDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, DirName))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v %v", fi, err)
	}
	r := result("11111111-2222-4333-8444-555555555555")
	r.Late = true
	if err := s.Put(r); err != nil {
		t.Fatal(err)
	}
	p := s.path(r.TaskID)
	fi, err = os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file %v %v", fi, err)
	}
	b, _ := os.ReadFile(p)
	want, _ := protocol.Marshal(result(r.TaskID))
	if string(b) != string(want) {
		t.Fatalf("body %s\nwant %s", b, want)
	}
	got, ok := s.Get(r.TaskID)
	if !ok || got.TaskID != r.TaskID || got.Late || got.State != "succeeded" {
		t.Fatalf("%+v %v", got, ok)
	}
	if err := s.Delete(r.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(r.TaskID); ok {
		t.Fatal("still there")
	}
	if err := s.Delete(r.TaskID); err != nil {
		t.Fatal("second delete", err)
	}
}

func TestBadTaskIDNotStored(t *testing.T) {
	s, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../x", "a/b", ".hidden", strings.Repeat("a", 129)} {
		if err := s.Put(result(id)); err != ErrTaskID {
			t.Fatalf("%q: %v", id, err)
		}
	}
}

func TestCapOldestFirst(t *testing.T) {
	// The new file gets the wall-clock mtime, so the others are set before it.
	now := time.Now()
	s, err := Open(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxFiles; i++ {
		id := fmt.Sprintf("t%03d", i)
		if err := s.Put(result(id)); err != nil {
			t.Fatal(err)
		}
		setMod(t, s, id, now.Add(-time.Hour+time.Duration(i)*time.Second))
	}
	if err := s.Put(result("t100")); err != nil {
		t.Fatal(err)
	}
	list := s.List()
	if len(list) != MaxFiles {
		t.Fatalf("%d files", len(list))
	}
	if _, ok := s.Get("t000"); ok {
		t.Fatal("oldest kept")
	}
	if list[0].TaskID != "t001" || list[len(list)-1].TaskID != "t100" {
		t.Fatalf("order %s .. %s", list[0].TaskID, list[len(list)-1].TaskID)
	}
}

func TestAgeLimit(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s, err := Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "fresh"} {
		if err := s.Put(result(id)); err != nil {
			t.Fatal(err)
		}
	}
	setMod(t, s, "old", now.Add(-MaxAge-time.Minute))
	setMod(t, s, "fresh", now.Add(-MaxAge+time.Minute))
	list := s.List()
	if len(list) != 1 || list[0].TaskID != "fresh" {
		t.Fatalf("%+v", list)
	}
	// Open prunes too.
	setMod(t, s, "fresh", now.Add(-MaxAge-time.Minute))
	if _, err := Open(dir, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path("fresh")); !os.IsNotExist(err) {
		t.Fatal("open did not prune")
	}
}

// A file that is not a task_result for its own name is dropped, and a temp
// file left by a crash goes at the next prune.
func TestCorruptAndLeftovers(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	rd := filepath.Join(dir, DirName)
	if err := os.WriteFile(filepath.Join(rd, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	other, _ := protocol.Marshal(result("other"))
	if err := os.WriteFile(filepath.Join(rd, "named.json"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rd, ".tmp-123"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
	for _, name := range []string{"bad.json", "named.json", ".tmp-123"} {
		if _, err := os.Stat(filepath.Join(rd, name)); !os.IsNotExist(err) {
			t.Fatalf("%s kept", name)
		}
	}
}
