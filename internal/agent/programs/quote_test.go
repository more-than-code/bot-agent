package programs

import "testing"

func TestQuoteExecArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{`100%`, `"100%%"`},
		{`$HOME`, `"$$HOME"`},
		{`say "hi"`, `"say \"hi\""`},
		{`a\b`, `"a\\b"`},
		{`hello world`, `"hello world"`},
		{`it's`, `"it\'s"`},
	}
	for _, tc := range cases {
		if got := QuoteExecArg(tc.in); got != tc.want {
			t.Errorf("QuoteExecArg(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
	line := ExecStart("/usr/bin/pnpm", []string{`100%`, `$HOME`, `say "hi"`, `a\b`, `hello world`})
	want := `"/usr/bin/pnpm" "100%%" "$$HOME" "say \"hi\"" "a\\b" "hello world"`
	if line != want {
		t.Fatalf("ExecStart\n%s\n%s", line, want)
	}
}
