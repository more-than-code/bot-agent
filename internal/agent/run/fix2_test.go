package run

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestContainerRollbackFakeDecodesEntry(t *testing.T) {
	deploy := []byte(`{"project":"acme","entry":"web","spec":{"kind":"service","image":"ghcr.io/acme/web:1"},"env":{},"digest":"` + imgDigest + `","spec_hash":"` + imgHash + `"}`)
	good := []byte(`{"project":"acme","entry":"web"}`)
	wrong := []byte(`{"project":"other","entry":"web"}`)
	fakes := []struct {
		name string
		fn   func(string, []byte) ([]byte, error)
	}{
		{"swap", (&swapHelper{}).call},
		{"kept", (&keptFiles{prev: &keptFile{digest: imgDigest, spec: imgHash}}).call},
		{"diff", (&diffSwap{}).call},
		{"seq", (&imageSeq{}).call},
	}
	for _, f := range fakes {
		t.Run(f.name, func(t *testing.T) {
			if _, err := f.fn(images.VerbRollback, deploy); err == nil {
				t.Fatal("deploy body accepted")
			}
			if _, err := f.fn(images.VerbRollback, wrong); err == nil {
				t.Fatal("project accepted")
			}
			if _, err := f.fn(images.VerbRollback, good); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProgramHelperLogs(t *testing.T) {
	const marker = "LOGMARKER9secret"
	logsSpec := []byte(`{"project":"tour-agent","entry":"tra-backend","lines":10}`)
	cases := []struct {
		name, kind, verb, reason string
		run                      func(t *testing.T, a *agent, h *fakeHelper, spec []byte)
	}{
		{
			name: "peek decode", kind: "program_status", verb: "status", reason: "decode",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = func(string, []byte) ([]byte, error) {
					return []byte("not-json-" + marker), nil
				}
				mustRun(t, a, "program_status", refSpec())
			},
		},
		{
			name: "peek refusal", kind: "program_stop", verb: "stop", reason: "bad_spec",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = func(string, []byte) ([]byte, error) {
					return []byte(`{"ok":false,"reason":"bad_spec"}`), nil
				}
				mustRun(t, a, "program_stop", refSpec())
			},
		},
		{
			name: "peek refusal fixed", kind: "program_stop", verb: "stop", reason: "policy_denied",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = func(string, []byte) ([]byte, error) {
					return []byte(`{"ok":false,"reason":"` + marker + `"}`), nil
				}
				mustRun(t, a, "program_stop", refSpec())
			},
		},
		{
			name: "start decode", kind: "program_start", verb: "start", reason: "decode",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = programReply(t, map[string][]byte{
					"list":  programs.EncodeList(nil),
					"start": []byte(`{"ok":true,"leak":"` + marker + `"}`),
				})
				mustRun(t, a, "program_start", spec)
			},
		},
		{
			name: "finish decode", kind: "program_status", verb: "status", reason: "decode",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = func(string, []byte) ([]byte, error) {
					return []byte(`{"ok":true}`), nil
				}
				mustRun(t, a, "program_status", refSpec())
			},
		},
		{
			name: "finish refusal", kind: "program_status", verb: "status", reason: "conflict",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				a.setHelperKind("program_status")
				a.finishStatus(context.Background(), programTask("program_status", refSpec()), []byte(`{"ok":false,"reason":"conflict"}`))
			},
		},
		{
			name: "logs decode", kind: "program_logs", verb: "logs", reason: "decode",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = func(string, []byte) ([]byte, error) {
					return []byte(`{"ok":true}`), nil
				}
				mustRun(t, a, "program_logs", logsSpec)
			},
		},
		{
			name: "logs refusal", kind: "program_logs", verb: "logs", reason: "io_error",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				a.setHelperKind("program_logs")
				a.finishLogs(programTask("program_logs", logsSpec), []byte(`{"ok":false,"reason":"io_error"}`))
			},
		},
		{
			name: "tail decode", kind: "program_start", verb: "logs", reason: "decode",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = programReply(t, map[string][]byte{
					"list":    programs.EncodeList(nil),
					"start":   programFresh(false, "activating"),
					"sockets": programs.EncodeSockets([]int{}, nil),
					"logs":    []byte(`{"leak":"` + marker + `"}`),
				})
				mustRun(t, a, "program_start", spec)
			},
		},
		{
			name: "tail refusal", kind: "program_start", verb: "logs", reason: "not_created",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = programReply(t, map[string][]byte{
					"list":    programs.EncodeList(nil),
					"start":   programFresh(false, "activating"),
					"sockets": programs.EncodeSockets([]int{}, nil),
					"logs":    []byte(`{"ok":false,"reason":"not_created"}`),
				})
				mustRun(t, a, "program_start", spec)
			},
		},
		{
			name: "status decode", kind: "program_start", verb: "status", reason: "decode",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = programReply(t, map[string][]byte{
					"list":   programs.EncodeList(nil),
					"start":  programFresh(true, "activating"),
					"status": []byte(`{"leak":"` + marker + `"}`),
				})
				mustRun(t, a, "program_start", spec)
			},
		},
		{
			name: "status refusal", kind: "program_start", verb: "status", reason: "policy_denied",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = programReply(t, map[string][]byte{
					"list":   programs.EncodeList(nil),
					"start":  programFresh(true, "activating"),
					"status": []byte(`{"ok":false,"reason":"policy_denied"}`),
				})
				mustRun(t, a, "program_start", spec)
			},
		},
		{
			name: "sockets decode", kind: "program_start", verb: "sockets", reason: "decode",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = programReply(t, map[string][]byte{
					"list":    programs.EncodeList(nil),
					"start":   programFresh(true, "activating"),
					"status":  statusBody("active", "running", nil),
					"sockets": []byte(`{"leak":"` + marker + `"}`),
				})
				mustRun(t, a, "program_start", spec)
			},
		},
		{
			name: "sockets refusal", kind: "program_start", verb: "sockets", reason: "not_created",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = programReply(t, map[string][]byte{
					"list":    programs.EncodeList(nil),
					"start":   programFresh(true, "activating"),
					"status":  statusBody("active", "running", nil),
					"sockets": []byte(`{"ok":false,"reason":"not_created"}`),
				})
				mustRun(t, a, "program_start", spec)
			},
		},
		{
			name: "list decode", kind: "program_status", verb: "list", reason: "decode",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				a.setHelperKind("program_status")
				h.fn = func(verb string, body []byte) ([]byte, error) {
					if verb != "list" {
						return nil, fmt.Errorf("%s", verb)
					}
					return []byte(`{"leak":"` + marker + `"}`), nil
				}
				_, _ = a.listUnits(context.Background())
			},
		},
		{
			name: "list refusal", kind: "program_status", verb: "list", reason: "policy_denied",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				a.setHelperKind("program_status")
				h.fn = func(string, []byte) ([]byte, error) {
					return []byte(`{"ok":false,"reason":"policy_denied"}`), nil
				}
				_, _ = a.listUnits(context.Background())
			},
		},
		{
			name: "transport", kind: "program_status", verb: "status", reason: "transport",
			run: func(t *testing.T, a *agent, h *fakeHelper, spec []byte) {
				h.fn = func(string, []byte) ([]byte, error) {
					return nil, fmt.Errorf("%s", marker)
				}
				mustRun(t, a, "program_status", refSpec())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h, _, spec := programAgent(t, 20)
			var buf bytes.Buffer
			a.opt.Log = stdLog(&buf)
			tc.run(t, a, h, spec)
			want := fmt.Sprintf("bot-agent: helper kind=%q verb=%q reason=%q\n", tc.kind, tc.verb, tc.reason)
			if buf.String() != want {
				t.Fatalf("log %q", buf.String())
			}
			if strings.Contains(buf.String(), marker) || strings.Contains(buf.String(), "tour-agent") || strings.Contains(buf.String(), "tra-backend") {
				t.Fatalf("request in log %s", buf.String())
			}
		})
	}
}

func programReply(t *testing.T, replies map[string][]byte) func(string, []byte) ([]byte, error) {
	t.Helper()
	return func(verb string, body []byte) ([]byte, error) {
		raw, ok := replies[verb]
		if !ok {
			return nil, fmt.Errorf("%s", verb)
		}
		return raw, nil
	}
}

func programFresh(fresh bool, state string) []byte {
	return programs.EncodeStart(programs.Status{
		Unit: "deploybot-tour-agent-tra-backend.service", ActiveState: state, SubState: "start", Ports: []int{},
	}, fresh)
}
