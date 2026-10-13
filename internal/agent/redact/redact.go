// Package redact applies the contract's cmdline rule before a value is sent.
package redact

import (
	"regexp"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// userinfoRE is the v1.0.3 URL-userinfo pattern. secretRE is the v1.0.2
// keyword pattern and runs second. The keyword pattern may also redact a
// longer name such as --token-file.
var userinfoRE = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^/\s:@]*:)[^\s/]*@`)

var secretRE = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:authorization|api[_-]?key|password|passwd|token|secret|credential)[A-Za-z0-9_.-]*)(\s*[=:]\s*|\s+)(bearer\s+)?("[^"]*"|'[^']*'|\S+)`)

// Cmdline replaces URL userinfo and secret assignments, then truncates to
// 1024 characters. Invalid UTF-8 is dropped so the frame stays valid JSON.
func Cmdline(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = userinfoRE.ReplaceAllString(s, "$1[REDACTED]@")
	s = secretRE.ReplaceAllString(s, "$1=[REDACTED]")
	r := []rune(s)
	if len(r) > protocol.CmdlineMax {
		s = string(r[:protocol.CmdlineMax])
	}
	return s
}
