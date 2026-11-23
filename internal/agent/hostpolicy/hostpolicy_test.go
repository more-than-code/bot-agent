package hostpolicy

import (
	"strings"
	"testing"
)

var hash = strings.Repeat("a", 64)

func TestParseRequest(t *testing.T) {
	ok := `{"base_hash":"` + hash + `","edge":true,"images":["ghcr.io/acme/web"],"projects":["acme"]}`
	if r, good := ParseRequest([]byte(ok)); !good || !r.Edge || r.Images[0] != "ghcr.io/acme/web" {
		t.Fatalf("%+v %v", r, good)
	}
	for _, bad := range []string{
		`{}`,
		`{"base_hash":"x","edge":true,"images":[],"projects":[]}`,
		`{"base_hash":"` + hash + `","edge":true,"projects":[]}`,
		`{"base_hash":"` + hash + `","edge":true,"images":[],"projects":[],"run_as":["root"]}`,
		`{"base_hash":"` + hash + `","edge":true,"images":["a","a"],"projects":[]}`,
		`{"base_hash":"` + hash + `","edge":true,"images":[],"projects":["Bad Slug"]}`,
		`{"base_hash":"` + hash + `","edge":true,"images":[""],"projects":[]}`,
	} {
		if _, good := ParseRequest([]byte(bad)); good {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestDecodeStartReply(t *testing.T) {
	if _, ok := DecodeStartReply([]byte(`{"ok":true,"state":"accepted","new_hash":"` + hash + `"}`)); !ok {
		t.Fatal("refused a good reply")
	}
	if _, ok := DecodeStartReply([]byte(`{"ok":true,"state":"accepted"}`)); ok {
		t.Fatal("accepted a reply with no hash")
	}
}
