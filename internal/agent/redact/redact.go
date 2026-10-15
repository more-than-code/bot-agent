// Package redact applies the contract's cmdline rule before a value is sent.
package redact

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// userinfoRE is the v1.0.3 URL-userinfo pattern. secretRE is the v1.0.2
// keyword pattern and runs second. The keyword pattern may also redact a
// longer name such as --token-file.
var userinfoRE = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@]*:)[^\s/]*@`)

var secretRE = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:authorization|api[_-]?key|password|passwd|token|secret|credential)[A-Za-z0-9_.-]*)(\s*[=:]\s*|\s+)(bearer\s+)?("[^"]*"|'[^']*'|\S+)`)

const (
	// JournalLineMax is the per-line cap for program logs (v1.3).
	JournalLineMax = 2048
	// JournalBytes is the total cap for one program log result.
	JournalBytes = 64 * 1024
)

// Cmdline replaces URL userinfo and secret assignments, then truncates to
// 1024 characters. Invalid UTF-8 is dropped so the frame stays valid JSON.
func Cmdline(s string) string {
	return clip(apply(s), protocol.CmdlineMax)
}

// Journal redacts each line, caps each line at 2048 characters, and keeps
// the newest lines that fit in 64 KiB. truncated is set when a line or the
// total was cut. journalctl prints the oldest of the window first, so lines
// dropped from the front are the older ones.
func Journal(lines []string) ([]string, bool) {
	return journal(lines, Exact{})
}

func journal(lines []string, ex Exact) ([]string, bool) {
	out := make([]string, 0, len(lines))
	cut := false
	for _, line := range lines {
		s := ex.redact(line)
		if utf8.RuneCountInString(s) > JournalLineMax {
			s = clip(s, JournalLineMax)
			cut = true
		}
		out = append(out, s)
	}
	total := 0
	for _, s := range out {
		total += len(s)
	}
	for len(out) > 0 && total > JournalBytes {
		total -= len(out[0])
		out = out[1:]
		cut = true
	}
	return out, cut
}

func apply(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = userinfoRE.ReplaceAllString(s, "$1[REDACTED]@")
	s = secretRE.ReplaceAllString(s, "$1=[REDACTED]")
	return s
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}
