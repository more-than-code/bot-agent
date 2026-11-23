package policy

import (
	"strings"
)

// Managed is the part of runner.toml the console may change on a host whose
// root set console_policy = true (agent-protocol v1.21): edge, images and
// projects. Nothing else in the file is touched.
type Managed struct {
	Edge     bool
	Images   []string
	Projects []string
}

// ManagedOf is p's console-editable values. Images and Projects are never nil.
func ManagedOf(p Policy) Managed {
	return Managed{
		Edge:     p.Edge,
		Images:   append([]string{}, p.Images...),
		Projects: append([]string{}, p.Projects...),
	}
}

// Rewrite returns data with the edge, images and projects lines replaced by
// m, keeping every other line, comment and the order. A key the file does
// not have is appended at the end. The caller validates the result with
// FromBytes; Rewrite only edits lines.
func Rewrite(data []byte, m Managed) []byte {
	want := map[string]string{
		"edge":     "edge = " + formatBool(m.Edge),
		"images":   "images = " + formatArray(m.Images),
		"projects": "projects = " + formatArray(m.Projects),
	}
	text := string(data)
	lines := strings.Split(text, "\n")
	done := map[string]bool{}
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		key, _, ok := strings.Cut(trim, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		repl, managed := want[key]
		if !managed || done[key] {
			continue
		}
		lines[i] = repl
		done[key] = true
	}
	out := strings.Join(lines, "\n")
	for _, key := range []string{"edge", "images", "projects"} {
		if done[key] {
			continue
		}
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += want[key] + "\n"
	}
	return []byte(out)
}

func formatBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// formatArray writes a one-line TOML array of basic strings. Only \ and "
// need escaping in the strings parseString accepts.
func formatArray(xs []string) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		x = strings.ReplaceAll(x, `\`, `\\`)
		x = strings.ReplaceAll(x, `"`, `\"`)
		parts = append(parts, `"`+x+`"`)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
