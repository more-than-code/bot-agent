package programs

import "strings"

// QuoteExecArg quotes one ExecStart argument for systemd.
// % and $ are doubled because systemd expands both in ExecStart.
// Quotes and backslashes are escaped. The result is always double-quoted
// so spaces stay inside one argument.
func QuoteExecArg(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '%', '$':
			b.WriteRune(r)
			b.WriteRune(r)
		case '\\', '"', '\'':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// QuotePath quotes a unit-file path. % is doubled because specifiers apply
// outside ExecStart. $ is not doubled: Environment= and WorkingDirectory do
// not expand it, so a doubled dollar would reach the process as two dollars.
func QuotePath(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '%':
			b.WriteRune('%')
			b.WriteRune('%')
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// QuoteEnv quotes an EnvironmentFile value. systemd does not expand $ or %
// in that file, so neither is doubled. Quotes and backslashes are escaped.
func QuoteEnv(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ExecStart joins the resolved binary and its arguments.
func ExecStart(bin string, args []string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, QuoteExecArg(bin))
	for _, a := range args {
		parts = append(parts, QuoteExecArg(a))
	}
	return strings.Join(parts, " ")
}
