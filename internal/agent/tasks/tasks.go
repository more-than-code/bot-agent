// Package tasks applies the five phase-1 checks and runs the ones that pass.
package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/collect"
	"github.com/more-than-code/bot-agent/internal/agent/probe"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// ProbeSpec is a validated probe_health spec.
type ProbeSpec struct {
	Protocol  string
	Port      int
	Path      *string
	ExpectMin int
	ExpectMax int
}

// Decide runs the contract checks in order. An empty code means the task may run.
func Decide(task protocol.Task, hostID string, now time.Time, services []protocol.Discovered) (string, ProbeSpec) {
	if task.HostID != hostID {
		return "wrong_host", ProbeSpec{}
	}
	exp, err := protocol.ParseTime(task.ExpiresAt)
	if err != nil || !now.Before(exp) {
		return "expired", ProbeSpec{}
	}
	if task.Kind != "host_snapshot" && task.Kind != "probe_health" {
		return "forbidden", ProbeSpec{}
	}
	spec, err := parseSpec(task)
	if err != nil {
		return "bad_spec", ProbeSpec{}
	}
	if task.Kind == "probe_health" {
		name := ""
		if task.Service != nil {
			name = *task.Service
		}
		svc, ok := findService(services, name)
		if name == "" || !ok {
			return "unknown_service", ProbeSpec{}
		}
		if !containsPort(svc.Ports, spec.Port) {
			return "forbidden", ProbeSpec{}
		}
	}
	return "", spec
}

func findService(services []protocol.Discovered, name string) (protocol.Discovered, bool) {
	for _, s := range services {
		if s.Name == name {
			return s, true
		}
	}
	return protocol.Discovered{}, false
}

func containsPort(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

func parseSpec(task protocol.Task) (ProbeSpec, error) {
	if len(task.Spec) == 0 {
		return ProbeSpec{}, errors.New("spec")
	}
	switch task.Kind {
	case "host_snapshot":
		dec := json.NewDecoder(bytes.NewReader(task.Spec))
		dec.DisallowUnknownFields()
		var empty struct{}
		if err := dec.Decode(&empty); err != nil {
			return ProbeSpec{}, err
		}
		return ProbeSpec{}, nil
	case "probe_health":
		return parseProbeSpec(task.Spec)
	default:
		return ProbeSpec{}, errors.New("kind")
	}
}

type rawProbe struct {
	Protocol        string  `json:"protocol"`
	Port            int     `json:"port"`
	Path            *string `json:"path"`
	ExpectStatusMin int     `json:"expect_status_min"`
	ExpectStatusMax int     `json:"expect_status_max"`
}

func parseProbeSpec(raw json.RawMessage) (ProbeSpec, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in rawProbe
	if err := dec.Decode(&in); err != nil {
		return ProbeSpec{}, err
	}
	if in.Protocol != "http" && in.Protocol != "tcp" {
		return ProbeSpec{}, errors.New("protocol")
	}
	if in.Port < 1 || in.Port > 65535 {
		return ProbeSpec{}, errors.New("port")
	}
	if in.ExpectStatusMin < 100 || in.ExpectStatusMax > 599 || in.ExpectStatusMin > in.ExpectStatusMax {
		return ProbeSpec{}, errors.New("status")
	}
	if in.Path != nil && !probe.ValidPath(*in.Path) {
		return ProbeSpec{}, errors.New("path")
	}
	return ProbeSpec{
		Protocol: in.Protocol, Port: in.Port, Path: in.Path,
		ExpectMin: in.ExpectStatusMin, ExpectMax: in.ExpectStatusMax,
	}, nil
}

// Env is what a task is allowed to touch.
type Env struct {
	Now     time.Time
	Host    collect.HostView
	Procs   []collect.ProcView
	Probe   func(context.Context, probe.Spec) probe.Result
	Timeout time.Duration
}

// Run executes an already-accepted task.
func Run(ctx context.Context, task protocol.Task, spec ProbeSpec, env Env) protocol.TaskResult {
	if ctx.Err() != nil {
		return failed(task.TaskID, "timed_out", "timed_out")
	}
	switch task.Kind {
	case "host_snapshot":
		return protocol.TaskResult{
			V: version(), Type: "task_result", TaskID: task.TaskID,
			State: "succeeded", Payload: snapshotPayload(env),
		}
	case "probe_health":
		return runProbe(ctx, task, spec, env)
	default:
		return failed(task.TaskID, "failed", "forbidden")
	}
}

func runProbe(ctx context.Context, task protocol.Task, spec ProbeSpec, env Env) protocol.TaskResult {
	timeout := env.Timeout
	if timeout <= 0 {
		timeout = time.Duration(task.TimeoutSec) * time.Second
	}
	if timeout <= 0 || timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	path := ""
	if spec.Path != nil {
		path = *spec.Path
	}
	do := env.Probe
	if do == nil {
		do = probe.Do
	}
	res := do(ctx, probe.Spec{
		Protocol: spec.Protocol, Port: spec.Port, Path: path,
		ExpectMin: spec.ExpectMin, ExpectMax: spec.ExpectMax, Timeout: timeout,
	})
	if ctx.Err() != nil && res.Result == "" {
		return failed(task.TaskID, "timed_out", "timed_out")
	}
	service := ""
	if task.Service != nil {
		service = *task.Service
	}
	var pathVal any
	if spec.Path == nil {
		pathVal = nil
	} else {
		pathVal = *spec.Path
	}
	var status any
	if res.HTTPStatus == nil {
		status = nil
	} else {
		status = *res.HTTPStatus
	}
	return protocol.TaskResult{
		V: version(), Type: "task_result", TaskID: task.TaskID, State: "succeeded",
		Payload: map[string]any{
			"service": service, "protocol": spec.Protocol, "port": spec.Port,
			"path": pathVal, "result": res.Result, "http_status": status,
			"latency_ms": res.LatencyMS, "checked_at": protocol.FormatTime(env.Now),
		},
	}
}

func snapshotPayload(env Env) map[string]any {
	cpu := 0.0
	if env.Host.CPUPercent != nil {
		cpu = *env.Host.CPUPercent
	}
	var temp any
	if env.Host.TempC == nil {
		temp = nil
	} else {
		temp = *env.Host.TempC
	}
	disks := make([]any, 0, len(env.Host.Disks))
	for _, d := range env.Host.Disks {
		disks = append(disks, map[string]any{
			"path": d.Path, "total": d.Total, "used": d.Used, "used_percent": d.UsedPercent,
		})
	}
	top := topProcs(env.Procs)
	rows := make([]any, 0, len(top))
	for _, p := range top {
		cpuUsage := 0.0
		if p.CPU != nil {
			cpuUsage = *p.CPU
		}
		rows = append(rows, map[string]any{
			"pid": p.PID, "process_name": p.Name, "user": p.User,
			"cmdline": p.Cmdline, "cpu_usage": cpuUsage, "memory_rss": p.RSS,
		})
	}
	return map[string]any{
		"sampled_at":  protocol.FormatTime(env.Now),
		"cpu_percent": cpu,
		"mem": map[string]any{
			"total": env.Host.MemTotal, "used": env.Host.MemUsed,
			"available": env.Host.MemAvailable, "used_percent": env.Host.MemUsedPercent,
		},
		"disks":        disks,
		"load":         []any{env.Host.Load[0], env.Host.Load[1], env.Host.Load[2]},
		"temp_celsius": temp,
		"uptime_sec":   env.Host.Uptime,
		"top":          rows,
	}
}

func topProcs(all []collect.ProcView) []collect.ProcView {
	chosen := collect.Select(all)
	sort.SliceStable(chosen, func(i, j int) bool {
		ai, bi := -1.0, -1.0
		if chosen[i].CPU != nil {
			ai = *chosen[i].CPU
		}
		if chosen[j].CPU != nil {
			bi = *chosen[j].CPU
		}
		if ai != bi {
			return ai > bi
		}
		if chosen[i].RSS != chosen[j].RSS {
			return chosen[i].RSS > chosen[j].RSS
		}
		return chosen[i].PID < chosen[j].PID
	})
	if len(chosen) > 20 {
		return chosen[:20]
	}
	return chosen
}

func failed(id, state, code string) protocol.TaskResult {
	return protocol.TaskResult{
		V: version(), Type: "task_result", TaskID: id, State: state,
		ErrorCode: protocol.Str(code), Payload: map[string]any{},
	}
}

func version() int { return protocol.Version }
