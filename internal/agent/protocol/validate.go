package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	projectSlugRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	entrySlugRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	deploybotUnitRE = regexp.MustCompile(`^deploybot-[a-z0-9-]{1,64}-[a-z0-9-]{1,48}\.service$`)
	policyHashRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidateAgent reports why an agent→server frame violates the contract.
// The fake server calls this on every frame the agent sends.
func ValidateAgent(raw []byte) error {
	if len(raw) > MaxFrame {
		return fmt.Errorf("frame exceeds %d bytes", MaxFrame)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("envelope: %w", err)
	}
	if env.V != Version {
		return fmt.Errorf("v=%d", env.V)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	switch env.Type {
	case "enroll":
		var f Enroll
		if err := dec.Decode(&f); err != nil {
			return err
		}
		return checkEnroll(f)
	case "hello":
		var f Hello
		if err := dec.Decode(&f); err != nil {
			return err
		}
		return checkHello(f)
	case "ping":
		var f Ping
		if err := dec.Decode(&f); err != nil {
			return err
		}
		_, err := ParseTime(f.TS)
		return err
	case "metrics":
		return checkMetrics(raw)
	case "services":
		var f Services
		if err := dec.Decode(&f); err != nil {
			return err
		}
		return checkDiscovered(f.Services)
	case "task_reject":
		var f TaskReject
		if err := dec.Decode(&f); err != nil {
			return err
		}
		if f.TaskID == "" || !rejectCode(f.ErrorCode) {
			return fmt.Errorf("task_reject")
		}
		return nil
	case "task_result":
		return checkTaskResult(raw)
	default:
		return fmt.Errorf("type %q", env.Type)
	}
}

func checkEnroll(f Enroll) error {
	if f.JoinToken == "" || f.AgentVersion == "" || f.OS == "" || f.Arch == "" {
		return fmt.Errorf("enroll fields")
	}
	if !hostnameOK(f.Hostname) {
		return fmt.Errorf("hostname %q", f.Hostname)
	}
	return nil
}

func hostnameOK(s string) bool {
	if len(s) < 1 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func checkHello(f Hello) error {
	if f.HostID == "" || f.CredentialID == "" || f.AgentVersion == "" || f.OS == "" || f.Arch == "" {
		return fmt.Errorf("hello fields")
	}
	if err := checkCaps(f); err != nil {
		return err
	}
	if f.Programs != nil {
		if f.Programs.RunAs == nil {
			return fmt.Errorf("run_as")
		}
		if f.Programs.PolicyHash != "" && !policyHashRE.MatchString(f.Programs.PolicyHash) {
			return fmt.Errorf("policy_hash")
		}
		if f.Programs.Enabled && f.Programs.PolicyHash == "" {
			return fmt.Errorf("policy_hash")
		}
	}
	return checkDiscovered(f.Services)
}

func checkCaps(f Hello) error {
	caps := f.Capabilities
	base := len(caps) == 2 && caps[0] == "metrics" && caps[1] == "probe"
	with := len(caps) == 3 && caps[0] == "metrics" && caps[1] == "probe" && caps[2] == "programs"
	if !base && !with {
		return fmt.Errorf("capabilities %v", caps)
	}
	if with && (f.Programs == nil || !f.Programs.Enabled) {
		return fmt.Errorf("programs capability")
	}
	if f.Programs != nil && f.Programs.Enabled && !with {
		return fmt.Errorf("programs enabled")
	}
	return nil
}

func checkDiscovered(list []Discovered) error {
	seen := map[string]bool{}
	for _, d := range list {
		if !NamePattern.MatchString(d.Name) {
			return fmt.Errorf("name %q", d.Name)
		}
		if seen[d.Name] {
			return fmt.Errorf("duplicate name %q", d.Name)
		}
		seen[d.Name] = true
		if d.ContainerName != nil {
			return fmt.Errorf("container_name set")
		}
		if d.Ports == nil {
			return fmt.Errorf("ports null")
		}
		for _, p := range d.Ports {
			if p < 1 || p > 65535 {
				return fmt.Errorf("port %d", p)
			}
		}
		switch d.Kind {
		case "systemd":
			if d.SystemdUnit == nil || *d.SystemdUnit == "" {
				return fmt.Errorf("systemd unit")
			}
			if d.SystemdScope == nil || (*d.SystemdScope != "system" && *d.SystemdScope != "user") {
				return fmt.Errorf("systemd scope")
			}
		case "process":
			if d.SystemdUnit != nil || d.SystemdScope != nil {
				return fmt.Errorf("process has unit")
			}
		default:
			return fmt.Errorf("kind %q", d.Kind)
		}
		if err := checkOwner(d); err != nil {
			return err
		}
	}
	return nil
}

func checkOwner(d Discovered) error {
	if d.Owner == nil {
		return nil
	}
	if d.Owner.Kind != "program" {
		return fmt.Errorf("owner kind")
	}
	if !projectSlugRE.MatchString(d.Owner.Project) || !entrySlugRE.MatchString(d.Owner.Entry) {
		return fmt.Errorf("owner name")
	}
	if d.Name != d.Owner.Entry {
		return fmt.Errorf("owner entry")
	}
	want := "deploybot-" + d.Owner.Project + "-" + d.Owner.Entry + ".service"
	if d.Kind != "systemd" || d.SystemdUnit == nil || *d.SystemdUnit != want {
		return fmt.Errorf("owner unit")
	}
	if d.SystemdScope == nil || *d.SystemdScope != "system" {
		return fmt.Errorf("owner scope")
	}
	return nil
}

func rejectCode(c string) bool {
	switch c {
	case "wrong_host", "expired", "forbidden", "bad_spec", "unknown_service", "busy", "policy_denied":
		return true
	default:
		return false
	}
}

func checkTaskResult(raw []byte) error {
	var head struct {
		V         int             `json:"v"`
		Type      string          `json:"type"`
		TaskID    string          `json:"task_id"`
		State     string          `json:"state"`
		ErrorCode *string         `json:"error_code"`
		Payload   json.RawMessage `json:"payload"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&head); err != nil {
		return err
	}
	if head.TaskID == "" {
		return fmt.Errorf("task_id")
	}
	switch head.State {
	case "succeeded", "failed", "timed_out":
	default:
		return fmt.Errorf("state %q", head.State)
	}
	if head.State == "succeeded" && head.ErrorCode != nil {
		return fmt.Errorf("succeeded with error_code")
	}
	if head.State != "succeeded" && (head.ErrorCode == nil || *head.ErrorCode == "") {
		return fmt.Errorf("missing error_code")
	}
	if len(head.Payload) == 0 {
		return fmt.Errorf("payload")
	}
	return nil
}

// ValidateSnapshot checks a host_snapshot payload.
func ValidateSnapshot(raw []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	need := []string{"sampled_at", "cpu_percent", "mem", "disks", "load", "temp_celsius", "uptime_sec", "top"}
	if len(m) != len(need) {
		return fmt.Errorf("snapshot keys %d", len(m))
	}
	for _, k := range need {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("snapshot missing %s", k)
		}
	}
	if _, err := ParseTime(unquote(m["sampled_at"])); err != nil {
		return fmt.Errorf("sampled_at: %w", err)
	}
	if err := wantNumber(m["cpu_percent"]); err != nil {
		return fmt.Errorf("cpu_percent: %w", err)
	}
	if err := wantNumber(m["uptime_sec"]); err != nil {
		return fmt.Errorf("uptime_sec: %w", err)
	}
	if err := wantNumberOrNull(m["temp_celsius"]); err != nil {
		return fmt.Errorf("temp_celsius: %w", err)
	}
	var mem map[string]json.RawMessage
	if err := json.Unmarshal(m["mem"], &mem); err != nil {
		return err
	}
	for _, k := range []string{"total", "used", "available", "used_percent"} {
		if _, ok := mem[k]; !ok {
			return fmt.Errorf("mem.%s", k)
		}
	}
	var load []json.RawMessage
	if err := json.Unmarshal(m["load"], &load); err != nil || len(load) != 3 {
		return fmt.Errorf("load")
	}
	var top []map[string]json.RawMessage
	if err := json.Unmarshal(m["top"], &top); err != nil {
		return err
	}
	if len(top) > 20 {
		return fmt.Errorf("top %d", len(top))
	}
	for _, row := range top {
		for _, k := range []string{"pid", "process_name", "user", "cmdline", "cpu_usage", "memory_rss"} {
			if _, ok := row[k]; !ok {
				return fmt.Errorf("top missing %s", k)
			}
		}
	}
	return nil
}

// ValidateProbeResult checks a probe_health payload.
func ValidateProbeResult(raw []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	need := []string{"service", "protocol", "port", "path", "result", "http_status", "latency_ms", "checked_at"}
	if len(m) != len(need) {
		return fmt.Errorf("probe keys %d", len(m))
	}
	for _, k := range need {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("probe missing %s", k)
		}
	}
	switch unquote(m["result"]) {
	case "success", "status_mismatch", "timeout", "connection_refused", "error":
	default:
		return fmt.Errorf("result")
	}
	switch unquote(m["protocol"]) {
	case "http", "tcp":
	default:
		return fmt.Errorf("protocol")
	}
	if _, err := ParseTime(unquote(m["checked_at"])); err != nil {
		return err
	}
	if err := wantNumberOrNull(m["http_status"]); err != nil {
		return err
	}
	return wantNumber(m["latency_ms"])
}

// ValidateProgramStatus checks a succeeded program_start/stop/restart/status payload.
func ValidateProgramStatus(raw []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	need := []string{"unit", "active_state", "sub_state", "main_pid", "started_at", "ports"}
	if len(m) != len(need) {
		return fmt.Errorf("status keys %d", len(m))
	}
	for _, k := range need {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("status missing %s", k)
		}
	}
	if !deploybotUnitRE.MatchString(unquote(m["unit"])) {
		return fmt.Errorf("unit")
	}
	if unquote(m["active_state"]) == "" || unquote(m["sub_state"]) == "" {
		return fmt.Errorf("state")
	}
	if err := wantIntOrNull(m["main_pid"]); err != nil {
		return fmt.Errorf("main_pid: %w", err)
	}
	if err := wantStringOrNull(m["started_at"]); err != nil {
		return fmt.Errorf("started_at: %w", err)
	}
	if !bytes.Equal(bytes.TrimSpace(m["started_at"]), []byte("null")) {
		if _, err := ParseTime(unquote(m["started_at"])); err != nil {
			return fmt.Errorf("started_at: %w", err)
		}
	}
	var ports []int
	if err := json.Unmarshal(m["ports"], &ports); err != nil {
		return fmt.Errorf("ports")
	}
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("port %d", p)
		}
	}
	return nil
}

// ValidateProgramLogs checks a succeeded program_logs payload.
func ValidateProgramLogs(raw []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	need := []string{"unit", "lines", "truncated"}
	if len(m) != len(need) {
		return fmt.Errorf("logs keys %d", len(m))
	}
	for _, k := range need {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("logs missing %s", k)
		}
	}
	if !deploybotUnitRE.MatchString(unquote(m["unit"])) {
		return fmt.Errorf("unit")
	}
	var lines []string
	if err := json.Unmarshal(m["lines"], &lines); err != nil {
		return fmt.Errorf("lines")
	}
	total := 0
	for _, line := range lines {
		if utf8.RuneCountInString(line) > 2048 {
			return fmt.Errorf("line length")
		}
		total += len(line)
	}
	if total > 64*1024 {
		return fmt.Errorf("log bytes")
	}
	return wantBool(m["truncated"])
}

func wantStringOrNull(raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return wantString(raw)
}

func wantIntOrNull(raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return wantInt(raw)
}

func wantBool(raw json.RawMessage) error {
	switch string(bytes.TrimSpace(raw)) {
	case "true", "false":
		return nil
	default:
		return fmt.Errorf("not a bool")
	}
}

func unquote(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func wantNumber(raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] == '"' || bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("not a number")
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return err
	}
	return nil
}

func wantNumberOrNull(raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return wantNumber(raw)
}

type fieldKind int

const (
	kindFloat fieldKind = iota
	kindInt
	kindString
	kindNullInt
)

var measurementFields = map[string]map[string]fieldKind{
	"cpu": {
		"usage_user": kindFloat, "usage_system": kindFloat,
		"usage_idle": kindFloat, "usage_iowait": kindFloat,
	},
	"mem": {
		"total": kindInt, "available": kindInt, "used": kindInt, "used_percent": kindFloat,
	},
	"disk": {
		"total": kindInt, "used": kindInt, "free": kindInt, "used_percent": kindFloat,
	},
	"diskio": {"read_bytes": kindInt, "write_bytes": kindInt},
	"net":    {"bytes_recv": kindInt, "bytes_sent": kindInt},
	"system": {
		"load1": kindFloat, "load5": kindFloat, "load15": kindFloat,
		"uptime": kindInt, "n_cpus": kindInt,
	},
	"temp":    {"celsius": kindFloat},
	"process": {"cpu_usage": kindFloat, "memory_rss": kindInt, "memory_usage": kindFloat, "num_threads": kindInt, "started_at": kindString},
	"health":  {"result": kindString, "http_status": kindNullInt, "latency_ms": kindFloat},
}

var requiredFields = map[string][]string{
	"cpu":     {"usage_user", "usage_system", "usage_idle", "usage_iowait"},
	"mem":     {"total", "available", "used", "used_percent"},
	"disk":    {"total", "used", "free", "used_percent"},
	"diskio":  {"read_bytes", "write_bytes"},
	"net":     {"bytes_recv", "bytes_sent"},
	"system":  {"load1", "load5", "load15", "uptime", "n_cpus"},
	"temp":    {"celsius"},
	"process": {"memory_rss", "memory_usage", "num_threads", "started_at"},
	"health":  {"result", "http_status", "latency_ms"},
}

var measurementTags = map[string]map[string]bool{
	"cpu":     {"cpu": true},
	"mem":     {},
	"disk":    {"path": true, "fstype": true},
	"diskio":  {"name": true},
	"net":     {"interface": true},
	"system":  {},
	"temp":    {"sensor": true},
	"process": {"pid": true, "process_name": true, "user": true, "cmdline": true, "service": false},
	"health":  {"service": true, "check_id": true, "protocol": true, "port": true},
}

func checkMetrics(raw []byte) error {
	var head struct {
		V       int    `json:"v"`
		Type    string `json:"type"`
		BatchID string `json:"batch_id"`
		Samples []struct {
			M    string                     `json:"m"`
			T    string                     `json:"t"`
			Tags map[string]json.RawMessage `json:"tags"`
			F    map[string]json.RawMessage `json:"f"`
		} `json:"samples"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&head); err != nil {
		return err
	}
	if _, err := parseUUID(head.BatchID); err != nil {
		return fmt.Errorf("batch_id: %w", err)
	}
	if len(head.Samples) > MaxSamples {
		return fmt.Errorf("samples %d", len(head.Samples))
	}
	for i, s := range head.Samples {
		if err := checkSample(s.M, s.T, s.Tags, s.F); err != nil {
			return fmt.Errorf("sample %d: %w", i, err)
		}
	}
	return nil
}

func checkSample(m, ts string, tags, fields map[string]json.RawMessage) error {
	allowF, ok := measurementFields[m]
	if !ok {
		return fmt.Errorf("m %q", m)
	}
	if _, err := ParseTime(ts); err != nil {
		return fmt.Errorf("t: %w", err)
	}
	allowT, ok := measurementTags[m]
	if !ok {
		return fmt.Errorf("tags for %s", m)
	}
	if tags == nil {
		return fmt.Errorf("tags null")
	}
	for k := range tags {
		opt, known := allowT[k]
		if !known && !opt {
			return fmt.Errorf("tag %s", k)
		}
		if _, known := allowT[k]; !known {
			return fmt.Errorf("tag %s", k)
		}
		if err := wantString(tags[k]); err != nil {
			return fmt.Errorf("tag %s: %w", k, err)
		}
	}
	for k, req := range allowT {
		if req {
			if _, ok := tags[k]; !ok {
				return fmt.Errorf("missing tag %s", k)
			}
		}
	}
	if err := checkTagValues(m, tags); err != nil {
		return err
	}
	for k, raw := range fields {
		kind, ok := allowF[k]
		if !ok {
			return fmt.Errorf("field %s", k)
		}
		if err := checkFieldType(kind, raw); err != nil {
			return fmt.Errorf("field %s: %w", k, err)
		}
	}
	for _, k := range requiredFields[m] {
		if _, ok := fields[k]; !ok {
			return fmt.Errorf("missing field %s", k)
		}
	}
	if m == "health" {
		switch unquote(fields["result"]) {
		case "success", "status_mismatch", "timeout", "connection_refused", "error":
		default:
			return fmt.Errorf("health result")
		}
	}
	if m == "process" {
		if utf8.RuneCountInString(unquote(tags["cmdline"])) > CmdlineMax {
			return fmt.Errorf("cmdline length")
		}
		if s, ok := fields["started_at"]; ok {
			if _, err := ParseTime(unquote(s)); err != nil {
				return fmt.Errorf("started_at: %w", err)
			}
		}
	}
	return nil
}

func checkTagValues(m string, tags map[string]json.RawMessage) error {
	switch m {
	case "cpu":
		if unquote(tags["cpu"]) != "cpu-total" {
			return fmt.Errorf("cpu tag")
		}
	case "diskio":
		if !WholeDisk.MatchString(unquote(tags["name"])) {
			return fmt.Errorf("diskio name")
		}
	case "net":
		iface := unquote(tags["interface"])
		if iface == "" || iface == "lo" {
			return fmt.Errorf("interface %q", iface)
		}
	case "health":
		p := unquote(tags["protocol"])
		if p != "http" && p != "tcp" {
			return fmt.Errorf("protocol")
		}
		n, err := strconv.Atoi(unquote(tags["port"]))
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("port")
		}
	}
	return nil
}

func wantString(raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' {
		return fmt.Errorf("not a string")
	}
	return nil
}

func checkFieldType(kind fieldKind, raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	switch kind {
	case kindFloat:
		return wantNumber(raw)
	case kindInt:
		return wantInt(raw)
	case kindString:
		return wantString(raw)
	case kindNullInt:
		if bytes.Equal(raw, []byte("null")) {
			return nil
		}
		return wantInt(raw)
	default:
		return fmt.Errorf("kind")
	}
}

func wantInt(raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] == '"' || bytes.ContainsAny(raw, ".eE") {
		return fmt.Errorf("not an int")
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return err
	}
	if _, err := n.Int64(); err != nil {
		return err
	}
	return nil
}

// parseUUID checks the canonical 8-4-4-4-12 hex form without pulling uuid
// into every caller of the validator. The agent itself uses google/uuid.
func parseUUID(s string) (string, error) {
	if len(s) != 36 {
		return "", fmt.Errorf("len")
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return "", fmt.Errorf("dash")
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return "", fmt.Errorf("hex")
			}
		}
	}
	return strings.ToLower(s), nil
}
