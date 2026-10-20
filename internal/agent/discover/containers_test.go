package discover

import (
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestContainerHealthRecorded(t *testing.T) {
	const host = "0123456789abcdef"
	healthy := "healthy"
	unhealthy := "unhealthy"
	starting := "starting"
	other := "none"
	items := []images.ContainerInfo{
		{
			ID: "1", Name: "acme-web", State: "running", Health: &healthy,
			Labels: ownedLabels(host, "acme", "web"),
		},
		{
			ID: "2", Name: "acme-web-prev", State: "exited", Health: &unhealthy,
			Labels: ownedLabels(host, "acme", "web"),
		},
		{ID: "3", Name: "redis", State: "running", Health: &starting},
		{ID: "4", Name: "idle", State: "running"},
		{ID: "5", Name: "odd", State: "running", Health: &other},
	}
	out := ApplyContainers(Result{}, items, host, []string{"acme"})
	if out.ContainerHealth["web"] != "healthy" || out.ContainerHealth["redis"] != "starting" {
		t.Fatalf("%v", out.ContainerHealth)
	}
	if _, ok := out.ContainerHealth["acme-web-prev"]; ok {
		t.Fatal("prev health recorded")
	}
	if _, ok := out.ContainerHealth["idle"]; ok {
		t.Fatal("null health recorded")
	}
	if _, ok := out.ContainerHealth["odd"]; ok {
		t.Fatal("non-enum health recorded")
	}
}

func TestApplyContainersOwnedOtherAndPorts(t *testing.T) {
	const host = "0123456789abcdef"
	digest := "sha256:" + strings.Repeat("ab", 32)
	bad := "not-a-digest"
	items := []images.ContainerInfo{
		{
			ID: "1", Name: "acme-web", State: "running", Digest: &digest,
			Ports:  []images.PublishedPort{{HostIP: "127.0.0.1", HostPort: 3031, ContainerPort: 3031}},
			Labels: ownedLabels(host, "acme", "web"),
		},
		{
			ID: "2", Name: "acme-web-prev", State: "exited", Digest: &digest,
			Ports:  []images.PublishedPort{{HostIP: "127.0.0.1", HostPort: 3032, ContainerPort: 3031}},
			Labels: ownedLabels(host, "acme", "web"),
		},
		{
			ID: "3", Name: "/Redis", State: "running", Digest: &digest,
			Ports: []images.PublishedPort{{HostIP: "0.0.0.0", HostPort: 6379, ContainerPort: 6379}},
		},
		{
			ID: "4", Name: "other-host", State: "running", Digest: &bad,
			Ports:  []images.PublishedPort{{HostIP: "127.0.0.1", HostPort: 9, ContainerPort: 9}},
			Labels: ownedLabels("ffffffffffffffff", "acme", "api"),
		},
		{ID: "5", Name: "a/b", State: "running"},
		{
			ID: "6", Name: "acme-api", State: "running", Digest: &digest,
			Labels: map[string]string{
				images.LabelOwner: "compose", images.LabelHost: host,
				images.LabelProject: "acme", images.LabelEntry: "api",
			},
		},
		{
			ID: "7", Name: "acme-worker", State: "running", Digest: &digest,
			Labels: map[string]string{
				images.LabelHost: host, images.LabelProject: "acme", images.LabelEntry: "worker",
			},
		},
	}
	res := Result{
		Services: []protocol.Discovered{
			{Name: "port-3031", Kind: "process", Ports: []int{3031}},
			{Name: "port-3032", Kind: "process", Ports: []int{3032}},
			{Name: "port-6379", Kind: "process", Ports: []int{6379}},
			{Name: "port-9", Kind: "process", Ports: []int{9}},
			{Name: "nginx", Kind: "process", Ports: []int{80}, PID: protocol.Int(42), ProcessName: protocol.Str("nginx")},
		},
		Unmapped: 4,
	}
	out := ApplyContainers(res, items, host, []string{"acme"})
	if out.Unmapped != 0 {
		t.Fatalf("unmapped %d", out.Unmapped)
	}
	byName := map[string]protocol.Discovered{}
	for _, s := range out.Services {
		if _, ok := byName[s.Name]; ok {
			t.Fatalf("duplicate %s", s.Name)
		}
		byName[s.Name] = s
	}
	web, ok := byName["web"]
	if !ok || web.Kind != "container" || web.Owner == nil || web.Owner.Kind != "image" || web.Owner.Entry != "web" {
		t.Fatalf("web %+v", web)
	}
	if web.ContainerName == nil || *web.ContainerName != "acme-web" || web.ImageDigest == nil || *web.ImageDigest != digest {
		t.Fatalf("web identity %+v", web)
	}
	if _, ok := byName["acme-web-prev"]; ok {
		t.Fatal("prev slot reported")
	}
	if _, ok := byName["port-3031"]; ok || byNameHas(out, "port-3032") {
		t.Fatalf("unmapped ports kept %+v", out.Services)
	}
	redis, ok := byName["redis"]
	if !ok || redis.Owner != nil || redis.ImageDigest != nil || redis.ContainerName == nil || *redis.ContainerName != "Redis" {
		t.Fatalf("redis %+v", redis)
	}
	if len(redis.Ports) != 1 || redis.Ports[0] != 6379 {
		t.Fatalf("redis ports %+v", redis.Ports)
	}
	if nginx := byName["nginx"]; nginx.PID == nil || len(nginx.Ports) != 1 {
		t.Fatalf("pid process stripped %+v", nginx)
	}
	raw, err := protocol.Marshal(redis)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"owner":null`) || !strings.Contains(string(raw), `"image_digest":null`) {
		t.Fatalf("other json %s", raw)
	}
	if _, ok := byName["a"]; ok || byNameHas(out, "b") {
		t.Fatal("slash name reported")
	}
	foreign := byName["other-host"]
	if foreign.Owner != nil || foreign.ImageDigest != nil {
		t.Fatalf("wrong host %+v", foreign)
	}
	if _, ok := byName["port-6379"]; ok || byNameHas(out, "port-9") {
		t.Fatal("other published ports stayed unmapped")
	}
	for _, name := range []string{"api", "worker", "acme-api", "acme-worker"} {
		row, ok := byName[name]
		if name == "api" || name == "worker" {
			if ok {
				t.Fatalf("wrong owner became %s", name)
			}
			continue
		}
		if !ok || row.Owner != nil || row.ImageDigest != nil || row.Kind != "container" {
			t.Fatalf("label %s %+v", name, row)
		}
	}
}

func TestApplyContainersHostFallback(t *testing.T) {
	items := []images.ContainerInfo{{
		ID: "1", Name: "acme-web", State: "running",
		Labels: ownedLabels("anything", "acme", "web"),
	}}
	out := ApplyContainers(Result{}, items, "", []string{"acme"})
	if len(out.Services) != 1 || out.Services[0].Owner == nil || out.Services[0].Name != "web" {
		t.Fatalf("%+v", out.Services)
	}
	miss := ApplyContainers(Result{}, items, "0123456789abcdef", []string{"acme"})
	if len(miss.Services) != 1 || miss.Services[0].Owner != nil {
		t.Fatalf("host required %+v", miss.Services)
	}
	outside := ApplyContainers(Result{}, items, "", []string{"other"})
	if len(outside.Services) != 1 || outside.Services[0].Owner != nil || outside.Services[0].Name != "acme-web" {
		t.Fatalf("project %+v", outside.Services)
	}
}

func TestApplyContainersPrevOnly(t *testing.T) {
	items := []images.ContainerInfo{{
		ID: "1", Name: "acme-web-prev", State: "running",
		Ports:  []images.PublishedPort{{HostPort: 3031, ContainerPort: 80}},
		Labels: ownedLabels("0123456789abcdef", "acme", "web"),
	}}
	res := Result{Services: []protocol.Discovered{{Name: "port-3031", Kind: "process", Ports: []int{3031}}}, Unmapped: 1}
	out := ApplyContainers(res, items, "0123456789abcdef", []string{"acme"})
	if len(out.Services) != 1 || out.Services[0].Name != "web" || out.Services[0].Owner == nil {
		t.Fatalf("%+v", out.Services)
	}
	if out.Services[0].ContainerName == nil || *out.Services[0].ContainerName != "acme-web-prev" || out.Unmapped != 0 {
		t.Fatalf("%+v unmapped %d", out.Services[0], out.Unmapped)
	}
}

func TestApplyContainersOwnedNameIsEntry(t *testing.T) {
	const host = "0123456789abcdef"
	res := Result{
		Services: []protocol.Discovered{
			{Name: "web", Kind: "process", Ports: []int{80}, PID: protocol.Int(4), ProcessName: protocol.Str("web")},
		},
		byPID: map[int]string{4: "web"},
	}
	items := []images.ContainerInfo{
		{ID: "1", Name: "acme-web", State: "running", Labels: ownedLabels(host, "acme", "web")},
		{ID: "3", Name: "beta-web", State: "running", Labels: ownedLabels(host, "beta", "web")},
	}
	out := ApplyContainers(res, items, host, []string{"acme", "beta"})
	byName := map[string]protocol.Discovered{}
	for _, s := range out.Services {
		if _, ok := byName[s.Name]; ok {
			t.Fatalf("duplicate %s", s.Name)
		}
		byName[s.Name] = s
	}
	web := byName["web"]
	if web.Kind != "container" || web.Owner == nil || web.Owner.Project != "acme" || web.Owner.Entry != "web" {
		t.Fatalf("owned %+v", web)
	}
	if _, ok := byName["web-2"]; !ok || byName["web-2"].Owner != nil {
		t.Fatalf("other row %+v", byName["web-2"])
	}
	for _, s := range out.Services {
		if s.Owner != nil && s.Name != s.Owner.Entry {
			t.Fatalf("suffixed owned %+v", s)
		}
	}
	if out.byPID[4] != "web-2" {
		t.Fatalf("byPID %+v", out.byPID)
	}
}

func TestApplyContainersKeepsOwnedProgramName(t *testing.T) {
	const host = "0123456789abcdef"
	unit := "deploybot-alpha-web.service"
	res := Result{
		Services: []protocol.Discovered{{
			Name: "web", Kind: "systemd",
			SystemdUnit: protocol.Str(unit), SystemdScope: protocol.Str("system"),
			Owner: &protocol.Owner{Kind: "program", Project: "alpha", Entry: "web"},
		}},
		byUnit: map[string]string{unit: "web"},
	}
	items := []images.ContainerInfo{{
		ID: "1", Name: "acme-web", State: "running", Labels: ownedLabels(host, "acme", "web"),
	}}
	out := ApplyContainers(res, items, host, []string{"acme", "alpha"})
	if len(out.Services) != 1 || out.Services[0].Name != "web" || out.Services[0].Kind != "systemd" {
		t.Fatalf("%+v", out.Services)
	}
	if out.Services[0].Owner == nil || out.Services[0].Owner.Kind != "program" {
		t.Fatalf("owner %+v", out.Services[0].Owner)
	}
	if out.byUnit[unit] != "web" {
		t.Fatalf("byUnit %+v", out.byUnit)
	}
}

func ownedLabels(host, project, entry string) map[string]string {
	return map[string]string{
		images.LabelOwner:   images.OwnerValue,
		images.LabelHost:    host,
		images.LabelProject: project,
		images.LabelEntry:   entry,
	}
}

func byNameHas(res Result, name string) bool {
	for _, s := range res.Services {
		if s.Name == name {
			return true
		}
	}
	return false
}
