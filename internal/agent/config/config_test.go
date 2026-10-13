package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRefusesModeAndSymlink(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	body := "BOT_AGENT_SERVER_URL=wss://example.invalid/agent/v1/ws\nBOT_AGENT_JOIN_TOKEN=join-token-test-value\nBOT_AGENT_STATE_DIR=/var/lib/bot-agent\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("mode: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink: %v", err)
	}
}

func TestServerURL(t *testing.T) {
	if err := ValidateServerURL("ws://127.0.0.1:1/agent/v1/ws", false); err == nil {
		t.Fatal("ws without the dev flag")
	}
	if err := ValidateServerURL("ws://127.0.0.1:1/agent/v1/ws", true); err != nil {
		t.Fatal(err)
	}
	if err := ValidateServerURL("ws://localhost:1/agent/v1/ws", true); err != nil {
		t.Fatal(err)
	}
	if err := ValidateServerURL("ws://example.invalid/ws", true); err == nil {
		t.Fatal("public ws")
	}
	if err := ValidateServerURL("wss://example.invalid/agent/v1/ws", false); err != nil {
		t.Fatal(err)
	}
	err := ValidateServerURL("wss://user:s3cret@example.invalid/ws", false)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("%v", err)
	}
}

func TestInitAndPersist(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "nested", "env")
	token := "join-token-test-value"
	args := []string{"--server", "wss://example.invalid/agent/v1/ws", "--join-token-stdin", "--env-file", path}
	err := Init([]string{"--server", "http://evil.example/ws", "--join-token", token, "--env-file", path})
	if err == nil || !strings.Contains(err.Error(), "--join-token-stdin") || strings.Contains(err.Error(), token) {
		t.Fatalf("init error leaked or missing: %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("failed init wrote a file")
	}
	if err := initFrom([]string{"--server=wss://example.invalid/agent/v1/ws", "--join-token-stdin", "--env-file", path}, strings.NewReader("  "+token+"\n")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JoinToken != token || cfg.HasCredential() {
		t.Fatalf("%+v", cfg)
	}
	host := "11111111-1111-4111-8111-111111111111"
	credID := "22222222-2222-4222-8222-222222222222"
	cred := strings.Repeat("c", 43)
	if err := cfg.PersistEnrolled(host, credID, cred); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "JOIN_TOKEN") || strings.Contains(text, token) {
		t.Fatal("join token still on disk")
	}
	if !strings.Contains(text, "BOT_AGENT_HOST_ID="+host) || !strings.Contains(text, "BOT_AGENT_CREDENTIAL="+cred) {
		t.Fatal("credential missing")
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.DiskHasJoin() || !again.HasCredential() || again.Credential != cred {
		t.Fatal("reload")
	}
	if err := initFrom(args, strings.NewReader(token+"\n")); err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), cred) {
		t.Fatalf("second init: %v", err)
	}
	fresh := filepath.Join(t.TempDir(), "env")
	badArgs := []string{"--server", "wss://example.invalid/agent/v1/ws", "--join-token-stdin", "--env-file", fresh}
	for _, bad := range []string{"", "short", "has space pad!!", strings.Repeat("a", 129), "slash/not-allowed"} {
		err := initFrom(badArgs, strings.NewReader(bad+"\n"))
		if err == nil || (bad != "" && strings.Contains(err.Error(), bad)) {
			t.Fatalf("bad token %q: %v", bad, err)
		}
		if _, statErr := os.Stat(fresh); statErr == nil {
			t.Fatalf("bad token %q wrote a file", bad)
		}
	}
	err = Init([]string{"--server", "wss://example.invalid/agent/v1/ws", "--join-token=" + token})
	if err == nil || !strings.Contains(err.Error(), "--join-token-stdin") || strings.Contains(err.Error(), token) {
		t.Fatalf("equals form: %v", err)
	}
}

func TestOverlayAndHostname(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "env")
	body := "BOT_AGENT_SERVER_URL=wss://example.invalid/agent/v1/ws\nBOT_AGENT_JOIN_TOKEN=join-token-test-value\nBOT_AGENT_HOSTNAME=Pi One\nBOT_AGENT_STATE_DIR=/var/lib/bot-agent\nBOT_AGENT_ALWAYS_ON=false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOT_AGENT_JOIN_TOKEN", "override-token-value")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JoinToken != "override-token-value" || !cfg.DiskHasJoin() || cfg.AlwaysOn {
		t.Fatalf("%+v", cfg)
	}
	host, err := SanitizeHostname(cfg.Hostname)
	if err != nil || host != "pi-one" {
		t.Fatalf("%s %v", host, err)
	}
	if _, err := SanitizeHostname("..."); err != nil {
		t.Fatal(err)
	}
	if _, err := SanitizeHostname(""); err == nil {
		t.Fatal("empty")
	}
	t.Setenv("BOT_AGENT_HOST_ID", "not-a-uuid")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || strings.Contains(err.Error(), "override-token-value") {
		t.Fatalf("%v", err)
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"BOT_AGENT_SERVER_URL", "BOT_AGENT_JOIN_TOKEN", "BOT_AGENT_HOST_ID",
		"BOT_AGENT_CREDENTIAL_ID", "BOT_AGENT_CREDENTIAL", "BOT_AGENT_HOSTNAME",
		"BOT_AGENT_ALWAYS_ON", "BOT_AGENT_INSECURE_DEV", "BOT_AGENT_STATE_DIR",
		"BOT_AGENT_ENV_FILE",
	} {
		t.Setenv(k, "")
	}
}
