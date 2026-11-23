// Package release checks the signature on a bot-agent release
// (agent-protocol v1.20, spec docs/spec-agent-self-upgrade.md §4.1).
// SHA256SUMS.sig is one line: the base64 of a plain Ed25519 signature
// (RFC 8032) over the exact bytes of SHA256SUMS.
package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
)

// MaxSigFile bounds SHA256SUMS.sig: 88 base64 characters, a newline and
// some slack for a CRLF.
const MaxSigFile = 128

// PublicKey is the release key compiled into the helper.
//
// PLACEHOLDER: all zero bytes. Replace it with the base64-decoded public
// key from `release-sign keygen` (Joe's offline key) before the first
// signed release. A build with the placeholder refuses every bundle:
// Verify rejects it before checking the signature.
var PublicKey = ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))

var (
	ErrPlaceholder = errors.New("release key is the placeholder")
	ErrBadKey      = errors.New("bad public key")
	ErrBadSig      = errors.New("bad signature file")
	ErrMismatch    = errors.New("signature does not verify")
)

// IsPlaceholder reports whether pub is the all-zero placeholder key.
func IsPlaceholder(pub ed25519.PublicKey) bool {
	return len(pub) == ed25519.PublicKeySize && bytes.Equal(pub, make([]byte, ed25519.PublicKeySize))
}

// EncodeSig is the SHA256SUMS.sig content for a raw signature.
func EncodeSig(sig []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// DecodeSig reads a SHA256SUMS.sig file: one base64 line of 64 bytes,
// with an optional trailing newline.
func DecodeSig(file []byte) ([]byte, error) {
	if len(file) == 0 || len(file) > MaxSigFile {
		return nil, ErrBadSig
	}
	line := bytes.TrimSuffix(file, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if bytes.ContainsAny(line, "\r\n \t") {
		return nil, ErrBadSig
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(string(line))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrBadSig
	}
	return sig, nil
}

// Verify checks sigFile (the SHA256SUMS.sig bytes) over sums (the exact
// SHA256SUMS bytes) with pub. The placeholder key is refused.
func Verify(pub ed25519.PublicKey, sums, sigFile []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return ErrBadKey
	}
	if IsPlaceholder(pub) {
		return ErrPlaceholder
	}
	sig, err := DecodeSig(sigFile)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, sums, sig) {
		return ErrMismatch
	}
	return nil
}

// DecodeKey reads a base64 key file (one line) of the given size.
func DecodeKey(file []byte, size int) ([]byte, error) {
	line := bytes.TrimSpace(file)
	key, err := base64.StdEncoding.Strict().DecodeString(string(line))
	if err != nil || len(key) != size {
		return nil, ErrBadKey
	}
	return key, nil
}
