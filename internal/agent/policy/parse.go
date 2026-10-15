package policy

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// parsed is a syntactically accepted file. run_as names are not filtered yet.
type parsed struct {
	enabled       bool
	allowShell    bool
	allowPublic   bool
	maxPrograms   int
	maxContainers int
	maxSet        bool
	runAs         []string
	dirs          []string
	execAllow     []string
	images        []string
	projects      []string
}

// parseTOML accepts the v1.3 keys and the v1.4 image keys. Unknown keys,
// duplicates, and wrong types return a fixed reason. Values are not
// included in the reason. Pattern and slug checks happen in Load.
func parseTOML(data []byte) (parsed, string) {
	if !utf8.Valid(data) || strings.Contains(string(data), "\x00") {
		return parsed{}, "parse"
	}
	var out parsed
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		key, rest, ok := strings.Cut(trim, "=")
		if !ok {
			return parsed{}, "parse"
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimSpace(rest)
		if key == "" || strings.ContainsAny(key, " \t#") {
			return parsed{}, "parse"
		}
		if !knownKey(key) {
			return parsed{}, "unknown_key"
		}
		if seen[key] {
			return parsed{}, "parse"
		}
		seen[key] = true
		switch key {
		case "enabled":
			v, ok := parseBool(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.enabled = v
		case "allow_shell":
			v, ok := parseBool(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.allowShell = v
		case "max_programs":
			n, ok := parseInt(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.maxPrograms = n
		case "run_as":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.runAs = xs
		case "dirs":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.dirs = xs
		case "exec_allow":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.execAllow = xs
		case "images":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.images = xs
		case "projects":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.projects = xs
		case "max_containers":
			n, ok := parseInt(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.maxContainers = n
			out.maxSet = true
		case "allow_public_ports":
			v, ok := parseBool(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.allowPublic = v
		default:
			return parsed{}, "unknown_key"
		}
	}
	if out.runAs == nil {
		out.runAs = []string{}
	}
	if out.dirs == nil {
		out.dirs = []string{}
	}
	if out.execAllow == nil {
		out.execAllow = []string{}
	}
	return out, ""
}

func knownKey(k string) bool {
	switch k {
	case "enabled", "run_as", "dirs", "exec_allow", "allow_shell", "max_programs",
		"images", "projects", "max_containers", "allow_public_ports":
		return true
	default:
		return false
	}
}

func parseBool(s string) (bool, bool) {
	switch s {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func parseInt(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func parseArray(s string) ([]string, bool) {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, false
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	if body == "" {
		return []string{}, true
	}
	var out []string
	for len(body) > 0 {
		body = strings.TrimSpace(body)
		if body == "" {
			break
		}
		if body[0] != '"' {
			return nil, false
		}
		val, rest, ok := parseString(body)
		if !ok {
			return nil, false
		}
		out = append(out, val)
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		if rest[0] != ',' {
			return nil, false
		}
		body = strings.TrimSpace(rest[1:])
		if body == "" {
			return nil, false
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, true
}

// parseString reads a double-quoted TOML basic string. Only \\ and \" are escapes.
func parseString(s string) (string, string, bool) {
	if s == "" || s[0] != '"' {
		return "", "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			return b.String(), s[i+1:], true
		}
		if c == '\n' || c == '\r' || c == 0 {
			return "", "", false
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			return "", "", false
		}
		n := s[i+1]
		switch n {
		case '\\', '"':
			b.WriteByte(n)
			i++
		default:
			return "", "", false
		}
	}
	return "", "", false
}
