package runner

import (
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// hostEnvMerge is agent-protocol v1.21 item 4 for container_deploy. When the
// entry has a host_env_files line, the file is read and checked again now,
// and its keys are added to a copy of req.Env. sum is the file's SHA-256 for
// LabelHostEnv, "" without a line. A non-nil refusal is the verb's answer:
// policy_denied with detail host_env_file, or bad_spec with detail
// env_conflict and the key. The caller must clear merged when done.
func hostEnvMerge(req images.DeployRequest, pol policy.Policy, sys Sys) (merged map[string]string, sum string, refusal []byte) {
	line, ok := pol.HostEnv(req.Project, req.Entry)
	if !ok {
		return req.Env, "", nil
	}
	// v1.21 item 7: the file's secrets go only to the image the host owner
	// named for this entry, by tag and by the pinned digest.
	if !hostEnvImageOK(line, req.Spec.Image, req.Digest) {
		return nil, "", envRefusal(sys, programs.ReasonPolicy, images.DetailHostEnvImage, "")
	}
	// v1.26: and only in the run the host owner pinned, so the server
	// cannot choose code that prints or sends the file's values.
	if field := hostEnvRunField(line, req); field != "" {
		return nil, "", envRefusal(sys, programs.ReasonPolicy, images.DetailHostEnvRun, field)
	}
	fileEnv, sum, err := policy.ReadHostEnv(sys.Root, line.Path, sys.envOwner)
	if err != nil {
		return nil, "", envRefusal(sys, programs.ReasonPolicy, images.DetailHostEnvFile, "")
	}
	defer clearMap(fileEnv)
	merged = make(map[string]string, len(req.Env)+len(fileEnv))
	for k, v := range req.Env {
		merged[k] = v
	}
	for k, v := range fileEnv {
		_, inEnv := req.Env[k]
		_, inSpec := req.Spec.Env[k]
		if inEnv || inSpec || policy.HostEnvKeyDenied(k) {
			clearMap(merged)
			return nil, "", envRefusal(sys, programs.ReasonBadSpec, images.DetailEnvConflict, k)
		}
		merged[k] = v
	}
	return merged, sum, nil
}

// envRefusal logs and encodes a refusal with a v1.21 detail. key is a
// validated env key name (never a value).
func envRefusal(sys Sys, code, detail, key string) []byte {
	if sys.Logf != nil {
		if key != "" {
			sys.Logf("bot-agent-runner: refuse %s %s %s %s", refusalVerb(sys.logVerb), code, detail, key)
		} else {
			sys.Logf("bot-agent-runner: refuse %s %s %s", refusalVerb(sys.logVerb), code, detail)
		}
	}
	return images.EncodeErrorDetail(code, detail, key)
}

// hostEnvCurrent is the SHA-256 of the entry's host env file now: "" when
// the entry has no line, ok false when the file fails its checks.
func hostEnvCurrent(pol policy.Policy, sys Sys, project, entry string) (sum string, ok bool) {
	line, has := pol.HostEnv(project, entry)
	if !has {
		return "", true
	}
	env, sum, err := policy.ReadHostEnv(sys.Root, line.Path, sys.envOwner)
	clearMap(env)
	if err != nil {
		return "", false
	}
	return sum, true
}

// envHashFor is the env_hash container_status reports for one container
// (v1.21 item 4): its env_hash label only while the host env file it was
// created with is still the entry's file now. A container created before a
// file changed, was added or was removed reports none, so the agent sees
// env as changed and deploys again (or pulls instead of the kept swap).
func envHashFor(labels map[string]string, current string, currentOK bool) string {
	if !currentOK || labels[images.LabelHostEnv] != current {
		return ""
	}
	return envHashOf(labels)
}

// hostEnvLogs is the log redaction for one container (v1.21 item 7). It
// fails closed: when the entry has a host env file, or the container was
// created with one, the values are redacted only while the file's sha256
// equals the container's label. Otherwise withhold is true and the caller
// returns no lines but withheldLine. vals are the values of 6 bytes and
// longer; shorter ones cannot be matched exactly (documented).
func hostEnvLogs(pol policy.Policy, sys Sys, project, entry string, labels map[string]string) (vals []string, withhold bool) {
	label := labels[images.LabelHostEnv]
	line, has := pol.HostEnv(project, entry)
	if !has {
		return nil, label != ""
	}
	env, sum, err := policy.ReadHostEnv(sys.Root, line.Path, sys.envOwner)
	defer clearMap(env)
	if err != nil || sum != label {
		return nil, true
	}
	vals = make([]string, 0, len(env))
	for _, v := range env {
		if len(v) >= images.MinRedactBytes {
			vals = append(vals, v)
		}
	}
	return vals, false
}

// withheldLine replaces a container's log lines when hostEnvLogs cannot
// redact them. Fixed text, no value.
const withheldLine = "[deploybot] log lines withheld: the host env file changed since this container was created, or cannot be read; deploy again to see logs"

// hostEnvImageOK is the v1.21 image binding: the spec image and its pinned
// digest both match the entry's host_env_images pattern.
func hostEnvImageOK(line policy.HostEnvFile, image, digest string) bool {
	ref, err := images.Parse(image)
	if err != nil || !line.HostEnvImageAllowed(ref) {
		return false
	}
	if digest == "" {
		return true
	}
	pinned, err := images.Parse(ref.Name + "@" + digest)
	return err == nil && line.HostEnvImageAllowed(pinned)
}

// hostEnvRunField is the v1.26 run pin check for an entry with a host env
// file: "" when the request runs the pinned shape, else the first field
// that differs (one of images.HostEnvRunFields). command and healthcheck
// test must equal the pins (absent without a line: the image's own); the
// entrypoint, user, mounts and the entry's own env must be empty, because
// each of them can change what code runs with the file's values or where
// that code sends them.
func hostEnvRunField(line policy.HostEnvFile, req images.DeployRequest) string {
	spec := req.Spec
	switch {
	case !argvEqual(spec.Command, line.Command):
		return "command"
	case len(spec.Entrypoint) > 0:
		return "entrypoint"
	case !healthcheckPinned(spec.Healthcheck, line.Healthcheck):
		return "healthcheck"
	case spec.User != "":
		return "user"
	case len(spec.Volumes) > 0:
		return "volumes"
	case len(spec.Files) > 0 || len(req.Files) > 0:
		return "files"
	case len(spec.Env) > 0 || len(spec.SecretEnv) > 0 || len(spec.EnvSets) > 0 || len(req.Env) > 0:
		return "env"
	default:
		return ""
	}
}

// healthcheckPinned: no healthcheck without a pin; with one, the same test
// argv (the timings stay the entry's).
func healthcheckPinned(hc *images.Healthcheck, pin []string) bool {
	if len(pin) == 0 {
		return hc == nil
	}
	return hc != nil && argvEqual(hc.Test, pin)
}

func argvEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hostNetDenied is the v1.21 item 7 re-check for a manual rollback, start
// or restart: a container created with host networking may run again only
// while its project is in host_network_projects.
func hostNetDenied(pol policy.Policy, project string, labels map[string]string) bool {
	return labels[images.LabelHostNetwork] == "true" && !pol.HostNetworkAllowed(project)
}

func clearMap(m map[string]string) {
	for k := range m {
		m[k] = ""
		delete(m, k)
	}
}
