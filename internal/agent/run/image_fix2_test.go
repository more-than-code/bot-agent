package run

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestTruncatedContainerListIsComplete(t *testing.T) {
	proc, _, passwd := fixtureProc(t, 9)
	idPath, host := writeMachineID(t)
	digest := imgDigest
	items := make([]images.ContainerInfo, 0, 200)
	items = append(items, images.ContainerInfo{
		ID: strings.Repeat("ab", 32), Name: "zzz-last", State: "running", Digest: &digest,
		Labels: ownedTestLabels(host, "acme", "web"),
		Ports:  []images.PublishedPort{},
	})
	for i := 0; i < 199; i++ {
		items = append(items, images.ContainerInfo{
			ID: strings.Repeat("cd", 32), Name: "aaa-" + strings.Repeat("x", 3), State: "running",
			Ports: []images.PublishedPort{{HostIP: "127.0.0.1", HostPort: 20000 + i, ContainerPort: 80}},
		})
	}
	h := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		switch verb {
		case "list":
			return programs.EncodeList(nil), nil
		case images.VerbList:
			return images.Encode(images.ListResponse{Items: items, Truncated: true}), nil
		default:
			return nil, errHelper
		}
	}}
	var buf bytes.Buffer
	a := &agent{
		opt: Options{ProcRoot: proc, Passwd: passwd, MachineID: idPath, Helper: h, Log: stdLog(&buf)},
		policy: policy.Policy{
			Exists: true, Valid: true, Enabled: true,
			Images: []string{"ghcr.io/acme/*"}, Projects: []string{"acme"},
		},
	}
	res, complete := a.discover(context.Background())
	if !complete || !hasService(res.Services, "web") {
		t.Fatalf("complete %v services %+v", complete, res.Services)
	}
	if _, complete := a.discover(context.Background()); !complete || !hasService(a.services, "web") {
		t.Fatal("second pass")
	}
	if n := strings.Count(buf.String(), "bot-agent: container list truncated"); n != 1 {
		t.Fatalf("logs %d\n%s", n, buf.String())
	}
}
