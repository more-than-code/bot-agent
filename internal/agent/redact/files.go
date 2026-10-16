package redact

import (
	"encoding/json"
	"strings"
)

// File line bounds from agent-protocol v1.5. The whole content uses 6–4096,
// which is the exact-set floor. Lines and JSON string values start at 16.
const (
	fileLineMin  = 16
	fileLineMax  = 4096
	fileWholeMin = 6
	fileWholeMax = 4096
)

// FileSecrets is the v1.5 exact-value addition for one secret file.
// Plain files add nothing; the caller skips them.
// A value outside these bounds is not added. NewExact still drops anything under 6 bytes.
func FileSecrets(content string) []string {
	var out []string
	if n := len(content); n >= fileWholeMin && n <= fileWholeMax {
		out = append(out, content)
	}
	out = append(out, trimmedLines(content, fileLineMin, fileLineMax)...)
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return out
	}
	walkJSONStrings(v, func(s string) {
		if n := len(s); n >= fileLineMin && n <= fileLineMax {
			out = append(out, s)
		}
		out = append(out, trimmedLines(s, fileLineMin, fileLineMax)...)
	})
	return out
}

func trimmedLines(s string, min, max int) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if len(t) >= min && len(t) <= max {
			out = append(out, t)
		}
	}
	return out
}

func walkJSONStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case map[string]any:
		for _, val := range t {
			walkJSONStrings(val, fn)
		}
	case []any:
		for _, val := range t {
			walkJSONStrings(val, fn)
		}
	}
}
