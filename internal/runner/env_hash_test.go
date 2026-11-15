package runner

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// agent-protocol v1.14: container_deploy env_hash becomes a label, and
// container_status returns it for the current and the -prev container.

const envHashA = "3333333333333333333333333333333333333333333333333333333333333333"

func TestEnvHashLabelOnCreate(t *testing.T) {
	for _, kind := range []string{"service", "job"} {
		t.Run(kind, func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			req := serviceDeploy(hashC)
			if kind == "job" {
				req = jobDeploy()
			}
			req.EnvHash = envHashA
			if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != "" {
				t.Fatal("deploy")
			}
			if got := createLabels(t, e.fake.body(http.MethodPost, "/containers/create"))[images.LabelEnvHash]; got != envHashA {
				t.Fatalf("label %v", got)
			}
		})
	}

	t.Run("absent without env_hash", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))) != "" {
			t.Fatal("deploy")
		}
		if _, ok := createLabels(t, e.fake.body(http.MethodPost, "/containers/create"))[images.LabelEnvHash]; ok {
			t.Fatal("env_hash label without env_hash")
		}
		raw := e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
		if strings.Contains(string(raw), "env_hash") {
			t.Fatalf("status %s", raw)
		}
	})
}

func TestEnvHashStatus(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	first := serviceDeploy(hashC)
	first.EnvHash = envHashA
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, first))) != "" {
		t.Fatal("first")
	}
	raw := e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	var st images.StatusResult
	if json.Unmarshal(raw, &st) != nil || st.EnvHash != envHashA || st.Prev != nil {
		t.Fatalf("status %s", raw)
	}
	// A second deploy without env_hash parks the first as -prev.
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))) != "" {
		t.Fatal("second")
	}
	raw = e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	st = images.StatusResult{}
	if json.Unmarshal(raw, &st) != nil || st.EnvHash != "" || st.Prev == nil || st.Prev.EnvHash != envHashA {
		t.Fatalf("status %s", raw)
	}
}

func TestEnvHashMalformedRefusedBeforeEngine(t *testing.T) {
	for _, v := range []string{"zz", strings.Repeat("A", 64), envHashA[:63], envHashA + "3", envHashA[:63] + "\n"} {
		e := newImgEnv(t, imagePolicy)
		req := serviceDeploy(hashC)
		req.EnvHash = v
		raw := e.call(images.VerbDeploy, mustJSON(t, req))
		if string(raw) != `{"error":"`+programs.ReasonBadSpec+`"}` {
			t.Fatalf("%q: %s", v, raw)
		}
		if len(e.fake.Calls()) != 0 {
			t.Fatalf("%q: engine called", v)
		}
	}
}

func TestEnvHashStatusMalformedLabel(t *testing.T) {
	entry := images.EntryRequest{Project: "mohiguide", Entry: "web"}
	for _, bad := range []string{"zz", strings.Repeat("A", 64), envHashA[:63], ""} {
		e := newImgEnv(t, imagePolicy)
		cur := e.labels("web")
		cur[images.LabelEnvHash] = bad
		prev := e.labels("web")
		prev[images.LabelEnvHash] = bad
		e.fake.add("mohiguide-web", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, cur)
		e.fake.add("mohiguide-web-prev", "exited", "ghcr.io/joe-and-his-friends/app@"+digestB, prev)
		raw := e.call(images.VerbStatus, mustJSON(t, entry))
		var body map[string]any
		if json.Unmarshal(raw, &body) != nil {
			t.Fatalf("%q: %s", bad, raw)
		}
		if _, ok := body["env_hash"]; ok {
			t.Fatalf("%q: env_hash %s", bad, raw)
		}
		p, ok := body["prev"].(map[string]any)
		if !ok {
			t.Fatalf("%q: prev %s", bad, raw)
		}
		if _, ok := p["env_hash"]; ok {
			t.Fatalf("%q: prev.env_hash %s", bad, raw)
		}
		var st images.StatusResult
		if json.Unmarshal(raw, &st) != nil || st.EnvHash != "" || st.Prev == nil || st.Prev.EnvHash != "" || st.SpecHash != hashC {
			t.Fatalf("%q: %s", bad, raw)
		}
	}
}

// The typed request omits an empty EnvHash, so these frames are raw JSON.
func TestEnvHashRawFrame(t *testing.T) {
	base := string(mustJSON(t, serviceDeploy(hashC)))
	with := func(v string) []byte {
		return []byte(strings.TrimSuffix(base, "}") + `,"env_hash":` + v + `}`)
	}
	for _, v := range []string{`""`, `"zz"`, `1`} {
		e := newImgEnv(t, imagePolicy)
		raw := e.call(images.VerbDeploy, with(v))
		if string(raw) != `{"error":"`+programs.ReasonBadSpec+`"}` {
			t.Fatalf("%s: %s", v, raw)
		}
		if len(e.fake.Calls()) != 0 {
			t.Fatalf("%s: engine called", v)
		}
	}
	for _, tc := range []struct {
		frame []byte
		label string
	}{
		{[]byte(base), ""},
		{with(`null`), ""},
		{with(`"` + envHashA + `"`), envHashA},
	} {
		e := newImgEnv(t, imagePolicy)
		if code := imgCode(t, e.call(images.VerbDeploy, tc.frame)); code != "" {
			t.Fatalf("%s: %s", tc.frame, code)
		}
		got, ok := createLabels(t, e.fake.body(http.MethodPost, "/containers/create"))[images.LabelEnvHash]
		if got != tc.label || ok != (tc.label != "") {
			t.Fatalf("%s: label %q", tc.frame, got)
		}
	}
}
