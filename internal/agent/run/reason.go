package run

import (
	"fmt"
	"strconv"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/probe"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// Deploy failure reasons (agent-protocol v1.19). Every reason is built by
// the agent from fixed words, a refusal code, a container state, or a
// health check's protocol, port, path and answer. None carries helper,
// Docker, registry or container text, so it never holds an env value.

const (
	whyNotHealthyInTime = "the container did not become healthy in time"
	whyDockerUnhealthy  = "the Docker health check reported unhealthy"
	whyDockerStarting   = "the Docker health check was still starting when the wait ended"
	whyNotRunning       = "the container was not running"
	whyHelper           = "the Docker helper on the host failed"
)

// maxReasonBytes is the protocol cap on reason.
const maxReasonBytes = 512

// codeWhy is the reason for a helper refusal code.
func codeWhy(code string) string {
	switch code {
	case programs.ReasonImage:
		return "the image could not be pulled: the tag is missing or the registry refused the pull"
	case programs.ReasonPolicy:
		return "the host policy (runner.toml) does not allow this deploy"
	case programs.ReasonBadSpec:
		return "the host found the spec invalid"
	case programs.ReasonConflict:
		return "another container is in the way on the host"
	case programs.ReasonDocker:
		return "Docker is not reachable on the host"
	case programs.ReasonNetworksDocker:
		return "shared networks need Docker 25 or later on the host"
	case programs.ReasonHelper:
		return "the Docker helper is not available on the host"
	case "":
		return ""
	default:
		return "the host refused the deploy (" + code + ")"
	}
}

// stateWhy is the reason for a container that stopped or restarts.
func stateWhy(st images.ImageStatus) string {
	switch {
	case st.State == "restarting" || st.RestartCount > 0:
		if st.ExitCode != nil && *st.ExitCode != 0 {
			return fmt.Sprintf("the container kept restarting (%d restarts, last exit code %d)", st.RestartCount, *st.ExitCode)
		}
		return fmt.Sprintf("the container kept restarting (%d restarts)", st.RestartCount)
	case (st.State == "exited" || st.State == "dead") && st.ExitCode != nil:
		return fmt.Sprintf("the container exited with code %d", *st.ExitCode)
	case st.State == "exited" || st.State == "dead":
		return "the container stopped"
	case st.State == "missing":
		return "the container is gone"
	default:
		return whyNotHealthyInTime
	}
}

// stableWhy is the reason when a wait without health checks ends.
func stableWhy(last images.ImageStatus, need time.Duration) string {
	if last.State != "running" {
		return whyNotRunning
	}
	return fmt.Sprintf("the container did not stay running for %s", need.Round(time.Second))
}

// probeWhy names the first failing health check and its answer.
func probeWhy(c protocol.HealthCheck, res probe.Result) string {
	target := c.Protocol + " :" + strconv.Itoa(c.Port)
	if c.Protocol == "http" {
		path := c.Path
		if path == "" {
			path = "/"
		}
		target = "GET :" + strconv.Itoa(c.Port) + path
	}
	var why string
	switch {
	case res.HTTPStatus != nil:
		why = fmt.Sprintf("health check %s answered %d", target, *res.HTTPStatus)
		if c.ExpectStatusMin > 0 && c.ExpectStatusMax > 0 {
			why += fmt.Sprintf(" (expected %d-%d)", c.ExpectStatusMin, c.ExpectStatusMax)
		}
	case res.Result == "timeout":
		why = "health check " + target + " timed out"
	case res.Result == "connection_refused":
		why = "health check " + target + ": connection refused (nothing listens on that port)"
	default:
		why = "health check " + target + " failed"
	}
	if len(why) > maxReasonBytes {
		why = why[:maxReasonBytes]
	}
	return why
}

// v1.21 item 4 reasons. The key is a validated env key name from the
// helper, never a value.
const (
	whyHostEnvFile  = "host_env_file: the host env file for this entry is missing or fails its checks (root:root, mode 0600, one link, at most 64 KiB, KEY=VALUE lines)"
	whyHostEnvImage = "host_env_image: the host env file of this entry is bound to other images (host_env_images in runner.toml)"
	envKeyMaxBytes  = 128
)

// detailWhy is the reason for a container_deploy refusal with a v1.21
// detail, or "" when the detail is absent or not one the agent knows.
func detailWhy(r imageRefusalBody) string {
	switch {
	case r.detail == images.DetailHostEnvFile && r.code == programs.ReasonPolicy:
		return whyHostEnvFile
	case r.detail == images.DetailHostEnvImage && r.code == programs.ReasonPolicy:
		return whyHostEnvImage
	case r.detail == images.DetailEnvConflict && r.code == programs.ReasonBadSpec && validEnvKeyName(r.key):
		return "env_conflict " + r.key + ": the host env file sets a key that the entry's env also sets or that no container env may set"
	default:
		return ""
	}
}

// refusalWhy prefers the v1.21 detail sentence over the code's sentence.
func refusalWhy(code, detail string) string {
	if detail != "" {
		return detail
	}
	return codeWhy(code)
}

// validEnvKeyName is the host env key rule ^[A-Za-z_][A-Za-z0-9_]{0,127}$.
func validEnvKeyName(k string) bool {
	if k == "" || len(k) > envKeyMaxBytes {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		alpha := c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if !alpha && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}
