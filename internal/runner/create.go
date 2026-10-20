package runner

import (
	"sort"
	"strconv"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// The structs below are the whole ContainerCreate body. A new field here is
// a new key on the wire. Optional slices use omitempty so an unset command
// is absent rather than an empty value of some other Docker field. HostIp is
// never omitempty: an empty string would publish on every interface.

type emptyObj struct{}

type portBind struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type mountBody struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

type restartBody struct {
	Name string `json:"Name"`
}

type hostConfigBody struct {
	PortBindings  map[string][]portBind `json:"PortBindings,omitempty"`
	Mounts        []mountBody           `json:"Mounts,omitempty"`
	RestartPolicy restartBody           `json:"RestartPolicy"`
	NetworkMode   string                `json:"NetworkMode"`
	CgroupnsMode  string                `json:"CgroupnsMode"`
	PidsLimit     int64                 `json:"PidsLimit"`
	Memory        int64                 `json:"Memory,omitempty"`
	MemorySwap    int64                 `json:"MemorySwap,omitempty"`
}

type endpointBody struct {
	Aliases []string `json:"Aliases"`
}

type networkConfigBody struct {
	EndpointsConfig map[string]endpointBody `json:"EndpointsConfig"`
}

// healthcheckBody is the Engine ContainerCreate Healthcheck object.
// API v1.47 already accepts it. Durations are nanoseconds, the same unit as
// time.Duration. The inner numbers are not omitempty: StartPeriod 0 is sent.
// Test is "CMD" plus the catalog argv. The key is omitted when the spec has
// no healthcheck, so the image keeps its own check.
type healthcheckBody struct {
	Test        []string `json:"Test"`
	Interval    int64    `json:"Interval"`
	Timeout     int64    `json:"Timeout"`
	Retries     int      `json:"Retries"`
	StartPeriod int64    `json:"StartPeriod"`
}

type createBody struct {
	Image            string              `json:"Image"`
	Cmd              []string            `json:"Cmd,omitempty"`
	Entrypoint       []string            `json:"Entrypoint,omitempty"`
	Env              []string            `json:"Env,omitempty"`
	Labels           map[string]string   `json:"Labels"`
	ExposedPorts     map[string]emptyObj `json:"ExposedPorts,omitempty"`
	User             string              `json:"User,omitempty"`
	Healthcheck      *healthcheckBody    `json:"Healthcheck,omitempty"`
	HostConfig       hostConfigBody      `json:"HostConfig"`
	NetworkingConfig networkConfigBody   `json:"NetworkingConfig"`
}

func buildCreate(project, entry, digest, specHash, host string, spec images.ImageSpec, env map[string]string, files []images.MountFile, patterns []string, allowPublic bool) ([]byte, string) {
	ref, err := images.Parse(spec.Image)
	if err != nil {
		return nil, programs.ReasonBadSpec
	}
	if !images.ValidDigest(digest) {
		return nil, programs.ReasonBadSpec
	}
	pinned, err := images.Parse(ref.Name + "@" + digest)
	if err != nil || !images.Match(ref, patterns) || !images.Match(pinned, patterns) {
		return nil, programs.ReasonPolicy
	}
	net, ok := images.NetworkName(project)
	if !ok {
		return nil, programs.ReasonBadSpec
	}
	restart := spec.Restart
	if spec.Kind == "job" {
		restart = "no"
	}
	body := createBody{
		Image:  pinned.Name + "@" + digest,
		Labels: containerLabels(project, entry, specHash, host, images.FilesHash(files)),
		HostConfig: hostConfigBody{
			RestartPolicy: restartBody{Name: restart},
			NetworkMode:   net,
			CgroupnsMode:  "private",
			PidsLimit:     1024,
		},
		NetworkingConfig: networkConfigBody{
			EndpointsConfig: map[string]endpointBody{
				net: {Aliases: []string{entry}},
			},
		},
	}
	if len(spec.Command) > 0 {
		body.Cmd = append([]string(nil), spec.Command...)
	}
	if len(spec.Entrypoint) > 0 {
		body.Entrypoint = append([]string(nil), spec.Entrypoint...)
	}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			body.Env = append(body.Env, k+"="+env[k])
		}
	}
	if spec.User != "" {
		body.User = spec.User
	}
	if len(spec.Ports) > 0 {
		body.ExposedPorts = map[string]emptyObj{}
		body.HostConfig.PortBindings = map[string][]portBind{}
		for _, p := range spec.Ports {
			ip := "127.0.0.1"
			if p.Public {
				if !allowPublic {
					return nil, programs.ReasonPolicy
				}
				ip = "0.0.0.0"
			}
			key := strconv.Itoa(p.Container) + "/tcp"
			body.ExposedPorts[key] = emptyObj{}
			body.HostConfig.PortBindings[key] = append(body.HostConfig.PortBindings[key], portBind{
				HostIP: ip, HostPort: strconv.Itoa(p.Host),
			})
		}
	}
	for _, v := range spec.Volumes {
		src, ok := images.VolumeName(project, v.Name)
		if !ok {
			return nil, programs.ReasonBadSpec
		}
		body.HostConfig.Mounts = append(body.HostConfig.Mounts, mountBody{
			Type: "volume", Source: src, Target: v.Target, ReadOnly: v.ReadOnly,
		})
	}
	// An existing slot may contain other valid 0444 files from a previous
	// generation. Only the files listed in this request are mounted.
	for _, f := range files {
		src, ok := images.MountSource(project, entry, f.Name)
		if !ok {
			return nil, programs.ReasonBadSpec
		}
		// ReadOnly is fixed. mountBody has no BindOptions field, so the
		// JSON has none. Source is the production store path, not a test root.
		body.HostConfig.Mounts = append(body.HostConfig.Mounts, mountBody{
			Type: "bind", Source: src, Target: f.Target, ReadOnly: true,
		})
	}
	if spec.MemoryMB != nil {
		mem := int64(*spec.MemoryMB) * 1024 * 1024
		body.HostConfig.Memory = mem
		body.HostConfig.MemorySwap = mem
	}
	if spec.Healthcheck != nil {
		hc := spec.Healthcheck
		body.Healthcheck = &healthcheckBody{
			Test:        append([]string{"CMD"}, hc.Test...),
			Interval:    int64(hc.IntervalSec) * int64(time.Second),
			Timeout:     int64(hc.TimeoutSec) * int64(time.Second),
			Retries:     hc.Retries,
			StartPeriod: int64(hc.StartPeriodSec) * int64(time.Second),
		}
	}
	raw, err := marshalJSON(body)
	if err != nil {
		return nil, programs.ReasonDocker
	}
	return raw, ""
}

func containerLabels(project, entry, specHash, host, filesHash string) map[string]string {
	labels := map[string]string{
		images.LabelOwner:    images.OwnerValue,
		images.LabelHost:     host,
		images.LabelProject:  project,
		images.LabelEntry:    entry,
		images.LabelSpecHash: specHash,
	}
	if filesHash != "" {
		labels[images.LabelFilesHash] = filesHash
	}
	return labels
}
