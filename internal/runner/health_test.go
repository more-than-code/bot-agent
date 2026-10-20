package runner

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

func TestParseListHealth(t *testing.T) {
	id := strings.Repeat("ab", 32)
	body := `[
		{"Id":"` + id + `","Names":["/structured"],"State":"running","Status":"Up (healthy)","Health":{"Status":"unhealthy","FailingStreak":4}},
		{"Id":"` + id + `","Names":["/none"],"State":"running","Status":"Up (healthy)","Health":{"Status":"none"}},
		{"Id":"` + id + `","Names":["/lower"],"State":"running","Status":"Up (unhealthy)","Health":{"status":"healthy"}},
		{"Id":"` + id + `","Names":["/badobj"],"State":"running","Status":"Up (healthy)","Health":{"Status":1}},
		{"Id":"` + id + `","Names":["/suffix-h"],"State":"running","Status":"Up 2 hours (healthy)"},
		{"Id":"` + id + `","Names":["/suffix-u"],"State":"running","Status":"Up (unhealthy)","Health":null},
		{"Id":"` + id + `","Names":["/suffix-s"],"State":"running","Status":"Up (health: starting)","Health":"starting"},
		{"Id":"` + id + `","Names":["/plain"],"State":"running","Status":"Up 3 seconds"},
		{"Id":"` + id + `","Names":["/empty"],"State":"running"}
	]`
	rows, err := parseList([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range rows {
		if row.Health == nil {
			got[row.Names[0]] = "null"
			continue
		}
		got[row.Names[0]] = *row.Health
	}
	want := map[string]string{
		"structured": "unhealthy",
		"none":       "null",
		"lower":      "null",
		"badobj":     "null",
		"suffix-h":   "healthy",
		"suffix-u":   "unhealthy",
		"suffix-s":   "starting",
		"plain":      "null",
		"empty":      "null",
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for name, health := range want {
		if got[name] != health {
			t.Fatalf("%s got %s", name, got[name])
		}
	}
	raw, err := json.Marshal(projectRows(rows))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"health":"unhealthy"`) || !strings.Contains(string(raw), `"health":null`) {
		t.Fatal(string(raw))
	}
}

func TestCreateHealthcheckBody(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	req := serviceDeploy(hashC)
	req.Spec.Healthcheck = &images.Healthcheck{
		Test:           []string{"wget", "-q", "http://127.0.0.1/"},
		IntervalSec:    30,
		TimeoutSec:     5,
		Retries:        3,
		StartPeriodSec: 0,
	}
	raw := e.call(images.VerbDeploy, mustJSON(t, req))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	body := e.fake.body(http.MethodPost, "/containers/create")
	var got struct {
		Healthcheck *struct {
			Test        []string
			Interval    int64
			Timeout     int64
			Retries     int
			StartPeriod int64
		}
	}
	if json.Unmarshal(body, &got) != nil || got.Healthcheck == nil {
		t.Fatal(string(body))
	}
	hc := got.Healthcheck
	if strings.Join(hc.Test, " ") != "CMD wget -q http://127.0.0.1/" {
		t.Fatalf("test %v", hc.Test)
	}
	if hc.Interval != 30*1e9 || hc.Timeout != 5*1e9 || hc.Retries != 3 || hc.StartPeriod != 0 {
		t.Fatalf("%+v", hc)
	}
	if !strings.Contains(string(body), `"StartPeriod":0`) {
		t.Fatal(string(body))
	}
}

func TestCreateWithoutHealthcheckOmitsKey(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	raw := e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	body := e.fake.body(http.MethodPost, "/containers/create")
	if strings.Contains(string(body), "Healthcheck") {
		t.Fatal(string(body))
	}
}

func TestJobHealthcheckRefused(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	req := jobDeploy()
	req.Spec.Healthcheck = &images.Healthcheck{
		Test: []string{"echo", "secret-argv"}, IntervalSec: 5, TimeoutSec: 1, Retries: 1,
	}
	raw := e.call(images.VerbDeploy, mustJSON(t, req))
	if imgCode(t, raw) != "bad_spec" {
		t.Fatal(string(raw))
	}
	if strings.Contains(string(raw), "secret-argv") {
		t.Fatal(string(raw))
	}
	if e.fake.body(http.MethodPost, "/containers/create") != nil {
		t.Fatal("job with healthcheck created a container")
	}
}
