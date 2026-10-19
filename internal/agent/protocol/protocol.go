// Package protocol is the phase-1 agent wire model from
// docs/contracts/agent-protocol-v1.md. The server is the other side of the
// same document; do not add fields that the contract does not list.
package protocol

import (
	"bytes"
	"encoding/json"
	"regexp"
	"time"
)

const (
	Version      = 1
	AgentVersion = "0.2.2"
	MaxFrame     = 1 << 20
	MaxSamples   = 5000
	CmdlineMax   = 1024
)

// NamePattern is the discovered-service name constraint.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// WholeDisk matches the diskio name allowlist (whole disks, not partitions).
var WholeDisk = regexp.MustCompile(`^(?:mmcblk\d+|nvme\d+n\d+|sd[a-z]+|vd[a-z]+)$`)

// Capabilities is the phase-1 hello set. metrics covers host_snapshot;
// probe covers probe_health. "programs" is added by the daemon only when
// the host policy is valid and enabled.
func Capabilities() []string {
	return []string{"metrics", "probe"}
}

// FormatTime renders a contract timestamp.
func FormatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ParseTime accepts RFC3339 with or without a fractional second.
func ParseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// Marshal encodes a frame without HTML escaping and without the trailing
// newline encoding/json.Encoder adds.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Envelope is the common prefix of every frame.
type Envelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

// Discovered is one listening service.
// User is the socket owner's name, or the numeric uid when passwd has no
// entry. It is null when the uid column is not a number.
type Discovered struct {
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Ports         []int   `json:"ports"`
	SystemdUnit   *string `json:"systemd_unit"`
	SystemdScope  *string `json:"systemd_scope"`
	ContainerName *string `json:"container_name"`
	ProcessName   *string `json:"process_name"`
	PID           *int    `json:"pid"`
	User          *string `json:"user"`
	// ImageDigest is set for an owned container and null for every other row.
	// It is always sent so a container row can carry JSON null.
	ImageDigest *string `json:"image_digest"`
	// Owner is null when the row has no owner. Other containers send JSON null.
	Owner *Owner `json:"owner"`
}

// Owner marks a service started by the root helper. Old servers ignore it.
type Owner struct {
	Kind    string `json:"kind"`
	Project string `json:"project"`
	Entry   string `json:"entry"`
}

// Programs is the hello summary of the host policy. Paths, image patterns,
// and project lists are never included. Images and Docker are sent whenever
// the policy file exists, including when they are false.
type Programs struct {
	Enabled    bool     `json:"enabled"`
	RunAs      []string `json:"run_as"`
	AllowShell bool     `json:"allow_shell"`
	PolicyHash string   `json:"policy_hash"`
	Images     bool     `json:"images"`
	Docker     bool     `json:"docker"`
	// Files is true when this agent and its helper implement v1.5 managed files.
	Files bool `json:"files"`
}

// NormalizePorts makes a nil port list encode as [] rather than null.
func (d Discovered) NormalizePorts() Discovered {
	if d.Ports == nil {
		d.Ports = []int{}
	}
	return d
}

// Sample is one metrics point. Tags are strings. Field values keep their
// concrete Go types so integers do not become floats.
type Sample struct {
	M    string            `json:"m"`
	T    string            `json:"t"`
	Tags map[string]string `json:"tags"`
	F    map[string]any    `json:"f"`
}

// NewSample builds a sample with an empty tag map when tags is nil.
func NewSample(m string, at time.Time, tags map[string]string, fields map[string]any) Sample {
	if tags == nil {
		tags = map[string]string{}
	}
	if fields == nil {
		fields = map[string]any{}
	}
	return Sample{M: m, T: FormatTime(at), Tags: tags, F: fields}
}

// HealthCheck is one entry of hello_ok.config.health_checks.
type HealthCheck struct {
	ID              string `json:"id"`
	Service         string `json:"service"`
	Protocol        string `json:"protocol"`
	Port            int    `json:"port"`
	Path            string `json:"path"`
	IntervalSec     int    `json:"interval_sec"`
	ExpectStatusMin int    `json:"expect_status_min"`
	ExpectStatusMax int    `json:"expect_status_max"`
}

// Config is hello_ok.config and config_update.config.
type Config struct {
	MetricsIntervalSec int           `json:"metrics_interval_sec"`
	ProcessIntervalSec int           `json:"process_interval_sec"`
	HealthChecks       []HealthCheck `json:"health_checks"`
}

// Enroll is the first frame on an unauthenticated connection.
type Enroll struct {
	V            int    `json:"v"`
	Type         string `json:"type"`
	JoinToken    string `json:"join_token"`
	Hostname     string `json:"hostname"`
	AgentVersion string `json:"agent_version"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	AlwaysOn     bool   `json:"always_on"`
}

// Hello is the first frame on a Bearer connection.
type Hello struct {
	V            int          `json:"v"`
	Type         string       `json:"type"`
	HostID       string       `json:"host_id"`
	CredentialID string       `json:"credential_id"`
	AgentVersion string       `json:"agent_version"`
	OS           string       `json:"os"`
	Arch         string       `json:"arch"`
	Capabilities []string     `json:"capabilities"`
	Programs     *Programs    `json:"programs,omitempty"`
	Services     []Discovered `json:"services"`
	// ServicesComplete is false when the first discovery did not finish.
	// It is always sent. Older agents omit it, and the server reads that as true.
	ServicesComplete bool `json:"services_complete"`
}

// Services is the discovered set. It is sent when the set changes and at least every 300s while connected.
type Services struct {
	V        int          `json:"v"`
	Type     string       `json:"type"`
	Services []Discovered `json:"services"`
}

// Ping is the application heartbeat.
type Ping struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	TS   string `json:"ts"`
}

// Metrics is one ingest batch.
type Metrics struct {
	V       int      `json:"v"`
	Type    string   `json:"type"`
	BatchID string   `json:"batch_id"`
	Samples []Sample `json:"samples"`
}

// TaskReject is the agent's refusal to run a task.
type TaskReject struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	TaskID    string `json:"task_id"`
	ErrorCode string `json:"error_code"`
}

// TaskResult is resent until task_ack. ErrorCode is null on success.
type TaskResult struct {
	V         int            `json:"v"`
	Type      string         `json:"type"`
	TaskID    string         `json:"task_id"`
	State     string         `json:"state"`
	ErrorCode *string        `json:"error_code"`
	Payload   map[string]any `json:"payload"`
}

// Task is a server-assigned phase-1 task. Spec stays raw so extra keys can
// be rejected before any work runs.
type Task struct {
	V          int             `json:"v"`
	Type       string          `json:"type"`
	TaskID     string          `json:"task_id"`
	HostID     string          `json:"host_id"`
	Kind       string          `json:"kind"`
	Service    *string         `json:"service"`
	ExpiresAt  string          `json:"expires_at"`
	TimeoutSec int             `json:"timeout_sec"`
	Spec       json.RawMessage `json:"spec"`
}

// HelloOK is the server's acceptance of hello.
type HelloOK struct {
	V            int    `json:"v"`
	Type         string `json:"type"`
	HostID       string `json:"host_id"`
	HeartbeatSec int    `json:"heartbeat_sec"`
	ServerTime   string `json:"server_time"`
	Config       Config `json:"config"`
}

// ServerError is error_code unauthenticated or forbidden. Message is never logged.
type ServerError struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

// MetricsAck acknowledges a committed batch.
type MetricsAck struct {
	V       int    `json:"v"`
	Type    string `json:"type"`
	BatchID string `json:"batch_id"`
}

// TaskAck stops resending a task_result.
type TaskAck struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
}

// ConfigUpdate replaces the running config.
type ConfigUpdate struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	Config Config `json:"config"`
}

// Pong answers ping. The agent does not depend on its body.
type Pong struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	TS   string `json:"ts"`
}

// Str returns a pointer to s for nullable JSON strings.
func Str(s string) *string { return &s }

// Int returns a pointer to n for nullable JSON integers.
func Int(n int) *int { return &n }
