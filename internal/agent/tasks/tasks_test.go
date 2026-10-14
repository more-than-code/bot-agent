package tasks

import (
	"context"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/collect"
	"github.com/more-than-code/bot-agent/internal/agent/probe"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestDecideOrder(t *testing.T) {
	host := "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	live := protocol.FormatTime(now.Add(time.Minute))
	services := []protocol.Discovered{{Name: "nginx", Kind: "systemd", Ports: []int{80, 9}}}
	cases := []struct {
		name string
		task protocol.Task
		want string
	}{
		{"wrong host beats later checks", task("other", "host_snapshot", nil, "not-a-time", []byte(`{"x":1}`)), "wrong_host"},
		{"expired beats kind", task(host, "logs", nil, protocol.FormatTime(now), []byte(`{}`)), "expired"},
		{"equal expiry", task(host, "host_snapshot", nil, protocol.FormatTime(now), []byte(`{}`)), "expired"},
		{"bad expiry", task(host, "host_snapshot", nil, "yesterday", []byte(`{}`)), "expired"},
		{"kind", task(host, "logs", nil, live, []byte(`{}`)), "forbidden"},
		{"bad spec beats service", task(host, "probe_health", protocol.Str("missing"), live, []byte(`{"protocol":"http","port":80,"path":"../x","expect_status_min":200,"expect_status_max":299}`)), "bad_spec"},
		{"ssrf path", task(host, "probe_health", protocol.Str("nginx"), live, []byte(`{"protocol":"http","port":80,"path":"@evil.example/","expect_status_min":200,"expect_status_max":299}`)), "bad_spec"},
		{"extra snapshot key", task(host, "host_snapshot", nil, live, []byte(`{"x":1}`)), "bad_spec"},
		{"unknown service", task(host, "probe_health", protocol.Str("missing"), live, probeSpec(80)), "unknown_service"},
		{"null service", task(host, "probe_health", nil, live, probeSpec(80)), "unknown_service"},
		{"forbidden port", task(host, "probe_health", protocol.Str("nginx"), live, probeSpec(22)), "forbidden"},
		{"ok snapshot", task(host, "host_snapshot", nil, live, []byte(`{}`)), ""},
		{"ok probe", task(host, "probe_health", protocol.Str("nginx"), live, probeSpec(80)), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := Decide(tc.task, host, now, services)
			if code != tc.want {
				t.Fatalf("got %s", code)
			}
		})
	}
}

func TestSnapshotAndProbe(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	cpu := 3.5
	procs := make([]collect.ProcView, 25)
	for i := range procs {
		c := float64(i)
		procs[i] = collect.ProcView{PID: i + 1, Name: "p", User: "u", Cmdline: "c", CPU: &c, RSS: int64(i)}
	}
	env := Env{
		Now: now,
		Host: collect.HostView{
			CPUPercent: &cpu, MemTotal: 10, MemUsed: 4, MemAvailable: 6, MemUsedPercent: 40,
			Disks: []collect.DiskView{{Path: "/", Total: 1000 * 4096, Used: 600 * 4096, UsedPercent: 600.0 / 950.0 * 100}},
			Load:  [3]float64{1, 0.5, 0.25}, Uptime: 3600,
		},
		Procs: procs,
		Probe: func(context.Context, probe.Spec) probe.Result {
			return probe.Result{Result: "connection_refused"}
		},
	}
	snapTask := task("11111111-1111-4111-8111-111111111111", "host_snapshot", nil, protocol.FormatTime(now.Add(time.Minute)), []byte(`{}`))
	res := Run(context.Background(), snapTask, ProbeSpec{}, env)
	if res.State != "succeeded" || res.ErrorCode != nil {
		t.Fatalf("%+v", res)
	}
	raw, err := protocol.Marshal(res.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	disks := res.Payload["disks"].([]any)
	if len(disks) != 1 {
		t.Fatalf("disks %d", len(disks))
	}
	d0 := disks[0].(map[string]any)
	if d0["path"] != "/" || d0["total"] != uint64(1000*4096) || d0["used"] != uint64(600*4096) {
		t.Fatalf("disk %+v", d0)
	}
	if d0["used_percent"] != 600.0/950.0*100 {
		t.Fatalf("disk percent %v", d0["used_percent"])
	}
	top := res.Payload["top"].([]any)
	if len(top) != 20 {
		t.Fatalf("top %d", len(top))
	}
	first := top[0].(map[string]any)
	if first["pid"] != 25 {
		t.Fatalf("first pid %v", first["pid"])
	}
	p := probeTask(80)
	pref := Run(context.Background(), p, ProbeSpec{Protocol: "tcp", Port: 9}, env)
	if pref.State != "succeeded" || pref.Payload["result"] != "connection_refused" || pref.Payload["http_status"] != nil {
		t.Fatalf("%+v", pref.Payload)
	}
	praw, err := protocol.Marshal(pref.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateProbeResult(praw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	timed := Run(ctx, snapTask, ProbeSpec{}, env)
	if timed.State != "timed_out" || timed.ErrorCode == nil || *timed.ErrorCode != "timed_out" {
		t.Fatalf("%+v", timed)
	}
}

func task(host, kind string, service *string, exp string, spec []byte) protocol.Task {
	return protocol.Task{
		V: protocol.Version, Type: "task", TaskID: "task-1", HostID: host, Kind: kind,
		Service: service, ExpiresAt: exp, TimeoutSec: 15, Spec: spec,
	}
}

func probeTask(port int) protocol.Task {
	return task("11111111-1111-4111-8111-111111111111", "probe_health", protocol.Str("nginx"),
		protocol.FormatTime(time.Date(2026, 10, 13, 12, 1, 0, 0, time.UTC)), probeSpec(port))
}

func probeSpec(port int) []byte {
	return []byte(`{"protocol":"tcp","port":` + itoa(port) + `,"path":null,"expect_status_min":200,"expect_status_max":299}`)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
