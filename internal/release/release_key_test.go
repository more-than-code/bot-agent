//go:build release

package release

import "testing"

// TestReleaseKeyNotPlaceholder is the release gate: a release build must
// carry the real public key. Run it with `go test -tags release
// ./internal/release`; without the tag it is not compiled.
func TestReleaseKeyNotPlaceholder(t *testing.T) {
	if IsPlaceholder(PublicKey) {
		t.Fatal("internal/release PublicKey is the all-zero placeholder; set the release key before tagging")
	}
	if len(PublicKey) != 32 {
		t.Fatalf("PublicKey is %d bytes", len(PublicKey))
	}
}
