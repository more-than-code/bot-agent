package images

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Helper verbs. programs.KnownVerb delegates here so the registry cannot drift.
const (
	VerbPull     = "image_pull"
	VerbDeploy   = "container_deploy"
	VerbRollback = "container_rollback"
	VerbRemove   = "container_remove"
	VerbStart    = "container_start"
	VerbStop     = "container_stop"
	VerbRestart  = "container_restart"
	VerbStatus   = "container_status"
	VerbLogs     = "container_logs"
	VerbList     = "container_list"
)

// IsVerb reports whether verb is a helper image verb.
func IsVerb(verb string) bool {
	switch verb {
	case VerbPull, VerbDeploy, VerbRollback, VerbRemove, VerbStart, VerbStop, VerbRestart, VerbStatus, VerbLogs, VerbList:
		return true
	default:
		return false
	}
}

// RegistryAuth is the optional pull credential. Token is the registry
// password. The helper builds X-Registry-Auth; it never forwards this object.
type RegistryAuth struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

// PullRequest is image_pull.
type PullRequest struct {
	Project      string        `json:"project"`
	Ref          string        `json:"ref"`
	RegistryAuth *RegistryAuth `json:"registry_auth"`
}

// PullResponse is the repo digest and the image id from the pull stream.
type PullResponse struct {
	Digest  string `json:"digest"`
	ImageID string `json:"image_id"`
}

// DeployRequest is container_deploy. Env is the merged environment, already
// resolved. Spec is the catalog spec; its env_sets are not looked up here.
type DeployRequest struct {
	Project  string            `json:"project"`
	Entry    string            `json:"entry"`
	Spec     ImageSpec         `json:"spec"`
	Env      map[string]string `json:"env"`
	Digest   string            `json:"digest"`
	SpecHash string            `json:"spec_hash"`
	// Files is empty for a v1.4 deploy. The key is omitted then, so a
	// no-file request stays the same shape.
	Files []MountFile `json:"files,omitempty"`
}

// MaxFrame is the image-verb request cap (agent-protocol v1.5).
// Program verbs stay at programs.MaxFrame.
const MaxFrame = 2 * 1024 * 1024

// SocketSlack is the {"verb","request"} wrapper and the trailing newline.
// The socket reader does not know the verb until the frame is parsed.
const SocketSlack = 128

// IDResponse is container_deploy and container_rollback.
type IDResponse struct {
	ContainerID string `json:"container_id"`
}

// EntryRequest is remove, start, stop, restart, rollback, and the name half of status.
type EntryRequest struct {
	Project string `json:"project"`
	Entry   string `json:"entry"`
}

// Redact limits for container_logs (v1.5). The list is optional.
const (
	MaxRedactCount = 2048
	MinRedactBytes = 6
	MaxRedactBytes = 4096
	MaxRedactTotal = 512 * 1024
)

// LogsRequest is container_logs. Lines is 1..2000.
// Redact is the task's exact values. The helper removes them from each raw
// line and does not log or return the list. Omit it when there are none.
type LogsRequest struct {
	Project string   `json:"project"`
	Entry   string   `json:"entry"`
	Lines   int      `json:"lines"`
	Redact  []string `json:"redact,omitempty"`
}

// ValidRedact reports whether redact is absent or within the v1.5 limits.
// Each value is 6..4096 bytes, the list is at most 2048 values and 512 KiB, and no value contains a NUL.
func ValidRedact(values []string) bool {
	if len(values) > MaxRedactCount {
		return false
	}
	total := 0
	for _, v := range values {
		if len(v) < MinRedactBytes || len(v) > MaxRedactBytes || strings.IndexByte(v, 0) >= 0 {
			return false
		}
		total += len(v)
		if total > MaxRedactTotal {
			return false
		}
	}
	return true
}

// ImageStatus is the projection of one container. Pointers are JSON null.
type ImageStatus struct {
	Container    *string `json:"container"`
	State        string  `json:"state"`
	ExitCode     *int    `json:"exit_code"`
	RestartCount int     `json:"restart_count"`
	StartedAt    *string `json:"started_at"`
	FinishedAt   *string `json:"finished_at"`
	Digest       *string `json:"digest"`
	Ports        []int   `json:"ports"`
}

// ActionResult is start, stop, restart, and remove.
type ActionResult struct {
	Status ImageStatus `json:"status"`
}

// StatusResult is container_status. SpecHash and FilesHash are empty when
// the container is missing or has no such label. FilesHash is not omitempty:
// the contract sends "" when there are no files.
type StatusResult struct {
	Status    ImageStatus `json:"status"`
	SpecHash  string      `json:"spec_hash"`
	FilesHash string      `json:"files_hash"`
}

// LogsResponse is container_logs.
type LogsResponse struct {
	Container string   `json:"container"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
}

// PublishedPort is one published binding in container_list.
type PublishedPort struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

// ContainerInfo is one container_list row. Labels contain only dev.deploybot.*.
type ContainerInfo struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	State  string            `json:"state"`
	Ports  []PublishedPort   `json:"ports"`
	Digest *string           `json:"digest"`
	Labels map[string]string `json:"labels"`
}

// ListResponse is container_list. Truncated is true when rows past the
// helper's cap were dropped. Deploybot-labelled rows are kept first.
type ListResponse struct {
	Items     []ContainerInfo `json:"items"`
	Truncated bool            `json:"truncated"`
}

// ListRequest is the empty object container_list requires.
type ListRequest struct{}

// ErrorBody is the image-verb refusal. Program verbs use a different shape.
type ErrorBody struct {
	Error string `json:"error"`
}

// Decode reads one JSON object into dest. Unknown fields and a second value
// are errors. The raw text must be an object, not an array or null.
func Decode(raw []byte, dest any) error {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || trim[0] != '{' {
		return errors.New("json")
	}
	dec := json.NewDecoder(bytes.NewReader(trim))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("trailing")
	}
	return nil
}

// Encode writes one JSON object. A marshal failure becomes a docker refusal
// so a response never falls back to fmt of the value.
func Encode(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return EncodeError("docker_unavailable")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// EncodeError is {"error":code}. code is a fixed reason, not a message.
func EncodeError(code string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(ErrorBody{Error: code})
	return bytes.TrimRight(buf.Bytes(), "\n")
}
