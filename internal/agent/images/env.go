package images

import (
	"regexp"
	"strconv"
	"strings"
)

const (
	maxEnvKey  = 64
	maxEnvVal  = 4096
	maxEnvN    = 64
	maxMerged  = 8*256 + 64 + 64
	maxArgs    = 32
	maxArgLen  = 4096
	maxPorts   = 16
	maxVolumes = 8
	maxSets    = 8
	maxSecrets = 64
	maxTarget  = 255
)

var (
	envKeyRE     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	secretLookRE = regexp.MustCompile(`(?i)(password|passwd|token|secret|credential|api[_-]?key|private[_-]?key)`)
	userNameRE   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	setNameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
)

// ResolvedEnv checks the merged environment the helper puts in the container.
// Secret-looking keys are allowed here: they are the resolved secret_env
// values. Keys that change which code runs are still refused.
func ResolvedEnv(env map[string]string) string {
	return checkEnv(env, false, maxMerged)
}

func inlineEnv(env map[string]string) string {
	return checkEnv(env, true, maxEnvN)
}

func checkEnv(env map[string]string, refuseSecrets bool, limit int) string {
	if len(env) > limit {
		return "env"
	}
	for k, v := range env {
		if !envKeyRE.MatchString(k) || len(k) > maxEnvKey || k == "PATH" || k == "HOME" || changesRuntime(k) {
			return "env"
		}
		if refuseSecrets && secretLookRE.MatchString(k) {
			return "env"
		}
		if len(v) > maxEnvVal || strings.ContainsAny(v, "\x00\n\r") {
			return "env"
		}
	}
	return ""
}

func changesRuntime(k string) bool {
	if strings.HasPrefix(k, "LD_") || strings.HasPrefix(k, "DYLD_") {
		return true
	}
	switch k {
	case "NODE_OPTIONS", "NODE_PATH", "BASH_ENV", "ENV", "IFS",
		"PYTHONPATH", "PYTHONSTARTUP", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB":
		return true
	default:
		return false
	}
}

func checkSecretRefs(secret map[string]string) string {
	if len(secret) > maxSecrets {
		return "secret_env"
	}
	for k, v := range secret {
		if !envKeyRE.MatchString(k) || k == "PATH" || k == "HOME" || changesRuntime(k) {
			return "secret_env"
		}
		set, key, ok := strings.Cut(v, ".")
		if !ok || !setNameRE.MatchString(set) || !envKeyRE.MatchString(key) || key == "PATH" || key == "HOME" || changesRuntime(key) {
			return "secret_env"
		}
	}
	return ""
}

func checkSets(names []string) string {
	if len(names) > maxSets {
		return "env_sets"
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !setNameRE.MatchString(n) || seen[n] {
			return "env_sets"
		}
		seen[n] = true
	}
	return ""
}

// ValidUser implements api-v1 v1.8 item 10 and spec §12.6.
// Empty is the image default. uid 0, gid 0, "root" in either position,
// and leading-zero numbers are refused.
func ValidUser(s string) bool {
	if s == "" {
		return true
	}
	if strings.ContainsAny(s, "\r\n\x00 \t") {
		return false
	}
	user, group, two := strings.Cut(s, ":")
	if strings.Contains(group, ":") || !validUserPart(user) {
		return false
	}
	if two && !validUserPart(group) {
		return false
	}
	return true
}

func validUserPart(s string) bool {
	if s == "" || strings.EqualFold(s, "root") {
		return false
	}
	if userNameRE.MatchString(s) {
		return true
	}
	return validNumericID(s)
}

func validNumericID(s string) bool {
	if s == "" || len(s) > 10 || s[0] == '0' {
		return false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 || n > 4294967294 {
		return false
	}
	return true
}

func validTarget(s string) bool {
	if s == "/" {
		return true
	}
	if s == "" || len(s) > maxTarget || s[0] != '/' || strings.ContainsAny(s, "\r\n\x00\\") {
		return false
	}
	parts := strings.Split(s, "/")
	for i, p := range parts {
		if i == 0 {
			if p != "" {
				return false
			}
			continue
		}
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}

func validArgv(v []string) bool {
	if len(v) > maxArgs {
		return false
	}
	for _, s := range v {
		if len(s) > maxArgLen || strings.ContainsAny(s, "\x00\n\r") {
			return false
		}
	}
	return true
}
