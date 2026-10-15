package programs

import (
	"encoding/json"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

func TestEncodeRefusalIsBool(t *testing.T) {
	raw := EncodeRefusal(ReasonPolicy)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	ok, isBool := m["ok"].(bool)
	if !isBool || ok {
		t.Fatalf("ok %#v in %s", m["ok"], raw)
	}
	if m["reason"] != ReasonPolicy {
		t.Fatalf("reason %#v", m["reason"])
	}
	if len(m) != 2 {
		t.Fatalf("keys %v", m)
	}
	st, reason, err := DecodeStatus(raw)
	if err != nil || reason != ReasonPolicy || st.Unit != "" {
		t.Fatalf("decode %q %s %v", reason, st.Unit, err)
	}
}

func TestImageVerbsStayDistinct(t *testing.T) {
	for _, verb := range []string{
		images.VerbPull, images.VerbDeploy, images.VerbRollback, images.VerbRemove,
		images.VerbStart, images.VerbStop, images.VerbRestart, images.VerbStatus,
		images.VerbLogs, images.VerbList,
	} {
		if !KnownVerb(verb) {
			t.Fatalf("image verb %s missing from the helper registry", verb)
		}
	}
	if KnownVerb("image_deploy") {
		t.Fatal("task kind was registered as a helper verb")
	}
	for _, reason := range []string{ReasonConflict, ReasonDocker, ReasonImage} {
		if FixedReason(reason) != reason {
			t.Fatalf("collapsed %s", reason)
		}
	}
	if FixedReason("token=secret") != ReasonPolicy {
		t.Fatal("unknown reason escaped")
	}
}
