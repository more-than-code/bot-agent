package discover

import (
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestApplyOwnedReplacesUnmappedAndUnit(t *testing.T) {
	unit := "deploybot-tour-agent-web.service"
	res := Result{
		Unmapped: 2,
		Services: []protocol.Discovered{
			{Name: "port-3031", Kind: "process", Ports: []int{3031}, User: protocol.Str("joe")},
			{Name: "web", Kind: "process", Ports: []int{9}, PID: protocol.Int(3), User: protocol.Str("joe")},
			{
				Name: "tra-backend", Kind: "systemd", Ports: []int{80},
				SystemdUnit: protocol.Str(unit), SystemdScope: protocol.Str("system"),
				PID: protocol.Int(7), User: protocol.Str("joe"),
			},
		},
		byPID:  map[int]string{3: "web", 7: "tra-backend"},
		byUnit: map[string]string{unit: "tra-backend"},
	}
	out := ApplyOwned(res, []Owned{{
		Unit: unit, Project: "tour-agent", Entry: "web", Ports: []int{3031}, User: protocol.Str("joe"),
	}})
	if out.Unmapped != 1 {
		t.Fatalf("unmapped %d", out.Unmapped)
	}
	if out.byUnit[unit] != "web" {
		t.Fatalf("byUnit %q", out.byUnit[unit])
	}
	if out.byPID[7] != "web" || out.byPID[3] != "web-2" {
		t.Fatalf("byPID %+v", out.byPID)
	}
	seen := map[string]protocol.Discovered{}
	for _, s := range out.Services {
		seen[s.Name] = s
	}
	if _, ok := seen["port-3031"]; ok {
		t.Fatal("unmapped listener kept")
	}
	if _, ok := seen["tra-backend"]; ok {
		t.Fatal("cgroup service kept")
	}
	web, ok := seen["web"]
	if !ok || web.Kind != "systemd" || web.Owner == nil || web.Owner.Kind != "program" || web.Owner.Entry != "web" {
		t.Fatalf("owned %+v", web)
	}
	if web.PID != nil || len(web.Ports) != 1 || web.Ports[0] != 3031 {
		t.Fatalf("owned ports %+v", web)
	}
	if web.SystemdUnit == nil || *web.SystemdUnit != unit || web.SystemdScope == nil || *web.SystemdScope != "system" {
		t.Fatalf("unit %+v", web)
	}
	if _, ok := seen["web-2"]; !ok {
		t.Fatal("colliding process name was not suffixed")
	}
}

func TestApplyOwnedKeepsCollidingEntries(t *testing.T) {
	alpha := "deploybot-alpha-web.service"
	beta := "deploybot-beta-web.service"
	res := Result{
		Unmapped: 2,
		Services: []protocol.Discovered{
			{Name: "port-3031", Kind: "process", Ports: []int{3031}},
			{Name: "port-3032", Kind: "process", Ports: []int{3032}},
		},
		byPID:  map[int]string{},
		byUnit: map[string]string{},
	}
	out := ApplyOwned(res, []Owned{
		{Unit: alpha, Project: "alpha", Entry: "web", Ports: []int{3031}, User: protocol.Str("joe")},
		{Unit: beta, Project: "beta", Entry: "web", Ports: []int{3032}, User: protocol.Str("joe")},
	})
	if out.Unmapped != 0 {
		t.Fatalf("unmapped %d", out.Unmapped)
	}
	seen := map[string]protocol.Discovered{}
	for _, s := range out.Services {
		seen[s.Name] = s
	}
	if seen["web"].Owner == nil || seen["web"].Owner.Project != "alpha" || seen["web"].Owner.Entry != "web" {
		t.Fatalf("web %+v", seen["web"])
	}
	second, ok := seen["web-2"]
	if !ok || second.Owner == nil || second.Owner.Project != "beta" || second.Owner.Entry != "web" {
		t.Fatalf("second %+v", second)
	}
	if out.byUnit[alpha] != "web" || out.byUnit[beta] != "web-2" {
		t.Fatalf("byUnit %+v", out.byUnit)
	}
	if len(second.Ports) != 1 || second.Ports[0] != 3032 {
		t.Fatalf("ports %+v", second.Ports)
	}
}

func TestApplyOwnedSkipsBadIdentity(t *testing.T) {
	res := Result{
		Unmapped: 1,
		Services: []protocol.Discovered{{Name: "port-9", Kind: "process", Ports: []int{9}}},
		byPID:    map[int]string{},
		byUnit:   map[string]string{},
	}
	out := ApplyOwned(res, []Owned{
		{Unit: "not-a-unit", Project: "tour-agent", Entry: "web", Ports: []int{9}},
		{Unit: "deploybot-other-web.service", Project: "tour-agent", Entry: "web", Ports: []int{9}},
	})
	if len(out.Services) != 1 || out.Services[0].Name != "port-9" || out.Unmapped != 1 {
		t.Fatalf("%+v unmapped %d", out.Services, out.Unmapped)
	}
}
