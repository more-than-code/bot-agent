//go:build release

package release

import "testing"

// TestReleaseKeyNotPlaceholder is the release gate: a release build must
// carry the real public key. Run it with `go test -tags release
// ./internal/release`; without the tag it is not compiled.
func TestReleaseKeyNotPlaceholder(t *testing.T) {
	if len(PublicKeys) == 0 {
		t.Fatal("internal/release PublicKeys is empty")
	}
	seen := map[string]bool{}
	for i, pub := range PublicKeys {
		if len(pub) != 32 {
			t.Fatalf("PublicKeys[%d] is %d bytes", i, len(pub))
		}
		if IsPlaceholder(pub) {
			t.Fatalf("internal/release PublicKeys[%d] is the all-zero placeholder; set the release key before tagging", i)
		}
		if seen[string(pub)] {
			t.Fatalf("PublicKeys[%d] is listed twice", i)
		}
		seen[string(pub)] = true
	}
}
