// Package dockerusage is the shared wire model of agent-protocol v1.25
// (docs/contracts/agent-protocol-v1.md "v1.25 Docker disk usage"): the task
// kind, the helper verb, the reason codes and the summary the helper builds
// from the Engine's GET /system/df. The agent and the helper both import it.
// It does not talk to Docker.
package dockerusage

import (
	"strings"
	"unicode/utf8"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

const (
	// Kind is the task kind; Verb is the read-only helper verb.
	Kind = "docker_usage"
	Verb = "docker_df"

	// ReasonDisabled: neither images nor edge is on in runner.toml.
	// ReasonUnavailable: Docker did not answer, or answered badly.
	// ReasonTimeout: the Engine call ran past its deadline.
	ReasonDisabled    = "docker_usage_disabled"
	ReasonUnavailable = "docker_unavailable"
	ReasonTimeout     = "timeout"

	// MaxList is the length of each top list; MaxString caps every string
	// in the summary, in characters.
	MaxList   = 10
	MaxString = 200
)

// Category is one row of totals. Sizes are bytes.
type Category struct {
	Count       int64 `json:"count"`
	Active      int64 `json:"active"`
	Size        int64 `json:"size"`
	Reclaimable int64 `json:"reclaimable"`
}

// Totals covers every image, container, volume and build cache record.
type Totals struct {
	Images     Category `json:"images"`
	Containers Category `json:"containers"`
	Volumes    Category `json:"volumes"`
	BuildCache Category `json:"build_cache"`
}

// Image is one top-list image. Size is null when Docker did not know it.
type Image struct {
	Ref        string `json:"ref"`
	ID         string `json:"id"`
	Size       *int64 `json:"size"`
	Containers int64  `json:"containers"`
}

// Volume is one top-list volume.
type Volume struct {
	Name  string `json:"name"`
	Size  *int64 `json:"size"`
	InUse bool   `json:"in_use"`
}

// Container is one top-list container; Size is its writable layer.
type Container struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Size  *int64 `json:"size"`
}

// Summary is the helper's answer and the task result's summary.
type Summary struct {
	Totals     Totals      `json:"totals"`
	Images     []Image     `json:"images"`
	Volumes    []Volume    `json:"volumes"`
	Containers []Container `json:"containers"`
}

// Reply is the helper's success object.
type Reply struct {
	Summary Summary `json:"summary"`
}

// ParseSpec reports whether raw is exactly {} (the task spec and the
// helper request).
func ParseSpec(raw []byte) bool {
	return images.Decode(raw, &struct{}{}) == nil
}

// DecodeReply reads the helper's success object strictly and checks the
// limits again, so a helper bug cannot put more on the wire than v1.25
// allows.
func DecodeReply(raw []byte) (Summary, bool) {
	var r Reply
	if images.Decode(raw, &r) != nil {
		return Summary{}, false
	}
	s := r.Summary
	if s.Images == nil || s.Volumes == nil || s.Containers == nil ||
		len(s.Images) > MaxList || len(s.Volumes) > MaxList || len(s.Containers) > MaxList {
		return Summary{}, false
	}
	for _, c := range []Category{s.Totals.Images, s.Totals.Containers, s.Totals.Volumes, s.Totals.BuildCache} {
		if c.Count < 0 || c.Active < 0 || c.Size < 0 || c.Reclaimable < 0 {
			return Summary{}, false
		}
	}
	for _, i := range s.Images {
		if !okString(i.Ref) || !okString(i.ID) || !okSize(i.Size) || i.Containers < 0 {
			return Summary{}, false
		}
	}
	for _, v := range s.Volumes {
		if !okString(v.Name) || !okSize(v.Size) {
			return Summary{}, false
		}
	}
	for _, c := range s.Containers {
		if !okString(c.Name) || !okString(c.State) || !okSize(c.Size) {
			return Summary{}, false
		}
	}
	return s, true
}

// Cap cuts s to MaxString characters and replaces invalid UTF-8.
func Cap(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if utf8.RuneCountInString(s) <= MaxString {
		return s
	}
	n := 0
	for i := range s {
		if n == MaxString {
			return s[:i]
		}
		n++
	}
	return s
}

func okString(s string) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= MaxString
}

func okSize(p *int64) bool {
	return p == nil || *p >= 0
}
