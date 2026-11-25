// Package hostfiles is the shared vocabulary of host config files
// (agent-protocol v1.18): the task kind, helper verbs, frame, the
// runner.toml value rules, the request check and the result shapes. The
// agent, the policy loader and the root helper all use it. It does not read
// the filesystem or run anything.
package hostfiles

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Task kind (server → agent), helper verbs and the status frame.
const (
	Kind        = "host_file_apply"
	VerbApply   = "host_file_apply"
	VerbStatus  = "host_file_status"
	FrameStatus = "host_files_status"
)

// Bounds (spec §2.1, §2.3, §2.5).
const (
	MaxFiles      = 8
	MaxContent    = 64 * 1024
	MaxArgs       = 16
	MaxFrame      = 160 * 1024
	MinSettle     = 30
	MaxSettle     = 180
	DefaultSettle = 60
	// SettleSlack is added to a health command's --wait value: settle_sec
	// must be at least that sum.
	SettleSlack = 15
	MaxStderr   = 512

	// MaxHealthWait is the largest health --wait a policy may carry, so
	// wait + SettleSlack still fits MaxSettle (spec rev 1a A5).
	MaxHealthWait = MaxSettle - SettleSlack

	// Budgets (spec rev 1a A4). A command's unit gets RuntimeMaxSec equal
	// to its budget; the helper kills systemd-run RunGrace later.
	CheckBudget   = 15 * time.Second
	IdleBudget    = 10 * time.Second
	RunGrace      = 5 * time.Second
	RestartBudget = 60 * time.Second
	ShowBudget    = 10 * time.Second
	// TaskTimeout is the agent's budget for one apply; it is also the cap
	// on a frame's timeout_sec. WorstCase must stay below it.
	TaskTimeout = 600 * time.Second
	// StatusEvery is how often the agent sends host_files_status.
	StatusEvery = 10 * time.Minute

	// StateDir holds <name>.prev and <name>.applied (root 0700).
	StateDir = "/var/lib/bot-agent-runner/host-files"
	// UnitPrefix names the transient systemd-run units.
	UnitPrefix = "deploybot-hostfile-check-"
	// CandidateSuffix is the candidate file written beside the target as
	// ".<file>" + CandidateSuffix.
	CandidateSuffix = ".deploybot-new"
)

// Outcomes of host_file_apply.
const (
	OutcomeSucceeded      = "succeeded"
	OutcomeUnchanged      = "unchanged"
	OutcomeWorkerBusy     = "worker_busy"
	OutcomeCheckFailed    = "check_failed"
	OutcomeRolledBack     = "rolled_back"
	OutcomeRollbackFailed = "rollback_failed"
	OutcomeForbiddenSpec  = "forbidden_spec"
)

// Stages. The empty stage is allowed on succeeded and unchanged.
const (
	StageLookup   = "lookup"
	StageSettle   = "settle"
	StageExec     = "exec"
	StageTarget   = "target"
	StageCheck    = "check"
	StageIdle     = "idle"
	StageRestart  = "restart"
	StageHealth   = "health"
	StageRollback = "rollback"
)

// Fixed reasons. A check reason is also any token matching CheckReasonRE.
const (
	ReasonCheckError    = "check_error"
	ReasonIdleError     = "idle_error"
	ReasonHealthError   = "health_error"
	ReasonNotConnected  = "not_connected"
	ReasonPolicyInvalid = "policy_invalid"
	ReasonNotRunning    = "not_running"
)

// Status reasons (owner_ok false or exists false).
const (
	StatusMissing    = "missing"
	StatusSymlink    = "symlink"
	StatusNotRegular = "not_regular"
	StatusOwner      = "owner"
	StatusMode       = "mode"
	StatusNlink      = "nlink"
	StatusDir        = "dir"
	StatusTooLarge   = "too_large"
	StatusIO         = "io_error"
)

var (
	nameRE        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	unitRE        = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9@._-]{0,127}\.service$`)
	segRE         = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	CheckReasonRE = regexp.MustCompile(`^bad_[a-z0-9_]{1,64}$`)
	hashRE        = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// WorstCase is the longest apply (spec rev 1a A4): check and idle with
// their grace, restart and settle, then the rollback's restart and settle.
func WorstCase() time.Duration {
	settle := time.Duration(MaxSettle) * time.Second
	return CheckBudget + RunGrace + IdleBudget + RunGrace + 2*(RestartBudget+settle)
}

// reservedDirs may never hold a host file (spec §2.1, rev 1a A6). A dir
// starting with "cron." is reserved too.
var reservedDirs = map[string]bool{
	"bot-agent": true, "systemd": true, "ssh": true, "sudoers.d": true,
	"pam.d": true, "security": true, "cron.d": true, "polkit-1": true,
	"apt": true, "docker": true, "caddy": true, "postgresql": true,
	"tmpfiles.d": true, "sysusers.d": true, "sysctl.d": true, "modprobe.d": true,
	"profile.d": true, "ld.so.conf.d": true, "logrotate.d": true, "update-motd.d": true,
	"udev": true, "init.d": true, "apparmor.d": true, "dpkg": true, "rsyslog.d": true,
	"default": true,
}

// reservedUnitPrefixes may never start a host file's unit.
var reservedUnitPrefixes = []string{
	"deploybot-", "bot-agent", "ssh", "systemd-", "dbus", "docker",
	"containerd", "caddy", "postgresql",
	"networking", "NetworkManager", "tailscaled", "cron", "rsyslog", "polkit", "user@", "getty",
}

// ValidName is the host file name rule.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ValidPath accepts exactly /etc/<dir>/<file> with a non-reserved dir and
// returns the two components.
func ValidPath(p string) (dir, file string, ok bool) {
	if !strings.HasPrefix(p, "/etc/") || path.Clean(p) != p {
		return "", "", false
	}
	parts := strings.Split(p[len("/etc/"):], "/")
	if len(parts) != 2 {
		return "", "", false
	}
	dir, file = parts[0], parts[1]
	if !segRE.MatchString(dir) || !segRE.MatchString(file) || dir == "." || dir == ".." || file == "." || file == ".." {
		return "", "", false
	}
	if reservedDirs[dir] || strings.HasPrefix(dir, "cron.") {
		return "", "", false
	}
	return dir, file, true
}

// ValidMode accepts 0600, 0640 and 0644.
func ValidMode(s string) (uint32, bool) {
	switch s {
	case "0600":
		return 0o600, true
	case "0640":
		return 0o640, true
	case "0644":
		return 0o644, true
	default:
		return 0, false
	}
}

// ValidUnit is the unit rule: a .service name starting with a letter or
// digit, not a reserved prefix.
func ValidUnit(u string) bool {
	if !unitRE.MatchString(u) {
		return false
	}
	for _, p := range reservedUnitPrefixes {
		if strings.HasPrefix(u, p) {
			return false
		}
	}
	return true
}

// SplitCommand splits a policy command on single spaces. No shell, no
// quoting. The executable is absolute and clean; at most MaxArgs arguments
// follow it. Control characters are refused.
func SplitCommand(s string) ([]string, bool) {
	if s == "" {
		return nil, false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return nil, false
		}
	}
	argv := strings.Split(s, " ")
	if len(argv)-1 > MaxArgs {
		return nil, false
	}
	for _, a := range argv {
		if a == "" {
			return nil, false
		}
	}
	exe := argv[0]
	if !strings.HasPrefix(exe, "/") || path.Clean(exe) != exe || exe == "/" {
		return nil, false
	}
	return argv, true
}

// WaitSeconds reads a "--wait <dur>" or "--wait=<dur>" argument. present
// is false when there is none; ok is false when the value does not parse
// as a positive duration. A fraction of a second rounds up.
func WaitSeconds(argv []string) (secs int, present, ok bool) {
	val := ""
	for i, a := range argv {
		if a == "--wait" {
			if i+1 >= len(argv) {
				return 0, true, false
			}
			val = argv[i+1]
			present = true
			break
		}
		if strings.HasPrefix(a, "--wait=") {
			val = strings.TrimPrefix(a, "--wait=")
			present = true
			break
		}
	}
	if !present {
		return 0, false, true
	}
	d, err := time.ParseDuration(val)
	if err != nil || d <= 0 {
		return 0, true, false
	}
	secs = int((d + time.Second - 1) / time.Second)
	return secs, true, true
}

// ValidHealth is the load-time rule for a health command: a --wait, when
// present, parses and is at most MaxHealthWait seconds.
func ValidHealth(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	w, present, ok := WaitSeconds(argv[1:])
	return ok && (!present || w <= MaxHealthWait)
}

// Settle resolves the request's settle_sec against the health command.
// A missing value is DefaultSettle, raised to the health wait plus
// SettleSlack when that is larger (and still at most MaxSettle). A value
// below wait + SettleSlack, or an unreadable --wait, is refused.
func Settle(req *int, health []string) (int, bool) {
	floor := 0
	if len(health) > 0 {
		w, present, ok := WaitSeconds(health[1:])
		if !ok {
			return 0, false
		}
		if present {
			floor = w + SettleSlack
		}
	}
	if req == nil {
		s := DefaultSettle
		if floor > s {
			s = floor
		}
		if s > MaxSettle {
			return 0, false
		}
		return s, true
	}
	s := *req
	if s < MinSettle || s > MaxSettle || s < floor {
		return 0, false
	}
	return s, true
}

// ApplyRequest is the task spec and the helper request.
type ApplyRequest struct {
	Name       string `json:"name"`
	ContentB64 string `json:"content_b64"`
	SettleSec  *int   `json:"settle_sec,omitempty"`
	Force      bool   `json:"force,omitempty"`
}

var errSpec = errors.New("host file spec")

// ParseApply decodes the spec strictly and returns the content bytes.
// settle_sec, when present, is bounded here; the health floor is the
// helper's check.
func ParseApply(raw []byte) (ApplyRequest, []byte, error) {
	var r ApplyRequest
	if err := decodeStrict(raw, &r); err != nil {
		return ApplyRequest{}, nil, errSpec
	}
	if !ValidName(r.Name) {
		return ApplyRequest{}, nil, errSpec
	}
	if len(r.ContentB64) > base64.StdEncoding.EncodedLen(MaxContent) {
		return ApplyRequest{}, nil, errSpec
	}
	content, err := base64.StdEncoding.Strict().DecodeString(r.ContentB64)
	if err != nil || len(content) < 1 || len(content) > MaxContent {
		return ApplyRequest{}, nil, errSpec
	}
	if r.SettleSec != nil && (*r.SettleSec < MinSettle || *r.SettleSec > MaxSettle) {
		return ApplyRequest{}, nil, errSpec
	}
	return r, content, nil
}

// ParseEmpty accepts exactly {} (host_file_status).
func ParseEmpty(raw []byte) error {
	var empty struct{}
	return decodeStrict(raw, &empty)
}

func decodeStrict(raw []byte, dest any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errSpec
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return errSpec
	}
	if dec.More() {
		return errSpec
	}
	return nil
}

// ApplyResult is the helper's reply and the task result payload. SHA256
// is set only for a non-secret file.
type ApplyResult struct {
	Outcome string `json:"outcome"`
	Stage   string `json:"stage"`
	Reason  string `json:"reason"`
	TookMS  int64  `json:"took_ms"`
	SHA256  string `json:"sha256,omitempty"`
}

// ValidResult checks the enums and the reason token, so a helper can never
// put anything else into a task result.
func ValidResult(r ApplyResult) bool {
	switch r.Outcome {
	case OutcomeSucceeded, OutcomeUnchanged, OutcomeWorkerBusy, OutcomeCheckFailed,
		OutcomeRolledBack, OutcomeRollbackFailed, OutcomeForbiddenSpec:
	default:
		return false
	}
	switch r.Stage {
	case "", StageLookup, StageSettle, StageExec, StageTarget, StageCheck, StageIdle,
		StageRestart, StageHealth, StageRollback:
	default:
		return false
	}
	if r.Reason != "" && !ValidReason(r.Reason) {
		return false
	}
	if r.SHA256 != "" {
		// sha256 only on succeeded and unchanged (rev 1a A5).
		if !hashRE.MatchString(r.SHA256) || (r.Outcome != OutcomeSucceeded && r.Outcome != OutcomeUnchanged) {
			return false
		}
	}
	return r.TookMS >= 0
}

// ValidReason is the result reason allowlist.
func ValidReason(s string) bool {
	switch s {
	case ReasonCheckError, ReasonIdleError, ReasonHealthError,
		ReasonNotConnected, ReasonPolicyInvalid, ReasonNotRunning:
		return true
	}
	return CheckReasonRE.MatchString(s)
}

// HealthReason accepts the three reasons DD's health command prints.
func HealthReason(s string) bool {
	return s == ReasonNotConnected || s == ReasonPolicyInvalid || s == ReasonNotRunning
}

// FileStatus is one host_files_status item. Size and SHA256 are set only
// for a non-secret file. MatchesApplied is null until a first apply.
type FileStatus struct {
	Name           string `json:"name"`
	Exists         bool   `json:"exists"`
	OwnerOK        bool   `json:"owner_ok"`
	Reason         string `json:"reason,omitempty"`
	MatchesApplied *bool  `json:"matches_applied"`
	Size           *int64 `json:"size,omitempty"`
	SHA256         string `json:"sha256,omitempty"`
}

// StatusResult is the helper's host_file_status reply.
type StatusResult struct {
	Files []FileStatus `json:"files"`
}

// ValidStatus checks a status reply.
func ValidStatus(s StatusResult) bool {
	if s.Files == nil || len(s.Files) > MaxFiles {
		return false
	}
	for _, f := range s.Files {
		if !ValidName(f.Name) {
			return false
		}
		switch f.Reason {
		case "", StatusMissing, StatusSymlink, StatusNotRegular, StatusOwner, StatusMode,
			StatusNlink, StatusDir, StatusTooLarge, StatusIO:
		default:
			return false
		}
		if f.SHA256 != "" && !hashRE.MatchString(f.SHA256) {
			return false
		}
		if f.Size != nil && (*f.Size < 0 || *f.Size > MaxContent) {
			return false
		}
	}
	return true
}

// DecodeStrict decodes a helper reply with unknown fields refused.
func DecodeStrict(raw []byte, dest any) error { return decodeStrict(raw, dest) }

// FormatMode prints a mode as four octal digits.
func FormatMode(m uint32) string {
	s := strconv.FormatUint(uint64(m), 8)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}
