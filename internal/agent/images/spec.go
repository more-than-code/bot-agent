package images

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
	Restart         string            `json:"restart"`
	User            string            `json:"user"`
	HealthyAfterSec int               `json:"healthy_after_sec"`
	MemoryMB        *int              `json:"memory_mb"`
	TimeoutSec      int               `json:"timeout_sec"`
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
	if !ValidUser(s.User) {
		return "user"
	}
	if s.MemoryMB != nil && (*s.MemoryMB < 16 || *s.MemoryMB > 65536) {
		return "memory_mb"
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
