package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/runner"
)

// policyRun is `bot-agent-runner policy-run <base_hash> <new_hash>`
// (agent-protocol v1.21 item 4). Both hashes are checked again here
// because the subcommand can be run by hand.
func policyRun(baseHash, newHash string, sys runner.Sys) int {
	if !isHex64(baseHash) || !isHex64(newHash) {
		sys.Logf("bot-agent-runner: policy-run bad hash")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if _, err := runner.PolicyRun(ctx, baseHash, newHash, sys); err != nil {
		sys.Logf("bot-agent-runner: policy-run could not write the result")
		return 1
	}
	return 0
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
