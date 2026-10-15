// Command bot-agent-runner is the root helper. systemd starts it as
// serve-conn on an accepted socket. It refuses to run when the effective
// uid is not 0, and serve-conn also requires the peer uid to be bot-agent.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	if euid != 0 {
		return writeOut(out, programs.EncodeRefusal(programs.ReasonPolicy))
	}
	if len(args) == 1 && args[0] == "serve-conn" {
		return serveConn(in, out, sys, peer)
	}
	if len(args) != 1 || !programs.KnownVerb(args[0]) {
		return writeOut(out, programs.EncodeRefusal(programs.ReasonVerb))
	}
	body, err := io.ReadAll(io.LimitReader(in, programs.MaxFrame+1))
	if err != nil {
		return writeOut(out, programs.EncodeRefusal(programs.ReasonIO))
	}
	if len(body) > programs.MaxFrame {
		return writeOut(out, programs.EncodeRefusal(programs.ReasonBadSpec))
	}
	return writeOut(out, runner.Handle(context.Background(), args[0], body, sys))
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
		return writeOut(out, programs.EncodeRefusal(programs.ReasonPolicy))
	}
	line, tooBig, err := readFrame(in)
	if tooBig {
		return writeOut(out, programs.EncodeRefusal(programs.ReasonBadSpec))
	}
	if err != nil {
		return writeOut(out, programs.EncodeRefusal(programs.ReasonIO))
	}
	verb, req, reason := parseEnvelope(line)
	if reason != "" {
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
		if buf.Len() >= programs.MaxFrame {
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
