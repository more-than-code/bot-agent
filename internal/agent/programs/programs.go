// Package programs is the shared program-spec check used by bot-agent and
// by the root helper. Both call Check. A bypass in either binary fails its
// own tests only if that binary stops calling this package.
package programs

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
)

const (
	// MaxFrame is the program-verb request cap from agent-protocol v1.3.
	// Image verbs use images.MaxFrame (2 MiB). FrameLimit picks one.
	MaxFrame = 64 * 1024

	ReasonPolicy   = "policy_denied"
	ReasonBadSpec  = "bad_spec"
	ReasonNotOwned = "not_created"
	ReasonVerb     = "invalid_verb"
	ReasonIO       = "io_error"
	ReasonHelper   = "helper_unavailable"
	// Image verbs (agent-protocol v1.4). FixedReason must keep these;
	// collapsing them would hide a conflict or a registry refusal.
	ReasonConflict = "conflict"
	ReasonDocker   = "docker_unavailable"
	ReasonImage    = "image_unavailable"

	maxArgs    = 32
	maxArgLen  = 4096
	maxEnvKeys = 64
	maxEnvVal  = 4096
	maxDirLen  = 255
	maxLines   = 1000
)

// Resolved is a start the policy allows. Env is the plain environment the
// helper writes to the credentials file. Callers must not log it.
type Resolved struct {
	Unit    string
	Project string
	Entry   string
	Dir     string
	Exec    string
	Args    []string
	RunAs   string
	Group   string
	Home    string
	Restart string
	Env     map[string]string
	Shell   bool
	Lines   int
}

// Gate is the host side of checks 5 and 7.
// CountKnown is false when the caller has not asked the helper how many
// deploybot unit files exist. Check then skips max_programs.
// Active is that set of unit names. A start or restart of one of them does
// not take another slot. Files is set only by the helper. The agent cannot
// see /home, so it leaves Files false and skips realpath and exec resolution.
type Gate struct {
	Policy     policy.Policy
	Running    int
	Active     map[string]bool
	CountKnown bool
	Files      bool
}

// Check applies checks 4–7 for a task kind. An empty code means the spec may run.
// kind is program_start, program_stop, program_restart, program_status, or program_logs.
// Check 4 is the JSON shape. Check 5 is the policy. Check 6 is the field rules
// (bad_spec) and the policy constraints (policy_denied). Check 7 is max_programs.
func Check(kind string, spec []byte, gate Gate) (Resolved, string) {
	if len(spec) > MaxFrame {
		return Resolved{}, ReasonBadSpec
	}
	req, code := decode(kind, spec)
	if code != "" {
		return Resolved{}, code
	}
	p := gate.Policy
	if !p.Valid || !p.Enabled {
		return Resolved{}, ReasonPolicy
	}
	if code = lexical(kind, req); code != "" {
		return Resolved{}, code
	}
	// The agent calls this bad_spec. The helper calls the same keys policy_denied.
	if code = denyEnv(req, gate.Files); code != "" {
		return Resolved{}, code
	}
	return authorize(p, kind, req, gate)
}

type request struct {
	Project string
	Entry   string
	Dir     string
	Exec    string
	Args    []string
	Shell   bool
	RunAs   string
	Env     map[string]string
	Restart string
	Lines   int
}

func decode(kind string, spec []byte) (request, string) {
	switch kind {
	case "program_start":
		return decodeStart(spec)
	case "program_stop", "program_restart", "program_status":
		return decodeRef(spec)
	case "program_logs":
		return decodeLogs(spec)
	default:
		return request{}, ReasonBadSpec
	}
}

func decodeStart(spec []byte) (request, string) {
	var raw struct {
		Project *string         `json:"project"`
		Entry   *string         `json:"entry"`
		Dir     *string         `json:"dir"`
		Exec    *string         `json:"exec"`
		Args    *[]string       `json:"args"`
		Shell   *bool           `json:"shell"`
		RunAs   *string         `json:"run_as"`
		Env     json.RawMessage `json:"env"`
		Restart *string         `json:"restart"`
	}
	if err := strictDecode(spec, &raw); err != nil {
		return request{}, ReasonBadSpec
	}
	if raw.Project == nil || raw.Entry == nil || raw.Dir == nil || raw.Exec == nil ||
		raw.Args == nil || raw.Shell == nil || raw.RunAs == nil || raw.Restart == nil ||
		len(raw.Env) == 0 {
		return request{}, ReasonBadSpec
	}
	env, ok := decodeEnv(raw.Env)
	if !ok {
		return request{}, ReasonBadSpec
	}
	return request{
		Project: *raw.Project, Entry: *raw.Entry, Dir: *raw.Dir, Exec: *raw.Exec,
		Args: *raw.Args, Shell: *raw.Shell, RunAs: *raw.RunAs, Env: env, Restart: *raw.Restart,
	}, ""
}

func decodeRef(spec []byte) (request, string) {
	var raw struct {
		Project *string `json:"project"`
		Entry   *string `json:"entry"`
	}
	if err := strictDecode(spec, &raw); err != nil || raw.Project == nil || raw.Entry == nil {
		return request{}, ReasonBadSpec
	}
	return request{Project: *raw.Project, Entry: *raw.Entry}, ""
}

func decodeLogs(spec []byte) (request, string) {
	var raw struct {
		Project *string          `json:"project"`
		Entry   *string          `json:"entry"`
		Lines   *json.RawMessage `json:"lines"`
	}
	if err := strictDecode(spec, &raw); err != nil || raw.Project == nil || raw.Entry == nil || raw.Lines == nil {
		return request{}, ReasonBadSpec
	}
	n, ok := jsonInt(*raw.Lines)
	if !ok || n < 1 || n > maxLines {
		return request{}, ReasonBadSpec
	}
	return request{Project: *raw.Project, Entry: *raw.Entry, Lines: n}, ""
}

func strictDecode(spec []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(spec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if dec.More() {
		return bytes.ErrTooLarge
	}
	return nil
}

func decodeEnv(raw json.RawMessage) (map[string]string, bool) {
	var obj map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&obj); err != nil || dec.More() {
		return nil, false
	}
	if obj == nil {
		return nil, false
	}
	out := make(map[string]string, len(obj))
	for k, v := range obj {
		s, ok := jsonString(v)
		if !ok {
			return nil, false
		}
		out[k] = s
	}
	return out, true
}

func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func jsonInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || raw[0] == '-' || raw[0] == '"' {
		return 0, false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

var (
	projectRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	entryRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	unitRE    = regexp.MustCompile(`^deploybot-[a-z0-9-]{1,64}-[a-z0-9-]{1,48}\.service$`)
	bareRE    = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,128}$`)
	absRE     = regexp.MustCompile(`^/[A-Za-z0-9._+/-]{1,255}$`)
	runAsRE   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	envKeyRE  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	secretRE  = regexp.MustCompile(`(?i)(password|passwd|token|secret|credential|api[_-]?key|private[_-]?key)`)
)

// UnitName returns the system unit for a project and entry.
func UnitName(project, entry string) (string, bool) {
	if !projectRE.MatchString(project) || !entryRE.MatchString(entry) {
		return "", false
	}
	name := "deploybot-" + project + "-" + entry + ".service"
	if !unitRE.MatchString(name) {
		return "", false
	}
	return name, true
}

func lexical(kind string, req request) string {
	if _, ok := UnitName(req.Project, req.Entry); !ok {
		return ReasonBadSpec
	}
	if kind != "program_start" {
		return ""
	}
	if req.RunAs == "root" || !runAsRE.MatchString(req.RunAs) {
		return ReasonBadSpec
	}
	if req.Restart != "on-failure" && req.Restart != "no" {
		return ReasonBadSpec
	}
	if badDir(req.Dir) || badExec(req.Exec) {
		return ReasonBadSpec
	}
	if req.Shell && req.Exec != "/bin/sh" {
		return ReasonBadSpec
	}
	if req.Shell && (len(req.Args) != 2 || req.Args[0] != "-c") {
		return ReasonBadSpec
	}
	if len(req.Args) > maxArgs {
		return ReasonBadSpec
	}
	for _, a := range req.Args {
		if len(a) > maxArgLen || strings.ContainsAny(a, "\x00\n") {
			return ReasonBadSpec
		}
	}
	if len(req.Env) > maxEnvKeys {
		return ReasonBadSpec
	}
	for k, v := range req.Env {
		if !envKeyRE.MatchString(k) || k == "PATH" || k == "HOME" || secretRE.MatchString(k) {
			return ReasonBadSpec
		}
		if len(v) > maxEnvVal || strings.ContainsAny(v, "\x00\n") {
			return ReasonBadSpec
		}
	}
	return ""
}

func badDir(dir string) bool {
	if len(dir) == 0 || len(dir) > maxDirLen || dir[0] != '/' {
		return true
	}
	if strings.ContainsAny(dir, "\x00\n") {
		return true
	}
	return badSegments(dir)
}

func badExec(exec string) bool {
	if strings.ContainsAny(exec, "\x00\n") {
		return true
	}
	if strings.HasPrefix(exec, "/") {
		if !absRE.MatchString(exec) || badSegments(exec) {
			return true
		}
		return false
	}
	if exec == "." || exec == ".." || !bareRE.MatchString(exec) {
		return true
	}
	return false
}

func badSegments(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func authorize(p policy.Policy, kind string, req request, gate Gate) (Resolved, string) {
	unit, ok := UnitName(req.Project, req.Entry)
	if !ok {
		return Resolved{}, ReasonBadSpec
	}
	out := Resolved{Unit: unit, Project: req.Project, Entry: req.Entry, Lines: req.Lines}
	if kind != "program_start" {
		if kind == "program_restart" && gate.CountKnown && overMax(p, unit, gate) {
			return Resolved{}, ReasonPolicy
		}
		return out, ""
	}
	ac, ok := p.Accounts[req.RunAs]
	listed := false
	for _, name := range p.RunAs {
		if name == req.RunAs {
			listed = true
			break
		}
	}
	if !listed || !ok || ac.Home == "" || ac.Group == "" {
		return Resolved{}, ReasonPolicy
	}
	if req.Shell && !p.AllowShell {
		return Resolved{}, ReasonPolicy
	}
	env := make(map[string]string, len(req.Env))
	for k, v := range req.Env {
		env[k] = v
	}
	if !gate.Files {
		if gate.CountKnown && overMax(p, unit, gate) {
			return Resolved{}, ReasonPolicy
		}
		out.Dir = req.Dir
		out.Exec = req.Exec
		out.Args = append([]string(nil), req.Args...)
		out.RunAs = req.RunAs
		out.Group = ac.Group
		out.Home = ac.Home
		out.Restart = req.Restart
		out.Env = env
		out.Shell = req.Shell
		return out, ""
	}
	dir, ok := resolveDir(req.Dir, p.Dirs)
	if !ok {
		return Resolved{}, ReasonPolicy
	}
	execPath, ok := resolveExec(req, dir, p)
	if !ok {
		return Resolved{}, ReasonPolicy
	}
	if isShell(execPath) && !(req.Shell && execPath == "/bin/sh") {
		return Resolved{}, ReasonPolicy
	}
	if gate.CountKnown && overMax(p, unit, gate) {
		return Resolved{}, ReasonPolicy
	}
	out.Dir = dir
	out.Exec = execPath
	out.Args = append([]string(nil), req.Args...)
	out.RunAs = req.RunAs
	out.Group = ac.Group
	out.Home = ac.Home
	out.Restart = req.Restart
	out.Env = env
	out.Shell = req.Shell
	return out, ""
}

func overMax(p policy.Policy, unit string, gate Gate) bool {
	if gate.Active[unit] {
		return false
	}
	return gate.Running >= p.MaxPrograms
}

// denyEnv refuses variables that change which code runs, including a hoisted
// KEY=value argument. helper is true on the helper path (policy_denied).
func denyEnv(req request, helper bool) string {
	reason := ReasonBadSpec
	if helper {
		reason = ReasonPolicy
	}
	for k := range req.Env {
		if deniedEnvKey(k) {
			return reason
		}
	}
	for _, arg := range req.Args {
		if k, ok := hoistedKey(arg); ok && deniedEnvKey(k) {
			return reason
		}
	}
	return ""
}

func deniedEnvKey(k string) bool {
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

func hoistedKey(arg string) (string, bool) {
	if strings.ContainsAny(arg, " \t") {
		return "", false
	}
	k, _, ok := strings.Cut(arg, "=")
	if !ok || !envKeyRE.MatchString(k) {
		return "", false
	}
	return k, true
}

func isShell(path string) bool {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	switch base {
	case "sh", "bash", "dash", "zsh", "ksh", "csh", "tcsh", "fish":
		return true
	default:
		return false
	}
}
