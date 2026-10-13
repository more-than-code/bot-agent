package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := dispatch([]string{"version"}, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "0.1.0\n" {
		t.Fatalf("%q", buf.String())
	}
	buf.Reset()
	if err := dispatch([]string{"--version"}, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "0.1.0\n" {
		t.Fatalf("%q", buf.String())
	}
	buf.Reset()
	if err := dispatch([]string{"-version"}, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "0.1.0\n" {
		t.Fatalf("%q", buf.String())
	}
	if err := dispatch([]string{"version", "extra"}, &buf); err == nil {
		t.Fatal("extra args")
	}
	if strings.Contains(buf.String(), "extra") {
		t.Fatal("printed extra")
	}
}

func TestInitDoesNotPrintConfig(t *testing.T) {
	for _, k := range []string{
		"BOT_AGENT_SERVER_URL", "BOT_AGENT_JOIN_TOKEN", "BOT_AGENT_HOST_ID",
		"BOT_AGENT_CREDENTIAL_ID", "BOT_AGENT_CREDENTIAL", "BOT_AGENT_HOSTNAME",
		"BOT_AGENT_ALWAYS_ON", "BOT_AGENT_INSECURE_DEV", "BOT_AGENT_STATE_DIR",
		"BOT_AGENT_ENV_FILE",
	} {
		t.Setenv(k, "")
	}
	token := "super-secret-token-value"
	var buf bytes.Buffer
	err := dispatch([]string{"init", "--server", "http://evil.example/ws", "--join-token", token}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--join-token-stdin") || strings.Contains(err.Error(), token) || strings.Contains(buf.String(), token) {
		t.Fatalf("err=%v out=%q", err, buf.String())
	}
	path := filepath.Join(t.TempDir(), "env")
	buf.Reset()
	restore := pipeStdin(t, token+"\n")
	defer restore()
	if err := dispatch([]string{"init", "--server", "wss://example.invalid/agent/v1/ws", "--join-token-stdin", "--env-file", path}, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 || strings.Contains(buf.String(), token) {
		t.Fatalf("stdout %q", buf.String())
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	cred := "BOT_AGENT_HOST_ID=11111111-1111-4111-8111-111111111111\nBOT_AGENT_CREDENTIAL_ID=22222222-2222-4222-8222-222222222222\nBOT_AGENT_CREDENTIAL=" + strings.Repeat("c", 43) + "\n"
	if err := os.WriteFile(path, []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	restore = pipeStdin(t, token+"\n")
	defer restore()
	err = dispatch([]string{"init", "--server", "wss://example.invalid/agent/v1/ws", "--join-token-stdin", "--env-file", path}, &buf)
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), strings.Repeat("c", 43)) {
		t.Fatalf("%v", err)
	}
	err = dispatch([]string{"nope"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "--join-token-stdin") {
		t.Fatalf("%v", err)
	}
}

func pipeStdin(t *testing.T, line string) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, line); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	return func() {
		os.Stdin = old
		r.Close()
	}
}

func TestJoinTokenStdinNotInProcessArgs(t *testing.T) {
	token := "join-token-from-pipe"
	if os.Getenv("BOT_AGENT_EXEC_INIT") == "1" {
		for _, a := range os.Args {
			if strings.Contains(a, token) {
				os.Stderr.WriteString("token in argv\n")
				os.Exit(2)
			}
		}
		for _, k := range []string{
			"BOT_AGENT_SERVER_URL", "BOT_AGENT_JOIN_TOKEN", "BOT_AGENT_HOST_ID",
			"BOT_AGENT_CREDENTIAL_ID", "BOT_AGENT_CREDENTIAL", "BOT_AGENT_HOSTNAME",
			"BOT_AGENT_ALWAYS_ON", "BOT_AGENT_INSECURE_DEV", "BOT_AGENT_STATE_DIR",
			"BOT_AGENT_ENV_FILE",
		} {
			os.Unsetenv(k)
		}
		path := os.Getenv("BOT_AGENT_INIT_ENV")
		err := dispatch([]string{
			"init", "--server", "wss://example.invalid/agent/v1/ws",
			"--join-token-stdin", "--env-file", path,
		}, io.Discard)
		if err != nil {
			os.Stderr.WriteString("init failed\n")
			os.Exit(1)
		}
		body, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(body), "BOT_AGENT_JOIN_TOKEN="+token) {
			os.Stderr.WriteString("token missing from env file\n")
			os.Exit(1)
		}
		for _, a := range os.Args {
			if strings.Contains(a, token) {
				os.Stderr.WriteString("token in argv after read\n")
				os.Exit(2)
			}
		}
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "env")
	cmd := exec.Command(os.Args[0], "-test.run=^TestJoinTokenStdinNotInProcessArgs$")
	cmd.Env = append(os.Environ(), "BOT_AGENT_EXEC_INIT=1", "BOT_AGENT_INIT_ENV="+path)
	cmd.Stdin = strings.NewReader(token + "\n")
	for _, a := range cmd.Args {
		if strings.Contains(a, token) {
			t.Fatal("token is in the child argv")
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v %s", err, out)
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), "BOT_AGENT_JOIN_TOKEN="+token) {
		t.Fatalf("env file %q err %v", body, err)
	}
}
