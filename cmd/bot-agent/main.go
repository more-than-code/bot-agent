// Command bot-agent is the phase-1 host daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/config"
	"github.com/more-than-code/bot-agent/internal/agent/hostfiles"
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
	p := checkPolicy(path)
	if !p.Exists {
		fmt.Fprintln(w, "invalid: missing")
		return exitCode(1)
	}
	if !p.Valid {
		// v1.21 item 1: a host_env_file failure names the entry, never the
		// path or a value. ReasonEntry is two validated names.
		if p.Reason == policy.ReasonHostEnvFile && p.ReasonEntry != "" {
			fmt.Fprintf(w, "invalid: %s %s\n", safeReason(p.Reason), p.ReasonEntry)
			return exitCode(1)
		}
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
	_, _, overlap := policy.OverlappingProjects(p.Projects)
	fmt.Fprintf(w, "projects_overlap=%t\n", overlap)
	// v1.13 item 6: the shared networks, in file order. Names, subnets and
	// slugs were validated by the loader, so they cannot carry a newline.
	fmt.Fprintf(w, "shared_networks=%d\n", len(p.SharedNetworks))
	for _, n := range p.SharedNetworks {
		members := "-"
		if len(n.Projects) > 0 {
			members = strings.Join(n.Projects, ",")
		}
		fmt.Fprintf(w, "shared_network %s %s %s\n", n.Name, n.Subnet, members)
	}
	// v1.15 item 1: the edge key, after the shared-network lines.
	fmt.Fprintf(w, "edge=%t\n", p.EdgeEnabled())
	// v1.18 item 2: the host files, in file order, after the edge line
	// (which already follows the shared-network lines). Every field was
	// validated by the loader; command lines are not printed.
	fmt.Fprintf(w, "host_files=%d\n", len(p.HostFiles))
	for _, f := range p.HostFiles {
		// exec=unsafe (rev 1a A2): an executable is missing or not safely
		// root-owned; applies of this file are refused until it is fixed.
		execNote := ""
		if !f.ExecSafe {
			execNote = " exec=unsafe"
		}
		fmt.Fprintf(w, "host_file %s %s %s %s %s secret=%t health=%t%s\n",
			f.Name, f.Path, hostfiles.FormatMode(f.Mode), f.Unit, f.RunAs.Name, f.Secret, len(f.Health) > 0, execNote)
	}
	// v1.20 item 1: the agent_upgrade key, after the host_file lines.
	fmt.Fprintf(w, "agent_upgrade=%t\n", p.AgentUpgradeEnabled())
	proxy := "unset"
	if p.AgentUpgradeProxy != "" {
		proxy = "set"
	}
	fmt.Fprintf(w, "agent_upgrade_https_proxy=%s\n", proxy)
	// v1.21 items 1 and 7: the host network projects, then one line per
	// host env file with its key count, the first 12 hex of its SHA-256 and
	// its image pattern (validated). Never a path's contents or a value.
	hostNet := "none"
	if len(p.HostNetworkProjects) > 0 {
		hostNet = strings.Join(p.HostNetworkProjects, ",")
	}
	fmt.Fprintf(w, "host_network_projects=%s\n", hostNet)
	for _, f := range p.HostEnvFiles {
		fmt.Fprintf(w, "host_env_file %s keys=%d sha256=%s image=%s\n", f.Label(), f.Keys, shortSum(f.SHA256), f.Image)
	}
	// v1.22 item 1: the console_policy key.
	fmt.Fprintf(w, "console_policy=%t\n", p.ConsoleManaged())
	// v1.24: the container cgroup namespace mode, last.
	fmt.Fprintf(w, "container_cgroupns=%s\n", p.CgroupnsMode())
	return nil
}

// checkPolicy is policy.Check against the host's passwd and group. Tests
// replace it to supply their own account files.
var checkPolicy = func(path string) policy.Policy {
	return policy.Check(path, "", "")
}

// shortSum is the first 12 hex of a SHA-256, or "-" when the file was not read.
func shortSum(sum string) string {
	if len(sum) < 12 {
		return "-"
	}
	return sum[:12]
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
