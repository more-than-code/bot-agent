package runner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestCreateLogConfig(t *testing.T) {
	for _, kind := range []string{"service", "job"} {
		t.Run(kind+" default", func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			req := serviceDeploy(hashC)
			if kind == "job" {
				req = jobDeploy()
			}
			raw := e.call(images.VerbDeploy, mustJSON(t, req))
			if imgCode(t, raw) != "" {
				t.Fatal(string(raw))
			}
			assertLogConfig(t, e.fake.body(http.MethodPost, "/containers/create"), "10m", "3")
		})
		t.Run(kind+" values", func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			req := serviceDeploy(hashC)
			if kind == "job" {
				req = jobDeploy()
			}
			req.Spec.Logging = &images.Logging{MaxSizeMB: 5, MaxFiles: 2}
			raw := e.call(images.VerbDeploy, mustJSON(t, req))
			if imgCode(t, raw) != "" {
				t.Fatal(string(raw))
			}
			assertLogConfig(t, e.fake.body(http.MethodPost, "/containers/create"), "5m", "2")
		})
		t.Run(kind+" null", func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			req := serviceDeploy(hashC)
			if kind == "job" {
				req = jobDeploy()
			}
			raw := e.call(images.VerbDeploy, deployRaw(t, req, "null"))
			if imgCode(t, raw) != "" {
				t.Fatal(string(raw))
			}
			assertLogConfig(t, e.fake.body(http.MethodPost, "/containers/create"), "10m", "3")
		})
	}
}

func TestLoggingRefusedBeforeEngine(t *testing.T) {
	cases := []struct {
		name string
		kind string
		raw  string
	}{
		{"size 0", "service", `{"max_size_mb":0,"max_files":3}`},
		{"size 101", "service", `{"max_size_mb":101,"max_files":3}`},
		{"size negative", "job", `{"max_size_mb":-1,"max_files":1}`},
		{"files 0", "job", `{"max_size_mb":10,"max_files":0}`},
		{"files 11", "service", `{"max_size_mb":1,"max_files":11}`},
		{"one key", "service", `{"max_size_mb":10}`},
		{"empty object", "job", `{}`},
		{"unknown key", "service", `{"max_size_mb":10,"max_files":3,"driver":"local"}`},
		{"fraction", "job", `{"max_size_mb":10.5,"max_files":3}`},
		{"string", "service", `{"max_size_mb":"10","max_files":3}`},
		{"array", "job", `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.kind+" "+tc.name, func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			req := serviceDeploy(hashC)
			if tc.kind == "job" {
				req = jobDeploy()
			}
			raw := e.call(images.VerbDeploy, deployRaw(t, req, tc.raw))
			if string(raw) != `{"error":"`+programs.ReasonBadSpec+`"}` {
				t.Fatal(string(raw))
			}
			if strings.Contains(string(raw), "driver") || strings.Contains(string(raw), "local") || strings.Contains(string(raw), "10.5") {
				t.Fatal(string(raw))
			}
			if len(e.fake.Calls()) != 0 || e.fake.body(http.MethodPost, "/containers/create") != nil {
				t.Fatal("engine called")
			}
		})
	}
}

func TestContainerLogsUnchanged(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	req := serviceDeploy(hashC)
	req.Spec.Logging = &images.Logging{MaxSizeMB: 5, MaxFiles: 2}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != "" {
		t.Fatal("deploy")
	}
	e.fake.logBody = append(frame(1, "line-one\n"), frame(1, "line-two\n")...)
	before := len(e.fake.Calls())
	raw := e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 20}))
	var logs images.LogsResponse
	if json.Unmarshal(raw, &logs) != nil || logs.Container != "mohiguide-web" || logs.Truncated || len(logs.Lines) != 2 || logs.Lines[0] != "line-one" || logs.Lines[1] != "line-two" {
		t.Fatal(string(raw))
	}
	if strings.Contains(string(raw), "max-size") || strings.Contains(string(raw), "LogConfig") || strings.Contains(string(raw), "logging") {
		t.Fatal(string(raw))
	}
	var query string
	var creates int
	for _, call := range e.fake.Calls()[before:] {
		if strings.HasSuffix(call.Path, "/logs") {
			query = call.Query
			if call.Method != http.MethodGet || len(call.Body) != 0 {
				t.Fatalf("logs call %s %s", call.Method, call.Body)
			}
		}
		if call.Method == http.MethodPost && strings.HasSuffix(call.Path, "/containers/create") {
			creates++
		}
	}
	if query != "stderr=1&stdout=1&tail=20&timestamps=0" || creates != 0 {
		t.Fatalf("query %s creates %d", query, creates)
	}

	n := len(e.fake.Calls())
	refused := e.call(images.VerbLogs, []byte(`{"project":"mohiguide","entry":"web","lines":5,"logging":{"max_size_mb":10,"max_files":3}}`))
	if string(refused) != `{"error":"`+programs.ReasonBadSpec+`"}` {
		t.Fatal(string(refused))
	}
	if len(e.fake.Calls()) != n {
		t.Fatal("engine called for a logs request with logging")
	}
}

func assertLogConfig(t *testing.T, body []byte, size, files string) {
	t.Helper()
	var got struct {
		HostConfig struct {
			LogConfig struct {
				Type   string
				Config map[string]string
			}
		}
	}
	if json.Unmarshal(body, &got) != nil {
		t.Fatal(string(body))
	}
	cfg := got.HostConfig.LogConfig
	if cfg.Type != "json-file" || len(cfg.Config) != 2 || cfg.Config["max-size"] != size || cfg.Config["max-file"] != files {
		t.Fatalf("%s", body)
	}
}

func deployRaw(t *testing.T, req images.DeployRequest, logging string) []byte {
	t.Helper()
	raw := mustJSON(t, req)
	needle := []byte(`"kind":"` + req.Spec.Kind + `"`)
	if !bytes.Contains(raw, needle) {
		t.Fatalf("kind missing in %s", raw)
	}
	insert := []byte(`"kind":"` + req.Spec.Kind + `","logging":` + logging)
	return bytes.Replace(raw, needle, insert, 1)
}
