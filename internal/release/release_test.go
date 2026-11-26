package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
)

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestVerifyGood(t *testing.T) {
	pub, priv := keypair(t)
	sums := []byte("abc  bot-agent-v0.2.16-linux-arm64\n")
	if err := Verify(pub, sums, EncodeSig(ed25519.Sign(priv, sums))); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBadSignature(t *testing.T) {
	pub, priv := keypair(t)
	sums := []byte("abc  install.sh\n")
	sig := ed25519.Sign(priv, sums)
	if err := Verify(pub, append(sums, ' '), EncodeSig(sig)); !errors.Is(err, ErrMismatch) {
		t.Fatalf("changed sums: %v", err)
	}
	sig[0] ^= 1
	if err := Verify(pub, sums, EncodeSig(sig)); !errors.Is(err, ErrMismatch) {
		t.Fatalf("changed sig: %v", err)
	}
	for _, bad := range []string{"", "not base64!\n", "QUJD\n", " " + string(EncodeSig(sig))} {
		if err := Verify(pub, sums, []byte(bad)); !errors.Is(err, ErrBadSig) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

func TestVerifyWrongKey(t *testing.T) {
	_, priv := keypair(t)
	other, _ := keypair(t)
	sums := []byte("abc  install.sh\n")
	if err := Verify(other, sums, EncodeSig(ed25519.Sign(priv, sums))); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestPlaceholderRefusesEverything(t *testing.T) {
	_, priv := keypair(t)
	sums := []byte("abc  install.sh\n")
	zero := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	if !IsPlaceholder(zero) {
		t.Fatal("zero key not seen as the placeholder")
	}
	if err := Verify(zero, sums, EncodeSig(ed25519.Sign(priv, sums))); !errors.Is(err, ErrPlaceholder) {
		t.Fatalf("zero key: %v", err)
	}
	if err := Verify(ed25519.PublicKey{1, 2}, sums, nil); !errors.Is(err, ErrBadKey) {
		t.Fatalf("short key: %v", err)
	}
}

func TestVerifyAny(t *testing.T) {
	pubA, privA := keypair(t)
	pubB, privB := keypair(t)
	_, privC := keypair(t)
	sums := []byte("abc  install.sh\n")
	keys := []ed25519.PublicKey{pubA, pubB}
	for name, priv := range map[string]ed25519.PrivateKey{"first": privA, "second": privB} {
		if err := VerifyAny(keys, sums, EncodeSig(ed25519.Sign(priv, sums))); err != nil {
			t.Fatalf("%s key: %v", name, err)
		}
	}
	if err := VerifyAny(keys, sums, EncodeSig(ed25519.Sign(privC, sums))); !errors.Is(err, ErrMismatch) {
		t.Fatalf("unlisted key: %v", err)
	}
	if err := VerifyAny(keys, sums, []byte("QUJD\n")); !errors.Is(err, ErrBadSig) {
		t.Fatalf("bad sig file: %v", err)
	}
	good := EncodeSig(ed25519.Sign(privA, sums))
	if err := VerifyAny(nil, sums, good); !errors.Is(err, ErrBadKey) {
		t.Fatalf("empty list: %v", err)
	}
	zero := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	if err := VerifyAny([]ed25519.PublicKey{pubA, zero}, sums, good); !errors.Is(err, ErrPlaceholder) {
		t.Fatalf("placeholder in list: %v", err)
	}
	if err := VerifyAny([]ed25519.PublicKey{pubA, {1, 2}}, sums, good); !errors.Is(err, ErrBadKey) {
		t.Fatalf("short key in list: %v", err)
	}
}
