// Command bot-agent is the phase-1 host daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/config"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/run"
)

func main() {
	if err := dispatch(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "bot-agent: %s\n", err.Error())
		os.Exit(1)
	}
}

const usageText = "usage: bot-agent run | bot-agent version | bot-agent init --server <url> --join-token-stdin [--env-file <path>]\n(the join token is one trimmed line on stdin; --join-token is not accepted)"

func dispatch(args []string, stdout io.Writer) error {
	cmd := "run"
	rest := args
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}
	switch cmd {
	case "run":
		if len(rest) != 0 {
			return errors.New("unexpected arguments")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return run.Run(ctx, run.Options{})
	case "version", "--version", "-version":
		if len(rest) != 0 {
			return errors.New("unexpected arguments")
		}
		fmt.Fprintln(stdout, protocol.AgentVersion)
		return nil
	case "init":
		return config.Init(rest)
	default:
		return errors.New(usageText)
	}
}
