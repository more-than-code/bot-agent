package discover

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/edge"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func edgeInfo(host, state string) images.ContainerInfo {
	digest := "sha256:" + strings.Repeat("cd", 32)
	return images.ContainerInfo{
		ID: "e", Name: "deploybot-edge", State: state, Digest: &digest,
		Labels: map[string]string{
			images.LabelOwner: images.OwnerValue, images.LabelHost: host, images.LabelKind: "edge",
		},
	}
}

func listeners() Result {
	return Result{Services: []protocol.Discovered{
		{Name: "port-80", Kind: "process", Ports: []int{80}},
		{Name: "port-443", Kind: "process", Ports: []int{443, 8443}},
		{Name: "sshd", Kind: "process", Ports: []int{22}, PID: protocol.Int(7), ProcessName: protocol.Str("sshd")},
	}, Unmapped: 3}
}

func findRow(t *testing.T, res Result, name string) (protocol.Discovered, bool) {
	t.Helper()
	for _, s := range res.Services {
		if s.Name == name {
			return s, true
		}
	}
	return protocol.Discovered{}, false
}

// v1.15 item 5: a running edge owns the host's unmapped 80 and 443; they
// are reported once, on the edge row, with owner kind edge and no project.
func TestApplyContainersEdgeRunning(t *testing.T) {
	const host = "0123456789abcdef"
	out := ApplyContainers(listeners(), []images.ContainerInfo{edgeInfo(host, "running")}, host, []string{"acme"})
	row, ok := findRow(t, out, edge.ContainerName)
	if !ok {
		t.Fatalf("%+v", out.Services)
	}
	b, _ := json.Marshal(row)
	want := `{"name":"deploybot-edge","kind":"container","ports":[80,443],"systemd_unit":null,"systemd_scope":null,` +
		`"container_name":"deploybot-edge","process_name":null,"pid":null,"user":null,` +
		`"image_digest":"sha256:` + strings.Repeat("cd", 32) + `","owner":{"kind":"edge","project":"","entry":""}}`
	if string(b) != want {
		t.Fatalf("%s", b)
	}
	if _, ok := findRow(t, out, "port-80"); ok {
		t.Fatal("80 still unmapped")
	}
	if r, ok := findRow(t, out, "port-443"); !ok || len(r.Ports) != 1 || r.Ports[0] != 8443 {
		t.Fatalf("%+v", r)
	}
	if _, ok := findRow(t, out, "sshd"); !ok {
		t.Fatal("mapped row dropped")
	}
	frame, _ := json.Marshal(protocol.Services{V: protocol.Version, Type: "services", Services: out.Services})
	if err := protocol.ValidateAgent(frame); err != nil {
		t.Fatal(err)
	}
}

// A stopped edge claims nothing: 80 and 443 stay with whoever holds them.
// A container named deploybot-edge without the edge labels, or for another
// host, is an ordinary container row.
func TestApplyContainersEdgeNotOurs(t *testing.T) {
	const host = "0123456789abcdef"
	out := ApplyContainers(listeners(), []images.ContainerInfo{edgeInfo(host, "exited")}, host, nil)
	row, ok := findRow(t, out, edge.ContainerName)
	if !ok || row.Owner == nil || row.Owner.Kind != "edge" || len(row.Ports) != 0 {
		t.Fatalf("%+v", row)
	}
	if _, ok := findRow(t, out, "port-80"); !ok {
		t.Fatal("stopped edge claimed 80")
	}
	for name, it := range map[string]images.ContainerInfo{
		"other host": edgeInfo("ffffffffffffffff", "running"),
		"no labels":  {ID: "x", Name: "deploybot-edge", State: "running"},
		"project": func() images.ContainerInfo {
			it := edgeInfo(host, "running")
			it.Labels[images.LabelProject] = "deploybot"
			return it
		}(),
	} {
		out := ApplyContainers(listeners(), []images.ContainerInfo{it}, host, []string{"deploybot"})
		row, ok := findRow(t, out, edge.ContainerName)
		if !ok || row.Owner != nil {
			t.Fatalf("%s: %+v", name, row)
		}
		if _, ok := findRow(t, out, "port-80"); !ok {
			t.Fatalf("%s: claimed 80", name)
		}
	}
}

// The edge row keeps its name over an image entry that wants the same one.
func TestApplyContainersEdgeNameReserved(t *testing.T) {
	const host = "0123456789abcdef"
	items := []images.ContainerInfo{
		edgeInfo(host, "running"),
		{ID: "2", Name: "acme-deploybot-edge", State: "running", Labels: ownedLabels(host, "acme", "deploybot-edge")},
	}
	out := ApplyContainers(Result{}, items, host, []string{"acme"})
	n := 0
	for _, s := range out.Services {
		if s.Name == edge.ContainerName {
			n++
			if s.Owner == nil || s.Owner.Kind != "edge" {
				t.Fatalf("%+v", s)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d rows", n)
	}
}
