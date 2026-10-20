package run

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/probe"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

var checkIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

func (a *agent) apply(cfg protocol.Config, services []protocol.Discovered) {
	clamped, extra := protocol.ClampConfig(cfg)
	for _, id := range extra {
		a.logDrop(id)
	}
	a.metricsEvery = time.Duration(clamped.MetricsIntervalSec) * time.Second
	a.procEvery = time.Duration(clamped.ProcessIntervalSec) * time.Second
	a.healthEvery = 0
	if p := a.opt.Pace; p != nil {
		if d := time.Duration(p.Metrics.Load()); d > 0 {
			a.metricsEvery = d
		}
		if d := time.Duration(p.Process.Load()); d > 0 {
			a.procEvery = d
		}
		if d := time.Duration(p.Health.Load()); d > 0 {
			a.healthEvery = d
		}
	}
	a.configured = append([]protocol.HealthCheck(nil), clamped.HealthChecks...)
	a.mu.Lock()
	a.dockerHealth = clamped.DockerHealth
	if !a.dockerHealth {
		a.dockerPending = nil
	}
	a.mu.Unlock()
	a.setActive(services)
}

func (a *agent) refilter(ctx context.Context) {
	a.mu.Lock()
	svcs := append([]protocol.Discovered(nil), a.services...)
	a.mu.Unlock()
	a.setActive(svcs)
	a.startHealth(ctx)
}

func (a *agent) setActive(services []protocol.Discovered) {
	keep, dropped := protocol.FilterHealthChecks(a.configured, services)
	valid := make([]protocol.HealthCheck, 0, len(keep))
	for _, c := range keep {
		if !probe.ValidPath(c.Path) {
			dropped = append(dropped, c.ID)
			continue
		}
		valid = append(valid, c)
	}
	for _, id := range dropped {
		a.logDrop(id)
	}
	a.mu.Lock()
	a.checks = valid
	a.mu.Unlock()
}

func (a *agent) logDrop(id string) {
	if !checkIDPattern.MatchString(id) {
		id = "invalid"
	}
	a.opt.Log.Printf("bot-agent: drop health check id=%s", quoteLog(id))
}

func (a *agent) startHealth(ctx context.Context) {
	if a.healthCancel != nil {
		a.healthCancel()
	}
	hctx, cancel := context.WithCancel(ctx)
	a.healthCancel = cancel
	a.mu.Lock()
	checks := append([]protocol.HealthCheck(nil), a.checks...)
	a.mu.Unlock()
	every := a.healthEvery
	for _, chk := range checks {
		go a.probeLoop(hctx, chk, every)
	}
}

func (a *agent) probeLoop(ctx context.Context, chk protocol.HealthCheck, every time.Duration) {
	interval := time.Duration(chk.IntervalSec) * time.Second
	if every > 0 {
		interval = every
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			res := probe.Do(ctx, probe.Spec{
				Protocol: chk.Protocol, Port: chk.Port, Path: chk.Path,
				ExpectMin: chk.ExpectStatusMin, ExpectMax: chk.ExpectStatusMax,
				Timeout: 10 * time.Second,
			})
			sample := healthSample(chk, res, a.opt.Now())
			select {
			case a.healthCh <- sample:
			case <-ctx.Done():
				return
			}
			timer.Reset(interval)
		}
	}
}

func healthSample(chk protocol.HealthCheck, res probe.Result, at time.Time) protocol.Sample {
	var status any
	if res.HTTPStatus != nil {
		status = *res.HTTPStatus
	}
	return protocol.NewSample("health", at, map[string]string{
		"service":  chk.Service,
		"check_id": chk.ID,
		"protocol": chk.Protocol,
		"port":     strconv.Itoa(chk.Port),
	}, map[string]any{
		"result":      res.Result,
		"http_status": status,
		"latency_ms":  res.LatencyMS,
	})
}
