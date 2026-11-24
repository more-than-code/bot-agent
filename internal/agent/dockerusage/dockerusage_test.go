package dockerusage

import (
	"strings"
	"testing"
)

func TestCap(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"short", "pgdata", "pgdata"},
		{"200 runes", strings.Repeat("a", 200), strings.Repeat("a", 200)},
		{"201 runes", strings.Repeat("a", 201), strings.Repeat("a", 200)},
		{"multi-byte", strings.Repeat("é", 201), strings.Repeat("é", 200)},
		{"multi-byte 200", strings.Repeat("日", 200), strings.Repeat("日", 200)},
		{"invalid utf-8", "a\xffb", "a�b"},
		{"invalid then long", "\xff" + strings.Repeat("a", 250), "�" + strings.Repeat("a", 199)},
	} {
		if got := Cap(tc.in); got != tc.want {
			t.Errorf("%s: %q", tc.name, got)
		}
	}
}

func reply(images, volumes, containers string) string {
	return `{"summary":{"totals":{"images":{"count":1,"active":0,"size":1,"reclaimable":1},` +
		`"containers":{"count":0,"active":0,"size":0,"reclaimable":0},` +
		`"volumes":{"count":0,"active":0,"size":0,"reclaimable":0},` +
		`"build_cache":{"count":0,"active":0,"size":0,"reclaimable":0}},` +
		`"images":` + images + `,"volumes":` + volumes + `,"containers":` + containers + `}}`
}

func TestDecodeReply(t *testing.T) {
	one := `{"ref":"a:1","id":"0123456789ab","size":1,"containers":0}`
	eleven := "[" + strings.TrimSuffix(strings.Repeat(one+",", 11), ",") + "]"
	ten := "[" + strings.TrimSuffix(strings.Repeat(one+",", 10), ",") + "]"
	for _, tc := range []struct {
		name string
		raw  string
		ok   bool
	}{
		{"ok", reply("["+one+"]", "[]", "[]"), true},
		{"ten", reply(ten, "[]", "[]"), true},
		{"null size", reply(`[{"ref":"a","id":"","size":null,"containers":0}]`, `[{"name":"v","size":null,"in_use":false}]`, `[{"name":"c","state":"x","size":null}]`), true},
		{"eleven", reply(eleven, "[]", "[]"), false},
		{"negative size", reply(`[{"ref":"a","id":"","size":-1,"containers":0}]`, "[]", "[]"), false},
		{"negative count", reply(`[{"ref":"a","id":"","size":1,"containers":-1}]`, "[]", "[]"), false},
		{"negative total", strings.Replace(reply("[]", "[]", "[]"), `"reclaimable":1`, `"reclaimable":-1`, 1), false},
		{"null list", reply("null", "[]", "[]"), false},
		{"missing list", `{"summary":{"totals":{},"images":[],"volumes":[]}}`, false},
		{"long string", reply(`[{"ref":"`+strings.Repeat("a", 201)+`","id":"","size":1,"containers":0}]`, "[]", "[]"), false},
		{"long name", reply("[]", `[{"name":"`+strings.Repeat("é", 201)+`","size":1,"in_use":true}]`, "[]"), false},
		{"unknown key", strings.Replace(reply("[]", "[]", "[]"), `"images":[]`, `"images":[],"labels":{}`, 1), false},
		{"unknown nested key", reply(`[{"ref":"a","id":"","size":1,"containers":0,"env":["X=1"]}]`, "[]", "[]"), false},
		{"trailing json", reply("[]", "[]", "[]") + `{}`, false},
		{"not an object", `[]`, false},
		{"refusal", `{"error":"timeout"}`, false},
	} {
		if _, ok := DecodeReply([]byte(tc.raw)); ok != tc.ok {
			t.Errorf("%s: ok=%v", tc.name, ok)
		}
	}
}

func TestParseSpec(t *testing.T) {
	for raw, ok := range map[string]bool{`{}`: true, ` {} `: true, `{"a":1}`: false, `null`: false, `[]`: false, ``: false, `{}{}`: false} {
		if ParseSpec([]byte(raw)) != ok {
			t.Errorf("%q", raw)
		}
	}
}
