package run

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestDockerHealthSamples(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	idPath, host := writeMachineID(t)
	healthy, unhealthy, starting := "healthy", "unhealthy", "starting"
	other := "none"
	items := []images.ContainerInfo{
		{
			ID: "1", Name: "acme-web", State: "running", Health: &healthy,
			Labels: ownedTestLabels(host, "acme", "web"),
		},
		{
			ID: "2", Name: "acme-web-prev", State: "exited", Health: &unhealthy,
			Labels: ownedTestLabels(host, "acme", "web"),
		},
		{ID: "3", Name: "cache", State: "running", Health: &unhealthy},
		{ID: "4", Name: "queue", State: "running", Health: &starting},
		{ID: "5", Name: "idle", State: "running"},
		{ID: "6", Name: "odd", State: "running", Health: &other},
	}
	h := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case images.VerbList:
			return images.Encode(images.ListResponse{Items: items}), nil
		default:
			return nil, errHelper
		}
	}}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	a := &agent{
		opt: Options{
			ProcRoot: proc, Passwd: passwd, Helper: h, MachineID: idPath,
			Log: stdLog(&bytes.Buffer{}), Now: func() time.Time { return now },
		},
		policy: policy.Policy{
			Exists: true, Valid: true, Enabled: true,
			Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
		},
	}
	if _, complete := a.discover(context.Background()); !complete {
		t.Fatal("incomplete")
	}
	if got := a.takeDockerSamples(); len(got) != 0 {
		t.Fatalf("config false emitted %d", len(got))
	}

	a.apply(protocol.Config{DockerHealth: true, MetricsIntervalSec: 10, ProcessIntervalSec: 60}, nil)
	if _, complete := a.discover(context.Background()); !complete {
		t.Fatal("incomplete")
	}
	got := a.takeDockerSamples()
	if len(got) != 2 || got[0].Tags["service"] != "cache" || got[1].Tags["service"] != "web" {
		t.Fatalf("%+v", got)
	}
	wantResult := []string{"unhealthy", "success"}
	for i, s := range got {
		if s.M != "health" || s.T != protocol.FormatTime(now) {
			t.Fatalf("%+v", s)
		}
		if len(s.Tags) != 2 || s.Tags["protocol"] != "docker" || s.F["result"] != wantResult[i] {
			t.Fatalf("%+v", s)
		}
		if _, ok := s.Tags["check_id"]; ok {
			t.Fatal("check_id")
		}
		if _, ok := s.Tags["port"]; ok {
			t.Fatal("port")
		}
		if s.F["http_status"] != nil || s.F["latency_ms"] != nil || len(s.F) != 3 {
			t.Fatalf("%+v", s.F)
		}
	}
	raw, err := protocol.Marshal(protocol.Metrics{
		V: protocol.Version, Type: "metrics",
		BatchID: "11111111-1111-4111-8111-111111111111",
		Samples: got,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateAgent(raw); err != nil {
		t.Fatalf("%v %s", err, raw)
	}
	if strings.Contains(string(raw), "acme-web-prev") || strings.Contains(string(raw), `"queue"`) || strings.Contains(string(raw), `"idle"`) {
		t.Fatal(string(raw))
	}

	a.apply(protocol.Config{MetricsIntervalSec: 10, ProcessIntervalSec: 60}, nil)
	if _, complete := a.discover(context.Background()); !complete {
		t.Fatal("incomplete")
	}
	if again := a.takeDockerSamples(); len(again) != 0 {
		t.Fatalf("cleared flag emitted %d", len(again))
	}
}

func TestDockerHealthCleared(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	idPath, host := writeMachineID(t)
	healthy := "healthy"
	items := []images.ContainerInfo{{
		ID: "1", Name: "acme-web", State: "running", Health: &healthy,
		Labels: ownedTestLabels(host, "acme", "web"),
	}}
	h := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case images.VerbList:
			return images.Encode(images.ListResponse{Items: items}), nil
		default:
			return nil, errHelper
		}
	}}
	a := &agent{
		opt: Options{
			ProcRoot: proc, Passwd: passwd, Helper: h, MachineID: idPath,
			Log: stdLog(&bytes.Buffer{}),
			Now: func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) },
		},
		policy: policy.Policy{
			Exists: true, Valid: true, Enabled: true,
			Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
		},
		healthCh: make(chan protocol.Sample, 1),
	}
	cfgOn := protocol.Config{DockerHealth: true, MetricsIntervalSec: 10, ProcessIntervalSec: 60}
	queue := func() {
		t.Helper()
		a.apply(cfgOn, nil)
		if _, complete := a.discover(context.Background()); !complete {
			t.Fatal("incomplete")
		}
		if len(a.dockerPending) == 0 {
			t.Fatal("nothing queued")
		}
	}

	queue()
	raw, err := protocol.Marshal(protocol.ConfigUpdate{
		V: protocol.Version, Type: "config_update",
		Config: protocol.Config{MetricsIntervalSec: 10, ProcessIntervalSec: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.onEvent(context.Background(), nil, incoming{typ: "config_update", raw: raw}); err != nil {
		t.Fatal(err)
	}
	if len(a.dockerPending) != 0 {
		t.Fatalf("config_update left %d", len(a.dockerPending))
	}
	if got := a.takeDockerSamples(); len(got) != 0 {
		t.Fatalf("config_update drained %d", len(got))
	}

	queue()
	a.clearDockerGate()
	if a.dockerHealth || len(a.dockerPending) != 0 {
		t.Fatal("reconnect kept the gate")
	}
	if _, complete := a.discover(context.Background()); !complete {
		t.Fatal("incomplete")
	}
	if got := a.takeDockerSamples(); len(got) != 0 {
		t.Fatalf("reconnect drained %d", len(got))
	}

	a.mu.Lock()
	a.dockerHealth = false
	a.dockerPending = []protocol.Sample{{M: "health"}}
	a.mu.Unlock()
	if got := a.takeDockerSamples(); len(got) != 0 || len(a.dockerPending) != 0 {
		t.Fatalf("flag false drained %d left %d", len(got), len(a.dockerPending))
	}
}
