package agentupgrade

import "testing"

func TestValidVersion(t *testing.T) {
	for _, v := range []string{"0.2.16", "1.0.0", "9999.9999.9999"} {
		if !ValidVersion(v) {
			t.Errorf("%q refused", v)
		}
	}
	for _, v := range []string{"", "v0.2.16", "0.2", "0.2.16.1", "0.2.16-rc1", "10000.0.0", "0.2.x", " 0.2.16", "0.2.16\n", "../0.2.16", "0..2"} {
		if ValidVersion(v) {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.16", "0.2.15", true},
		{"0.2.15", "0.2.15", false},
		{"0.2.9", "0.2.10", false},
		{"0.2.10", "0.2.9", true},
		{"1.0.0", "0.99.99", true},
		{"0.3.0", "0.2.99", true},
		{"0.2.16", "", false},
		{"0.2.16", "unknown", false},
		{"x", "0.1.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q,%q)=%v", c.a, c.b, got)
		}
	}
}

func TestParseRequest(t *testing.T) {
	if r, ok := ParseRequest([]byte(`{"version":"0.2.16"}`)); !ok || r.Version != "0.2.16" {
		t.Fatal("good request refused")
	}
	for _, bad := range []string{``, `{}`, `{"version":"0.2"}`, `{"version":"0.2.16","url":"x"}`, `{"version":"0.2.16"}{}`, `[]`, `{"version":1}`} {
		if _, ok := ParseRequest([]byte(bad)); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestResultRoundTrip(t *testing.T) {
	r := Result{Version: "0.2.16", Outcome: OutcomeRolledBack, Reason: ReasonNotReconnected, At: "2026-11-23T10:00:00Z"}
	got, ok := DecodeResult(Encode(r))
	if !ok || got != r {
		t.Fatalf("round trip %+v %v", got, ok)
	}
	for _, bad := range []Result{
		{Version: "0.2.16", Outcome: "done", At: r.At},
		{Version: "0.2.16", Outcome: OutcomeFailed, Reason: "/etc/passwd", At: r.At},
		{Version: "0.2.16", Outcome: OutcomeFailed, At: "yesterday"},
		{Version: "x", Outcome: OutcomeFailed, At: r.At},
	} {
		if _, ok := DecodeResult(Encode(bad)); ok {
			t.Errorf("%+v accepted", bad)
		}
	}
	rr, ok := DecodeResultReply([]byte(`{"ok":true,"result":null,"running":true}`))
	if !ok || rr.Result != nil || !rr.Running {
		t.Fatal("null result")
	}
	rr, ok = DecodeResultReply(Encode(ResultReply{OK: true, Result: &r}))
	if !ok || rr.Result == nil || *rr.Result != r || rr.Running {
		t.Fatal("result reply")
	}
	if !DecodeStartReply(Encode(StartReply{OK: true, State: StateAccepted})) {
		t.Fatal("start reply")
	}
	if DecodeStartReply([]byte(`{"ok":false,"reason":"busy"}`)) {
		t.Fatal("refusal read as accepted")
	}
}
