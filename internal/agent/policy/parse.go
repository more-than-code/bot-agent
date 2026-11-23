package policy

import (
	"net/url"
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
	sharedNets    []string
	sharedMembers []string
	edge          bool
	agentUpgrade  bool
	upgradeProxy  string
	consolePolicy bool
	// cgroupns is the v1.24 container_cgroupns key; empty when absent.
	cgroupns  string
	hostFiles hostFileKeys
	// hostNetwork and hostEnv are the v1.21 keys; nil when absent.
	hostNetwork []string
	hostEnv     []string
	hostEnvImgs []string
}

// parseTOML accepts the v1.3 keys, the v1.4 image keys, the v1.13 shared
// network keys, the v1.15 edge key, the v1.18 host file keys, the v1.20
// agent_upgrade key and the v1.21 host network and host env keys,
// and the v1.22 console_policy key. Unknown keys,
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
		case "shared_networks":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.sharedNets = xs
		case "shared_network_projects":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.sharedMembers = xs
		case "edge":
			v, ok := parseBool(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.edge = v
		case "agent_upgrade":
			v, ok := parseBool(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.agentUpgrade = v
		case "console_policy":
			v, ok := parseBool(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.consolePolicy = v
		case "container_cgroupns":
			v, rest, ok := parseString(rest)
			if !ok || strings.TrimSpace(rest) != "" || (v != CgroupnsPrivate && v != CgroupnsHost) {
				return parsed{}, "parse"
			}
			out.cgroupns = v
		case "agent_upgrade_https_proxy":
			v, rest, ok := parseString(rest)
			if !ok || strings.TrimSpace(rest) != "" || !ValidUpgradeProxy(v) {
				return parsed{}, "parse"
			}
			out.upgradeProxy = v
		case "host_network_projects":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.hostNetwork = xs
		case "host_env_files":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.hostEnv = xs
		case "host_env_images":
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			out.hostEnvImgs = xs
		default:
			slot := out.hostFiles.field(key)
			if slot == nil {
				return parsed{}, "unknown_key"
			}
			xs, ok := parseArray(rest)
			if !ok {
				return parsed{}, "parse"
			}
			*slot = xs
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
		"images", "projects", "max_containers", "allow_public_ports",
		"shared_networks", "shared_network_projects", "edge", "agent_upgrade",
		"agent_upgrade_https_proxy", "host_network_projects", "host_env_files", "host_env_images", "console_policy",
		"container_cgroupns":
		return true
	default:
		return isHostFileKey(k)
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

// isHostFileKey reports the eight v1.18 keys.
func isHostFileKey(key string) bool {
	return (&hostFileKeys{}).field(key) != nil
}

// field is the slot for one v1.18 key, nil for any other key.
func (k *hostFileKeys) field(key string) *[]string {
	switch key {
	case "host_files":
		return &k.files
	case "host_file_modes":
		return &k.modes
	case "host_file_secret":
		return &k.secret
	case "host_file_units":
		return &k.units
	case "host_file_run_as":
		return &k.runAs
	case "host_file_checks":
		return &k.checks
	case "host_file_health":
		return &k.health
	case "host_file_idle":
		return &k.idle
	default:
		return nil
	}
}

// MaxUpgradeProxy bounds agent_upgrade_https_proxy (agent-protocol v1.20
// item 1).
const MaxUpgradeProxy = 256

// ValidUpgradeProxy accepts http(s)://host:port: a host, a numeric port
// 1-65535, and no userinfo, path, query or fragment.
func ValidUpgradeProxy(s string) bool {
	if s == "" || len(s) > MaxUpgradeProxy {
		return false
	}
	for _, c := range s {
		if c <= ' ' || c == 0x7f {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if u.Hostname() == "" || u.Port() == "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 1 && port <= 65535 && strconv.Itoa(port) == u.Port()
}
