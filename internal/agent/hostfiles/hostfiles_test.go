package hostfiles

import (
	"strings"
	"testing"
)

func TestValidPath(t *testing.T) {
	for p, ok := range map[string]bool{
		"/etc/dd-worker/policy.toml": true,
		"/etc/app/.env":              false,
		"/etc/app/a.conf":            true,
		"/etc/app":                   false,
		"/etc/app/sub/a":             false,
		"/etc/../app/a":              false,
		"/etc//app/a":                false,
		"/etc/app/a/":                false,
		"etc/app/a":                  false,
		"/etc/ssh/sshd_config":       false,
		"/etc/sudoers.d/x":           false,
		"/etc/bot-agent/runner.toml": false,
		"/etc/app/a b":               false,
	} {
		if _, _, got := ValidPath(p); got != ok {
			t.Errorf("%q: %v", p, got)
		}
	}
}

func TestValidUnit(t *testing.T) {
	for u, ok := range map[string]bool{
		"dd-worker.service": true, "app@1.service": true, "app.timer": false,
		"deploybot-x.service": false, "sshd.service": false, "systemd-journald.service": false,
		"dbus.service": false, "docker.service": false, "containerd.service": false,
		"caddy.service": false, "postgresql@18-main.service": false, "bot-agent.service": false,
	} {
		if got := ValidUnit(u); got != ok {
			t.Errorf("%q: %v", u, got)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	if argv, ok := SplitCommand("/usr/local/bin/dd-worker policy check --file -"); !ok || len(argv) != 5 {
		t.Fatal(argv)
	}
	for _, bad := range []string{"", "dd-worker x", "/a  b", "/a b ", "/a\tb", "/a/../b", "/", "/a " + strings.Repeat("x ", 16) + "x"} {
		if _, ok := SplitCommand(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, ok := SplitCommand("/a" + strings.Repeat(" x", 16)); !ok {
		t.Fatal("16 arguments refused")
	}
}

func TestSettle(t *testing.T) {
	health := []string{"/x", "health", "--wait", "30s"}
	n := func(v int) *int { return &v }
	cases := []struct {
		req    *int
		health []string
		want   int
		ok     bool
	}{
		{nil, nil, 60, true},
		{nil, health, 60, true},
		{n(44), health, 0, false},
		{n(45), health, 45, true},
		{n(30), nil, 30, true},
		{n(29), nil, 0, false},
		{n(181), nil, 0, false},
		{nil, []string{"/x", "health", "--wait=90s"}, 105, true},
		{nil, []string{"/x", "health", "--wait", "1m10s"}, 85, true},
		{nil, []string{"/x", "health", "--wait", "170s"}, 0, false},
		{nil, []string{"/x", "health", "--wait", "soon"}, 0, false},
		{nil, []string{"/x", "health", "--wait"}, 0, false},
	}
	for _, c := range cases {
		got, ok := Settle(c.req, c.health)
		if got != c.want || ok != c.ok {
			t.Errorf("%v %v: %d %v", c.req, c.health, got, ok)
		}
	}
}

func TestParseApplyAndResult(t *testing.T) {
	if _, b, err := ParseApply([]byte(`{"name":"a","content_b64":"eA==","settle_sec":30,"force":true}`)); err != nil || string(b) != "x" {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"name":"a","content_b64":"eA"}`, `{"name":"a"}`, `[]`, `{"name":"a","content_b64":"eA=="} {}`} {
		if _, _, err := ParseApply([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if !ValidResult(ApplyResult{Outcome: OutcomeCheckFailed, Stage: StageCheck, Reason: "bad_repo_url"}) {
		t.Fatal("bad_ token refused")
	}
	for _, r := range []ApplyResult{
		{Outcome: "ok"},
		{Outcome: OutcomeSucceeded, Stage: "swap"},
		{Outcome: OutcomeCheckFailed, Reason: "bad_Repo"},
		{Outcome: OutcomeSucceeded, SHA256: "abc"},
		{Outcome: OutcomeSucceeded, TookMS: -1},
	} {
		if ValidResult(r) {
			t.Errorf("accepted %+v", r)
		}
	}
}

// Rev 1a A5/A6 rules.
func TestRev1aRules(t *testing.T) {
	sum := strings.Repeat("a", 64)
	if !ValidResult(ApplyResult{Outcome: OutcomeUnchanged, SHA256: sum}) || ValidResult(ApplyResult{Outcome: OutcomeCheckFailed, Stage: StageCheck, SHA256: sum}) ||
		ValidResult(ApplyResult{Outcome: OutcomeRolledBack, Stage: StageHealth, SHA256: sum}) {
		t.Fatal("sha256 rule")
	}
	for argv, ok := range map[string]bool{
		"/x health --wait 165s": true, "/x health --wait 166s": false, "/x health": true, "/x --wait=1m": true, "/x --wait=3m": false, "/x --wait bad": false,
	} {
		if got := ValidHealth(strings.Split(argv, " ")); got != ok {
			t.Errorf("%q: %v", argv, got)
		}
	}
	for _, u := range []string{"-x.service", ".x.service", "_x.service", "@x.service", "networking.service", "NetworkManager-wait-online.service",
		"tailscaled.service", "cron.service", "rsyslog.service", "polkit.service", "user@1000.service", "getty@tty1.service"} {
		if ValidUnit(u) {
			t.Errorf("accepted %q", u)
		}
	}
	for _, d := range []string{"tmpfiles.d", "sysusers.d", "sysctl.d", "modprobe.d", "profile.d", "ld.so.conf.d", "cron.daily", "cron.hourly",
		"logrotate.d", "update-motd.d", "udev", "init.d", "apparmor.d", "dpkg", "rsyslog.d", "default"} {
		if _, _, ok := ValidPath("/etc/" + d + "/x"); ok {
			t.Errorf("accepted dir %q", d)
		}
	}
	if _, _, ok := ValidPath("/etc/crontab-app/x"); !ok {
		t.Error("cron prefix without a dot refused")
	}
	if WorstCase() >= TaskTimeout {
		t.Fatalf("worst case %v", WorstCase())
	}
}
