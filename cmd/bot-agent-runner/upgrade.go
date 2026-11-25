package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/agentupgrade"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/runner"
	"github.com/more-than-code/bot-agent/internal/upgrade"
)

// upgradeRun is `bot-agent-runner upgrade-run <version>` (agent-protocol
// v1.20 item 6). The version was checked by the helper; it is checked
// again here because the subcommand can be run by hand.
func upgradeRun(version string, sys runner.Sys) int {
	if !agentupgrade.ValidVersion(version) {
		sys.Logf("bot-agent-runner: upgrade-run bad version")
		return 2
	}
	// The only proxy is the root-owned runner.toml key; nothing is taken
	// from the agent's env file or the process environment.
	pol := policy.Load(policy.Files{Policy: sys.Policy, Passwd: sys.Passwd, Group: sys.Group, UID: sys.UID, SkipEnvFiles: true})
	proxy, err := upgrade.ProxyFunc(pol.AgentUpgradeProxy)
	if err != nil {
		sys.Logf("bot-agent-runner: upgrade-run bad agent_upgrade_https_proxy")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	env := upgrade.Env{
		StateDir:        runner.UpgradeStateDir(sys.Root),
		LockPath:        runner.UpgradeLockPath(sys.Root),
		RunnerInstalled: func() bool { return runner.RunnerInstalled(sys.Root) },
		Fetch:           upgrade.NewHTTPFetcher(proxy),
		Version:         upgrade.RunVersion,
		Install:         upgrade.RunInstall,
		Logf:            sys.Logf,
	}
	if _, err := upgrade.Run(ctx, version, env); err != nil {
		sys.Logf("bot-agent-runner: upgrade-run could not write the result")
		return 1
	}
	return 0
}
