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
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/run"
)

func main() {
	if err := dispatch(os.Args[1:], os.Stdout); err != nil {
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		fmt.Fprintf(os.Stderr, "bot-agent: %s\n", err.Error())
		os.Exit(1)
	}
}

// exitCode is a stdout result that already explains itself. main does not
// print it again. policy check uses 1 for an invalid file.
type exitCode int

func (e exitCode) Error() string { return "" }

const usageText = "usage: bot-agent run | bot-agent version | bot-agent init --server <url> --join-token-stdin [--env-file <path>] | bot-agent policy check FILE\n(the join token is one trimmed line on stdin; --join-token is not accepted)"

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
	case "policy":
		return policyCmd(rest, stdout)
	default:
		return errors.New(usageText)
	}
}

func policyCmd(args []string, stdout io.Writer) error {
	if len(args) != 2 || args[0] != "check" || args[1] == "" {
		return errors.New(usageText)
	}
	return printPolicyCheck(stdout, args[1])
}

// printPolicyCheck writes the effective flags, or invalid: <reason>.
// The reason is a fixed token from the policy package. File bytes are not printed.
func printPolicyCheck(w io.Writer, path string) error {
	p := policy.Check(path, "", "")
	if !p.Exists {
		fmt.Fprintln(w, "invalid: missing")
		return exitCode(1)
	}
	if !p.Valid {
		fmt.Fprintf(w, "invalid: %s\n", safeReason(p.Reason))
		return exitCode(1)
	}
	fmt.Fprintln(w, "valid")
	fmt.Fprintf(w, "enabled=%t\n", p.Enabled)
	fmt.Fprintf(w, "programs=%t\n", p.Enabled)
	fmt.Fprintf(w, "images=%t\n", p.ImagesEnabled())
	fmt.Fprintf(w, "projects=%d\n", len(p.Projects))
	fmt.Fprintf(w, "max_containers=%d\n", p.MaxContainers)
	fmt.Fprintf(w, "allow_public_ports=%t\n", p.AllowPublicPorts)
	fmt.Fprintf(w, "image_patterns=%d\n", len(p.Images))
	return nil
}

func safeReason(s string) string {
	if s == "" {
		return "invalid"
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && c != '_' {
			return "invalid"
		}
	}
	return s
}
