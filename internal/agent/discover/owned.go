package discover

import (
	"sort"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// Owned is one running deploybot program reported by the helper.
type Owned struct {
	Unit    string
	Project string
	Entry   string
	Ports   []int
	User    *string
}

// ApplyOwned adds catalog entries for running programs and drops the listeners
// those programs already own. Two units may share an entry name. The second
// catalog name is suffixed. byUnit then points at that catalog name.
func ApplyOwned(res Result, owned []Owned) Result {
	if res.byPID == nil {
		res.byPID = map[int]string{}
	}
	if res.byUnit == nil {
		res.byUnit = map[string]string{}
	}
	if res.Services == nil {
		res.Services = []protocol.Discovered{}
	}
	type ownedRow struct {
		o    Owned
		name string
	}
	var rows []ownedRow
	used := map[string]bool{}
	for _, o := range owned {
		name, ok := programs.UnitName(o.Project, o.Entry)
		if !ok || name != o.Unit || !protocol.NamePattern.MatchString(o.Entry) {
			continue
		}
		display := o.Entry
		if used[display] {
			display = uniqueName(display, used)
		}
		used[display] = true
		rows = append(rows, ownedRow{o, display})
	}
	if len(rows) == 0 {
		return res
	}
	ports := map[int]bool{}
	claimed := map[string]ownedRow{}
	for _, r := range rows {
		claimed[r.o.Unit] = r
		for _, p := range r.o.Ports {
			ports[p] = true
		}
	}
	renamed := map[string]string{}
	droppedUnit := map[string]string{}
	var kept []protocol.Discovered
	for _, s := range res.Services {
		if s.SystemdUnit != nil {
			if _, ok := claimed[*s.SystemdUnit]; ok {
				droppedUnit[*s.SystemdUnit] = s.Name
				continue
			}
		}
		if s.Kind == "process" && s.PID == nil && len(s.Ports) == 1 && ports[s.Ports[0]] {
			if res.Unmapped > 0 {
				res.Unmapped--
			}
			continue
		}
		if used[s.Name] {
			next := uniqueName(s.Name, used)
			renamed[s.Name] = next
			s.Name = next
		}
		used[s.Name] = true
		kept = append(kept, s)
	}
	for pid, name := range res.byPID {
		if next, ok := renamed[name]; ok {
			res.byPID[pid] = next
		}
	}
	for unit, name := range res.byUnit {
		if next, ok := renamed[name]; ok {
			res.byUnit[unit] = next
		}
	}
	for _, r := range rows {
		o := r.o
		old := droppedUnit[o.Unit]
		if old == "" {
			old = res.byUnit[o.Unit]
		}
		res.byUnit[o.Unit] = r.name
		if old != "" && old != r.name {
			for pid, name := range res.byPID {
				if name == old {
					res.byPID[pid] = r.name
				}
			}
		}
		svc := ownedService(o)
		svc.Name = r.name
		kept = append(kept, svc)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	res.Services = kept
	return res
}

func ownedService(o Owned) protocol.Discovered {
	ports := append([]int(nil), o.Ports...)
	sort.Ints(ports)
	var user *string
	if o.User != nil {
		user = protocol.Str(*o.User)
	}
	return protocol.Discovered{
		Name:         o.Entry,
		Kind:         "systemd",
		Ports:        ports,
		SystemdUnit:  protocol.Str(o.Unit),
		SystemdScope: protocol.Str("system"),
		User:         user,
		Owner:        &protocol.Owner{Kind: "program", Project: o.Project, Entry: o.Entry},
	}.NormalizePorts()
}
