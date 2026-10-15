package run

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/probe"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/redact"
	"github.com/more-than-code/bot-agent/internal/agent/tasks"
)

const (
	imagePoll = 2 * time.Second
	// maxImageTimeoutSec is the largest image-task budget: 300s of pull,
	// a 3600s job, and 60s on each side of the wait.
	maxImageTimeoutSec = 300 + 3600 + 60 + 60
	// machineIDPath is the systemd machine id. ProtectSystem=strict still
	// allows this file to be read.
	machineIDPath = "/etc/machine-id"
)

func (a *agent) runImage(ctx context.Context, task protocol.Task) protocol.TaskResult {
	req, code := tasks.ParseImage(task, a.policy)
	if code != "" {
		return a.imageRejected(task)
	}
	ex := redact.NewExact(req.SecretValues()...)
	values := redactValues(req)
	defer func() {
		scrubTask(task.Spec, &req)
		ex.Clear()
		for i := range values {
			values[i] = ""
		}
	}()
	tmo := imageTimeout(task.TimeoutSec)
	deadline := a.now().Add(tmo)
	tctx, cancel := context.WithTimeout(ctx, tmo)
	defer cancel()
	switch task.Kind {
	case "image_deploy", "image_rollback":
		return a.deployImage(tctx, task, req, deadline, ex, values)
	case "image_logs":
		return a.logsImage(tctx, task, req, ex)
	default:
		return a.actionImage(tctx, task, req, ex)
	}
}

func imageTimeout(sec int) time.Duration {
	if sec <= 0 {
		return 15 * time.Second
	}
	if sec > maxImageTimeoutSec {
		sec = maxImageTimeoutSec
	}
	return time.Duration(sec) * time.Second
}

func redactValues(req tasks.ImageReq) []string {
	var out []string
	for _, v := range req.SecretValues() {
		if len(v) < images.MinRedactBytes || len(v) > images.MaxRedactBytes || strings.IndexByte(v, 0) >= 0 {
			continue
		}
		out = append(out, v)
	}
	return out
}

func (a *agent) deployImage(ctx context.Context, task protocol.Task, req tasks.ImageReq, deadline time.Time, ex redact.Exact, values []string) protocol.TaskResult {
	ref, err := images.Parse(req.Spec.Image)
	if err != nil {
		return a.deployResult(task, "forbidden_spec", "", nil, missingStatus(), nil, ex)
	}
	pullRef := req.Spec.Image
	if task.Kind == "image_rollback" {
		pullRef = ref.Name + "@" + req.Digest
	}
	digest, code, err := a.pullImage(ctx, req, pullRef)
	if err != nil {
		return a.deployResult(task, "failed", "", nil, missingStatus(), a.tailImage(ctx, req.Project, req.Entry, ex, values), ex)
	}
	if code != "" {
		return a.deployResult(task, outcomeFor(code), "", nil, missingStatus(), a.tailImage(ctx, req.Project, req.Entry, ex, values), ex)
	}
	if task.Kind == "image_rollback" || req.Spec.Kind == "service" {
		return a.deployService(ctx, task, req, deadline, digest, ex, values)
	}
	return a.deployJob(ctx, task, req, deadline, digest, ex, values)
}

func (a *agent) deployService(ctx context.Context, task protocol.Task, req tasks.ImageReq, deadline time.Time, digest string, ex redact.Exact, values []string) protocol.TaskResult {
	st, hash, code, err := a.imageStatus(ctx, req.Project, req.Entry)
	if err != nil {
		return a.deployResult(task, "failed", digest, nil, missingStatus(), a.tailImage(ctx, req.Project, req.Entry, ex, values), ex)
	}
	if code != "" {
		return a.deployResult(task, outcomeFor(code), digest, nil, missingStatus(), a.tailImage(ctx, req.Project, req.Entry, ex, values), ex)
	}
	if !req.Force && st.State == "running" && st.Digest != nil && *st.Digest == digest && hash == req.SpecHash {
		return a.deployResult(task, "unchanged", digest, nil, st, a.tailImage(ctx, req.Project, req.Entry, ex, values), ex)
	}
	if code, err = a.deployContainer(ctx, req, digest); err != nil {
		return a.deployResult(task, "failed", digest, nil, st, a.tailImage(ctx, req.Project, req.Entry, ex, values), ex)
	}
	if code != "" {
		return a.deployResult(task, outcomeFor(code), digest, nil, st, a.tailImage(ctx, req.Project, req.Entry, ex, values), ex)
	}
	need := time.Duration(req.Spec.HealthyAfterSec) * time.Second
	if task.Kind == "image_rollback" && need <= 0 {
		need = 30 * time.Second
	}
	w := a.waitHealthy(ctx, deadline, req, need)
	if w.err != nil || w.code != "" {
		outcome := "failed"
		if w.code != "" {
			outcome = outcomeFor(w.code)
		}
		return a.deployResult(task, outcome, digest, nil, w.last, a.tailImage(ctx, req.Project, req.Entry, ex, values), ex)
	}
	if w.healthy {
		return a.deployResult(task, "succeeded", digest, nil, w.last, a.tailImage(ctx, req.Project, req.Entry, ex, values), ex)
	}
	rcode, rerr := a.rollbackContainer(ctx, req.Project, req.Entry)
	lines := a.tailImage(ctx, req.Project, req.Entry, ex, values)
	last := w.last
	if rerr != nil || rcode != "" {
		return a.deployResult(task, "rollback_failed", digest, nil, last, lines, ex)
	}
	if st, _, code, err = a.imageStatus(ctx, req.Project, req.Entry); err == nil && code == "" {
		last = st
	}
	return a.deployResult(task, "rolled_back", digest, nil, last, lines, ex)
}

func (a *agent) deployJob(ctx context.Context, task protocol.Task, req tasks.ImageReq, deadline time.Time, digest string, ex redact.Exact, values []string) protocol.TaskResult {
	tail := func() []string {
		return a.tailImage(ctx, req.Project, req.Entry, ex, values)
	}
	st, _, code, err := a.imageStatus(ctx, req.Project, req.Entry)
	if err != nil {
		return a.deployResult(task, "failed", digest, nil, missingStatus(), tail(), ex)
	}
	if code != "" {
		return a.deployResult(task, outcomeFor(code), digest, nil, missingStatus(), tail(), ex)
	}
	if st.State != "missing" {
		code, err = a.removeContainer(ctx, req.Project, req.Entry)
		if err != nil {
			return a.deployResult(task, "failed", digest, nil, st, tail(), ex)
		}
		if code != "" {
			return a.deployResult(task, outcomeFor(code), digest, nil, st, tail(), ex)
		}
	}
	if code, err = a.deployContainer(ctx, req, digest); err != nil {
		return a.deployResult(task, "failed", digest, nil, st, tail(), ex)
	}
	if code != "" {
		return a.deployResult(task, outcomeFor(code), digest, nil, st, tail(), ex)
	}
	jobDeadline := a.now().Add(time.Duration(req.Spec.TimeoutSec) * time.Second)
	if deadline.Before(jobDeadline) {
		jobDeadline = deadline
	}
	for {
		st, _, code, err = a.imageStatus(ctx, req.Project, req.Entry)
		if err != nil {
			return a.deployResult(task, "failed", digest, nil, missingStatus(), tail(), ex)
		}
		if code != "" {
			return a.deployResult(task, outcomeFor(code), digest, nil, st, tail(), ex)
		}
		if st.State == "exited" {
			outcome := "failed"
			if st.ExitCode != nil && *st.ExitCode == 0 {
				outcome = "succeeded"
			}
			return a.deployResult(task, outcome, digest, st.ExitCode, st, tail(), ex)
		}
		if !a.now().Before(jobDeadline) {
			a.stopContainer(ctx, req.Project, req.Entry)
			return a.deployResult(task, "timeout", digest, nil, st, tail(), ex)
		}
		if err := a.sleep(ctx, imagePoll); err != nil {
			if !a.now().Before(jobDeadline) {
				a.stopContainer(ctx, req.Project, req.Entry)
				return a.deployResult(task, "timeout", digest, nil, st, tail(), ex)
			}
			return a.deployResult(task, "failed", digest, nil, st, tail(), ex)
		}
	}
}

// healthStop is the health wait. healthy is a container that passed.
// err or code is a status failure: the caller must not roll back.
type healthStop struct {
	healthy bool
	code    string
	err     error
	last    images.ImageStatus
}

func (a *agent) waitHealthy(ctx context.Context, deadline time.Time, req tasks.ImageReq, need time.Duration) healthStop {
	checks := a.checksForEntry(req.Entry, hostPortsOf(req.Spec))
	if len(checks) == 0 {
		return a.waitStable(ctx, deadline, req.Project, req.Entry, need)
	}
	return a.waitProbes(ctx, deadline, req.Project, req.Entry, checks)
}

func (a *agent) waitStable(ctx context.Context, deadline time.Time, project, entry string, need time.Duration) healthStop {
	var since time.Time
	watching := false
	last := missingStatus()
	for {
		if a.expired(ctx, deadline) {
			return healthStop{last: last}
		}
		st, _, code, err := a.imageStatus(ctx, project, entry)
		if err != nil {
			return healthStop{err: err, last: last}
		}
		if code != "" {
			return healthStop{code: code, last: last}
		}
		last = st
		if terminalState(st.State) || st.RestartCount > 0 {
			return healthStop{last: last}
		}
		if st.State == "running" {
			now := a.now()
			if !watching {
				since = now
				watching = true
			}
			if now.Sub(since) >= need {
				return healthStop{healthy: true, last: last}
			}
		} else {
			watching = false
		}
		if err := a.sleep(ctx, imagePoll); err != nil {
			return healthStop{err: err, last: last}
		}
	}
}

func (a *agent) waitProbes(ctx context.Context, deadline time.Time, project, entry string, checks []protocol.HealthCheck) healthStop {
	last := missingStatus()
	for {
		if a.expired(ctx, deadline) {
			return healthStop{last: last}
		}
		st, _, code, err := a.imageStatus(ctx, project, entry)
		if err != nil {
			return healthStop{err: err, last: last}
		}
		if code != "" {
			return healthStop{code: code, last: last}
		}
		last = st
		if terminalState(st.State) {
			return healthStop{last: last}
		}
		if st.State == "running" && a.probesOK(ctx, checks) {
			return healthStop{healthy: true, last: last}
		}
		if err := a.sleep(ctx, imagePoll); err != nil {
			return healthStop{err: err, last: last}
		}
	}
}

func (a *agent) probesOK(ctx context.Context, checks []protocol.HealthCheck) bool {
	for _, c := range checks {
		res := probe.Do(ctx, probe.Spec{
			Protocol: c.Protocol, Port: c.Port, Path: c.Path,
			ExpectMin: c.ExpectStatusMin, ExpectMax: c.ExpectStatusMax,
			Timeout: 10 * time.Second,
		})
		if res.Result != "success" {
			return false
		}
	}
	return true
}

func (a *agent) checksForEntry(entry string, ports []int) []protocol.HealthCheck {
	a.mu.Lock()
	all := append([]protocol.HealthCheck(nil), a.configured...)
	a.mu.Unlock()
	var out []protocol.HealthCheck
	for _, c := range all {
		if c.Service != entry || (c.Protocol != "http" && c.Protocol != "tcp") || !probe.ValidPath(c.Path) {
			continue
		}
		if !portListed(ports, c.Port) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (a *agent) actionImage(ctx context.Context, task protocol.Task, req tasks.ImageReq, ex redact.Exact) protocol.TaskResult {
	verb := ""
	switch task.Kind {
	case "image_remove":
		verb = images.VerbRemove
	case "image_start":
		verb = images.VerbStart
	case "image_stop":
		verb = images.VerbStop
	case "image_restart":
		verb = images.VerbRestart
	case "image_status":
		verb = images.VerbStatus
	default:
		return a.imageRejected(task)
	}
	body := images.Encode(images.EntryRequest{Project: req.Project, Entry: req.Entry})
	if task.Kind == "image_status" {
		var resp images.StatusResult
		code, err := a.callImage(ctx, verb, body, &resp)
		if err != nil {
			return a.statusResult(task, "failed", missingStatus(), ex)
		}
		if code != "" {
			return a.statusResult(task, outcomeFor(code), missingStatus(), ex)
		}
		if !knownState(resp.Status.State) {
			return a.statusResult(task, "failed", missingStatus(), ex)
		}
		return a.statusResult(task, "succeeded", resp.Status, ex)
	}
	var resp images.ActionResult
	code, err := a.callImage(ctx, verb, body, &resp)
	if err != nil {
		return a.statusResult(task, "failed", missingStatus(), ex)
	}
	if code != "" {
		return a.statusResult(task, outcomeFor(code), missingStatus(), ex)
	}
	if !knownState(resp.Status.State) {
		return a.statusResult(task, "failed", missingStatus(), ex)
	}
	return a.statusResult(task, "succeeded", resp.Status, ex)
}

func (a *agent) logsImage(ctx context.Context, task protocol.Task, req tasks.ImageReq, ex redact.Exact) protocol.TaskResult {
	body := images.Encode(images.LogsRequest{Project: req.Project, Entry: req.Entry, Lines: req.Lines})
	var resp images.LogsResponse
	code, err := a.callImage(ctx, images.VerbLogs, body, &resp)
	name, _ := images.ContainerName(req.Project, req.Entry)
	if err != nil {
		return a.logsResult(task, "failed", name, nil, false, ex)
	}
	if code != "" {
		return a.logsResult(task, outcomeFor(code), name, nil, false, ex)
	}
	if resp.Container != "" {
		name = resp.Container
	}
	return a.logsResult(task, "succeeded", name, resp.Lines, resp.Truncated, ex)
}

func (a *agent) pullImage(ctx context.Context, req tasks.ImageReq, ref string) (string, string, error) {
	body := images.Encode(images.PullRequest{Project: req.Project, Ref: ref, RegistryAuth: req.Auth})
	var resp images.PullResponse
	code, err := a.callImage(ctx, images.VerbPull, body, &resp)
	if err != nil || code != "" {
		return "", code, err
	}
	if !images.ValidDigest(resp.Digest) {
		return "", "", errHelper
	}
	return resp.Digest, "", nil
}

func (a *agent) deployContainer(ctx context.Context, req tasks.ImageReq, digest string) (string, error) {
	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	body := images.Encode(images.DeployRequest{
		Project: req.Project, Entry: req.Entry, Spec: *req.Spec,
		Env: env, Digest: digest, SpecHash: req.SpecHash,
	})
	var id images.IDResponse
	return a.callImage(ctx, images.VerbDeploy, body, &id)
}

func (a *agent) imageStatus(ctx context.Context, project, entry string) (images.ImageStatus, string, string, error) {
	body := images.Encode(images.EntryRequest{Project: project, Entry: entry})
	var resp images.StatusResult
	code, err := a.callImage(ctx, images.VerbStatus, body, &resp)
	if err != nil || code != "" {
		return images.ImageStatus{}, "", code, err
	}
	if !knownState(resp.Status.State) {
		return resp.Status, resp.SpecHash, "", errHelper
	}
	return resp.Status, resp.SpecHash, "", nil
}

func (a *agent) removeContainer(ctx context.Context, project, entry string) (string, error) {
	body := images.Encode(images.EntryRequest{Project: project, Entry: entry})
	var resp images.ActionResult
	return a.callImage(ctx, images.VerbRemove, body, &resp)
}

func (a *agent) rollbackContainer(ctx context.Context, project, entry string) (string, error) {
	body := images.Encode(images.EntryRequest{Project: project, Entry: entry})
	var id images.IDResponse
	return a.callImage(ctx, images.VerbRollback, body, &id)
}

func (a *agent) stopContainer(ctx context.Context, project, entry string) {
	body := images.Encode(images.EntryRequest{Project: project, Entry: entry})
	var resp images.ActionResult
	_, _ = a.callImage(ctx, images.VerbStop, body, &resp)
}

func (a *agent) tailImage(ctx context.Context, project, entry string, ex redact.Exact, values []string) []string {
	if ctx.Err() != nil {
		return []string{}
	}
	body := images.Encode(images.LogsRequest{Project: project, Entry: entry, Lines: tailLines, Redact: values})
	var resp images.LogsResponse
	code, err := a.callImage(ctx, images.VerbLogs, body, &resp)
	if err != nil || code != "" || resp.Lines == nil {
		return []string{}
	}
	lines, _ := redact.JournalTask(resp.Lines, ex)
	if len(lines) > tailLines {
		lines = append([]string(nil), lines[len(lines)-tailLines:]...)
	}
	if lines == nil {
		return []string{}
	}
	return lines
}

func (a *agent) callImage(ctx context.Context, verb string, body []byte, dest any) (string, error) {
	if len(body) > programs.MaxFrame {
		zeroBytes(body)
		return "", errHelper
	}
	raw, err := a.call(ctx, verb, body)
	zeroBytes(body)
	if err != nil {
		return "", err
	}
	if code := imageRefusal(raw); code != "" {
		return code, nil
	}
	if dest == nil {
		return "", nil
	}
	if err := images.Decode(raw, dest); err != nil {
		return "", err
	}
	return "", nil
}

func imageRefusal(raw []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if rawError, ok := m["error"]; ok && len(m) == 1 {
		var code string
		if err := json.Unmarshal(rawError, &code); err != nil || code == "" {
			return ""
		}
		return programs.FixedReason(code)
	}
	rawOK, ok := m["ok"]
	if !ok {
		return ""
	}
	var passed bool
	if err := json.Unmarshal(rawOK, &passed); err != nil || passed {
		return ""
	}
	reason := ""
	if rawReason, ok := m["reason"]; ok {
		_ = json.Unmarshal(rawReason, &reason)
	}
	return programs.FixedReason(reason)
}

func (a *agent) listContainers(ctx context.Context) ([]images.ContainerInfo, bool, error) {
	if a.opt.Helper == nil {
		return nil, false, errHelper
	}
	hctx, cancel := context.WithTimeout(ctx, discoverHelperTimeout)
	defer cancel()
	raw, err := a.call(hctx, images.VerbList, []byte("{}"))
	if err != nil {
		return nil, false, err
	}
	if code := imageRefusal(raw); code != "" {
		return nil, false, errHelper
	}
	var resp images.ListResponse
	if err := images.Decode(raw, &resp); err != nil {
		return nil, false, err
	}
	if resp.Items == nil {
		return []images.ContainerInfo{}, resp.Truncated, nil
	}
	return resp.Items, resp.Truncated, nil
}

func (a *agent) noteDocker(ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dockerKnown {
		return
	}
	a.dockerOK = ok
	a.dockerKnown = true
}

func (a *agent) dockerUp() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dockerOK
}

func (a *agent) machineHost() string {
	a.idOnce.Do(func() {
		path := a.opt.MachineID
		if path == "" {
			path = machineIDPath
		}
		b, err := os.ReadFile(path)
		if err != nil {
			a.idMiss = true
			return
		}
		id, ok := images.HostID(string(b))
		if !ok {
			a.idMiss = true
			return
		}
		a.idLabel = id
	})
	if a.idMiss && a.policy.ImagesEnabled() && a.idLogOnce.CompareAndSwap(false, true) {
		a.opt.Log.Printf("bot-agent: machine-id unreadable; container ownership uses labels only")
	}
	return a.idLabel
}

func (a *agent) expired(ctx context.Context, deadline time.Time) bool {
	if ctx.Err() != nil {
		return true
	}
	return !a.now().Before(deadline)
}

func outcomeFor(code string) string {
	switch code {
	case programs.ReasonImage:
		return "image_unavailable"
	case programs.ReasonConflict:
		return "conflict"
	case programs.ReasonPolicy, programs.ReasonBadSpec:
		return "forbidden_spec"
	default:
		return "failed"
	}
}

func (a *agent) deployResult(task protocol.Task, outcome, digest string, exit *int, st images.ImageStatus, lines []string, ex redact.Exact) protocol.TaskResult {
	var dig any
	if digest != "" {
		dig = digest
	}
	var code any
	if exit != nil {
		code = *exit
	}
	if lines == nil {
		lines = []string{}
	}
	payload := ex.Payload(map[string]any{
		"outcome": outcome, "digest": dig, "exit_code": code,
		"status": normalizeStatus(st), "lines": lines,
	})
	return imageTaskResult(task, outcome, payload)
}

func (a *agent) statusResult(task protocol.Task, outcome string, st images.ImageStatus, ex redact.Exact) protocol.TaskResult {
	payload := ex.Payload(map[string]any{"status": normalizeStatus(st)})
	return imageTaskResult(task, outcome, payload)
}

func (a *agent) logsResult(task protocol.Task, outcome, container string, lines []string, truncated bool, ex redact.Exact) protocol.TaskResult {
	redacted, cut := redact.JournalTask(lines, ex)
	if redacted == nil {
		redacted = []string{}
	}
	payload := ex.Payload(map[string]any{
		"container": container, "lines": redacted, "truncated": truncated || cut,
	})
	return imageTaskResult(task, outcome, payload)
}

func (a *agent) imageRejected(task protocol.Task) protocol.TaskResult {
	switch task.Kind {
	case "image_logs":
		return imageTaskResult(task, "forbidden_spec", map[string]any{
			"container": "", "lines": []string{}, "truncated": false,
		})
	case "image_deploy", "image_rollback":
		return imageTaskResult(task, "forbidden_spec", map[string]any{
			"outcome": "forbidden_spec", "digest": nil, "exit_code": nil,
			"status": normalizeStatus(missingStatus()), "lines": []string{},
		})
	default:
		return imageTaskResult(task, "forbidden_spec", map[string]any{
			"status": normalizeStatus(missingStatus()),
		})
	}
}

func imageTaskResult(task protocol.Task, outcome string, payload map[string]any) protocol.TaskResult {
	res := protocol.TaskResult{
		V: protocol.Version, Type: "task_result", TaskID: task.TaskID,
		State: "failed", Payload: payload,
	}
	if outcome == "succeeded" || outcome == "unchanged" {
		res.State = "succeeded"
		return res
	}
	res.ErrorCode = protocol.Str(outcome)
	return res
}

func normalizeStatus(st images.ImageStatus) images.ImageStatus {
	if !knownState(st.State) {
		st.State = "missing"
	}
	if st.Ports == nil {
		st.Ports = []int{}
	}
	return st
}

func missingStatus() images.ImageStatus {
	return images.ImageStatus{State: "missing", Ports: []int{}}
}

func knownState(s string) bool {
	switch s {
	case "running", "exited", "created", "paused", "restarting", "dead", "missing":
		return true
	default:
		return false
	}
}

func terminalState(s string) bool {
	switch s {
	case "exited", "dead", "missing":
		return true
	default:
		return false
	}
}

func hostPortsOf(spec *images.ImageSpec) []int {
	if spec == nil {
		return nil
	}
	out := make([]int, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		out = append(out, p.Host)
	}
	return out
}

func portListed(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

func scrubTask(spec []byte, req *tasks.ImageReq) {
	if req != nil && req.Env != nil {
		for k := range req.Env {
			req.Env[k] = ""
		}
		req.Env = nil
	}
	if req != nil && req.Auth != nil {
		req.Auth.Token = ""
		req.Auth = nil
	}
	zeroBytes(spec)
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
