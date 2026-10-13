package run

import "fmt"

const logQuoteMax = 64

// quoteLog renders one server-supplied log field. It uses %q so a newline
// cannot break the line, and it keeps at most 64 runes of the value.
func quoteLog(s string) string {
	r := []rune(s)
	if len(r) > logQuoteMax {
		s = string(r[:logQuoteMax])
	}
	return fmt.Sprintf("%q", s)
}
