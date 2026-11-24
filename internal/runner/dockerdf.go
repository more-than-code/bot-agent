package runner

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/dockerusage"
	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// dfTimeout is the GET /system/df deadline (agent-protocol v1.25). A
// variable so tests can shorten it.
var dfTimeout = 30 * time.Second

var shortIDRE = regexp.MustCompile(`^sha256:([a-f0-9]{12})[a-f0-9]*$`)

// handleDockerDF serves docker_df (agent-protocol v1.25): read-only, gated
// like container_list (images or edge). The answer is the summary only;
// the Engine's own text goes no further than the helper journal.
func handleDockerDF(ctx context.Context, in []byte, sys Sys) []byte {
	if ctx.Err() != nil {
		return imgErr(sys, dockerusage.ReasonUnavailable)
	}
	if len(in) > programs.MaxFrame || !dockerusage.ParseSpec(in) {
		return imgErr(sys, programs.ReasonBadSpec)
	}
	pol := policy.Load(policy.Files{
		Policy: sys.Policy, Passwd: sys.Passwd, Group: sys.Group, UID: sys.UID, SkipEnvFiles: true,
	})
	if !pol.ImagesEnabled() && !pol.EdgeEnabled() {
		return imgErr(sys, dockerusage.ReasonDisabled)
	}
	raw, err := newEngine(dockerSock(sys), sys.Logf).SystemDF(ctx)
	if errors.Is(err, errTimeout) {
		return imgErr(sys, dockerusage.ReasonTimeout)
	}
	if err != nil {
		return imgErr(sys, dockerusage.ReasonUnavailable)
	}
	sum, ok := summarizeDF(raw)
	if !ok {
		if sys.Logf != nil {
			sys.Logf("bot-agent-runner: docker: GET /system/df: unreadable answer")
		}
		return imgErr(sys, dockerusage.ReasonUnavailable)
	}
	return images.Encode(dockerusage.Reply{Summary: sum})
}

// dfAnswer is the part of GET /system/df the summary reads. Every other
// field (labels, mounts, commands, digests, paths) is never decoded.
type dfAnswer struct {
	LayersSize *int64 `json:"LayersSize"`
	Images     []struct {
		ID         string   `json:"Id"`
		RepoTags   []string `json:"RepoTags"`
		Size       int64    `json:"Size"`
		SharedSize int64    `json:"SharedSize"`
		Containers int64    `json:"Containers"`
	} `json:"Images"`
	Containers []struct {
		Names  []string `json:"Names"`
		State  string   `json:"State"`
		SizeRw int64    `json:"SizeRw"`
	} `json:"Containers"`
	Volumes []struct {
		Name string `json:"Name"`
		// UsageData is null when the daemon did not compute it.
		UsageData *struct {
			Size     int64 `json:"Size"`
			RefCount int64 `json:"RefCount"`
		} `json:"UsageData"`
	} `json:"Volumes"`
	BuildCache []struct {
		InUse  bool  `json:"InUse"`
		Shared bool  `json:"Shared"`
		Size   int64 `json:"Size"`
	} `json:"BuildCache"`
}

// summarizeDF builds the v1.25 summary. A size Docker reports as -1 counts
// as 0 in the totals and is null in a list.
func summarizeDF(raw []byte) (dockerusage.Summary, bool) {
	var df dfAnswer
	if json.Unmarshal(raw, &df) != nil {
		return dockerusage.Summary{}, false
	}
	var s dockerusage.Summary
	s.Images = []dockerusage.Image{}
	s.Volumes = []dockerusage.Volume{}
	s.Containers = []dockerusage.Container{}

	// imgSum is the size for a daemon without LayersSize; unusedOwn is what
	// the images no container uses hold alone (Size - SharedSize), which is
	// docker system df's reclaimable.
	var imgSum, unusedOwn int64
	for _, im := range df.Images {
		n := dfKnown(im.Containers)
		s.Totals.Images.Count++
		imgSum += dfKnown(im.Size)
		if n > 0 {
			s.Totals.Images.Active++
		} else if im.Size >= 0 {
			own := im.Size
			if im.SharedSize >= 0 && im.SharedSize <= im.Size {
				own -= im.SharedSize
			}
			unusedOwn += own
		}
		ref := "<none>"
		if len(im.RepoTags) > 0 && im.RepoTags[0] != "<none>:<none>" && im.RepoTags[0] != "" {
			ref = im.RepoTags[0]
		}
		id := ""
		if m := shortIDRE.FindStringSubmatch(im.ID); m != nil {
			id = m[1]
		}
		s.Images = append(s.Images, dockerusage.Image{
			Ref: dockerusage.Cap(ref), ID: id, Size: dfListed(im.Size), Containers: n,
		})
	}
	// Image size and reclaimable follow docker system df, checked against
	// Docker 29.9 on pi3 (2026-11-24: 193.5 MB from the raw Engine answer):
	// size is the daemon's LayersSize, which counts a shared layer once (the
	// sum of the sizes when the daemon does not send it), and reclaimable is
	// what the images with no container hold alone, Size - SharedSize (Size
	// when SharedSize is unknown), at most the size.
	s.Totals.Images.Size = imgSum
	if df.LayersSize != nil && *df.LayersSize >= 0 {
		s.Totals.Images.Size = *df.LayersSize
	}
	s.Totals.Images.Reclaimable = min(unusedOwn, s.Totals.Images.Size)

	for _, c := range df.Containers {
		size := dfKnown(c.SizeRw)
		s.Totals.Containers.Count++
		s.Totals.Containers.Size += size
		if c.State == "running" {
			s.Totals.Containers.Active++
		} else {
			s.Totals.Containers.Reclaimable += size
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		s.Containers = append(s.Containers, dockerusage.Container{
			Name: dockerusage.Cap(name), State: dockerusage.Cap(c.State), Size: dfListed(c.SizeRw),
		})
	}

	for _, v := range df.Volumes {
		sizeRaw, inUse := int64(-1), false
		if v.UsageData != nil {
			sizeRaw, inUse = v.UsageData.Size, v.UsageData.RefCount > 0
		}
		size := dfKnown(sizeRaw)
		s.Totals.Volumes.Count++
		s.Totals.Volumes.Size += size
		if inUse {
			s.Totals.Volumes.Active++
		} else {
			s.Totals.Volumes.Reclaimable += size
		}
		s.Volumes = append(s.Volumes, dockerusage.Volume{
			Name: dockerusage.Cap(v.Name), Size: dfListed(sizeRaw), InUse: inUse,
		})
	}

	// As docker system df: a shared record (also held by another record)
	// is counted but left out of size and reclaimable.
	for _, b := range df.BuildCache {
		s.Totals.BuildCache.Count++
		if b.InUse {
			s.Totals.BuildCache.Active++
		}
		if b.Shared {
			continue
		}
		size := dfKnown(b.Size)
		s.Totals.BuildCache.Size += size
		if !b.InUse {
			s.Totals.BuildCache.Reclaimable += size
		}
	}

	sort.SliceStable(s.Images, func(i, j int) bool {
		a, b := s.Images[i], s.Images[j]
		if dfOrder(a.Size) != dfOrder(b.Size) {
			return dfOrder(a.Size) > dfOrder(b.Size)
		}
		if a.Ref != b.Ref {
			return a.Ref < b.Ref
		}
		return a.ID < b.ID
	})
	sort.SliceStable(s.Containers, func(i, j int) bool {
		a, b := s.Containers[i], s.Containers[j]
		if dfOrder(a.Size) != dfOrder(b.Size) {
			return dfOrder(a.Size) > dfOrder(b.Size)
		}
		return a.Name < b.Name
	})
	sort.SliceStable(s.Volumes, func(i, j int) bool {
		a, b := s.Volumes[i], s.Volumes[j]
		if dfOrder(a.Size) != dfOrder(b.Size) {
			return dfOrder(a.Size) > dfOrder(b.Size)
		}
		return a.Name < b.Name
	})
	s.Images = s.Images[:min(len(s.Images), dockerusage.MaxList)]
	s.Containers = s.Containers[:min(len(s.Containers), dockerusage.MaxList)]
	s.Volumes = s.Volumes[:min(len(s.Volumes), dockerusage.MaxList)]
	return s, true
}

// dfKnown is a Docker size or count with -1 (unknown) read as 0.
func dfKnown(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// dfListed is a list size: null when Docker did not know it.
func dfListed(n int64) *int64 {
	if n < 0 {
		return nil
	}
	return &n
}

// dfOrder sorts an unknown size below every known one.
func dfOrder(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}
