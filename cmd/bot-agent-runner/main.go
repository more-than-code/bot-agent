// Command bot-agent-runner is the root helper. systemd starts it as
// serve-conn on an accepted socket. It refuses to run when the effective
// uid is not 0, and serve-conn also requires the peer uid to be bot-agent.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/runner"
)

func main() {
	os.Exit(run(os.Geteuid(), os.Args[1:], os.Stdin, os.Stdout, runner.RealSys(), func() (int, bool) {
		return peercred(0)
	}))
}

func run(euid int, args []string, in io.Reader, out io.Writer, sys runner.Sys, peer func() (int, bool)) int {
	sys.Logf = stderrLogf
	if euid != 0 {
		verb := ""
		if len(args) == 1 {
			verb = args[0]
		}
		noteRefusal(sys, verb, programs.ReasonPolicy)
		return writeOut(out, programs.EncodeRefusal(programs.ReasonPolicy))
	}
	if len(args) == 1 && args[0] == "serve-conn" {
		return serveConn(in, out, sys, peer)
	}
	if len(args) != 1 || !programs.KnownVerb(args[0]) {
		noteRefusal(sys, "", programs.ReasonVerb)
		return writeOut(out, programs.EncodeRefusal(programs.ReasonVerb))
	}
	limit := programs.FrameLimit(args[0])
	body, err := io.ReadAll(io.LimitReader(in, int64(limit)+1))
	if err != nil {
		noteRefusal(sys, args[0], programs.ReasonIO)
		return writeOut(out, programs.EncodeRefusal(programs.ReasonIO))
	}
	if len(body) > limit {
		noteRefusal(sys, args[0], programs.ReasonBadSpec)
		return writeOut(out, programs.EncodeRefusal(programs.ReasonBadSpec))
	}
	noteOverlap(sys)
	return writeOut(out, runner.Handle(context.Background(), args[0], body, sys))
}

// stderrLogf is the production refusal logger. stdout is the socket, so
// the line has to go to stderr, which the unit sends to the journal.
func stderrLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// noteRefusal logs a refusal that returns before Handle. The verb is
// printed only when it is a helper verb; an arbitrary argv is not.
func noteRefusal(sys runner.Sys, verb, reason string) {
	if sys.Logf == nil {
		return
	}
	if !programs.KnownVerb(verb) {
		verb = "-"
	}
	sys.Logf("bot-agent-runner: refuse %s %s", verb, reason)
}

// noteOverlap logs v1.5.2 rule 6 once per connection. The line is the two
// slugs from the policy file and nothing from the request.
func noteOverlap(sys runner.Sys) {
	if sys.Logf == nil {
		return
	}
	pol := policy.Load(policy.Files{
		Policy: sys.Policy, Passwd: sys.Passwd, Group: sys.Group, UID: sys.UID,
	})
	a, b, ok := policy.OverlappingProjects(pol.Projects)
	if !ok {
		return
	}
	sys.Logf("bot-agent-runner: policy: overlapping projects %s %s", a, b)
}

// serveConn reads one newline-terminated request from the accepted socket
// and writes one JSON response. A second request on the same connection is
// not read. The peer uid must be the bot-agent account.
func serveConn(in io.Reader, out io.Writer, sys runner.Sys, peer func() (int, bool)) int {
	uid, ok := 0, false
	if peer != nil {
		uid, ok = peer()
	}
	want, found := lookupUID(sys.Passwd, "bot-agent")
	if !ok || !found || uid != want || uid == 0 {
		noteRefusal(sys, "", programs.ReasonPolicy)
		return writeOut(out, programs.EncodeRefusal(programs.ReasonPolicy))
	}
	line, tooBig, err := readFrame(in)
	if tooBig {
		noteRefusal(sys, "", programs.ReasonBadSpec)
		return writeOut(out, programs.EncodeRefusal(programs.ReasonBadSpec))
	}
	if err != nil {
		noteRefusal(sys, "", programs.ReasonIO)
		return writeOut(out, programs.EncodeRefusal(programs.ReasonIO))
	}
	verb, req, reason := parseEnvelope(line)
	if reason != "" {
		noteRefusal(sys, "", reason)
		return writeOut(out, programs.EncodeRefusal(reason))
	}
	// A socket close cancels systemctl. A bytes.Reader used by tests is
	// already at EOF, so only a real file (the accepted socket) is watched.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if f, ok := in.(*os.File); ok {
		// File() from a net.Conn is non-blocking. A blocking read is what
		// tells us the client has gone away.
		_ = syscall.SetNonblock(int(f.Fd()), false)
		go func() {
			var b [1]byte
			_, err := f.Read(b[:])
			if err == nil || err == io.EOF {
				cancel()
			}
		}()
	}
	noteOverlap(sys)
	return writeOut(out, runner.Handle(ctx, verb, req, sys))
}

func readFrame(in io.Reader) ([]byte, bool, error) {
	r := bufio.NewReaderSize(in, 1)
	var buf bytes.Buffer
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, false, err
		}
		if b == '\n' {
			return buf.Bytes(), false, nil
		}
		if buf.Len() >= programs.SocketLimit() {
			return nil, true, nil
		}
		buf.WriteByte(b)
	}
}

func parseEnvelope(line []byte) (string, []byte, string) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var env struct {
		Verb    string          `json:"verb"`
		Request json.RawMessage `json:"request"`
	}
	if err := dec.Decode(&env); err != nil || dec.More() {
		return "", nil, programs.ReasonBadSpec
	}
	if !programs.KnownVerb(env.Verb) {
		return "", nil, programs.ReasonVerb
	}
	if len(env.Request) == 0 || env.Request[0] != '{' {
		return "", nil, programs.ReasonBadSpec
	}
	return env.Verb, env.Request, ""
}

func lookupUID(path, name string) (int, bool) {
	if path == "" {
		path = policy.DefaultPasswd
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 3 || f[0] != name {
			continue
		}
		n, err := strconv.Atoi(f[2])
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func writeOut(out io.Writer, raw []byte) int {
	buf := append(append([]byte(nil), raw...), '\n')
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		buf = raw
	}
	if _, err := out.Write(buf); err != nil {
		return 1
	}
	return 0
}
