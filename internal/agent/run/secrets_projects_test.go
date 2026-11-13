package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

const runToken = "ghp_SECRETvalue0123456789"

// v1.11 item 1 and v1.12 item 5: hello.programs carries secrets and the
// policy's project list; patterns, dirs and exec_allow stay out.
func TestHelloSecretsAndProjects(t *testing.T) {
	proc, sys, passwd := fixtureProc(t, 9)
	h := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		if verb == "list" {
			return programs.EncodeList(nil), nil
		}
		return nil, errors.New(verb)
	}}
	hello, raw := helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd, GroupPath: joeGroup(t),
		PolicyPath: writeRunner(t, true, false), PolicyUID: rootUID, Helper: h,
	})
	if hello.Programs == nil || !hello.Programs.Secrets || strings.Join(hello.Programs.Projects, ",") != "tour-agent,acme" {
		t.Fatalf("%+v", hello.Programs)
	}
	if !strings.Contains(string(raw), `"secrets":true,"projects":["tour-agent","acme"]`) || strings.Contains(string(raw), "SENTINEL") {
		t.Fatalf("%s", raw)
	}
	// Disabled policy: still sent, with the list.
	hello, raw = helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd, GroupPath: joeGroup(t),
		PolicyPath: writeRunner(t, false, false), PolicyUID: rootUID, Helper: h,
	})
	if hello.Programs == nil || hello.Programs.Enabled || !strings.Contains(string(raw), `"projects":["tour-agent","acme"]`) {
		t.Fatalf("%s", raw)
	}
}

// An invalid policy sends projects as [], never null.
func TestHelloInvalidPolicyEmptyProjects(t *testing.T) {
	a := &agent{opt: Options{Log: stdLog(&bytes.Buffer{})}}
	a.policy.Exists = true
	a.policy.Projects = []string{"x"}
	p, _ := a.helloPrograms()
	b, err := json.Marshal(p)
	if err != nil || !strings.Contains(string(b), `"projects":[]`) {
		t.Fatalf("%s %v", b, err)
	}
}

// v1.12 item 4: one start-up log line per owned unit of an unlisted project.
func TestStartupLogsUnlistedUnits(t *testing.T) {
	proc, sys, passwd := fixtureProc(t, 9)
	h := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList([]programs.UnitInfo{
				{Unit: "deploybot-tour-agent-web.service", Project: "tour-agent", Entry: "web", ActiveState: "active"},
				{Unit: "deploybot-home-lab-tra-backend.service", Project: "home-lab", Entry: "tra-backend", ActiveState: "active"},
				{Unit: "deploybot-bad.service", Project: "x", Entry: "y", ActiveState: "active"},
			}), nil
		case "sockets":
			return programs.EncodeSockets([]int{}, nil), nil
		default:
			return nil, errors.New(verb)
		}
	}}
	var buf bytes.Buffer
	helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd, GroupPath: joeGroup(t),
		PolicyPath: writeRunner(t, true, false), PolicyUID: rootUID, Helper: h,
		Log: stdLog(&buf),
	})
	want := "bot-agent: program unit deploybot-home-lab-tra-backend.service is for project home-lab, which is not in projects\n"
	if strings.Count(buf.String(), "which is not in projects") != 1 || !strings.Contains(buf.String(), want) {
		t.Fatalf("log %q", buf.String())
	}
	if h.saw("stop") || h.saw("restart") {
		t.Fatalf("calls %v", h.Calls())
	}
}

// v1.12 item 1: the agent refuses start and restart of an unlisted project
// before calling the helper; stop, status and logs go through.
func TestAgentProjectGate(t *testing.T) {
	a, h, _, _ := programAgent(t, 20)
	h.fn = programScript(t, "active", "running", nil, nil, []string{"ok"})
	spec := []byte(`{"project":"other","entry":"web","dir":"/tmp","exec":"/usr/bin/pnpm","args":[],"shell":false,"run_as":"joe","env":{},"restart":"on-failure"}`)
	ref := []byte(`{"project":"other","entry":"web"}`)
	for _, tc := range []struct {
		kind string
		body []byte
	}{{"program_start", spec}, {"program_restart", ref}} {
		rej, _, ok := a.execute(context.Background(), programTask(tc.kind, tc.body))
		if ok || rej.ErrorCode != "policy_denied" {
			t.Fatalf("%s ok %v %+v", tc.kind, ok, rej)
		}
	}
	if len(h.Calls()) != 0 {
		t.Fatalf("helper called %v", h.Calls())
	}
	for _, tc := range []struct {
		kind string
		body []byte
	}{{"program_stop", ref}, {"program_status", ref}, {"program_logs", []byte(`{"project":"other","entry":"web","lines":5}`)}} {
		if _, _, ok := a.execute(context.Background(), programTask(tc.kind, tc.body)); !ok {
			t.Fatalf("%s rejected", tc.kind)
		}
	}
}

// v1.11 item 4: a start_failed tail is redacted with the frame's own secret
// values, as the preview, on top of the helper's redaction.
func TestStartFailedTailRedactsTaskSecrets(t *testing.T) {
	a, h, _, spec := programAgent(t, 20)
	var m map[string]any
	if err := json.Unmarshal(spec, &m); err != nil {
		t.Fatal(err)
	}
	m["env"] = map[string]string{"GH_TOKEN": runToken, "PORT": "3031"}
	m["secret_keys"] = []string{"GH_TOKEN"}
	spec, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	h.fn = programScript(t, "failed", "exit-code", nil, nil, []string{"using " + runToken, "plain"})
	res := mustRun(t, a, "program_start", spec)
	if res.State != "failed" || res.ErrorCode == nil || *res.ErrorCode != "start_failed" {
		t.Fatalf("%+v", res)
	}
	lines, _ := res.Payload["lines"].([]string)
	if len(lines) != 2 || lines[0] != "using ghp_…" || lines[1] != "plain" {
		t.Fatalf("lines %q", lines)
	}
	if b := marshalPayload(t, res.Payload); strings.Contains(string(b), runToken) {
		t.Fatalf("payload %s", b)
	}
}
