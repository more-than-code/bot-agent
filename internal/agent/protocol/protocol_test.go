package protocol

import (
	"strings"
	"testing"
)

func TestClampAndFilter(t *testing.T) {
	in := Config{
		MetricsIntervalSec: 1,
		ProcessIntervalSec: 1000,
		HealthChecks:       make([]HealthCheck, 51),
	}
	in.HealthChecks[0] = HealthCheck{ID: "ok", Service: "nginx", Protocol: "http", Port: 80, IntervalSec: 10, ExpectStatusMin: 200, ExpectStatusMax: 299}
	in.HealthChecks[1] = HealthCheck{ID: "udp", Service: "nginx", Protocol: "udp", Port: 80, IntervalSec: 0}
	in.HealthChecks[2] = HealthCheck{ID: "missing", Service: "gone", Protocol: "tcp", Port: 80}
	in.HealthChecks[3] = HealthCheck{ID: "port", Service: "nginx", Protocol: "tcp", Port: 22}
	in.HealthChecks[50] = HealthCheck{ID: "extra"}
	out, dropped := ClampConfig(in)
	if out.MetricsIntervalSec != 5 || out.ProcessIntervalSec != 300 {
		t.Fatalf("intervals %d %d", out.MetricsIntervalSec, out.ProcessIntervalSec)
	}
	if len(out.HealthChecks) != 50 || len(dropped) != 1 || dropped[0] != "extra" {
		t.Fatalf("cap %d %v", len(out.HealthChecks), dropped)
	}
	if out.HealthChecks[0].IntervalSec != 30 || out.HealthChecks[1].IntervalSec != 60 {
		t.Fatalf("health intervals %+v", out.HealthChecks[:2])
	}
	def, _ := ClampConfig(Config{})
	if def.MetricsIntervalSec != 10 || def.ProcessIntervalSec != 60 {
		t.Fatalf("defaults %+v", def)
	}
	keep, drop := FilterHealthChecks(out.HealthChecks, []Discovered{{Name: "nginx", Ports: []int{80}}})
	if len(keep) != 1 || keep[0].ID != "ok" {
		t.Fatalf("keep %+v", keep)
	}
	gotDrop := map[string]bool{}
	for _, id := range drop {
		gotDrop[id] = true
	}
	for _, id := range []string{"udp", "missing", "port"} {
		if !gotDrop[id] {
			t.Fatalf("missing drop %s in %v", id, drop)
		}
	}
	if Heartbeat(0) != 20 || Heartbeat(20) != 20 || Heartbeat(120) != 120 || Heartbeat(121) != 20 {
		t.Fatal("heartbeat")
	}
}

func TestValidateMetrics(t *testing.T) {
	ts := "2026-10-13T12:00:00Z"
	good := `{"v":1,"type":"metrics","batch_id":"11111111-1111-4111-8111-111111111111","samples":[` +
		`{"m":"cpu","t":"` + ts + `","tags":{"cpu":"cpu-total"},"f":{"usage_user":10,"usage_system":5,"usage_idle":80,"usage_iowait":5}},` +
		`{"m":"net","t":"` + ts + `","tags":{"interface":"eth0"},"f":{"bytes_recv":1,"bytes_sent":2}},` +
		`{"m":"process","t":"` + ts + `","tags":{"pid":"1","process_name":"init","user":"root","cmdline":"init"},"f":{"memory_rss":1,"memory_usage":0,"num_threads":1,"started_at":"` + ts + `"}}` +
		`]}`
	if err := ValidateAgent([]byte(good)); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		strings.Replace(good, `"interface":"eth0"`, `"interface":"lo"`, 1),
		strings.Replace(good, `"usage_iowait":5`, `"usage_iowait":5,"usage_guest":1`, 1),
		strings.Replace(good, `"m":"cpu"`, `"m":"foo"`, 1),
		`{"v":1,"type":"metrics","batch_id":"not-a-uuid","samples":[]}`,
	}
	for _, raw := range bad {
		if err := ValidateAgent([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	long := strings.Repeat("a", CmdlineMax+1)
	proc := `{"v":1,"type":"metrics","batch_id":"11111111-1111-4111-8111-111111111111","samples":[{"m":"process","t":"` + ts + `","tags":{"pid":"1","process_name":"init","user":"root","cmdline":"` + long + `"},"f":{"memory_rss":1,"memory_usage":0,"num_threads":1,"started_at":"` + ts + `"}}]}`
	if err := ValidateAgent([]byte(proc)); err == nil {
		t.Fatal("accepted long cmdline")
	}
	health := `{"v":1,"type":"metrics","batch_id":"11111111-1111-4111-8111-111111111111","samples":[{"m":"health","t":"` + ts + `","tags":{"service":"nginx","check_id":"live","protocol":"http","port":"8080"},"f":{"result":"success","http_status":200,"latency_ms":1.5}}]}`
	if err := ValidateAgent([]byte(health)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgent([]byte(strings.Replace(health, `"port":"8080"`, `"port":8080`, 1))); err == nil {
		t.Fatal("accepted numeric port tag")
	}
}

func TestValidateHelloAndReject(t *testing.T) {
	hello := `{"v":1,"type":"hello","host_id":"11111111-1111-4111-8111-111111111111","credential_id":"22222222-2222-4222-8222-222222222222","agent_version":"0.1.0","os":"linux","arch":"arm64","capabilities":["metrics","probe"],"services":[{"name":"nginx","kind":"systemd","ports":[80],"systemd_unit":"nginx.service","systemd_scope":"system","container_name":null,"process_name":"nginx","pid":1}]}`
	if err := ValidateAgent([]byte(hello)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgent([]byte(strings.Replace(hello, `["metrics","probe"]`, `["probe","metrics"]`, 1))); err == nil {
		t.Fatal("capability order")
	}
	rej := `{"v":1,"type":"task_reject","task_id":"t1","error_code":"wrong_host"}`
	if err := ValidateAgent([]byte(rej)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgent([]byte(strings.Replace(rej, "wrong_host", "busy", 1))); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgent([]byte(strings.Replace(rej, "wrong_host", "nope", 1))); err == nil {
		t.Fatal("bad code")
	}
	snap := []byte(`{"sampled_at":"2026-10-13T12:00:00Z","cpu_percent":0,"mem":{"total":1,"used":1,"available":0,"used_percent":100},"disks":[],"load":[0,0,0],"temp_celsius":null,"uptime_sec":1,"top":[{"pid":1,"process_name":"init","user":"root","cmdline":"init","cpu_usage":0,"memory_rss":1}]}`)
	if err := ValidateSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	probe := []byte(`{"service":"nginx","protocol":"http","port":80,"path":"/","result":"success","http_status":200,"latency_ms":1,"checked_at":"2026-10-13T12:00:00Z"}`)
	if err := ValidateProbeResult(probe); err != nil {
		t.Fatal(err)
	}
}
