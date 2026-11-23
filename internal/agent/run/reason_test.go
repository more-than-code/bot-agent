package run

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// agent-protocol v1.19: a deploy or rollback that does not succeed says why.

func reasonOf(t *testing.T, res protocol.TaskResult) (string, bool) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(marshalPayload(t, res.Payload), &body); err != nil {
		t.Fatal(err)
	}
	why, ok := body["reason"].(string)
	return why, ok
}

func TestDeployReason(t *testing.T) {
	t.Run("a restart loop", func(t *testing.T) {
		port := closedPort(t)
		a, h, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		seq.post = runningStatus(0)
		h.fn = restartAfter(seq, 1)
		a.configured = failingCheck(port)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, port), 300)
		assertDeploy(t, res, "rolled_back", "failed")
		if why, _ := reasonOf(t, res); why != "the container kept restarting (1 restarts)" {
			t.Fatalf("reason %q", why)
		}
	})

	t.Run("a health check nobody answers", func(t *testing.T) {
		port := closedPort(t)
		a, _, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		a.configured = failingCheck(port)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, port), 120)
		assertDeploy(t, res, "failed", "failed")
		want := fmt.Sprintf("health check tcp :%d: connection refused (nothing listens on that port)", port)
		if why, _ := reasonOf(t, res); why != want {
			t.Fatalf("reason %q want %q", why, want)
		}
	})

	t.Run("Docker health unhealthy", func(t *testing.T) {
		unhealthy := "unhealthy"
		a, _, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = strings.Repeat("ab", 32)
		seq.post = runningStatus(0)
		seq.health = &unhealthy
		res := mustImage(t, a, "image_deploy", specWithHealth(false, hcFast), 300)
		assertDeploy(t, res, "rolled_back", "failed")
		if why, _ := reasonOf(t, res); why != whyDockerUnhealthy {
			t.Fatalf("reason %q", why)
		}
	})

	t.Run("a pull refusal", func(t *testing.T) {
		a, _, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.pullCode = "image_unavailable"
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, 8080), 120)
		assertDeploy(t, res, "image_unavailable", "failed")
		if why, _ := reasonOf(t, res); !strings.HasPrefix(why, "the image could not be pulled") {
			t.Fatalf("reason %q", why)
		}
	})

	t.Run("a success has none", func(t *testing.T) {
		a, _, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = runningStatus(0)
		res := mustImage(t, a, "image_deploy", serviceSpec(false, 30, 8080), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if why, ok := reasonOf(t, res); ok {
			t.Fatalf("reason %q on success", why)
		}
	})
}

func TestStateWhy(t *testing.T) {
	one, three := 1, 3
	cases := []struct {
		st   images.ImageStatus
		want string
	}{
		{images.ImageStatus{State: "exited", ExitCode: &one}, "the container exited with code 1"},
		{images.ImageStatus{State: "restarting", RestartCount: 4, ExitCode: &three}, "the container kept restarting (4 restarts, last exit code 3)"},
		{images.ImageStatus{State: "dead"}, "the container stopped"},
		{images.ImageStatus{State: "missing"}, "the container is gone"},
	}
	for _, c := range cases {
		if got := stateWhy(c.st); got != c.want {
			t.Fatalf("%+v: %q", c.st, got)
		}
	}
	if codeWhy("weird") != "the host refused the deploy (weird)" || codeWhy("") != "" {
		t.Fatal("codeWhy")
	}
}

func TestValidateDeployReason(t *testing.T) {
	base := `{"outcome":"%s","digest":null,"exit_code":null,"status":{"container":null,"state":"missing","exit_code":null,"restart_count":0,"started_at":null,"finished_at":null,"digest":null,"ports":[]},"lines":[]%s}`
	if err := protocol.ValidateImageDeploy([]byte(fmt.Sprintf(base, "failed", `,"reason":"health check tcp :80 timed out"`))); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		fmt.Sprintf(base, "succeeded", `,"reason":"x"`),
		fmt.Sprintf(base, "failed", `,"reason":"a\nb"`),
		fmt.Sprintf(base, "failed", `,"reason":""`),
		fmt.Sprintf(base, "failed", `,"reason":"`+strings.Repeat("x", 513)+`"`),
	} {
		if protocol.ValidateImageDeploy([]byte(bad)) == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
