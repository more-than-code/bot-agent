package redact

import (
	"strings"
	"testing"
)

func TestFileSecretBounds(t *testing.T) {
	six := "abcdef"
	fifteen := "abcdefghijklmno"
	five := "abcde"
	if len(six) != 6 || len(fifteen) != 15 || len(five) != 5 {
		t.Fatal("fixture")
	}
	for _, s := range []string{six, fifteen} {
		if !hasSecret(FileSecrets(s), s) {
			t.Fatalf("%q left out of %q", s, FileSecrets(s))
		}
		if strings.Contains(NewExact(FileSecrets(s)...).Text("x"+s+"y"), s) {
			t.Fatalf("%q not redacted", s)
		}
	}
	if hasSecret(FileSecrets(five), five) || strings.Contains(NewExact(FileSecrets(five)...).Text("x"+five+"y"), "[REDACTED]") {
		t.Fatalf("5-byte joined %q", FileSecrets(five))
	}

	padded := "  s3cret-value-1-pad"
	trimmed := strings.TrimSpace(padded)
	if len(trimmed) < 16 || trimmed == padded {
		t.Fatal("fixture")
	}
	vals := FileSecrets(padded + "\n")
	if !hasSecret(vals, trimmed) {
		t.Fatalf("trimmed %q missing from %q", trimmed, vals)
	}
	if strings.Contains(NewExact(vals...).Text("saw "+trimmed), trimmed) {
		t.Fatal("leading spaces were not trimmed before the match")
	}
}

func hasSecret(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}
