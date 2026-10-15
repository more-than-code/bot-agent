package programs

import (
	"encoding/json"
	"testing"
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
