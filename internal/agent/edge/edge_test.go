package edge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

const goodConfig = `{"admin":{"listen":"unix//run/edge/admin.sock"},"apps":{"http":{"servers":{"s":{"listen":[":443"],"routes":[]}}}}}`

func applyBody(cfg, hash string) []byte {
	return []byte(`{"config":` + cfg + `,"config_hash":"` + hash + `"}`)
}

func TestParseApply(t *testing.T) {
	big := `{"admin":{"listen":"unix//run/edge/admin.sock"},"x":"` + strings.Repeat("a", MaxConfig) + `"}`
	for _, tc := range []struct {
		name string
		body []byte
		ok   bool
	}{
		{"good", applyBody(goodConfig, HashConfig([]byte(goodConfig))), true},
		{"persist", applyBody(`{"admin":{"listen":"unix//run/edge/admin.sock","config":{"persist":false}},"apps":{}}`,
			HashConfig([]byte(`{"admin":{"listen":"unix//run/edge/admin.sock","config":{"persist":false}},"apps":{}}`))), false},
		{"no apps", applyBody(`{"admin":{"listen":"unix//run/edge/admin.sock"}}`,
			HashConfig([]byte(`{"admin":{"listen":"unix//run/edge/admin.sock"}}`))), false},
		{"wrong hash", applyBody(goodConfig, strings.Repeat("a", 64)), false},
		{"upper hash", applyBody(goodConfig, strings.ToUpper(HashConfig([]byte(goodConfig)))), false},
		{"no admin", applyBody(`{"apps":{}}`, HashConfig([]byte(`{"apps":{}}`))), false},
		{"tcp admin", applyBody(`{"admin":{"listen":"localhost:2019"}}`, HashConfig([]byte(`{"admin":{"listen":"localhost:2019"}}`))), false},
		{"admin disabled", applyBody(`{"admin":{"listen":"unix//run/edge/admin.sock","disabled":true}}`,
			HashConfig([]byte(`{"admin":{"listen":"unix//run/edge/admin.sock","disabled":true}}`))), false},
		{"remote admin", applyBody(`{"admin":{"listen":"unix//run/edge/admin.sock","remote":{"listen":":2021"}}}`,
			HashConfig([]byte(`{"admin":{"listen":"unix//run/edge/admin.sock","remote":{"listen":":2021"}}}`))), false},
		{"config loader", applyBody(`{"admin":{"listen":"unix//run/edge/admin.sock","config":{"load":{}}}}`,
			HashConfig([]byte(`{"admin":{"listen":"unix//run/edge/admin.sock","config":{"load":{}}}}`))), false},
		{"not compact", applyBody(`{"admin": {"listen":"unix//run/edge/admin.sock"}}`,
			HashConfig([]byte(`{"admin": {"listen":"unix//run/edge/admin.sock"}}`))), false},
		{"array config", applyBody(`[]`, HashConfig([]byte(`[]`))), false},
		{"too big", applyBody(big, HashConfig([]byte(big))), false},
		{"extra key", []byte(`{"config":` + goodConfig + `,"config_hash":"` + HashConfig([]byte(goodConfig)) + `","x":1}`), false},
		{"missing hash", []byte(`{"config":` + goodConfig + `}`), false},
		{"trailing", append(applyBody(goodConfig, HashConfig([]byte(goodConfig))), []byte(`{}`)...), false},
		{"null", []byte(`null`), false},
	} {
		_, err := ParseApply(tc.body)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err=%v", tc.name, err)
		}
	}
}

func TestParseEmpty(t *testing.T) {
	if ParseEmpty([]byte(`{}`)) != nil {
		t.Fatal("{} refused")
	}
	for _, in := range []string{``, `null`, `[]`, `{"a":1}`, `{}{}`} {
		if ParseEmpty([]byte(in)) == nil {
			t.Fatalf("%q accepted", in)
		}
	}
}

func TestKindsAndVerbs(t *testing.T) {
	for kind, verb := range map[string]string{KindApply: VerbApply, KindStop: VerbStop, KindStatus: VerbStatus} {
		if !IsKind(kind) || !IsVerb(verb) {
			t.Fatal(kind)
		}
		if v, ok := VerbFor(kind); !ok || v != verb {
			t.Fatal(kind)
		}
	}
	if IsKind("edge_restart") || IsVerb("edge.restart") || IsVerb(KindApply) {
		t.Fatal("unknown accepted")
	}
}

// The pinned image must be a digest reference the helper's pull accepts.
func TestImagePinned(t *testing.T) {
	ref, err := images.Parse(Image)
	if err != nil || ref.Digest == "" || ref.Name != "docker.io/library/caddy" {
		t.Fatalf("%+v %v", ref, err)
	}
}

func TestDetail(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "",
		"\n\n  loading: bad\nsecond": "loading: bad",
		"tab\there":                  "tab?here",
		"bad \xff utf8":              "bad ? utf8",
	} {
		if got := Detail(in); got != want {
			t.Fatalf("%q: %q", in, got)
		}
	}
	long := strings.Repeat("é", MaxDetail)
	if got := Detail(long); len(got) > MaxDetail || !strings.HasPrefix(long, got) {
		t.Fatalf("%d", len(got))
	}
}

func TestFixedReason(t *testing.T) {
	for _, c := range []string{ReasonDisabled, ReasonPortInUse, ReasonInvalidConfig, ReasonConflict, ReasonDocker, ReasonImage, ReasonBadSpec, ReasonIO} {
		if FixedReason(c) != c {
			t.Fatal(c)
		}
	}
	if FixedReason("/etc/passwd") != ReasonHelper || FixedReason("policy_denied") != ReasonHelper {
		t.Fatal("free text kept")
	}
}

func TestResultShapes(t *testing.T) {
	b, _ := json.Marshal(StatusResult{State: StateAbsent})
	if string(b) != `{"state":"absent","image":"","config_hash":"","started_at":null}` {
		t.Fatal(string(b))
	}
	b, _ = json.Marshal(Refusal{Error: ReasonDisabled})
	if string(b) != `{"error":"edge_disabled"}` {
		t.Fatal(string(b))
	}
}

// Release gate (security review M4): fails while the pinned Caddy digest is
// the all-zero placeholder. Main Dev replaces PinnedImage before release.
func TestReleaseGateEdgeImageDigestNotPlaceholder(t *testing.T) {
	if strings.HasSuffix(PinnedImage, "@sha256:"+placeholderDigest) {
		t.Fatal("RELEASE BLOCKER: edge.PinnedImage is the all-zero placeholder digest; set the real docker.io/library/caddy:2.11.x index digest")
	}
}

func TestImagePinnedFollowsImage(t *testing.T) {
	old := Image
	t.Cleanup(func() { Image = old })
	Image = "docker.io/library/caddy@sha256:" + placeholderDigest
	if ImagePinned() {
		t.Fatal("placeholder counted as pinned")
	}
	Image = "docker.io/library/caddy@sha256:" + strings.Repeat("ab", 32)
	if !ImagePinned() {
		t.Fatal("real digest not pinned")
	}
}
