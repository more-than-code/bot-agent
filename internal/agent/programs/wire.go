package programs

import (
	"bytes"
	"encoding/json"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

// MaxHelperOut is the largest helper stdout the agent will parse
// for every verb except container_list. A larger buffer is not a
// complete JSON object.
const MaxHelperOut = 256 * 1024

// MaxListOut is the container_list stdout cap. Two hundred rows with a
// 255-byte name, the five deploybot labels, and sixteen published ports
// do not fit in MaxHelperOut. Other verbs stay at MaxHelperOut.
const MaxListOut = 512 * 1024

// Status is the helper's view of one unit. Ports may be empty; the agent
// fills them from the sockets verb.
type Status struct {
	Unit        string  `json:"unit"`
	ActiveState string  `json:"active_state"`
	SubState    string  `json:"sub_state"`
	MainPID     *int    `json:"main_pid"`
	StartedAt   *string `json:"started_at"`
	Ports       []int   `json:"ports"`
}

// LogBatch is the helper's journal reply before the agent redacts again.
type LogBatch struct {
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
}

// UnitInfo is one row of the list verb.
type UnitInfo struct {
	Unit        string `json:"unit"`
	Project     string `json:"project"`
	Entry       string `json:"entry"`
	ActiveState string `json:"active_state"`
}

// FixedReason returns reason when it is one of the helper's allowlisted
// codes. Anything else becomes policy_denied so a helper cannot put paths
// or env values into the task result.
func FixedReason(reason string) string {
	switch reason {
	case ReasonPolicy, ReasonBadSpec, ReasonNotOwned, ReasonVerb, ReasonIO,
		ReasonConflict, ReasonDocker, ReasonImage:
		return reason
	default:
		return ReasonPolicy
	}
}

// FrameLimit is the request-body cap for one verb.
// Image verbs are 2 MiB. Every other verb stays at MaxFrame.
func FrameLimit(verb string) int {
	if images.IsVerb(verb) {
		return images.MaxFrame
	}
	return MaxFrame
}

// SocketLimit is the largest newline-terminated helper frame.
// The verb is not known until the frame is parsed, so the read allows
// an image body plus the wrapper.
func SocketLimit() int {
	return images.MaxFrame + images.SocketSlack
}

// KnownVerb reports whether verb is one the helper implements.
// Image verbs live in the images package so the two lists cannot drift.
func KnownVerb(verb string) bool {
	switch verb {
	case "start", "stop", "restart", "status", "logs", "sockets", "list":
		return true
	default:
		return images.IsVerb(verb)
	}
}

// Verb is the helper verb for a task kind.
func Verb(kind string) (string, bool) {
	switch kind {
	case "program_start":
		return "start", true
	case "program_stop":
		return "stop", true
	case "program_restart":
		return "restart", true
	case "program_status":
		return "status", true
	case "program_logs":
		return "logs", true
	default:
		return "", false
	}
}

func encode(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte(`{"ok":false,"reason":"io_error"}`)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// EncodeRefusal is the helper's only error shape: ok false and one reason code.
func EncodeRefusal(reason string) []byte {
	return encode(struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}{OK: false, Reason: reason})
}

// EncodeStatus, EncodeLogs, EncodeSockets, and EncodeList are the success
// objects. They live here so the agent and the helper share one schema.
func EncodeStatus(st Status) []byte {
	return EncodeStart(st, true)
}

// EncodeStart is a start or restart reply. fresh is false when systemctl
// exited non-zero or ActiveEnterTimestampMonotonic did not move forward.
// The agent treats that as start_failed instead of a still-active unit.
func EncodeStart(st Status, fresh bool) []byte {
	if st.Ports == nil {
		st.Ports = []int{}
	}
	return encode(struct {
		OK     bool   `json:"ok"`
		Status Status `json:"status"`
		Fresh  bool   `json:"fresh"`
	}{OK: true, Status: st, Fresh: fresh})
}

// StatusFresh reports the start/restart truth bit. A missing bit is false,
// so a reply that skipped the monotonic check cannot count as success.
func StatusFresh(raw []byte) bool {
	var wrap struct {
		Fresh bool `json:"fresh"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return false
	}
	return wrap.Fresh
}

func EncodeLogs(batch LogBatch) []byte {
	if batch.Lines == nil {
		batch.Lines = []string{}
	}
	return encode(struct {
		OK   bool     `json:"ok"`
		Logs LogBatch `json:"logs"`
	}{OK: true, Logs: batch})
}

func EncodeSockets(ports []int, user *string) []byte {
	if ports == nil {
		ports = []int{}
	}
	return encode(struct {
		OK    bool    `json:"ok"`
		Ports []int   `json:"ports"`
		User  *string `json:"user"`
	}{OK: true, Ports: ports, User: user})
}

func EncodeList(units []UnitInfo) []byte {
	if units == nil {
		units = []UnitInfo{}
	}
	return encode(struct {
		OK    bool       `json:"ok"`
		Units []UnitInfo `json:"units"`
	}{OK: true, Units: units})
}

func decodeOne(raw []byte, dest any) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return "", err
	}
	if dec.More() {
		return "", bytes.ErrTooLarge
	}
	return "", nil
}

// DecodeStatus reads a status reply. A non-empty reason is a refusal.
// A non-nil error means the bytes are not a helper object.
func DecodeStatus(raw []byte) (Status, string, error) {
	var wrap struct {
		OK     bool    `json:"ok"`
		Reason string  `json:"reason"`
		Status *Status `json:"status"`
		Fresh  bool    `json:"fresh"`
	}
	if _, err := decodeOne(raw, &wrap); err != nil {
		return Status{}, "", err
	}
	if !wrap.OK {
		if wrap.Reason == "" {
			return Status{}, "", bytes.ErrTooLarge
		}
		return Status{}, wrap.Reason, nil
	}
	if wrap.Status == nil {
		return Status{}, "", bytes.ErrTooLarge
	}
	if wrap.Status.Ports == nil {
		wrap.Status.Ports = []int{}
	}
	return *wrap.Status, "", nil
}

// DecodeLogs reads a logs reply.
func DecodeLogs(raw []byte) (LogBatch, string, error) {
	var wrap struct {
		OK     bool      `json:"ok"`
		Reason string    `json:"reason"`
		Logs   *LogBatch `json:"logs"`
	}
	if _, err := decodeOne(raw, &wrap); err != nil {
		return LogBatch{}, "", err
	}
	if !wrap.OK {
		if wrap.Reason == "" {
			return LogBatch{}, "", bytes.ErrTooLarge
		}
		return LogBatch{}, wrap.Reason, nil
	}
	if wrap.Logs == nil {
		return LogBatch{}, "", bytes.ErrTooLarge
	}
	if wrap.Logs.Lines == nil {
		wrap.Logs.Lines = []string{}
	}
	return *wrap.Logs, "", nil
}

// DecodeSockets reads a sockets reply.
func DecodeSockets(raw []byte) ([]int, *string, string, error) {
	var wrap struct {
		OK     bool    `json:"ok"`
		Reason string  `json:"reason"`
		Ports  []int   `json:"ports"`
		User   *string `json:"user"`
	}
	if _, err := decodeOne(raw, &wrap); err != nil {
		return nil, nil, "", err
	}
	if !wrap.OK {
		if wrap.Reason == "" {
			return nil, nil, "", bytes.ErrTooLarge
		}
		return nil, nil, wrap.Reason, nil
	}
	if wrap.Ports == nil {
		wrap.Ports = []int{}
	}
	return wrap.Ports, wrap.User, "", nil
}

// DecodeList reads a list reply.
func DecodeList(raw []byte) ([]UnitInfo, string, error) {
	var wrap struct {
		OK     bool       `json:"ok"`
		Reason string     `json:"reason"`
		Units  []UnitInfo `json:"units"`
	}
	if _, err := decodeOne(raw, &wrap); err != nil {
		return nil, "", err
	}
	if !wrap.OK {
		if wrap.Reason == "" {
			return nil, "", bytes.ErrTooLarge
		}
		return nil, wrap.Reason, nil
	}
	if wrap.Units == nil {
		return []UnitInfo{}, "", nil
	}
	return wrap.Units, "", nil
}
