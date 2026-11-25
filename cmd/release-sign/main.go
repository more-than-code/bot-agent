// Command release-sign makes and checks the SHA256SUMS.sig of a bot-agent
// release (agent-protocol v1.20, spec docs/spec-agent-self-upgrade.md
// §4.1). It is a release tool run on the signer's machine with
// `go run ./cmd/release-sign`; it is never built into a bundle. Standard
// library only.
//
//	release-sign keygen -out DIR
//	release-sign sign -key PRIVATE_KEY SHA256SUMS > SHA256SUMS.sig
//	release-sign verify -pub PUBLIC_KEY SHA256SUMS SHA256SUMS.sig
//
// Keys are one base64 line. keygen writes DIR/release-key (the private
// key, mode 0600) and DIR/release-key.pub, and never overwrites a file.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/more-than-code/bot-agent/internal/release"
)

const usage = `usage:
  release-sign keygen -out DIR
  release-sign sign -key PRIVATE_KEY SHA256SUMS > SHA256SUMS.sig
  release-sign verify -pub PUBLIC_KEY SHA256SUMS SHA256SUMS.sig`

// maxSums bounds the SHA256SUMS file this tool reads.
const maxSums = 1 << 20

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	switch {
	case len(args) == 3 && args[0] == "keygen" && args[1] == "-out":
		return keygen(args[2], stdout)
	case len(args) == 4 && args[0] == "sign" && args[1] == "-key":
		return sign(args[2], args[3], stdout)
	case len(args) == 5 && args[0] == "verify" && args[1] == "-pub":
		return verify(args[2], args[3], args[4], stdout)
	default:
		return errors.New(usage)
	}
}

func keygen(dir string, stdout io.Writer) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privPath := filepath.Join(dir, "release-key")
	pubPath := filepath.Join(dir, "release-key.pub")
	if err := writeNew(privPath, base64.StdEncoding.EncodeToString(priv)+"\n", 0o600); err != nil {
		return err
	}
	if err := writeNew(pubPath, base64.StdEncoding.EncodeToString(pub)+"\n", 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s (keep offline, mode 0600) and %s\n", privPath, pubPath)
	return nil
}

// writeNew creates path with mode and fails when it already exists.
func writeNew(path, body string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func sign(keyPath, sumsPath string, stdout io.Writer) error {
	fi, err := os.Stat(keyPath)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s: private key is readable by group or other (mode %04o); chmod 600 it", keyPath, fi.Mode().Perm())
	}
	keyFile, err := readSmall(keyPath, 4096)
	if err != nil {
		return err
	}
	key, err := release.DecodeKey(keyFile, ed25519.PrivateKeySize)
	if err != nil {
		return fmt.Errorf("%s: %w", keyPath, err)
	}
	sums, err := readSmall(sumsPath, maxSums)
	if err != nil {
		return err
	}
	priv := ed25519.PrivateKey(key)
	sig := ed25519.Sign(priv, sums)
	// Check the result with the key's own public half before printing it.
	if err := release.Verify(priv.Public().(ed25519.PublicKey), sums, release.EncodeSig(sig)); err != nil {
		return err
	}
	_, err = stdout.Write(release.EncodeSig(sig))
	return err
}

func verify(pubPath, sumsPath, sigPath string, stdout io.Writer) error {
	pubFile, err := readSmall(pubPath, 4096)
	if err != nil {
		return err
	}
	pub, err := release.DecodeKey(pubFile, ed25519.PublicKeySize)
	if err != nil {
		return fmt.Errorf("%s: %w", pubPath, err)
	}
	sums, err := readSmall(sumsPath, maxSums)
	if err != nil {
		return err
	}
	sig, err := readSmall(sigPath, release.MaxSigFile)
	if err != nil {
		return err
	}
	if err := release.Verify(ed25519.PublicKey(pub), sums, sig); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "ok")
	return nil
}

func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: file too large", path)
	}
	return b, nil
}
