package run

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

func TestAgentForwardsLogging(t *testing.T) {
	for _, kind := range []string{"service", "job"} {
		t.Run(kind, func(t *testing.T) {
			a, _, _, seq := imageRun(t)
			seq.pre = missingStatus()
			var spec []byte
			if kind == "job" {
				seq.post = exitedStatus(0)
				spec = jobSpec(10)
			} else {
				seq.post = runningStatus(0)
				spec = serviceSpec(false, 5, 3031)
			}
			spec = bytes.Replace(spec, []byte(`"kind":"`+kind+`"`), []byte(`"kind":"`+kind+`","logging":{"max_size_mb":5,"max_files":2}`), 1)
			res := mustImage(t, a, "image_deploy", spec, 120)
			assertDeploy(t, res, "succeeded", "succeeded")
			var sent images.DeployRequest
			if json.Unmarshal(seq.deployBody, &sent) != nil || sent.Spec.Logging == nil {
				t.Fatal(string(seq.deployBody))
			}
			if sent.Spec.Logging.MaxSizeMB != 5 || sent.Spec.Logging.MaxFiles != 2 {
				t.Fatalf("%+v", sent.Spec.Logging)
			}
		})
	}

	t.Run("absent stays absent", func(t *testing.T) {
		a, _, _, seq := imageRun(t)
		seq.pre = missingStatus()
		seq.post = exitedStatus(0)
		res := mustImage(t, a, "image_deploy", jobSpec(10), 120)
		assertDeploy(t, res, "succeeded", "succeeded")
		if strings.Contains(string(seq.deployBody), `"logging"`) {
			t.Fatal(string(seq.deployBody))
		}
	})
}

func TestAgentRefusesLoggingBeforeHelper(t *testing.T) {
	cases := []struct {
		name string
		spec []byte
	}{
		{"size", withRunLogging(serviceSpec(false, 5, 3031), `{"max_size_mb":101,"max_files":3}`)},
		{"files", withRunLogging(jobSpec(10), `{"max_size_mb":10,"max_files":11}`)},
		{"one key", withRunLogging(serviceSpec(false, 5, 3031), `{"max_files":3}`)},
		{"unknown", withRunLogging(jobSpec(10), `{"max_size_mb":10,"max_files":3,"extra":1}`)},
		{"fraction", withRunLogging(serviceSpec(false, 5, 3031), `{"max_size_mb":1.5,"max_files":1}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h, _, _ := imageRun(t)
			rej, _, ok := a.execute(context.Background(), imageTask("image_deploy", tc.spec, 120))
			if ok || rej.ErrorCode != "bad_spec" {
				t.Fatalf("ok %v code %s", ok, rej.ErrorCode)
			}
			if len(h.Calls()) != 0 {
				t.Fatalf("helper called %v", h.Calls())
			}
		})
	}
}

func TestImageLogsRequestUnchanged(t *testing.T) {
	a, h, _, seq := imageRun(t)
	res := mustImage(t, a, "image_logs", []byte(`{"project":"acme","entry":"web","lines":20}`), 60)
	if res.State != "succeeded" || res.ErrorCode != nil {
		t.Fatalf("%+v", res)
	}
	if len(seq.logReqs) != 1 {
		t.Fatalf("%+v", seq.logReqs)
	}
	got := seq.logReqs[0]
	if got.Project != "acme" || got.Entry != "web" || got.Lines != 20 || got.Redact != nil || got.RedactEnv != nil {
		t.Fatalf("%+v", got)
	}
	verbOrder(t, h, images.VerbLogs)

	a, h, _, _ = imageRun(t)
	rej, _, ok := a.execute(context.Background(), imageTask("image_logs", []byte(`{"project":"acme","entry":"web","lines":20,"logging":{"max_size_mb":10,"max_files":3}}`), 60))
	if ok || rej.ErrorCode != "bad_spec" || len(h.Calls()) != 0 {
		t.Fatalf("ok %v code %s calls %v", ok, rej.ErrorCode, h.Calls())
	}
}

func withRunLogging(spec []byte, logging string) []byte {
	return bytes.Replace(spec, []byte(`"kind":"`), []byte(`"logging":`+logging+`,"kind":"`), 1)
}
