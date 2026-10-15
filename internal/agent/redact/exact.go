package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// Exact is the per-task set of secret values. It redacts each value of at
// least 6 bytes, plus its base64 and JSON-escaped forms, before the v1.0.1
// regex. The set is not a secrecy boundary: a value can still be printed in
// some other encoding.
type Exact struct {
	needles []string
}

// NewExact builds the set. Values shorter than 6 bytes are left out. The
// same bytes are also stored as standard and URL base64, padded and raw, and
// as a JSON string body with HTML escaping off and on.
func NewExact(values ...string) Exact {
	seen := map[string]struct{}{}
	var needles []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		needles = append(needles, s)
	}
	for _, v := range values {
		if len(v) < 6 {
			continue
		}
		add(v)
		add(base64.StdEncoding.EncodeToString([]byte(v)))
		add(base64.RawStdEncoding.EncodeToString([]byte(v)))
		add(base64.URLEncoding.EncodeToString([]byte(v)))
		add(base64.RawURLEncoding.EncodeToString([]byte(v)))
		add(jsonBody(v, false))
		add(jsonBody(v, true))
	}
	sort.Slice(needles, func(i, j int) bool {
		if len(needles[i]) != len(needles[j]) {
			return len(needles[i]) > len(needles[j])
		}
		return needles[i] < needles[j]
	})
	return Exact{needles: needles}
}

// Clear drops the set. Go strings cannot be overwritten in place; the caller
// also has to drop every other copy of the values.
func (e *Exact) Clear() {
	for i := range e.needles {
		e.needles[i] = ""
	}
	e.needles = nil
}

// Text applies this set and then the regex.
func (e Exact) Text(s string) string {
	return e.redact(s)
}

// JournalTask is Journal with this set applied to each line before the caps.
func JournalTask(lines []string, ex Exact) ([]string, bool) {
	return journal(lines, ex)
}

// Payload redacts string values and leaves object keys alone. Numbers stay
// integers when they were integers.
func (e Exact) Payload(v map[string]any) map[string]any {
	b, err := protocol.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return map[string]any{}
	}
	walked := e.walk(raw)
	m, ok := walked.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

func (e Exact) redact(s string) string {
	for _, n := range e.needles {
		if n == "" {
			continue
		}
		s = strings.ReplaceAll(s, n, "[REDACTED]")
	}
	return apply(s)
}

func (e Exact) walk(v any) any {
	switch t := v.(type) {
	case string:
		return e.Text(t)
	case map[string]any:
		for k, val := range t {
			t[k] = e.walk(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = e.walk(val)
		}
		return t
	case json.Number:
		if !strings.ContainsAny(t.String(), ".eE") {
			if i, err := t.Int64(); err == nil {
				return int(i)
			}
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	default:
		return v
	}
}

func jsonBody(s string, html bool) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(html)
	if err := enc.Encode(s); err != nil {
		return ""
	}
	raw := bytes.TrimRight(buf.Bytes(), "\n")
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return ""
	}
	return string(raw[1 : len(raw)-1])
}
