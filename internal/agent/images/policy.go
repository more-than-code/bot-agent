package images

import "strconv"

// OwnsLabels reports whether labels mark a container this host's helper
// created for project and entry: the owner value and the exact host,
// project and entry labels.
func OwnsLabels(labels map[string]string, project, entry, host string) bool {
	return labels[LabelOwner] == OwnerValue &&
		labels[LabelHost] == host &&
		labels[LabelProject] == project &&
		labels[LabelEntry] == entry
}

// ContainerPolicy is the container_list "policy" object (agent-protocol
// v1.9 item 3). It is read from the three policy labels of an owned container.
type ContainerPolicy struct {
	OnUnhealthy       string `json:"on_unhealthy"`
	UnhealthyAfterSec int    `json:"unhealthy_after_sec"`
	MaxActionsPerHour int    `json:"max_actions_per_hour"`
}

// PolicyLabels is the label set the helper adds at create. It is nil for a
// job, a spec without health_policy, or on_unhealthy none.
func PolicyLabels(s ImageSpec) map[string]string {
	if s.Kind != "service" {
		return nil
	}
	p := s.Health()
	if p.OnUnhealthy == UnhealthyNone {
		return nil
	}
	return map[string]string{
		LabelOnUnhealthy:       p.OnUnhealthy,
		LabelUnhealthyAfterSec: strconv.Itoa(p.UnhealthyAfterSec),
		LabelMaxActionsPerHour: strconv.Itoa(p.MaxActionsPerHour),
	}
}

// PolicyFromLabels reads the three policy labels. A missing label, a value
// outside item 1's bounds, a none action, or a number that is not a plain
// decimal string is nil.
func PolicyFromLabels(labels map[string]string) *ContainerPolicy {
	action, ok := labels[LabelOnUnhealthy]
	if !ok || (action != UnhealthyRestart && action != UnhealthyRollback) {
		return nil
	}
	after, ok := labelInt(labels, LabelUnhealthyAfterSec)
	if !ok || !validUnhealthyAfter(after) {
		return nil
	}
	max, ok := labelInt(labels, LabelMaxActionsPerHour)
	if !ok || !validActions(max) {
		return nil
	}
	return &ContainerPolicy{OnUnhealthy: action, UnhealthyAfterSec: after, MaxActionsPerHour: max}
}

func labelInt(labels map[string]string, key string) (int, bool) {
	v, ok := labels[key]
	if !ok || len(v) == 0 || len(v) > 4 {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(n) != v {
		return 0, false
	}
	return n, true
}
