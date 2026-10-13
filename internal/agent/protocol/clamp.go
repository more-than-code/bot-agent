package protocol

import "slices"

const (
	metricsMin = 5
	metricsMax = 60
	metricsDef = 10

	processMin = 30
	processMax = 300
	processDef = 60

	healthMin = 30
	healthMax = 300
	healthDef = 60

	maxChecks = 50
)

// ClampConfig applies the contract ranges. Zero means "use the default".
// Health checks beyond 50 are returned in dropped so the caller can log ids.
func ClampConfig(in Config) (Config, []string) {
	out := in
	out.MetricsIntervalSec = clamp(in.MetricsIntervalSec, metricsMin, metricsMax, metricsDef)
	out.ProcessIntervalSec = clamp(in.ProcessIntervalSec, processMin, processMax, processDef)
	var dropped []string
	if len(out.HealthChecks) > maxChecks {
		for _, c := range out.HealthChecks[maxChecks:] {
			dropped = append(dropped, c.ID)
		}
		out.HealthChecks = append([]HealthCheck(nil), out.HealthChecks[:maxChecks]...)
	} else {
		out.HealthChecks = append([]HealthCheck(nil), out.HealthChecks...)
	}
	for i := range out.HealthChecks {
		out.HealthChecks[i].IntervalSec = clamp(out.HealthChecks[i].IntervalSec, healthMin, healthMax, healthDef)
	}
	return out, dropped
}

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// FilterHealthChecks drops checks whose protocol is not http or tcp, whose
// service is not in the discovered set, or whose port is not one of that
// service's ports. The returned ids are the ones the caller logs.
func FilterHealthChecks(checks []HealthCheck, services []Discovered) (keep []HealthCheck, dropped []string) {
	byName := make(map[string]Discovered, len(services))
	for _, s := range services {
		byName[s.Name] = s
	}
	for _, c := range checks {
		svc, ok := byName[c.Service]
		if !ok || (c.Protocol != "http" && c.Protocol != "tcp") || !slices.Contains(svc.Ports, c.Port) {
			dropped = append(dropped, c.ID)
			continue
		}
		keep = append(keep, c)
	}
	return keep, dropped
}

// Heartbeat clamps a server heartbeat to a range the agent can live with.
// The contract example is 20. Out-of-range values fall back to that.
func Heartbeat(sec int) int {
	if sec < 1 || sec > 120 {
		return 20
	}
	return sec
}
