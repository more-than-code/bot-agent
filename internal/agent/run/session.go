package run

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"time"

	"github.com/coder/websocket"
	"github.com/more-than-code/bot-agent/internal/agent/conn"
	"github.com/more-than-code/bot-agent/internal/agent/discover"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func (a *agent) session(ctx context.Context) error {
	bearer := ""
	if a.cfg.HasCredential() {
		bearer = a.cfg.Credential
	}
	c, err := conn.Dial(ctx, a.cfg.ServerURL, bearer)
	if err != nil {
		return err
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if !a.cfg.HasCredential() {
		return a.enroll(ctx, c)
	}
	return a.steady(ctx, c)
}

func (a *agent) enroll(ctx context.Context, c *websocket.Conn) error {
	frame := protocol.Enroll{
		V: protocol.Version, Type: "enroll", JoinToken: a.cfg.JoinToken,
		Hostname: a.host, AgentVersion: protocol.AgentVersion,
		OS: runtime.GOOS, Arch: runtime.GOARCH, AlwaysOn: a.cfg.AlwaysOn,
	}
	if err := conn.WriteJSON(ctx, c, frame); err != nil {
		return err
	}
	typ, raw, err := conn.ReadFrame(ctx, c)
	if err != nil {
		return err
	}
	switch typ {
	case "error":
		return a.serverError(raw)
	case "enrolled":
		var resp struct {
			HostID       string `json:"host_id"`
			CredentialID string `json:"credential_id"`
			Credential   string `json:"credential"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return errors.New("enrolled frame")
		}
		if err := a.cfg.PersistEnrolled(resp.HostID, resp.CredentialID, resp.Credential); err != nil {
			a.opt.Log.Printf("bot-agent: could not persist enrollment")
			a.cfg.HostID = resp.HostID
			a.cfg.CredentialID = resp.CredentialID
			a.cfg.Credential = resp.Credential
			a.cfg.JoinToken = ""
		}
		return conn.ErrReconnect
	default:
		if typ != "" {
			a.opt.Log.Printf("bot-agent: ignored frame type %s", quoteLog(typ))
		}
		return errors.New("expected enrolled")
	}
}

func (a *agent) serverError(raw []byte) error {
	var e protocol.ServerError
	if err := json.Unmarshal(raw, &e); err != nil || e.ErrorCode == "" {
		return errors.New("server error")
	}
	a.opt.Log.Printf("bot-agent: server error_code=%s", quoteLog(e.ErrorCode))
	switch e.ErrorCode {
	case "forbidden":
		return conn.ErrForbidden
	case "unauthenticated":
		return conn.ErrUnauthenticated
	default:
		return errors.New("server error")
	}
}

func (a *agent) steady(ctx context.Context, c *websocket.Conn) error {
	// This connection has not applied hello_ok. Drop the previous
	// connection's gate so the discovery below cannot queue samples.
	a.clearDockerGate()
	disc, complete := a.discover(ctx)
	services := []protocol.Discovered{}
	if complete {
		services = disc.Services
	}
	programs, caps := a.helloPrograms()
	hello := protocol.Hello{
		V: protocol.Version, Type: "hello",
		HostID: a.cfg.HostID, CredentialID: a.cfg.CredentialID,
		AgentVersion: protocol.AgentVersion,
		OS:           runtime.GOOS, Arch: runtime.GOARCH,
		Capabilities:     caps,
		Programs:         programs,
		Services:         services,
		ServicesComplete: complete,
	}
	w := &writer{c: c, onSend: a.opt.OnSend}
	if err := w.Send(ctx, hello); err != nil {
		return err
	}
	typ, raw, err := conn.ReadFrame(ctx, c)
	if err != nil {
		return err
	}
	if typ == "error" {
		return a.serverError(raw)
	}
	if typ != "hello_ok" {
		if typ != "" {
			a.opt.Log.Printf("bot-agent: ignored frame type %s", quoteLog(typ))
		}
		return errors.New("expected hello_ok")
	}
	var okf protocol.HelloOK
	if err := json.Unmarshal(raw, &okf); err != nil {
		return errors.New("hello_ok")
	}
	if okf.HostID != "" && okf.HostID != a.cfg.HostID {
		return conn.ErrForbidden
	}
	serverNow, err := protocol.ParseTime(okf.ServerTime)
	if err != nil {
		return errors.New("server_time")
	}
	a.offset = serverNow.Sub(a.opt.Now())
	a.bo.Reset()
	a.pingEvery = time.Duration(protocol.Heartbeat(okf.HeartbeatSec)) * time.Second
	filtered := services
	if !complete {
		a.mu.Lock()
		filtered = append([]protocol.Discovered(nil), a.services...)
		a.mu.Unlock()
	}
	a.apply(okf.Config, filtered)
	if complete {
		a.lastCanon = canonical(services)
	} else {
		// This connection has not delivered a set. The agent object survives
		// reconnects, so drop the previous connection's baseline. Otherwise an
		// unchanged complete pass is held until ServicesRefreshEvery.
		a.lastCanon = ""
	}

	sess, cancel := context.WithCancel(ctx)
	defer cancel()
	a.startHealth(sess)

	events := make(chan incoming, 64)
	errCh := make(chan error, 2)
	go readLoop(sess, c, events, errCh)

	if err := w.Send(sess, protocol.Ping{V: protocol.Version, Type: "ping", TS: protocol.FormatTime(a.opt.Now())}); err != nil {
		return err
	}
	if err := a.replay(sess, w); err != nil {
		return err
	}
	a.autoSent = map[string]bool{}
	if err := a.sendAuto(sess, w, a.autoSent); err != nil {
		return err
	}
	return a.loop(sess, w, events, errCh)
}

type incoming struct {
	typ string
	raw []byte
}

func readLoop(ctx context.Context, c *websocket.Conn, events chan<- incoming, errCh chan<- error) {
	for {
		typ, raw, err := conn.ReadFrame(ctx, c)
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
		select {
		case events <- incoming{typ: typ, raw: raw}:
		case <-ctx.Done():
			return
		}
	}
}

func (a *agent) loop(ctx context.Context, w *writer, events <-chan incoming, errCh <-chan error) error {
	now := a.opt.Now()
	a.lastServicesSent = now
	nextHost := now
	nextProc := now
	nextDisc := now.Add(a.opt.DiscoverEvery)
	nextPing := now.Add(a.pingEvery)
	var health []protocol.Sample
	for {
		wait := earliest(a.opt.Now(), nextHost, nextProc, nextDisc, nextPing)
		tick, stop := a.arm(wait)
		select {
		case <-ctx.Done():
			stop()
			return ctx.Err()
		case err := <-errCh:
			stop()
			return err
		case ev := <-events:
			stop()
			changed, err := a.onEvent(ctx, w, ev)
			if err != nil {
				return err
			}
			if changed {
				now := a.opt.Now()
				if soon := now.Add(a.metricsEvery); nextHost.After(soon) {
					nextHost = soon
				}
				if soon := now.Add(a.procEvery); nextProc.After(soon) {
					nextProc = soon
				}
			}
		case <-a.autoq.wake():
			stop()
			if a.autoSent == nil {
				a.autoSent = map[string]bool{}
			}
			if err := a.sendAuto(ctx, w, a.autoSent); err != nil {
				return err
			}
		case s := <-a.healthCh:
			stop()
			if len(health) < 1000 {
				health = append(health, s)
			}
		case <-tick:
			now = a.opt.Now()
			if !now.Before(nextPing) {
				if err := w.Send(ctx, protocol.Ping{V: protocol.Version, Type: "ping", TS: protocol.FormatTime(now)}); err != nil {
					return err
				}
				nextPing = now.Add(a.pingEvery)
			}
			if !now.Before(nextDisc) {
				if err := a.maybeServices(ctx, w, now); err != nil {
					return err
				}
				nextDisc = now.Add(a.opt.DiscoverEvery)
			}
			hostDue := !now.Before(nextHost)
			procDue := !now.Before(nextProc)
			if hostDue || procDue {
				if err := a.sendMetrics(ctx, w, hostDue, procDue, &health); err != nil {
					return err
				}
				if hostDue {
					nextHost = now.Add(a.metricsEvery)
				}
				if procDue {
					nextProc = now.Add(a.procEvery)
				}
			}
		}
	}
}

func (a *agent) arm(d time.Duration) (<-chan time.Time, func()) {
	if a.opt.After != nil {
		return a.opt.After(d), func() {}
	}
	timer := time.NewTimer(d)
	return timer.C, func() { timer.Stop() }
}

func earliest(now time.Time, times ...time.Time) time.Duration {
	next := times[0]
	for _, t := range times[1:] {
		if t.Before(next) {
			next = t
		}
	}
	d := next.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

func (a *agent) onEvent(ctx context.Context, w *writer, ev incoming) (bool, error) {
	switch ev.typ {
	case "pong":
		return false, nil
	case "metrics_ack":
		var ack protocol.MetricsAck
		if err := json.Unmarshal(ev.raw, &ack); err != nil || ack.BatchID == "" {
			a.opt.Log.Printf("bot-agent: ignored invalid frame")
			return false, nil
		}
		if err := a.spool.Delete(ack.BatchID); err != nil {
			a.opt.Log.Printf("bot-agent: spool delete")
		}
		return false, nil
	case "task_ack":
		var ack protocol.TaskAck
		if err := json.Unmarshal(ev.raw, &ack); err != nil || ack.TaskID == "" {
			a.opt.Log.Printf("bot-agent: ignored invalid frame")
			return false, nil
		}
		a.pending.ack(ack.TaskID)
		return false, nil
	case "auto_action_ack":
		a.onAutoAck(ev.raw)
		return false, nil
	case "task":
		a.handleTask(ctx, w, ev.raw)
		return false, nil
	case "config_update":
		var up protocol.ConfigUpdate
		if err := json.Unmarshal(ev.raw, &up); err != nil {
			a.opt.Log.Printf("bot-agent: ignored invalid frame")
			return false, nil
		}
		a.mu.Lock()
		svcs := append([]protocol.Discovered(nil), a.services...)
		a.mu.Unlock()
		a.apply(up.Config, svcs)
		a.startHealth(ctx)
		return true, nil
	case "error":
		return false, a.serverError(ev.raw)
	default:
		if ev.typ != "" {
			a.opt.Log.Printf("bot-agent: ignored frame type %s", quoteLog(ev.typ))
		} else {
			a.opt.Log.Printf("bot-agent: ignored invalid frame")
		}
		return false, nil
	}
}

func (a *agent) replay(ctx context.Context, w *writer) error {
	for _, res := range a.pending.list() {
		if err := w.Send(ctx, res); err != nil {
			return err
		}
	}
	items, err := a.spool.List()
	if err != nil {
		return err
	}
	for _, it := range items {
		if err := w.SendRaw(ctx, it.Frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *agent) sendMetrics(ctx context.Context, w *writer, host, procs bool, health *[]protocol.Sample) error {
	now := a.opt.Now()
	var samples []protocol.Sample
	if host {
		hs, _, err := a.col.SampleHost(now)
		if err != nil {
			a.opt.Log.Printf("bot-agent: collect host")
		}
		samples = append(samples, hs...)
	}
	if procs {
		ps, _, err := a.col.SampleProcs(now, a.serviceFor)
		if err != nil {
			a.opt.Log.Printf("bot-agent: collect processes")
		}
		samples = append(samples, ps...)
	}
	if health != nil && len(*health) > 0 {
		samples = append(samples, *health...)
		*health = nil
	}
	if extra := a.takeDockerSamples(); len(extra) > 0 {
		samples = append(samples, extra...)
	}
	return a.emitMetrics(ctx, w.SendRaw, samples, now)
}

func (a *agent) discover(ctx context.Context) (discover.Result, bool) {
	res, err := discover.Scan(a.opt.ProcRoot, a.opt.Passwd)
	complete := err == nil
	if err != nil {
		a.opt.Log.Printf("bot-agent: discovery failed")
		res = discover.Result{}
	}
	if a.policy.Valid && a.policy.Enabled && a.opt.Helper != nil {
		hctx, cancel := context.WithTimeout(ctx, discoverHelperTimeout)
		owned, err := a.ownedPrograms(hctx)
		cancel()
		if err != nil {
			complete = false
			if a.ownedOnce.CompareAndSwap(false, true) {
				a.opt.Log.Printf("bot-agent: program discovery failed")
			}
		} else {
			res = discover.ApplyOwned(res, owned)
		}
	}
	if a.policy.ImagesEnabled() {
		items, truncated, err := a.listContainers(ctx)
		a.noteDocker(err == nil)
		if err != nil {
			complete = false
			if a.containerOnce.CompareAndSwap(false, true) {
				a.opt.Log.Printf("bot-agent: container discovery failed")
			}
		} else {
			if truncated && a.listCutOnce.CompareAndSwap(false, true) {
				a.opt.Log.Printf("bot-agent: container list truncated")
			}
			if complete {
				res = discover.ApplyContainers(res, items, a.machineHost(), a.policy.Projects)
			}
		}
	}
	if a.unmappedOnce.CompareAndSwap(false, true) {
		a.opt.Log.Printf("bot-agent: unmapped listeners: %d", res.Unmapped)
	}
	if res.Services == nil {
		res.Services = []protocol.Discovered{}
	}
	// An incomplete pass is not the host's set. Health filtering and
	// serviceFor keep the previous one.
	if !complete {
		return res, false
	}
	a.mu.Lock()
	a.disc = res
	a.services = res.Services
	a.mu.Unlock()
	a.queueDockerHealth(res)
	return res, true
}

const maxDockerPending = 1000

// queueDockerHealth records one sample per container service whose health is
// healthy or unhealthy. starting and a null health emit nothing. The queue
// is drained by sendMetrics so this does not block on the session loop.
func (a *agent) queueDockerHealth(res discover.Result) {
	a.mu.Lock()
	on := a.dockerHealth
	a.mu.Unlock()
	if !on || len(res.ContainerHealth) == 0 {
		return
	}
	now := a.now()
	var samples []protocol.Sample
	for _, svc := range res.Services {
		if svc.Kind != "container" {
			continue
		}
		h, ok := res.ContainerHealth[svc.Name]
		if !ok {
			continue
		}
		result := ""
		switch h {
		case "healthy":
			result = "success"
		case "unhealthy":
			result = "unhealthy"
		default:
			continue
		}
		samples = append(samples, protocol.NewSample("health", now, map[string]string{
			"service": svc.Name, "protocol": "docker",
		}, map[string]any{
			"result": result, "http_status": nil, "latency_ms": nil,
		}))
	}
	if len(samples) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.dockerHealth {
		return
	}
	room := maxDockerPending - len(a.dockerPending)
	if room <= 0 {
		return
	}
	if len(samples) > room {
		samples = samples[:room]
	}
	a.dockerPending = append(a.dockerPending, samples...)
}

func (a *agent) takeDockerSamples() []protocol.Sample {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.dockerHealth {
		a.dockerPending = nil
		return nil
	}
	out := a.dockerPending
	a.dockerPending = nil
	return out
}

// clearDockerGate runs at the start of a connection, before hello_ok.
// Samples stay off until that frame's config.docker_health is applied.
func (a *agent) clearDockerGate() {
	a.mu.Lock()
	a.dockerHealth = false
	a.dockerPending = nil
	a.mu.Unlock()
}

func (a *agent) maybeServices(ctx context.Context, w *writer, now time.Time) error {
	res, complete := a.discover(ctx)
	if !complete {
		return nil
	}
	canon := canonical(res.Services)
	changed := canon != a.lastCanon
	if !changed && now.Sub(a.lastServicesSent) < a.opt.ServicesRefreshEvery {
		return nil
	}
	if err := w.Send(ctx, protocol.Services{V: protocol.Version, Type: "services", Services: res.Services}); err != nil {
		return err
	}
	a.lastCanon = canon
	a.lastServicesSent = now
	if changed {
		a.refilter(ctx)
	}
	return nil
}

func (a *agent) serviceFor(pid int, cgroup string) string {
	a.mu.Lock()
	d := a.disc
	a.mu.Unlock()
	return d.ServiceFor(pid, cgroup)
}

func canonical(services []protocol.Discovered) string {
	b, err := protocol.Marshal(services)
	if err != nil {
		return ""
	}
	return string(b)
}
