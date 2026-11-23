package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/hostpolicy"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// Paths of the v1.22 console host policy, relative to Sys.Root.
const (
	policyStateRel = "var/lib/bot-agent-runner/policy"
	helloMarkerRel = "var/lib/bot-agent/last_hello"
	policyLockName = "policy.lock"
	agentUnit      = "bot-agent.service"
)

// PolicyStateDir holds staged.toml and result.json.
func PolicyStateDir(root string) string { return joinRoot(root, policyStateRel) }

func policyStagedPath(root string) string {
	return filepath.Join(PolicyStateDir(root), "staged.toml")
}

// PolicyResultPath is result.json under root.
func PolicyResultPath(root string) string {
	return filepath.Join(PolicyStateDir(root), "result.json")
}

func policyLockPath(root string) string {
	return joinRoot(root, filepath.Join("run", "bot-agent", policyLockName))
}

// policyFilePath is the runner.toml path policy-run writes: Sys.Policy, or
// the production path under root.
func (sys Sys) policyFilePath() string {
	if sys.Policy != "" {
		return sys.Policy
	}
	return joinRoot(sys.Root, "etc/bot-agent/runner.toml")
}

// handleHostPolicy serves host_policy_apply (agent-protocol v1.22 item 3).
// The checks run in the contract's order; on success the new file is
// staged under the state directory and the transient unit starts. The
// helper itself never writes /etc/bot-agent: its sandbox cannot.
func handleHostPolicy(ctx context.Context, in []byte, sys Sys) []byte {
	if len(in) > programs.MaxFrame {
		return refuse(sys, programs.ReasonBadSpec)
	}
	if ctx.Err() != nil {
		return refuse(sys, programs.ReasonIO)
	}
	pol := sys.loadHostPolicy()
	if !pol.ConsoleManaged() {
		return refuse(sys, programs.ReasonPolicy)
	}
	req, ok := hostpolicy.ParseRequest(in)
	if !ok {
		return refuse(sys, programs.ReasonBadSpec)
	}
	data, err := readSmallNoFollow(sys.policyFilePath())
	if err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if sha256Hex(data) != req.BaseHash {
		return refuse(sys, hostpolicy.ReasonConflict)
	}
	next := policy.Rewrite(data, policy.Managed{Edge: req.Edge, Images: req.Images, Projects: req.Projects})
	if string(next) == string(data) {
		return refuse(sys, programs.ReasonBadSpec)
	}
	np := policy.FromBytes(next, sys.Passwd, sys.Group, sys.execFS)
	if !np.ConsoleManaged() {
		return refuse(sys, programs.ReasonBadSpec)
	}
	if err := ensureRuntime(sys.Root); err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	unlock, held := tryLockHelper(sys.Root)
	if !held {
		return refuse(sys, hostpolicy.ReasonBusy)
	}
	defer unlock()
	if otherLocksHeld(sys.Root) || UpgradeRunning(sys.Root) {
		return refuse(sys, hostpolicy.ReasonBusy)
	}
	active, err := unitActive(ctx, sys, hostpolicy.Unit)
	if err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if active {
		return refuse(sys, hostpolicy.ReasonBusy)
	}
	if err := ensurePrivateDir(PolicyStateDir(sys.Root)); err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if err := atomicWrite(policyStagedPath(sys.Root), next, 0o600); err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if sys.RunUnit == nil {
		return refuse(sys, programs.ReasonIO)
	}
	newHash := sha256Hex(next)
	code, _, err := sys.RunUnit(ctx, PolicyUnitArgs(req.BaseHash, newHash), nil)
	if err != nil || code != 0 {
		sys.logf("bot-agent-runner: host_policy_apply systemd-run failed")
		return refuse(sys, programs.ReasonIO)
	}
	sys.logf("bot-agent-runner: host_policy_apply started")
	return hostpolicy.Encode(hostpolicy.StartReply{OK: true, State: hostpolicy.StateAccepted, NewHash: newHash})
}

// PolicyUnitArgs is the systemd-run argument vector. Both hashes are
// 64 hex characters; nothing else from the request reaches the command.
func PolicyUnitArgs(baseHash, newHash string) []string {
	return []string{
		"--unit=" + hostpolicy.Unit,
		"--collect",
		"--property=RuntimeMaxSec=" + strconv.Itoa(hostpolicy.RuntimeMax),
		"/usr/local/libexec/bot-agent-runner",
		hostpolicy.RunSubcommand,
		baseHash,
		newHash,
	}
}

// unitActive reports whether unit.service is running or changing state.
func unitActive(ctx context.Context, sys Sys, unit string) (bool, error) {
	if sys.Systemctl == nil {
		return false, errors.New("systemctl")
	}
	out, err := sys.Systemctl(ctx, []string{"is-active", unit + ".service"})
	switch firstWord(out) {
	case "inactive", "failed":
		return false, nil
	case "active", "activating", "deactivating", "reloading", "refreshing":
		return true, nil
	case "":
		if err != nil {
			return false, err
		}
		return false, errors.New("is-active")
	default:
		return true, nil
	}
}

// PolicyRun is `bot-agent-runner policy-run <base_hash> <new_hash>`
// (agent-protocol v1.22 item 4), run as root in the transient unit:
//  1. wait RestartDelay so the agent can send the accepted task result;
//  2. runner.toml must still hash to base_hash and the staged file to
//     new_hash, and the staged file must still be a valid opted-in policy,
//     else failed with changed;
//  3. keep runner.toml.prev (0600), write the staged file as runner.toml
//     (0644), restart bot-agent;
//  4. wait up to HelloWait for the hello marker to be newer than the
//     restart; without it put the previous file back, restart again and
//     record rolled_back not_reconnected.
//
// It writes result.json and returns the result.
func PolicyRun(ctx context.Context, baseHash, newHash string, sys Sys) (hostpolicy.Result, error) {
	lock, err := os.OpenFile(policyLockPath(sys.Root), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err == nil {
		defer lock.Close()
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
	}
	res := policyRun(ctx, baseHash, newHash, sys)
	res.At = sys.clock().UTC().Format(time.RFC3339)
	_ = os.Remove(policyStagedPath(sys.Root))
	b, _ := json.Marshal(res)
	if err := ensurePrivateDir(PolicyStateDir(sys.Root)); err != nil {
		return res, err
	}
	return res, atomicWrite(PolicyResultPath(sys.Root), b, 0o600)
}

func policyRun(ctx context.Context, baseHash, newHash string, sys Sys) hostpolicy.Result {
	failed := func(reason, hash string) hostpolicy.Result {
		sys.logf("bot-agent-runner: policy-run %s %s", hostpolicy.OutcomeFailed, reason)
		return hostpolicy.Result{Outcome: hostpolicy.OutcomeFailed, Reason: reason, Hash: hash}
	}
	sys.pause(ctx, hostpolicy.RestartDelay)
	path := sys.policyFilePath()
	old, err := readSmallNoFollow(path)
	if err != nil || sha256Hex(old) != baseHash {
		return failed(hostpolicy.ReasonChanged, baseHash)
	}
	next, err := readSmallNoFollow(policyStagedPath(sys.Root))
	if err != nil || sha256Hex(next) != newHash {
		return failed(hostpolicy.ReasonChanged, baseHash)
	}
	if !policy.FromBytes(next, sys.Passwd, sys.Group, sys.execFS).ConsoleManaged() {
		return failed(hostpolicy.ReasonChanged, baseHash)
	}
	prev := path + ".prev"
	if err := atomicWrite(prev, old, 0o600); err != nil {
		return failed(hostpolicy.ReasonWrite, baseHash)
	}
	if err := atomicWrite(path, next, 0o644); err != nil {
		return failed(hostpolicy.ReasonWrite, baseHash)
	}
	started := sys.clock()
	reason := ""
	if _, err := sys.Systemctl(ctx, []string{"restart", agentUnit}); err != nil {
		reason = hostpolicy.ReasonRestart
	} else if !waitHello(ctx, sys, started) {
		reason = hostpolicy.ReasonNotReconnected
	}
	if reason == "" {
		sys.logf("bot-agent-runner: policy-run %s", hostpolicy.OutcomeSucceeded)
		return hostpolicy.Result{Outcome: hostpolicy.OutcomeSucceeded, Hash: newHash}
	}
	// Put the previous file back and restart. The rollback does not wait
	// for a hello: with the server down no agent can say one.
	if err := atomicWrite(path, old, 0o644); err != nil {
		return failed(hostpolicy.ReasonWrite, newHash)
	}
	if _, err := sys.Systemctl(ctx, []string{"restart", agentUnit}); err != nil {
		return failed(hostpolicy.ReasonRestart, baseHash)
	}
	sys.logf("bot-agent-runner: policy-run %s %s", hostpolicy.OutcomeRolledBack, reason)
	return hostpolicy.Result{Outcome: hostpolicy.OutcomeRolledBack, Reason: reason, Hash: baseHash}
}

// waitHello waits up to HelloWait for the agent's hello marker to have an
// mtime after started.
func waitHello(ctx context.Context, sys Sys, started time.Time) bool {
	marker := joinRoot(sys.Root, helloMarkerRel)
	deadline := started.Add(hostpolicy.HelloWait)
	for {
		if fi, err := os.Lstat(marker); err == nil && fi.Mode().IsRegular() && fi.ModTime().After(started) {
			return true
		}
		if ctx.Err() != nil || !sys.clock().Before(deadline) {
			return false
		}
		sys.pause(ctx, 2*time.Second)
	}
}
