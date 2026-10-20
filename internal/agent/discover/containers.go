package discover

import (
	"sort"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// ApplyContainers merges container_list into a discovery pass.
// A container with the four owner labels, a matching host id, and a project
// in the policy becomes an owned service whose name is exactly its entry.
// A -prev slot with the same four labels is not a second service; its host
// ports are still claimed. A second owned entry that wants a name already
// taken by an owned row is omitted, not suffixed. A non-owned row that
// collides is renamed. Every other usable container is kind "container",
// named by its container name, with a null digest and owner null. Published
// host ports are removed from unmapped listeners (process rows with no pid).
// An empty hostID means the machine id could not be read. Ownership then
// accepts any non-empty host label.
func ApplyContainers(res Result, items []images.ContainerInfo, hostID string, projects []string) Result {
	if res.byPID == nil {
		res.byPID = map[int]string{}
	}
	if res.byUnit == nil {
		res.byUnit = map[string]string{}
	}
	if res.Services == nil {
		res.Services = []protocol.Discovered{}
	}
	allow := map[string]bool{}
	for _, p := range projects {
		allow[p] = true
	}
	claimed := map[int]bool{}
	for _, it := range items {
		for _, p := range it.Ports {
			if p.HostPort >= 1 && p.HostPort <= 65535 {
				claimed[p.HostPort] = true
			}
		}
	}
	var owned []containerCand
	var other []containerCand
	for _, it := range items {
		name := trimContainer(it.Name)
		if name == "" {
			continue
		}
		project, entry, ok := ownedContainer(it.Labels, hostID, allow)
		c := containerCand{info: it, project: project, entry: entry, name: name}
		if ok {
			owned = append(owned, c)
		} else {
			other = append(other, c)
		}
	}
	picked := map[string]containerCand{}
	for _, c := range owned {
		key := c.project + "\x00" + c.entry
		if prev, ok := picked[key]; ok && !preferContainer(c, prev) {
			continue
		}
		picked[key] = c
	}
	keys := make([]string, 0, len(picked))
	for k := range picked {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	used := map[string]bool{}
	for _, s := range res.Services {
		if s.Owner != nil && s.Name != "" {
			used[s.Name] = true
		}
	}
	var rows []containerRow
	for _, k := range keys {
		c := picked[k]
		display := c.entry
		if !protocol.NamePattern.MatchString(display) || used[display] {
			continue
		}
		used[display] = true
		rows = append(rows, containerRow{display: display, c: c, owned: true})
		noteContainerHealth(&res, display, c.info.Health)
	}
	sort.Slice(other, func(i, j int) bool { return other[i].name < other[j].name })
	for _, c := range other {
		display := c.name
		if !protocol.NamePattern.MatchString(display) {
			sanitized, ok := Sanitize(display)
			if !ok {
				continue
			}
			display = sanitized
		}
		if used[display] {
			display = uniqueName(display, used)
		}
		used[display] = true
		rows = append(rows, containerRow{display: display, c: c, owned: false})
		noteContainerHealth(&res, display, c.info.Health)
	}
	renamed := map[string]string{}
	var kept []protocol.Discovered
	for _, s := range res.Services {
		next, drop, removed := stripClaimed(s, claimed)
		if removed > 0 {
			if res.Unmapped > removed {
				res.Unmapped -= removed
			} else {
				res.Unmapped = 0
			}
		}
		if drop {
			continue
		}
		if removed > 0 {
			s = next
		}
		if s.Owner != nil {
			used[s.Name] = true
			kept = append(kept, s)
			continue
		}
		if used[s.Name] {
			n := uniqueName(s.Name, used)
			renamed[s.Name] = n
			s.Name = n
		}
		used[s.Name] = true
		kept = append(kept, s)
	}
	for pid, name := range res.byPID {
		if n, ok := renamed[name]; ok {
			res.byPID[pid] = n
		}
	}
	for unit, name := range res.byUnit {
		if n, ok := renamed[name]; ok {
			res.byUnit[unit] = n
		}
	}
	for _, r := range rows {
		kept = append(kept, containerService(r))
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	res.Services = kept
	return res
}

type containerCand struct {
	info    images.ContainerInfo
	project string
	entry   string
	name    string
}

type containerRow struct {
	display string
	c       containerCand
	owned   bool
}

func noteContainerHealth(res *Result, name string, health *string) {
	if health == nil {
		return
	}
	switch *health {
	case "healthy", "unhealthy", "starting":
	default:
		return
	}
	if res.ContainerHealth == nil {
		res.ContainerHealth = map[string]string{}
	}
	res.ContainerHealth[name] = *health
}

func trimContainer(name string) string {
	if strings.HasPrefix(name, "/") {
		name = name[1:]
	}
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "/\r\n\x00") {
		return ""
	}
	return name
}

func ownedContainer(labels map[string]string, hostID string, allow map[string]bool) (string, string, bool) {
	if labels[images.LabelOwner] != images.OwnerValue {
		return "", "", false
	}
	host := labels[images.LabelHost]
	project := labels[images.LabelProject]
	entry := labels[images.LabelEntry]
	if host == "" || !allow[project] || !images.ValidProject(project) || !images.ValidEntry(entry) {
		return "", "", false
	}
	if hostID != "" && host != hostID {
		return "", "", false
	}
	return project, entry, true
}

func preferContainer(c, prev containerCand) bool {
	want, ok := images.ContainerName(c.project, c.entry)
	if ok && c.name == want {
		return true
	}
	prevWant, pok := images.ContainerName(prev.project, prev.entry)
	if pok && prev.name == prevWant {
		return false
	}
	return c.name < prev.name
}

func stripClaimed(s protocol.Discovered, claimed map[int]bool) (protocol.Discovered, bool, int) {
	if s.Kind != "process" || s.PID != nil {
		return s, false, 0
	}
	var keep []int
	removed := 0
	for _, p := range s.Ports {
		if claimed[p] {
			removed++
			continue
		}
		keep = append(keep, p)
	}
	if removed == 0 {
		return s, false, 0
	}
	if len(keep) == 0 {
		return s, true, removed
	}
	s.Ports = keep
	return s, false, removed
}

func containerService(r containerRow) protocol.Discovered {
	d := protocol.Discovered{
		Name:          r.display,
		Kind:          "container",
		Ports:         hostPorts(r.c.info.Ports),
		ContainerName: protocol.Str(r.c.name),
	}.NormalizePorts()
	if !r.owned {
		return d
	}
	if r.c.info.Digest != nil && images.ValidDigest(*r.c.info.Digest) {
		d.ImageDigest = protocol.Str(*r.c.info.Digest)
	}
	d.Owner = &protocol.Owner{Kind: "image", Project: r.c.project, Entry: r.c.entry}
	return d
}

func hostPorts(ports []images.PublishedPort) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range ports {
		if p.HostPort < 1 || p.HostPort > 65535 || seen[p.HostPort] {
			continue
		}
		seen[p.HostPort] = true
		out = append(out, p.HostPort)
	}
	sort.Ints(out)
	return out
}
