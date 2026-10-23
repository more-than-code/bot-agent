package images

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

const (
	// LabelPrefix is the only label namespace a projection may return.
	LabelPrefix    = "dev.deploybot."
	LabelOwner     = "dev.deploybot.owner"
	LabelHost      = "dev.deploybot.host"
	LabelProject   = "dev.deploybot.project"
	LabelEntry     = "dev.deploybot.entry"
	LabelSpecHash  = "dev.deploybot.spec_hash"
	LabelFilesHash = "dev.deploybot.files_hash"
	// Health policy labels (agent-protocol v1.9 item 3). Set only on a
	// service whose on_unhealthy is not none. Values are decimal strings.
	LabelOnUnhealthy       = "dev.deploybot.on_unhealthy"
	LabelUnhealthyAfterSec = "dev.deploybot.unhealthy_after_sec"
	LabelMaxActionsPerHour = "dev.deploybot.max_actions_per_hour"
	// StopGraceSec is the helper's container stop grace (Engine stop t=).
	// Agent-side budgets for a stop must be longer (agent-protocol v1.9 rev 3).
	StopGraceSec = 30
	// OwnerValue is the only owner the helper writes. It is not taken from the request.
	OwnerValue = "bot-agent"
)

var (
	projectRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	entryRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	volumeRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	hashRE    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// ValidProject is the runner.toml projects slug.
func ValidProject(s string) bool {
	return projectRE.MatchString(s)
}

// ValidEntry is the catalog entry rule plus the rollback-slot ban.
// A name ending in "-", "-prev" or "-old" would make the prev slot or the
// rollback's parked original ambiguous (v1.9 rev 3b).
func ValidEntry(s string) bool {
	if !entryRE.MatchString(s) {
		return false
	}
	return !strings.HasSuffix(s, "-") && !strings.HasSuffix(s, "-prev") && !strings.HasSuffix(s, "-old")
}

// ValidVolume is the short volume name. The host volume is project-name.
func ValidVolume(s string) bool {
	return volumeRE.MatchString(s)
}

// ValidSpecHash is the lowercase SHA-256 hex the helper stores as a label.
func ValidSpecHash(s string) bool {
	return hashRE.MatchString(s)
}

// HostID is the first 16 hex characters of SHA-256 of the trimmed machine-id.
// Empty and multi-line contents are refused so a swapped-in file cannot
// become a stable impostor id by accident.
func HostID(contents string) (string, bool) {
	s := strings.TrimSpace(contents)
	if s == "" || len(s) > 256 || strings.ContainsAny(s, "\r\n\x00") {
		return "", false
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16], true
}

// ContainerName is <project>-<entry>. The bool is false when either part is
// not a validated token, so a caller cannot interpolate a raw request field.
func ContainerName(project, entry string) (string, bool) {
	if !ValidProject(project) || !ValidEntry(entry) {
		return "", false
	}
	return project + "-" + entry, true
}

// PrevName is the rollback slot <project>-<entry>-prev.
func PrevName(project, entry string) (string, bool) {
	base, ok := ContainerName(project, entry)
	if !ok {
		return "", false
	}
	return base + "-prev", true
}

// NewSlot is the transient directory <project>-<entry>.new.
func NewSlot(project, entry string) (string, bool) {
	base, ok := ContainerName(project, entry)
	if !ok {
		return "", false
	}
	return base + ".new", true
}

// VolumeName is <project>-<name>.
func VolumeName(project, name string) (string, bool) {
	if !ValidProject(project) || !ValidVolume(name) {
		return "", false
	}
	return project + "-" + name, true
}

// NetworkName is deploybot-<project>.
func NetworkName(project string) (string, bool) {
	if !ValidProject(project) {
		return "", false
	}
	return "deploybot-" + project, true
}

// LockPath is the per-entry flock. It is absolute on the host; the helper
// prefixes a test root itself.
func LockPath(project, entry string) (string, bool) {
	base, ok := ContainerName(project, entry)
	if !ok {
		return "", false
	}
	return "/run/bot-agent/image-" + base + ".lock", true
}
