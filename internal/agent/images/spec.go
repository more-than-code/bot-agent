package images

import "strings"

// Port is one published port. HostIp is chosen by the helper, not this struct.
type Port struct {
	Host      int  `json:"host"`
	Container int  `json:"container"`
	Public    bool `json:"public"`
}

// Volume is a named project volume. Name is the short form; the helper
// prefixes the project slug itself.
type Volume struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// ImageSpec is api-v1 v1.8 ImageSpec. EnvSets and SecretEnv are checked for
// shape only: the helper does not look up sets. The merged values arrive in
// the deploy request's env object.
type ImageSpec struct {
	Kind            string            `json:"kind"`
	Image           string            `json:"image"`
	Command         []string          `json:"command"`
	Entrypoint      []string          `json:"entrypoint"`
	Ports           []Port            `json:"ports"`
	Env             map[string]string `json:"env"`
	EnvSets         []string          `json:"env_sets"`
	SecretEnv       map[string]string `json:"secret_env"`
	Volumes         []Volume          `json:"volumes"`
	Files           []SpecFile        `json:"files,omitempty"`
	Restart         string            `json:"restart"`
	User            string            `json:"user"`
	HealthyAfterSec int               `json:"healthy_after_sec"`
	MemoryMB        *int              `json:"memory_mb"`
	TimeoutSec      int               `json:"timeout_sec"`
	// Healthcheck is api-v1 v1.11 item 7. Nil means the key was absent.
	// The API applies defaults; this process rejects an out-of-range number
	// and does not fill those defaults. A job cannot carry one.
	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`
	// Logging is api-v1 v1.14. Nil means absent or null: the create uses
	// 10 MB and 3 files. Both keys are required when the object is present.
	// A job may carry one. This process does not fill defaults onto the object.
	Logging *Logging `json:"logging,omitempty"`
}

// Log file bounds for ImageSpec logging (agent-protocol v1.8).
const (
	logSizeDefault  = 10
	logFilesDefault = 3
	logSizeMin      = 1
	logSizeMax      = 100
	logFilesMin     = 1
	logFilesMax     = 10
)

// Logging is the optional json-file cap. Both integers are required.
type Logging struct {
	MaxSizeMB int `json:"max_size_mb"`
	MaxFiles  int `json:"max_files"`
}

// Healthcheck is the catalog check for a service container.
// Test is the argv only. The helper prepends CMD.
type Healthcheck struct {
	Test           []string `json:"test"`
	IntervalSec    int      `json:"interval_sec"`
	TimeoutSec     int      `json:"timeout_sec"`
	Retries        int      `json:"retries"`
	StartPeriodSec int      `json:"start_period_sec"`
}

// Validate applies the host-side half of "Writing an image entry" rules 2–11.
// It returns the field name, or "" when the spec is acceptable. Defaults are
// written back onto s: service restart "unless-stopped", job restart "no",
// healthy_after_sec 30, timeout_sec 600. The value of a bad field is not
// included in the result.
func (s *ImageSpec) Validate() string {
	if s == nil {
		return "spec"
	}
	switch s.Kind {
	case "service", "job":
	default:
		return "kind"
	}
	if _, err := Parse(s.Image); err != nil {
		return "image"
	}
	if !validArgv(s.Command) {
		return "command"
	}
	if !validArgv(s.Entrypoint) {
		return "entrypoint"
	}
	if field := checkPorts(s.Ports); field != "" {
		return field
	}
	if field := inlineEnv(s.Env); field != "" {
		return field
	}
	if field := checkSets(s.EnvSets); field != "" {
		return field
	}
	if field := checkSecretRefs(s.SecretEnv); field != "" {
		return field
	}
	if field := checkVolumes(s.Volumes); field != "" {
		return field
	}
	if field := checkSpecFiles(s.Files, s.Volumes); field != "" {
		return field
	}
	if !ValidUser(s.User) {
		return "user"
	}
	if s.MemoryMB != nil && (*s.MemoryMB < 16 || *s.MemoryMB > 65536) {
		return "memory_mb"
	}
	if field := checkHealthcheck(s); field != "" {
		return field
	}
	if field := checkLogging(s); field != "" {
		return field
	}
	if s.Kind == "job" {
		s.Restart = "no"
		if s.HealthyAfterSec != 0 {
			return "healthy_after_sec"
		}
		if s.TimeoutSec == 0 {
			s.TimeoutSec = 600
		} else if s.TimeoutSec < 10 || s.TimeoutSec > 3600 {
			return "timeout_sec"
		}
		return ""
	}
	if s.Restart == "" {
		s.Restart = "unless-stopped"
	} else if s.Restart != "no" && s.Restart != "on-failure" && s.Restart != "unless-stopped" {
		return "restart"
	}
	if s.TimeoutSec != 0 {
		return "timeout_sec"
	}
	if s.HealthyAfterSec == 0 {
		s.HealthyAfterSec = 30
	} else if s.HealthyAfterSec < 5 || s.HealthyAfterSec > 600 {
		return "healthy_after_sec"
	}
	return ""
}

func checkLogging(s *ImageSpec) string {
	l := s.Logging
	if l == nil {
		return ""
	}
	if l.MaxSizeMB < logSizeMin || l.MaxSizeMB > logSizeMax {
		return "logging.max_size_mb"
	}
	if l.MaxFiles < logFilesMin || l.MaxFiles > logFilesMax {
		return "logging.max_files"
	}
	return ""
}

// LogLimit is the json-file cap for one create. A nil logging object is
// 10 MB and 3 files. ok is false when logging is set and a bound fails.
func LogLimit(s ImageSpec) (sizeMB, files int, ok bool) {
	if checkLogging(&s) != "" {
		return 0, 0, false
	}
	if s.Logging == nil {
		return logSizeDefault, logFilesDefault, true
	}
	return s.Logging.MaxSizeMB, s.Logging.MaxFiles, true
}

func checkHealthcheck(s *ImageSpec) string {
	h := s.Healthcheck
	if h == nil {
		return ""
	}
	if s.Kind == "job" {
		return "healthcheck"
	}
	if len(h.Test) < 1 || len(h.Test) > 32 {
		return "healthcheck"
	}
	for _, arg := range h.Test {
		if len(arg) < 1 || len(arg) > 1024 || strings.IndexByte(arg, 0) >= 0 {
			return "healthcheck"
		}
	}
	if h.IntervalSec < 5 || h.IntervalSec > 300 {
		return "interval_sec"
	}
	if h.TimeoutSec < 1 || h.TimeoutSec > 60 || h.TimeoutSec >= h.IntervalSec {
		return "timeout_sec"
	}
	if h.Retries < 1 || h.Retries > 10 {
		return "retries"
	}
	if h.StartPeriodSec < 0 || h.StartPeriodSec > 600 {
		return "start_period_sec"
	}
	return ""
}

func checkPorts(ports []Port) string {
	if len(ports) > maxPorts {
		return "ports"
	}
	hosts := map[int]bool{}
	for _, p := range ports {
		if p.Host < 1 || p.Host > 65535 || p.Container < 1 || p.Container > 65535 {
			return "ports"
		}
		if hosts[p.Host] {
			return "ports"
		}
		hosts[p.Host] = true
	}
	return ""
}

func checkVolumes(vols []Volume) string {
	if len(vols) > maxVolumes {
		return "volumes"
	}
	names := map[string]bool{}
	targets := map[string]bool{}
	for _, v := range vols {
		if !ValidVolume(v.Name) || !validTarget(v.Target) || names[v.Name] || targets[v.Target] {
			return "volumes"
		}
		names[v.Name] = true
		targets[v.Target] = true
	}
	return ""
}
