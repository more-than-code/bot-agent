package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// redactedMarker replaces a registry token or a secret-file piece.
// An env secret uses SecretPreview of the raw value instead.
const redactedMarker = "[REDACTED]"

// needle is one exact form and the text that replaces it.
type needle struct {
	form string
	repl string
}

// Exact is the per-task set of secret values. It replaces each value of at
// least 6 bytes, plus its base64 and JSON-escaped forms, before the v1.0.1
// regex. NewExact uses [REDACTED]. NewExactEnv uses the preview of the raw
// value. The set is not a secrecy boundary: a value can still be printed in
// some other encoding.
type Exact struct {
	needles []needle
}

// NewExact builds the set. Values shorter than 6 bytes are left out. The
// same bytes are also stored as standard and URL base64, padded and raw, and
// as a JSON string body with HTML escaping off and on. Every form is
// replaced with [REDACTED].
func NewExact(values ...string) Exact {
	return exactValues(func(string) string { return redactedMarker }, values)
}

// NewExactEnv is NewExact for env secret values. Every form of a value is
// replaced with SecretPreview of that raw value.
func NewExactEnv(values ...string) Exact {
	return exactValues(SecretPreview, values)
}

// Merge joins sets. Forms are longest first. The same form keeps
// [REDACTED] when either set uses it; otherwise the earlier set wins.
func Merge(parts ...Exact) Exact {
	replOf := map[string]string{}
	var forms []string
	for _, part := range parts {
		for _, n := range part.needles {
			if n.form == "" {
				continue
			}
			prev, ok := replOf[n.form]
			if !ok {
				replOf[n.form] = n.repl
				forms = append(forms, n.form)
				continue
			}
			if prev != redactedMarker && n.repl == redactedMarker {
				replOf[n.form] = redactedMarker
			}
		}
	}
	return sortedExact(forms, replOf)
}

func exactValues(replacement func(string) string, values []string) Exact {
	replOf := map[string]string{}
	var forms []string
	add := func(form, repl string) {
		if form == "" {
			return
		}
		if _, ok := replOf[form]; ok {
			return
		}
		replOf[form] = repl
		forms = append(forms, form)
	}
	for _, v := range values {
		if len(v) < 6 {
			continue
		}
		repl := replacement(v)
		add(v, repl)
		add(base64.StdEncoding.EncodeToString([]byte(v)), repl)
		add(base64.RawStdEncoding.EncodeToString([]byte(v)), repl)
		add(base64.URLEncoding.EncodeToString([]byte(v)), repl)
		add(base64.RawURLEncoding.EncodeToString([]byte(v)), repl)
		add(jsonBody(v, false), repl)
		add(jsonBody(v, true), repl)
	}
	return sortedExact(forms, replOf)
}

func sortedExact(forms []string, replOf map[string]string) Exact {
	sort.Slice(forms, func(i, j int) bool {
		if len(forms[i]) != len(forms[j]) {
			return len(forms[i]) > len(forms[j])
		}
		return forms[i] < forms[j]
	})
	needles := make([]needle, len(forms))
	for i, form := range forms {
		needles[i] = needle{form: form, repl: replOf[form]}
	}
	return Exact{needles: needles}
}

// Clear drops the set. Go strings cannot be overwritten in place; the caller
// also has to drop every other copy of the values.
func (e *Exact) Clear() {
	for i := range e.needles {
		e.needles[i].form = ""
		e.needles[i].repl = ""
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

// Payload redacts free text and leaves object keys alone. outcome, digest,
// exit_code, and every status field are copied through, so the exact set and
// the v1.0.1 regex do not run on them. Numbers stay integers when they were
// integers.
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
	walked := e.walkAt(raw, false)
	m, ok := walked.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

func (e Exact) redact(s string) string {
	for _, n := range e.needles {
		if n.form == "" {
			continue
		}
		s = strings.ReplaceAll(s, n.form, n.repl)
	}
	return apply(s)
}

// passThrough keys are structured task_result fields. status is the whole
// object; the nested names are listed so a status field that is not wrapped
// in status, such as image_logs container, is also left as returned.
func passThrough(k string) bool {
	switch k {
	case "outcome", "digest", "exit_code", "status",
		"container", "state", "restart_count", "started_at", "finished_at", "ports":
		return true
	default:
		return false
	}
}

func (e Exact) walkAt(v any, keep bool) any {
	switch t := v.(type) {
	case string:
		if keep {
			return t
		}
		return e.Text(t)
	case map[string]any:
		for k, val := range t {
			t[k] = e.walkAt(val, keep || passThrough(k))
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = e.walkAt(val, keep)
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
