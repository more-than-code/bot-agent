// Package runner is the root helper behind the bot-agent socket. The agent does not import it.
// Every verb re-reads the policy and re-runs programs.Check. Side effects go
// through Sys so tests can run as an unprivileged user.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/redact"
)

const maxJournalRead = 1 << 20

// Sys is the helper's effect surface. Root is the filesystem prefix for
// /run; an empty root or "/" is the real host. Unit text still cites the
// production EnvironmentFile path.
type Sys struct {
	Systemctl  func(context.Context, []string) (string, error)
	Journalctl func(context.Context, []string) (string, error)
	Root       string
	Proc       string
	Sysfs      string
	Passwd     string
	Group      string
	Policy     string
	UID        func(os.FileInfo) (uint32, bool)
	Logf       func(string, ...any)
	// MachineID is the host-id file. Empty means /etc/machine-id.
	// DockerSock is the Engine socket. Empty means /var/run/docker.sock.
	// Neither is taken from a request. Tests set both.
	MachineID  string
	DockerSock string
}

// Handle runs one verb. in is the raw JSON object from stdin.
// The result is one JSON object and never contains env values.
func Handle(ctx context.Context, verb string, in []byte, sys Sys) []byte {
	if images.IsVerb(verb) {
		return handleImage(ctx, verb, in, sys)
	}
	if ctx.Err() != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if !programs.KnownVerb(verb) {
		return refuse(sys, programs.ReasonVerb)
	}
	if len(in) > programs.MaxFrame {
		return refuse(sys, programs.ReasonBadSpec)
	}
	pol := policy.Load(policy.Files{
		Policy: sys.Policy, Passwd: sys.Passwd, Group: sys.Group, UID: sys.UID,
	})
	if err := ensureRuntime(sys.Root); err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if verb == "list" {
		return doList(ctx, in, pol, sys)
	}
	// Each connection is its own root process, so serialise the mutating verbs:
	// the max_programs count, the checks and the unit writes happen under one lock.
	if verb == "start" || verb == "restart" || verb == "stop" {
		unlock, err := lockHelper(sys.Root)
		if err != nil {
			return refuse(sys, programs.ReasonIO)
		}
		defer unlock()
	}
	gate := programs.Gate{Policy: pol, Files: true}
	if verb == "start" || verb == "restart" {
		n, have, ok := countUnits(sys)
		gate.CountKnown = true
		if !ok {
			gate.Running = math.MaxInt
		} else {
			gate.Running = n
			gate.Active = have
		}
	}
	res, code := programs.Check(verbKind(verb), in, gate)
	if code != "" {
		return refuse(sys, code)
	}
	switch verb {
	case "start":
		return doStart(ctx, res, sys)
	case "stop":
		return doStop(ctx, res, sys)
	case "restart":
		return doRestart(ctx, res, sys)
	case "status":
		return doStatus(ctx, res, sys)
	case "logs":
		return doLogs(ctx, res, sys)
	case "sockets":
		return doSockets(res, sys)
	default:
		return refuse(sys, programs.ReasonVerb)
	}
}

func verbKind(verb string) string {
	switch verb {
	case "start":
		return "program_start"
	case "stop":
		return "program_stop"
	case "restart":
		return "program_restart"
	case "status", "sockets":
		return "program_status"
	case "logs":
		return "program_logs"
	default:
		return ""
	}
}

func refuse(sys Sys, reason string) []byte {
	if sys.Logf != nil {
		sys.Logf("bot-agent-runner: refuse %s", reason)
	}
	return programs.EncodeRefusal(reason)
}

func doList(ctx context.Context, in []byte, pol policy.Policy, sys Sys) []byte {
	if !pol.Valid || !pol.Enabled {
		return refuse(sys, programs.ReasonPolicy)
	}
	if !emptyObject(in) {
		return refuse(sys, programs.ReasonBadSpec)
	}
	units, err := readOwned(sys.Root)
	if err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	out := make([]programs.UnitInfo, 0, len(units))
	for _, u := range units {
		st, err := isActive(ctx, sys, u.Unit)
		if err != nil {
			return refuse(sys, programs.ReasonIO)
		}
		out = append(out, programs.UnitInfo{
			Unit: u.Unit, Project: u.Project, Entry: u.Entry, ActiveState: st,
		})
	}
	return programs.EncodeList(out)
}

func emptyObject(in []byte) bool {
	if len(bytes.TrimSpace(in)) == 0 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	var empty struct{}
	if err := dec.Decode(&empty); err != nil {
		return false
	}
	return !dec.More()
}

// countUnits counts every unit file this helper created, including inactive
// and failed ones. A unit in the set already holds its slot.
func countUnits(sys Sys) (int, map[string]bool, bool) {
	units, err := readOwned(sys.Root)
	if err != nil {
		return 0, nil, false
	}
	have := map[string]bool{}
	for _, u := range units {
		have[u.Unit] = true
	}
	return len(units), have, true
}

func doStart(ctx context.Context, res programs.Resolved, sys Sys) []byte {
	if !safeToken(res.RunAs) || badGroup(res.Group) || !noBreak(res.Home) || !noBreak(res.Dir) || !noBreak(res.Exec) {
		return refuse(sys, programs.ReasonPolicy)
	}
	for k, v := range res.Env {
		if !noBreak(k) || !noBreak(v) {
			return refuse(sys, programs.ReasonBadSpec)
		}
	}
	for _, arg := range res.Args {
		if !noBreak(arg) {
			return refuse(sys, programs.ReasonBadSpec)
		}
	}
	path := unitFile(sys.Root, res.Unit)
	missing, err := fileMissing(path)
	if err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if !missing && !createdByUs(sys.Root, res) {
		return refuse(sys, programs.ReasonNotOwned)
	}
	if err := atomicWrite(path, []byte(renderUnit(res)), 0o644); err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if err := atomicWrite(envFile(sys.Root, res.Unit), []byte(renderEnv(res.Env)), 0o600); err != nil {
		_ = os.Remove(path)
		return refuse(sys, programs.ReasonIO)
	}
	spec, err := recordSpec(res)
	if err != nil {
		_ = os.Remove(path)
		_ = os.Remove(envFile(sys.Root, res.Unit))
		return refuse(sys, programs.ReasonIO)
	}
	if err := atomicWrite(specFile(sys.Root, res.Unit), spec, 0o600); err != nil {
		_ = os.Remove(path)
		_ = os.Remove(envFile(sys.Root, res.Unit))
		return refuse(sys, programs.ReasonIO)
	}
	if err := daemonReload(ctx, sys); err != nil {
		discardUnit(sys, res.Unit)
		return programs.EncodeStart(failedStatus(res.Unit), false)
	}
	before, _ := readMono(ctx, sys, res.Unit)
	var restartErr error
	if sys.Systemctl == nil {
		restartErr = errors.New("systemctl")
	} else {
		_, restartErr = sys.Systemctl(ctx, []string{"restart", res.Unit})
	}
	out, drop := finishActivation(ctx, sys, res.Unit, before, restartErr)
	if drop {
		discardUnit(sys, res.Unit)
	}
	return out
}

func doStop(ctx context.Context, res programs.Resolved, sys Sys) []byte {
	path := unitFile(sys.Root, res.Unit)
	missing, err := fileMissing(path)
	if err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if missing {
		return programs.EncodeStatus(inactive(res.Unit))
	}
	if !createdByUs(sys.Root, res) {
		return refuse(sys, programs.ReasonNotOwned)
	}
	if sys.Systemctl == nil {
		return refuse(sys, programs.ReasonIO)
	}
	if _, err := sys.Systemctl(ctx, []string{"stop", res.Unit}); err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return refuse(sys, programs.ReasonIO)
	}
	if err := os.Remove(envFile(sys.Root, res.Unit)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return refuse(sys, programs.ReasonIO)
	}
	if err := os.Remove(specFile(sys.Root, res.Unit)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return refuse(sys, programs.ReasonIO)
	}
	if err := daemonReload(ctx, sys); err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	return programs.EncodeStatus(inactive(res.Unit))
}

func doRestart(ctx context.Context, res programs.Resolved, sys Sys) []byte {
	if !createdByUs(sys.Root, res) {
		return refuse(sys, programs.ReasonNotOwned)
	}
	if code := recheckSpec(sys, res.Unit); code != "" {
		discardUnit(sys, res.Unit)
		return refuse(sys, programs.ReasonPolicy)
	}
	before, _ := readMono(ctx, sys, res.Unit)
	var restartErr error
	if sys.Systemctl == nil {
		restartErr = errors.New("systemctl")
	} else {
		_, restartErr = sys.Systemctl(ctx, []string{"restart", res.Unit})
	}
	out, _ := finishActivation(ctx, sys, res.Unit, before, restartErr)
	return out
}

func doStatus(ctx context.Context, res programs.Resolved, sys Sys) []byte {
	if !createdByUs(sys.Root, res) {
		return refuse(sys, programs.ReasonNotOwned)
	}
	st, err := show(ctx, sys, res.Unit)
	if err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	return programs.EncodeStatus(st)
}

func doLogs(ctx context.Context, res programs.Resolved, sys Sys) []byte {
	if !createdByUs(sys.Root, res) {
		return refuse(sys, programs.ReasonNotOwned)
	}
	if sys.Journalctl == nil {
		return refuse(sys, programs.ReasonIO)
	}
	out, err := sys.Journalctl(ctx, []string{
		"-u", res.Unit, "-n", strconv.Itoa(res.Lines), "-o", "cat", "--no-pager",
	})
	if err != nil && out == "" {
		return refuse(sys, programs.ReasonIO)
	}
	if len(out) > maxJournalRead {
		out = out[:maxJournalRead]
	}
	lines, truncated := redact.Journal(splitLines(out))
	return programs.EncodeLogs(programs.LogBatch{Lines: lines, Truncated: truncated})
}

func doSockets(res programs.Resolved, sys Sys) []byte {
	if !createdByUs(sys.Root, res) {
		return refuse(sys, programs.ReasonNotOwned)
	}
	proc, sysfs := sys.Proc, sys.Sysfs
	if proc == "" {
		proc = "/proc"
	}
	if sysfs == "" {
		sysfs = "/sys"
	}
	ports, user, err := mapSockets(proc, sysfs, sys.Passwd, res.Unit)
	if err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	return programs.EncodeSockets(ports, user)
}

func inactive(unit string) programs.Status {
	return programs.Status{Unit: unit, ActiveState: "inactive", SubState: "dead", Ports: []int{}}
}

func daemonReload(ctx context.Context, sys Sys) error {
	if sys.Systemctl == nil {
		return errors.New("systemctl")
	}
	_, err := sys.Systemctl(ctx, []string{"daemon-reload"})
	return err
}

func isActive(ctx context.Context, sys Sys, unit string) (string, error) {
	if sys.Systemctl == nil {
		return "", errors.New("systemctl")
	}
	out, err := sys.Systemctl(ctx, []string{"is-active", unit})
	st := firstWord(out)
	if st == "" {
		if err != nil {
			return "", err
		}
		return "", errors.New("is-active")
	}
	return st, nil
}

// finishActivation maps a restart onto start_failed (fresh false) when
// systemctl exits non-zero or the unit did not enter active again.
// The bool tells doStart to stop the unit and remove its files.
// A show failure after a zero exit stays io_error and keeps the files.
func finishActivation(ctx context.Context, sys Sys, unit string, before uint64, restartErr error) ([]byte, bool) {
	if restartErr != nil {
		st, err := show(ctx, sys, unit)
		if err != nil {
			st = failedStatus(unit)
		}
		return programs.EncodeStart(st, false), true
	}
	after, ok := readMono(ctx, sys, unit)
	st, err := show(ctx, sys, unit)
	if err != nil {
		return refuse(sys, programs.ReasonIO), false
	}
	if !ok || after <= before {
		return programs.EncodeStart(st, false), true
	}
	return programs.EncodeStart(st, true), false
}

func failedStatus(unit string) programs.Status {
	return programs.Status{Unit: unit, ActiveState: "failed", SubState: "failed", Ports: []int{}}
}

// recheckSpec runs the stored start request through the current policy.
// A missing or now-disallowed spec is policy_denied.
func recheckSpec(sys Sys, unit string) string {
	raw, err := os.ReadFile(specFile(sys.Root, unit))
	if err != nil || len(raw) > programs.MaxFrame {
		return programs.ReasonPolicy
	}
	pol := policy.Load(policy.Files{
		Policy: sys.Policy, Passwd: sys.Passwd, Group: sys.Group, UID: sys.UID,
	})
	checked, code := programs.Check("program_start", raw, programs.Gate{Policy: pol, Files: true})
	if code != "" || checked.Unit != unit {
		return programs.ReasonPolicy
	}
	return ""
}

// discardUnit stops a unit this call created and removes its files.
// Stop uses its own short context so a cancelled request still reaps the unit.
func discardUnit(sys Sys, unit string) {
	if sys.Systemctl != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = sys.Systemctl(stopCtx, []string{"stop", unit})
		_, _ = sys.Systemctl(stopCtx, []string{"daemon-reload"})
		cancel()
	}
	_ = os.Remove(unitFile(sys.Root, unit))
	_ = os.Remove(envFile(sys.Root, unit))
	_ = os.Remove(specFile(sys.Root, unit))
}

func readMono(ctx context.Context, sys Sys, unit string) (uint64, bool) {
	if sys.Systemctl == nil {
		return 0, false
	}
	out, err := sys.Systemctl(ctx, []string{"show", unit, "-p", "ActiveEnterTimestampMonotonic", "--no-pager"})
	if strings.TrimSpace(out) == "" {
		if err != nil {
			return 0, false
		}
		return 0, false
	}
	val := ""
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && k == "ActiveEnterTimestampMonotonic" {
			val = v
		}
	}
	n, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func show(ctx context.Context, sys Sys, unit string) (programs.Status, error) {
	if sys.Systemctl == nil {
		return programs.Status{}, errors.New("systemctl")
	}
	out, err := sys.Systemctl(ctx, []string{
		"show", unit,
		"-p", "ActiveState",
		"-p", "SubState",
		"-p", "MainPID",
		"-p", "ActiveEnterTimestampUSec",
		"--no-pager",
	})
	if strings.TrimSpace(out) == "" {
		if err != nil {
			return programs.Status{}, err
		}
		return programs.Status{}, errors.New("show")
	}
	vals := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			vals[k] = v
		}
	}
	if vals["ActiveState"] == "" {
		return programs.Status{}, errors.New("show")
	}
	st := programs.Status{
		Unit: unit, ActiveState: vals["ActiveState"], SubState: vals["SubState"], Ports: []int{},
	}
	if pid, err := strconv.Atoi(vals["MainPID"]); err == nil && pid > 0 {
		st.MainPID = &pid
	}
	if us, err := strconv.ParseInt(vals["ActiveEnterTimestampUSec"], 10, 64); err == nil && us > 0 {
		s := protocol.FormatTime(time.UnixMicro(us).UTC())
		st.StartedAt = &s
	}
	return st, nil
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func badGroup(s string) bool {
	return s == "root" || s == "0" || !safeToken(s)
}

func safeToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func noBreak(s string) bool {
	return !strings.ContainsAny(s, "\n\r\x00")
}
