package spool

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOrderAndMode(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenLimited(dir, time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		id := uuid.NewString()
		ids = append(ids, id)
		if err := s.Put(id, []byte(`{"n":`+string(rune('1'+i))+`}`), time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, "spool", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", fi.Mode().Perm())
		}
	}
	items, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("len %d", len(items))
	}
	for i, it := range items {
		if it.ID != ids[i] {
			t.Fatalf("order %v want %v", it.ID, ids[i])
		}
		if i > 0 && it.Seq <= items[i-1].Seq {
			t.Fatal("seq")
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir %o", fi.Mode().Perm())
	}
	if err := s.Delete(ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ids[1]); err != nil {
		t.Fatal(err)
	}
	items, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != ids[0] || items[1].ID != ids[2] {
		t.Fatalf("%v", items)
	}
	if err := s.Put("not-a-uuid", []byte(`{"a":1}`), time.Now()); err == nil {
		t.Fatal("accepted bad id")
	}
}

func TestAgeAndSize(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenLimited(dir, time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 200)
	for i := range big {
		big[i] = 'a'
	}
	if err := s.Put(uuid.NewString(), big, time.Now()); err == nil {
		t.Fatal("oversized batch stored")
	}
	s, err = OpenLimited(dir, time.Minute, 400)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	frame := append([]byte(`{"k":"`), bytes.Repeat([]byte("a"), 80)...)
	frame = append(frame, '"', '}')
	for i := 0; i < 6; i++ {
		id := uuid.NewString()
		ids = append(ids, id)
		if err := s.Put(id, frame, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 || len(items) >= 6 {
		t.Fatalf("cap kept %d", len(items))
	}
	if items[len(items)-1].ID != ids[len(ids)-1] {
		t.Fatal("newest dropped")
	}
	for i := 1; i < len(items); i++ {
		if items[i].Seq < items[i-1].Seq {
			t.Fatal("order after drop")
		}
	}
	old := uuid.NewString()
	if err := s.Put(old, []byte(`{"a":1}`), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == old {
			t.Fatal("expired batch kept")
		}
	}
}

func TestDropsCorrupt(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(dir, "spool")
	if err := os.WriteFile(filepath.Join(sp, "not-a-uuid.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if err := os.WriteFile(filepath.Join(sp, id+".json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("%v", items)
	}
	if _, err := os.Stat(filepath.Join(sp, "not-a-uuid.json")); !os.IsNotExist(err) {
		t.Fatal("junk kept")
	}
}
