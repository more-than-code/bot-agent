package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
	"github.com/more-than-code/bot-agent/internal/agent/edge"
	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// Paths of the v1.20 agent upgrade, relative to Sys.Root.
const (
	upgradeStateRel    = "var/lib/bot-agent-runner/upgrade"
	installerRecordRel = "var/lib/bot-agent-installer/installed"
)

// UpgradeStateDir is the staging and result directory under root.
func UpgradeStateDir(root string) string {
	return joinRoot(root, upgradeStateRel)
}

// UpgradeResultPath is result.json under root.
func UpgradeResultPath(root string) string {
	return filepath.Join(UpgradeStateDir(root), "result.json")
}

// InstallerRecordPath is the installer's record under root.
func InstallerRecordPath(root string) string {
	return joinRoot(root, installerRecordRel)
}

// UpgradeLockPath is /run/bot-agent/upgrade.lock under root. upgrade-run
// holds it for its whole run.
func UpgradeLockPath(root string) string {
	return joinRoot(root, filepath.Join("run", "bot-agent", agentupgrade.LockName))
}

// UpgradeRunning reports whether an upgrade-run holds its lock. A missing
// or unopenable file is not running.
func UpgradeRunning(root string) bool {
	f, err := os.OpenFile(UpgradeLockPath(root), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// mutatingVerb is a helper verb that changes a program, a container, the
// edge or a host file. While an upgrade runs, these answer busy (v1.20
// item 5).
func mutatingVerb(verb string) bool {
	switch verb {
	case "start", "stop", "restart",
		images.VerbPull, images.VerbDeploy, images.VerbRollback, images.VerbRemove,
		images.VerbStart, images.VerbStop, images.VerbRestart,
		edge.VerbApply, edge.VerbStop, hostfiles.VerbApply:
		return true
	default:
		return false
	}
}

// busyForUpgrade is the busy reply in each verb family's own shape, or nil
// when the verb may run.
func busyForUpgrade(verb string, sys Sys) []byte {
	if !mutatingVerb(verb) || !UpgradeRunning(sys.Root) {
		return nil
	}
	switch {
	case images.IsVerb(verb):
		return imgErr(sys, agentupgrade.ReasonBusy)
	case edge.IsVerb(verb):
		return edgeErr(sys, agentupgrade.ReasonBusy, "")
	default:
		return refuse(sys, agentupgrade.ReasonBusy)
	}
}

// maxSmallFile caps the upgrade files the helper reads (result.json and
// the installer record).
const maxSmallFile = 64 * 1024

func readSmallNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSmallFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSmallFile {
		return nil, errors.New("too large")
	}
	return b, nil
}

// handleAgentUpgrade serves agent_upgrade and agent_upgrade_result
// (agent-protocol v1.20 items 5 and 7).
func handleAgentUpgrade(ctx context.Context, verb string, in []byte, sys Sys) []byte {
	if len(in) > programs.MaxFrame {
		return refuse(sys, programs.ReasonBadSpec)
	}
	if ctx.Err() != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if verb == agentupgrade.VerbResult {
		return doUpgradeResult(in, sys)
	}
	return doUpgradeStart(ctx, in, sys)
}

// doUpgradeResult returns result.json, or null when there is none or it is
// not a valid result. It reads only.
func doUpgradeResult(in []byte, sys Sys) []byte {
	if !agentupgrade.ParseEmpty(in) {
		return refuse(sys, programs.ReasonBadSpec)
	}
	reply := agentupgrade.ResultReply{OK: true, Running: UpgradeRunning(sys.Root)}
	if r, ok := ReadUpgradeResult(sys.Root); ok {
		reply.Result = &r
	}
	return agentupgrade.Encode(reply)
}

// ReadUpgradeResult reads result.json under root. A missing, linked,
// oversized or invalid file is false.
func ReadUpgradeResult(root string) (agentupgrade.Result, bool) {
	b, err := readSmallNoFollow(UpgradeResultPath(root))
	if err != nil {
		return agentupgrade.Result{}, false
	}
	return agentupgrade.DecodeResult(b)
}

// doUpgradeStart runs the checks in the contract's order, then starts the
// transient unit. Nothing from the request reaches the command line except
// the checked version.
func doUpgradeStart(ctx context.Context, in []byte, sys Sys) []byte {
	pol := sys.loadHostPolicy()
	if !pol.AgentUpgradeEnabled() {
		return refuse(sys, programs.ReasonPolicy)
	}
	req, ok := agentupgrade.ParseRequest(in)
	if !ok {
		return refuse(sys, programs.ReasonBadSpec)
	}
	installed, _ := InstalledVersion(sys.Root)
	if !agentupgrade.Newer(req.Version, installed) {
		return refuse(sys, agentupgrade.ReasonNotNewer)
	}
	if err := ensureRuntime(sys.Root); err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	unlock, held := tryLockHelper(sys.Root)
	if !held {
		return refuse(sys, agentupgrade.ReasonBusy)
	}
	defer unlock()
	if otherLocksHeld(sys.Root) {
		return refuse(sys, agentupgrade.ReasonBusy)
	}
	active, err := upgradeUnitActive(ctx, sys)
	if err != nil {
		return refuse(sys, programs.ReasonIO)
	}
	if active {
		return refuse(sys, agentupgrade.ReasonBusy)
	}
	if coolingDown(sys, req.Version) {
		return refuse(sys, agentupgrade.ReasonBusy)
	}
	if sys.RunUnit == nil {
		return refuse(sys, programs.ReasonIO)
	}
	code, _, err := sys.RunUnit(ctx, UpgradeUnitArgs(req.Version), nil)
	if err != nil || code != 0 {
		sys.logf("bot-agent-runner: agent_upgrade systemd-run failed")
		return refuse(sys, programs.ReasonIO)
	}
	sys.logf("bot-agent-runner: agent_upgrade started %s", req.Version)
	return agentupgrade.Encode(agentupgrade.StartReply{OK: true, State: agentupgrade.StateAccepted})
}

// UpgradeUnitArgs is the systemd-run argument vector (item 5). version has
// already passed agentupgrade.ValidVersion.
func UpgradeUnitArgs(version string) []string {
	return []string{
		"--unit=" + agentupgrade.Unit,
		"--collect",
		"--property=RuntimeMaxSec=" + strconv.Itoa(agentupgrade.RuntimeMax),
		// The installer gets SIGTERM first and 120 s to roll back; systemd waits
		// TimeoutStopSec before killing, and KillMode=mixed sends that SIGTERM to
		// upgrade-run only, which passes it on to install.sh.
		"--property=TimeoutStopSec=150", "--property=KillMode=mixed",
		agentupgrade.RunnerPath,
		agentupgrade.RunSubcommand,
		version,
	}
}

// upgradeUnitActive reports whether bot-agent-upgrade.service is
// running or changing state. inactive, failed and an unknown unit are not.
func upgradeUnitActive(ctx context.Context, sys Sys) (bool, error) {
	if sys.Systemctl == nil {
		return false, errors.New("systemctl")
	}
	out, err := sys.Systemctl(ctx, []string{"is-active", agentupgrade.Unit + ".service"})
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
		// An unknown word is treated as running: never start a second one.
		return true, nil
	}
}

// tryLockHelper takes the helper-wide lock without waiting.
func tryLockHelper(root string) (func(), bool) {
	path := joinRoot(root, filepath.Join("run", "bot-agent", "helper.lock"))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
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

// otherLocksHeld probes every other lock file in /run/bot-agent (image,
// edge and host-file locks) without waiting, so an upgrade does not start
// while an image, edge or host-file mutation is running. Each lock that is
// free is released at once. A file that cannot be opened is skipped.
func otherLocksHeld(root string) bool {
	dir := joinRoot(root, filepath.Join("run", "bot-agent"))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, ent := range ents {
		name := ent.Name()
		if name == "helper.lock" || !strings.HasSuffix(name, ".lock") || !ent.Type().IsRegular() {
			continue
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true
		}
	}
	return false
}

// InstalledVersion is the `version X` line of the installer record.
func InstalledVersion(root string) (string, bool) {
	b, err := readSmallNoFollow(InstallerRecordPath(root))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "version" {
			return f[1], true
		}
	}
	return "", false
}

// RunnerInstalled reports whether the installer record lists the helper
// binary with a hash, which is how install.sh decides the same.
func RunnerInstalled(root string) bool {
	b, err := readSmallNoFollow(InstallerRecordPath(root))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == agentupgrade.RunnerPath && isHex64(f[0]) {
			return true
		}
	}
	return false
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// coolingDown is the v1.20 item 5 cool-down: the last result is for this
// version, is not succeeded, and is under CoolDown old.
func coolingDown(sys Sys, version string) bool {
	r, ok := ReadUpgradeResult(sys.Root)
	if !ok || r.Version != version || r.Outcome == agentupgrade.OutcomeSucceeded {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, r.At)
	if err != nil {
		return false
	}
	return sys.clock().Sub(at) < agentupgrade.CoolDown
}
