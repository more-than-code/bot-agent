package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/hostpolicy"
)

const managedOn = "# set by the host's root\nconsole_policy = true\nimages = [\"ghcr.io/acme/*\"]\nprojects = [\"acme\"]\n"

func policyBody(base string, edge bool, imgs, projs []string) string {
	b, _ := json.Marshal(hostpolicy.Request{BaseHash: base, Edge: edge, Images: imgs, Projects: projs})
	return string(b)
}

func (e *upEnv) policyHash() string {
	e.t.Helper()
	b, err := os.ReadFile(e.sys.Policy)
	if err != nil {
		e.t.Fatal(err)
	}
	return sha256Hex(b)
}

func (e *upEnv) applyPolicy(body string) string {
	e.t.Helper()
	return string(Handle(context.Background(), hostpolicy.Verb, []byte(body), e.sys))
}

func TestHostPolicyApplyAccepted(t *testing.T) {
	e := newUpEnv(t, managedOn)
	base := e.policyHash()
	out := e.applyPolicy(policyBody(base, true, []string{"ghcr.io/acme/*", "ghcr.io/kea/site"}, []string{"acme"}))
	reply, ok := hostpolicy.DecodeStartReply([]byte(out))
	if !ok {
		t.Fatalf("reply %s", out)
	}
	staged, err := os.ReadFile(policyStagedPath(e.root))
	if err != nil {
		t.Fatal(err)
	}
	if sha256Hex(staged) != reply.NewHash {
		t.Fatal("new_hash is not the staged file's hash")
	}
	if !strings.HasPrefix(string(staged), "# set by the host's root\n") || !strings.Contains(string(staged), "edge = true\n") ||
		!strings.Contains(string(staged), `images = ["ghcr.io/acme/*", "ghcr.io/kea/site"]`) {
		t.Fatalf("staged %q", staged)
	}
	want := [][]string{{
		"--unit=bot-agent-policy", "--collect", "--property=RuntimeMaxSec=300",
		"/usr/local/libexec/bot-agent-runner", "policy-run", base, reply.NewHash,
	}}
	if !reflect.DeepEqual(e.units, want) {
		t.Fatalf("systemd-run %q", e.units)
	}
	// The helper never writes runner.toml itself.
	if e.policyHash() != base {
		t.Fatal("runner.toml changed")
	}
}

func TestHostPolicyApplyRefusals(t *testing.T) {
	cases := []struct {
		name, policy, reason string
		body                 func(e *upEnv) string
		setup                func(e *upEnv) func()
	}{
		{"not opted in", "images = []\n", "policy_denied", func(e *upEnv) string {
			return policyBody(e.policyHash(), true, []string{}, []string{})
		}, nil},
		{"opt-in off", "console_policy = false\n", "policy_denied", func(e *upEnv) string {
			return policyBody(e.policyHash(), true, []string{}, []string{})
		}, nil},
		{"bad spec", managedOn, "bad_spec", func(e *upEnv) string { return `{"base_hash":"x"}` }, nil},
		{"stale base", managedOn, "conflict", func(e *upEnv) string {
			return policyBody(strings.Repeat("0", 64), true, []string{}, []string{"acme"})
		}, nil},
		{"no change", managedOn + "edge = false\n", "bad_spec", func(e *upEnv) string {
			return policyBody(e.policyHash(), false, []string{"ghcr.io/acme/*"}, []string{"acme"})
		}, nil},
		{"overlapping projects stay valid but helper lock busy", managedOn, "busy", func(e *upEnv) string {
			return policyBody(e.policyHash(), true, []string{}, []string{"acme"})
		}, func(e *upEnv) func() {
			return holdLock(e.t, joinRoot(e.root, "run/bot-agent/helper.lock"))
		}},
		{"unit active", managedOn, "busy", func(e *upEnv) string {
			return policyBody(e.policyHash(), true, []string{}, []string{"acme"})
		}, func(e *upEnv) func() { e.active = "active"; return func() {} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newUpEnv(t, tc.policy)
			if err := ensureRuntime(e.root); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				defer tc.setup(e)()
			}
			if got := refusal(t, e.applyPolicy(tc.body(e))); got != tc.reason {
				t.Fatalf("reason %q, want %q", got, tc.reason)
			}
			if len(e.units) != 0 {
				t.Fatalf("started %q", e.units)
			}
		})
	}
}

// policyRunEnv stages a change through the verb, then gives policy-run a
// clock that the fake systemctl restart moves on and a hello marker the
// "agent" writes, or not.
func policyRunEnv(t *testing.T, hello bool) (*upEnv, string, string, *[]string) {
	e := newUpEnv(t, managedOn)
	base := e.policyHash()
	reply, ok := hostpolicy.DecodeStartReply([]byte(e.applyPolicy(policyBody(base, true, []string{"ghcr.io/acme/*"}, []string{"acme"}))))
	if !ok {
		t.Fatal("not accepted")
	}
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	e.sys.now = func() time.Time { return now }
	e.sys.sleep = func(_ context.Context, d time.Duration) { now = now.Add(d) }
	var restarts []string
	marker := joinRoot(e.root, helloMarkerRel)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	e.sys.Systemctl = func(_ context.Context, args []string) (string, error) {
		restarts = append(restarts, strings.Join(args, " "))
		b, _ := os.ReadFile(e.sys.Policy)
		restarts = append(restarts, "file="+sha256Hex(b)[:8])
		if hello && len(restarts) == 2 {
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			at := now.Add(time.Second)
			_ = os.Chtimes(marker, at, at)
		}
		return "", nil
	}
	return e, base, reply.NewHash, &restarts
}

func TestPolicyRunSucceeded(t *testing.T) {
	e, base, next, restarts := policyRunEnv(t, true)
	res, err := PolicyRun(context.Background(), base, next, e.sys)
	if err != nil || res.Outcome != hostpolicy.OutcomeSucceeded || res.Hash != next {
		t.Fatalf("%+v %v", res, err)
	}
	if e.policyHash() != next {
		t.Fatal("runner.toml not written")
	}
	prev, err := os.ReadFile(e.sys.Policy + ".prev")
	if err != nil || sha256Hex(prev) != base {
		t.Fatal("previous file not kept")
	}
	if fi, _ := os.Stat(e.sys.Policy); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if len(*restarts) != 2 || (*restarts)[0] != "restart bot-agent.service" {
		t.Fatalf("%q", *restarts)
	}
	if _, err := os.Stat(policyStagedPath(e.root)); !os.IsNotExist(err) {
		t.Fatal("staged file left")
	}
	b, _ := os.ReadFile(PolicyResultPath(e.root))
	if !strings.Contains(string(b), `"outcome":"succeeded"`) {
		t.Fatalf("result %s", b)
	}
}

func TestPolicyRunRollsBackWithoutHello(t *testing.T) {
	e, base, next, restarts := policyRunEnv(t, false)
	res, err := PolicyRun(context.Background(), base, next, e.sys)
	if err != nil || res.Outcome != hostpolicy.OutcomeRolledBack || res.Reason != hostpolicy.ReasonNotReconnected {
		t.Fatalf("%+v %v", res, err)
	}
	if e.policyHash() != base {
		t.Fatal("previous file not put back")
	}
	// Two restarts: the new file, then the old one.
	want := []string{"restart bot-agent.service", "file=" + next[:8], "restart bot-agent.service", "file=" + base[:8]}
	if !reflect.DeepEqual(*restarts, want) {
		t.Fatalf("%q", *restarts)
	}
}

// Someone edited runner.toml between the verb and the unit: nothing is
// written and nothing restarts.
func TestPolicyRunChangedFile(t *testing.T) {
	e, base, next, restarts := policyRunEnv(t, true)
	if err := os.WriteFile(e.sys.Policy, []byte(managedOn+"edge = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := PolicyRun(context.Background(), base, next, e.sys)
	if res.Outcome != hostpolicy.OutcomeFailed || res.Reason != hostpolicy.ReasonChanged || len(*restarts) != 0 {
		t.Fatalf("%+v %q", res, *restarts)
	}
}

func TestHostPolicyBusyDuringUpgrade(t *testing.T) {
	e := newUpEnv(t, managedOn)
	if err := ensureRuntime(e.root); err != nil {
		t.Fatal(err)
	}
	defer holdLock(t, UpgradeLockPath(e.root))()
	if got := refusal(t, e.applyPolicy(policyBody(e.policyHash(), true, []string{}, []string{"acme"}))); got != "busy" {
		t.Fatalf("%q", got)
	}
}
