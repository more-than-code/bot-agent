package run

import (
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// dockerReconnectGap is the shortest time between two reconnects for a
// change to false, so a daemon that keeps coming and going cannot loop
// sessions. A change back to true reconnects as soon as it is confirmed.
// It is not a protocol field.
const dockerReconnectGap = 5 * time.Minute

// dockerHoldPasses is how many discovery passes in a row must disagree with
// the hello before the agent reconnects (agent-protocol v1.17 item 3).
const dockerHoldPasses = 2

// networksLine is the task line for a deploy the helper refused because
// Docker is below API 1.44 and the spec has shared networks (v1.17 item 2).
// The helper journal names the host's API version.
const networksLine = "shared networks need Docker 25 (API 1.44); this host's Docker is older (the bot-agent-runner journal names its API version)"

// dockerFlag is hello.programs.docker. up follows every discovery pass;
// sent is what the current session's hello carried; diff counts the passes
// in a row whose up differs from sent. lastDown is the last reconnect for a
// change to false.
type dockerFlag struct {
	up       bool
	sent     bool
	diff     int
	lastDown time.Time
}

// noteDocker records one discovery pass: whether container_list answered.
func (a *agent) noteDocker(ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := &a.docker
	d.up = ok
	if ok != d.sent {
		d.diff++
	} else {
		d.diff = 0
	}
}

func (a *agent) dockerUp() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.docker.up
}

// sentDocker records the value a hello carried.
func (a *agent) sentDocker(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.docker.sent = v
	a.docker.diff = 0
}

// dockerChanged is true when the Docker state has differed from the hello
// for dockerHoldPasses passes, no task is running or waiting, and, for a
// change to false, no such reconnect happened in the last
// dockerReconnectGap. The caller then ends the session so the next hello
// carries the new value. A held change that cannot reconnect yet stays
// confirmed and is tried again at the next pass.
func (a *agent) dockerChanged(now time.Time, busy bool) bool {
	a.mu.Lock()
	d := &a.docker
	if d.diff < dockerHoldPasses || busy {
		a.mu.Unlock()
		return false
	}
	if !d.up {
		if !d.lastDown.IsZero() && now.Sub(d.lastDown) < dockerReconnectGap {
			a.mu.Unlock()
			return false
		}
		d.lastDown = now
	}
	up := d.up
	a.mu.Unlock()
	a.opt.Log.Printf("bot-agent: docker available changed to %t; reconnecting", up)
	return true
}

// tasksBusy is true while a task is running or waiting for a slot. The
// session is not closed for a Docker change then (v1.17 item 3).
func (a *agent) tasksBusy() bool {
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	return a.taskRunning+a.taskWaiting > 0
}

// refusalLines puts the reason line of a helper refusal the task result
// cannot otherwise explain in front of the container's log lines. The
// result keeps at most tailLines lines, so the last tailLines-1 log lines
// follow the reason line.
func refusalLines(code string, lines []string) []string {
	if code != programs.ReasonNetworksDocker {
		return lines
	}
	if len(lines) > tailLines-1 {
		lines = lines[len(lines)-(tailLines-1):]
	}
	return append([]string{networksLine}, lines...)
}
