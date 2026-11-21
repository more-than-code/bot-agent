package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// lockPoll is how often a queued apply retries the per-unit lock.
const lockPoll = 100 * time.Millisecond

// logAppliedFailed is the fixed log token when <name>.applied cannot be
// written after succeeded or unchanged. The reply keeps its outcome; status
// then reports the file as not matching (or null) until the next apply.
const logAppliedFailed = "applied_record_failed"

// handleHostFile serves host_file_apply and host_file_status
// (agent-protocol v1.18, spec §2.3 and rev 1a §10).
func handleHostFile(ctx context.Context, verb string, in []byte, sys Sys) []byte {
	if len(in) > hostfiles.MaxFrame {
		return refuse(sys, programs.ReasonBadSpec)
	}
	if ctx.Err() != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if err := ensureRuntime(sys.Root); err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if verb == hostfiles.VerbStatus {
		return doHostFileStatus(in, sys.loadHostPolicy(), sys)
	}
	return doHostFileApply(ctx, in, sys)
}

func (sys Sys) loadHostPolicy() policy.Policy {
	return policy.Load(policy.Files{
		Policy: sys.Policy, Passwd: sys.Passwd, Group: sys.Group, UID: sys.UID, Exec: sys.execFS,
	})
}

func (sys Sys) clock() time.Time {
	if sys.now != nil {
		return sys.now()
	}
	return time.Now()
}

func (sys Sys) pause(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	if sys.sleep != nil {
		sys.sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (sys Sys) logf(format string, args ...any) {
	if sys.Logf != nil {
		sys.Logf(format, args...)
	}
}

// unitLockPath is /run/bot-agent/hostfile-unit-<unit>.lock. The unit passed
// the policy's unit rule, so it is one safe path component.
func unitLockPath(root, unit string) string {
	return joinRoot(root, filepath.Join("run", "bot-agent", "hostfile-unit-"+unit+".lock"))
}

// lockUnit serialises applies per unit (rev 1a A1): dd-policy and
// dd-secrets share dd-worker.service, and one's restart must not fail the
// other's health check. A queued apply polls with LOCK_NB, so a cancelled
// request stops waiting. It is not the helper-wide lock: program verbs
// are never blocked for minutes.
func lockUnit(ctx context.Context, root, unit string) (func(), error) {
	f, err := os.OpenFile(unitLockPath(root, unit), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		t := time.NewTimer(lockPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// tryLockUnit takes the unit lock only when it is free.
func tryLockUnit(root, unit string) (func(), bool) {
	f, err := os.OpenFile(unitLockPath(root, unit), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true
}

// verdict is the result of one restart-and-confirm.
type verdict struct {
	ok     bool
	stage  string
	reason string
}

// applyRun is one host_file_apply call.
type applyRun struct {
	sys     Sys
	name    string
	hf      policy.HostFile
	known   bool
	start   time.Time
	content []byte
	sum     string
	settle  int
	force   bool
	t       *hostTarget
	current []byte
	gid     int
}

// reply encodes a result. sha256 only on succeeded and unchanged of a
// non-secret file (rev 1a A5).
func (r *applyRun) reply(outcome, stage, reason string) []byte {
	res := hostfiles.ApplyResult{
		Outcome: outcome, Stage: stage, Reason: reason,
		TookMS: r.sys.clock().Sub(r.start).Milliseconds(),
	}
	if res.TookMS < 0 {
		res.TookMS = 0
	}
	if r.known && !r.hf.Secret && (outcome == hostfiles.OutcomeSucceeded || outcome == hostfiles.OutcomeUnchanged) {
		res.SHA256 = r.sum
	}
	r.sys.logf("bot-agent-runner: host_file_apply %s %s stage=%s reason=%s", r.name, outcome, dash(stage), dash(reason))
	return images.Encode(res)
}

func (r *applyRun) recordApplied() {
	if err := writeHostState(r.sys, r.hf.Name, ".applied", []byte(r.sum+"\n")); err != nil {
		r.sys.logf("bot-agent-runner: host_file_apply %s %s", r.hf.Name, logAppliedFailed)
	}
}

func (r *applyRun) fault(step string) error {
	if r.sys.hostFault == nil {
		return nil
	}
	return r.sys.hostFault(step)
}

// doHostFileApply is spec §2.3 steps 1–10 with rev 1a. Before the swap a
// cancelled request (the agent gave up) stops the apply. From the swap on,
// the restart, the confirmation and any rollback run to the end on their
// own budgets, so the host is never left with a new file and no verdict.
func doHostFileApply(ctx context.Context, in []byte, sys Sys) []byte {
	req, content, err := hostfiles.ParseApply(in)
	if err != nil {
		return refuse(sys, programs.ReasonBadSpec)
	}
	r := &applyRun{sys: sys, name: req.Name, start: sys.clock(), content: content, sum: sha256Hex(content), force: req.Force}
	// 1. Look up the name, take the unit lock, then load the policy again
	// so the checks below use what is in force while the lock is held.
	pol := sys.loadHostPolicy()
	hf, known := pol.HostFile(req.Name)
	if !pol.Valid || !known {
		return r.reply(hostfiles.OutcomeForbiddenSpec, hostfiles.StageLookup, "")
	}
	unlock, err := lockUnit(ctx, sys.Root, hf.Unit)
	if err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	defer unlock()
	if ctx.Err() != nil {
		return refuse(sys, programs.ReasonIO)
	}
	pol = sys.loadHostPolicy()
	hf2, known := pol.HostFile(req.Name)
	if !pol.Valid || !known || hf2.Unit != hf.Unit {
		return r.reply(hostfiles.OutcomeForbiddenSpec, hostfiles.StageLookup, "")
	}
	r.hf, r.known = hf2, true
	settle, ok := hostfiles.Settle(req.SettleSec, r.hf.Health)
	if !ok {
		return r.reply(hostfiles.OutcomeForbiddenSpec, hostfiles.StageSettle, "")
	}
	r.settle = settle
	if out := r.prepare(); out != nil {
		return out
	}
	defer r.t.close()
	// 3. Unchanged: the target is not written and nothing restarts; the
	// hash is recorded as applied.
	if sha256Hex(r.current) == r.sum {
		r.recordApplied()
		return r.reply(hostfiles.OutcomeUnchanged, "", "")
	}
	// 4. Candidate beside the target. From here one defer removes any
	// candidate left behind (after a swap the name is gone already).
	if err := r.t.writeCandidate(r.content, r.gid); err != nil {
		_ = r.t.removeCandidate()
		return refuse(sys, programs.ReasonIO)
	}
	defer func() { _ = r.t.removeCandidate() }()
	if out := r.checkCandidate(ctx); out != nil {
		return out
	}
	if ctx.Err() != nil {
		return refuse(sys, programs.ReasonIO)
	}
	return r.commitAndConfirm(context.WithoutCancel(ctx))
}

// prepare runs the executable check (stage exec) and step 2, and reads the
// current bytes.
func (r *applyRun) prepare() []byte {
	if !r.hf.CommandsSafe(r.sys.execFS) {
		return r.reply(hostfiles.OutcomeForbiddenSpec, hostfiles.StageExec, "")
	}
	t, reason := openHostTarget(r.sys, r.hf)
	if reason != "" {
		return r.reply(hostfiles.OutcomeForbiddenSpec, hostfiles.StageTarget, "")
	}
	current, gid, reason := t.read()
	if reason != "" {
		t.close()
		return r.reply(hostfiles.OutcomeForbiddenSpec, hostfiles.StageTarget, "")
	}
	r.t, r.current, r.gid = t, current, gid
	return nil
}

// checkCandidate is steps 5 and 5b.
func (r *applyRun) checkCandidate(ctx context.Context) []byte {
	cand, err := r.t.openCandidate()
	if err != nil {
		return refuse(r.sys, programs.ReasonIO)
	}
	code, stderr, runErr := runHostCmd(ctx, r.sys, r.hf.Check, r.hf, cand, hostfiles.CheckBudget, hostfiles.RunGrace)
	_ = cand.Close()
	if runErr != nil || code != 0 {
		if errors.Is(runErr, errExecUnsafe) {
			return r.reply(hostfiles.OutcomeForbiddenSpec, hostfiles.StageExec, "")
		}
		reason := hostfiles.ReasonCheckError
		if runErr == nil && code == 2 {
			if tok, ok := oneLine(stderr); ok && hostfiles.CheckReasonRE.MatchString(tok) {
				reason = tok
			}
		}
		return r.reply(hostfiles.OutcomeCheckFailed, hostfiles.StageCheck, reason)
	}
	if r.hf.Idle == nil || r.force {
		return nil
	}
	code, _, runErr = runHostCmd(ctx, r.sys, r.hf.Idle, r.hf, nil, hostfiles.IdleBudget, hostfiles.RunGrace)
	switch {
	case errors.Is(runErr, errExecUnsafe):
		return r.reply(hostfiles.OutcomeForbiddenSpec, hostfiles.StageExec, "")
	case runErr == nil && code == 4:
		return r.reply(hostfiles.OutcomeWorkerBusy, hostfiles.StageIdle, "")
	case runErr != nil || code != 0:
		return r.reply(hostfiles.OutcomeCheckFailed, hostfiles.StageIdle, hostfiles.ReasonIdleError)
	}
	return nil
}

// commitAndConfirm is steps 6–10 on a context the caller cannot cancel.
func (r *applyRun) commitAndConfirm(ctx context.Context) []byte {
	if err := writeHostState(r.sys, r.hf.Name, ".prev", r.current); err != nil {
		return refuse(r.sys, programs.ReasonIO)
	}
	renamed, err := r.t.swap()
	if !renamed {
		return refuse(r.sys, programs.ReasonIO)
	}
	if err != nil {
		// The new bytes are in place; go on to the restart and verdict.
		r.sys.logf("bot-agent-runner: host_file_apply %s dir_fsync_failed", r.hf.Name)
	}
	if v := confirmUnit(ctx, r.sys, r.hf, r.settle); !v.ok {
		return r.rollback(ctx, v)
	}
	r.recordApplied()
	return r.reply(hostfiles.OutcomeSucceeded, "", "")
}

// rollback is step 9: the previous bytes go back by the same
// candidate-and-rename path, then restart and confirm again.
func (r *applyRun) rollback(ctx context.Context, first verdict) []byte {
	failed := func(v verdict) []byte {
		reason := v.reason
		if reason == "" {
			reason = first.reason
		}
		return r.reply(hostfiles.OutcomeRollbackFailed, hostfiles.StageRollback, reason)
	}
	if err := r.fault("rollback_write"); err != nil {
		return failed(verdict{})
	}
	if err := r.t.writeCandidate(r.current, r.gid); err != nil {
		return failed(verdict{})
	}
	if err := r.fault("rollback_swap"); err != nil {
		return failed(verdict{})
	}
	renamed, err := r.t.swap()
	if !renamed {
		return failed(verdict{})
	}
	if err != nil {
		r.sys.logf("bot-agent-runner: host_file_apply %s dir_fsync_failed", r.hf.Name)
	}
	if v := confirmUnit(ctx, r.sys, r.hf, r.settle); !v.ok {
		return failed(v)
	}
	return r.reply(hostfiles.OutcomeRolledBack, first.stage, first.reason)
}

// confirmUnit restarts the unit (at most RestartBudget) and confirms it
// within settle seconds counted from when the restart returned: the health
// command exits 0, or, without one, the unit is active right after the
// restart and still active with the same InvocationID at the end.
func confirmUnit(ctx context.Context, sys Sys, hf policy.HostFile, settle int) verdict {
	failRestart := verdict{stage: hostfiles.StageRestart}
	if sys.Systemctl == nil {
		return failRestart
	}
	rctx, cancel := context.WithTimeout(ctx, hostfiles.RestartBudget)
	_, err := sys.Systemctl(rctx, []string{"restart", "--", hf.Unit})
	cancel()
	if err != nil {
		return failRestart
	}
	window := time.Duration(settle) * time.Second
	deadline := sys.clock().Add(window)
	if hf.Health != nil {
		// The health call is bounded by the window itself, with no grace.
		code, stderr, err := runHostCmd(ctx, sys, hf.Health, hf, nil, window, 0)
		if err == nil && code == 0 {
			return verdict{ok: true}
		}
		reason := hostfiles.ReasonHealthError
		if err == nil {
			if tok, ok := oneLine(stderr); ok && hostfiles.HealthReason(tok) {
				reason = tok
			}
		}
		return verdict{stage: hostfiles.StageHealth, reason: reason}
	}
	state, inv, err := unitInvocation(ctx, sys, hf.Unit)
	if err != nil || state != "active" || inv == "" {
		return failRestart
	}
	sys.pause(ctx, deadline.Sub(sys.clock()))
	state2, inv2, err := unitInvocation(ctx, sys, hf.Unit)
	if err != nil || state2 != "active" || inv2 != inv {
		return failRestart
	}
	return verdict{ok: true}
}

// unitInvocation reads ActiveState and InvocationID with its own short
// timeout.
func unitInvocation(ctx context.Context, sys Sys, unit string) (string, string, error) {
	sctx, cancel := context.WithTimeout(ctx, hostfiles.ShowBudget)
	defer cancel()
	out, err := sys.Systemctl(sctx, []string{"show", "-p", "ActiveState", "-p", "InvocationID", "--no-pager", "--", unit})
	if strings.TrimSpace(out) == "" {
		if err == nil {
			err = errors.New("show")
		}
		return "", "", err
	}
	vals := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			vals[k] = strings.TrimSpace(v)
		}
	}
	return vals["ActiveState"], vals["InvocationID"], nil
}

// errExecUnsafe is an executable that failed SafeExec right before a run.
var errExecUnsafe = errors.New("exec unsafe")

// runHostCmd runs argv as the file's run_as in a transient systemd-run
// unit (spec §2.4, rev 1a A3). stdin nil is /dev/null. The executable is
// checked again right before the run. The unit's RuntimeMaxSec is budget;
// systemd-run is killed grace later. err is set when no exit status was
// obtained, including the budget running out.
func runHostCmd(ctx context.Context, sys Sys, argv []string, hf policy.HostFile, stdin *os.File, budget, grace time.Duration) (int, []byte, error) {
	if sys.RunUnit == nil {
		return -1, nil, errors.New("systemd-run")
	}
	if !policy.SafeExec(sys.execFS, argv[0]) {
		return -1, nil, errExecUnsafe
	}
	args, err := hostRunArgs(hf, budget, argv)
	if err != nil {
		return -1, nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, budget+grace)
	defer cancel()
	code, stderr, err := sys.RunUnit(cctx, args, stdin)
	if err == nil && cctx.Err() != nil {
		err = cctx.Err()
	}
	return code, stderr, err
}

// sandboxProps are the transient unit's properties (spec §2.4, rev 1a A3).
var sandboxProps = []string{
	"NoNewPrivileges=yes",
	"ProtectSystem=strict",
	"ProtectHome=yes",
	"PrivateTmp=yes",
	"PrivateNetwork=yes",
	"PrivateDevices=yes",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectControlGroups=yes",
	"RestrictSUIDSGID=yes",
	"LockPersonality=yes",
	"LimitCORE=0",
	"TimeoutStopSec=2",
	"MemoryMax=256M",
	"TasksMax=64",
	"ProtectProc=invisible",
	"ProcSubset=pid",
	"InaccessiblePaths=-/run/docker.sock -/var/run/docker.sock -/run/containerd/containerd.sock -/run/podman/podman.sock",
}

// hostRunArgs is the systemd-run argument vector. The unit name carries the
// file name and 8 random bytes so it is recognisable and never collides
// with a catalog unit.
func hostRunArgs(hf policy.HostFile, budget time.Duration, argv []string) ([]string, error) {
	secs := int((budget + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	out := []string{
		"--wait", "--pipe", "--collect", "--quiet",
		"--uid=" + strconv.FormatUint(uint64(hf.RunAs.UID), 10),
		"--gid=" + strconv.FormatUint(uint64(hf.RunAs.GID), 10),
	}
	for _, p := range sandboxProps {
		out = append(out, "-p", p)
	}
	out = append(out,
		"-p", "RuntimeMaxSec="+strconv.Itoa(secs),
		"--unit="+hostfiles.UnitPrefix+hf.Name+"-"+hex.EncodeToString(rnd[:]),
		"--",
	)
	return append(out, argv...), nil
}

// oneLine accepts at most MaxStderr bytes holding exactly one non-empty
// line (an optional trailing newline). Nothing else from stderr is kept.
func oneLine(b []byte) (string, bool) {
	if len(b) == 0 || len(b) > hostfiles.MaxStderr {
		return "", false
	}
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return "", false
	}
	return s, true
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// doHostFileStatus reports every declared file. Size and sha256 only for a
// non-secret file. It waits for no lock, so a status call is never stuck
// behind an apply (which can take minutes).
func doHostFileStatus(in []byte, pol policy.Policy, sys Sys) []byte {
	if hostfiles.ParseEmpty(in) != nil {
		return refuse(sys, programs.ReasonBadSpec)
	}
	if !pol.Valid {
		return refuse(sys, programs.ReasonPolicy)
	}
	out := hostfiles.StatusResult{Files: make([]hostfiles.FileStatus, 0, len(pol.HostFiles))}
	for _, hf := range pol.HostFiles {
		out.Files = append(out.Files, hostFileStatus(sys, hf))
	}
	return images.Encode(out)
}

func hostFileStatus(sys Sys, hf policy.HostFile) hostfiles.FileStatus {
	fs := hostfiles.FileStatus{Name: hf.Name}
	var data []byte
	readOK := false
	t, reason := openHostTarget(sys, hf)
	if reason == "" {
		sweepCandidate(sys, hf, t)
		data, _, reason = t.read()
		t.close()
		readOK = reason == ""
	}
	fs.Exists = reason != hostfiles.StatusMissing
	fs.OwnerOK = readOK
	fs.Reason = reason
	sum := ""
	if readOK {
		sum = sha256Hex(data)
		if !hf.Secret {
			n := int64(len(data))
			fs.Size = &n
			fs.SHA256 = sum
		}
	}
	applied, err := readApplied(sys, hf.Name)
	if err == nil && applied != "" {
		m := readOK && applied == sum
		fs.MatchesApplied = &m
	}
	return fs
}

// sweepCandidate removes a candidate left by a helper that was killed
// mid-apply. It runs only when the unit lock is free, so it never touches a
// running apply's candidate. A directory at that name is left and logged.
func sweepCandidate(sys Sys, hf policy.HostFile, t *hostTarget) {
	exists, isDirectory, err := t.candidateKind()
	if err != nil || !exists {
		return
	}
	unlock, ok := tryLockUnit(sys.Root, hf.Unit)
	if !ok {
		return
	}
	defer unlock()
	if isDirectory {
		sys.logf("bot-agent-runner: host_file_status %s candidate_is_directory", hf.Name)
		return
	}
	if err := t.removeCandidate(); err != nil {
		sys.logf("bot-agent-runner: host_file_status %s candidate_remove_failed", hf.Name)
		return
	}
	sys.logf("bot-agent-runner: host_file_status %s leftover_candidate_removed", hf.Name)
}
