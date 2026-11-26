package main

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/release"
)

func TestKeygenSignVerify(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := run([]string{"keygen", "-out", dir}, &out); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "release-key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %o", fi.Mode().Perm())
	}
	if err := run([]string{"keygen", "-out", dir}, &out); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte(strings.Repeat("0", 64)+"  install.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sig bytes.Buffer
	if err := run([]string{"sign", "-key", filepath.Join(dir, "release-key"), sums}, &sig); err != nil {
		t.Fatal(err)
	}
	sigPath := filepath.Join(dir, "SHA256SUMS.sig")
	if err := os.WriteFile(sigPath, sig.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"verify", "-pub", filepath.Join(dir, "release-key.pub"), sums, sigPath}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ok\n" {
		t.Fatalf("verify printed %q", out.String())
	}
	if err := os.WriteFile(sums, []byte(strings.Repeat("1", 64)+"  install.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-pub", filepath.Join(dir, "release-key.pub"), sums, sigPath}, &out); err == nil {
		t.Fatal("verify accepted changed SHA256SUMS")
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"sign"}, {"keygen", "-out"}, {"verify", "-pub", "a", "b"}, {"verify-compiled", "a"}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestSignRefusesOpenKey(t *testing.T) {
	dir := t.TempDir()
	if err := run([]string{"keygen", "-out", dir}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "release-key")
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		if err := os.Chmod(key, mode); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := run([]string{"sign", "-key", key, sums}, &out); err == nil || out.Len() != 0 {
			t.Fatalf("mode %o: signed", mode)
		}
	}
	if err := os.Chmod(key, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"sign", "-key", key, sums}, &bytes.Buffer{}); err != nil {
		t.Fatalf("0400: %v", err)
	}
}

// verify-compiled accepts only a signature by a key in release.PublicKeys.
func TestVerifyCompiled(t *testing.T) {
	dir := t.TempDir()
	if err := run([]string{"keygen", "-out", dir}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte(strings.Repeat("0", 64)+"  install.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sig bytes.Buffer
	if err := run([]string{"sign", "-key", filepath.Join(dir, "release-key"), sums}, &sig); err != nil {
		t.Fatal(err)
	}
	sigPath := filepath.Join(dir, "SHA256SUMS.sig")
	if err := os.WriteFile(sigPath, sig.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify-compiled", sums, sigPath}, &bytes.Buffer{}); err == nil {
		t.Fatal("verify-compiled accepted a key that is not compiled in")
	}
	pubFile, err := os.ReadFile(filepath.Join(dir, "release-key.pub"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := release.DecodeKey(pubFile, ed25519.PublicKeySize)
	if err != nil {
		t.Fatal(err)
	}
	saved := release.PublicKeys
	t.Cleanup(func() { release.PublicKeys = saved })
	release.PublicKeys = append(append([]ed25519.PublicKey{}, saved...), pub)
	var out bytes.Buffer
	if err := run([]string{"verify-compiled", sums, sigPath}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ok\n" {
		t.Fatalf("verify-compiled printed %q", out.String())
	}
}
