package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCmdline(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"nginx -g daemon", "nginx -g daemon"},
		{"/usr/bin/node build", "/usr/bin/node build"},
		{"password=hunter2", "password=[REDACTED]"},
		{"Password=hunter2", "Password=[REDACTED]"},
		{"token:abc", "token=[REDACTED]"},
		{"token: x", "token=[REDACTED]"},
		{"token: abc", "token=[REDACTED]"},
		{"token abc", "token=[REDACTED]"},
		{"--password=y", "--password=[REDACTED]"},
		{"api_key z", "api_key=[REDACTED]"},
		{"TOKEN=abc", "TOKEN=[REDACTED]"},
		{"API_KEY=xyz", "API_KEY=[REDACTED]"},
		{"api-key:xyz", "api-key=[REDACTED]"},
		{"apikey=xyz", "apikey=[REDACTED]"},
		{"passwd=xyz", "passwd=[REDACTED]"},
		{"secret=xyz", "secret=[REDACTED]"},
		{"credential=xyz", "credential=[REDACTED]"},
		{"Authorization: Bearer abc", "Authorization=[REDACTED]"},
		{"Authorization: Bearer abc123", "Authorization=[REDACTED]"},
		{"Authorization:Bearer", "Authorization=[REDACTED]"},
		{"password=a token=b", "password=[REDACTED] token=[REDACTED]"},
		{"password=\"two words\" next", "password=[REDACTED] next"},
		{"AWS_SECRET_ACCESS_KEY=AKIAxyz", "AWS_SECRET_ACCESS_KEY=[REDACTED]"},
		{"postgres://app:s3cret@127.0.0.1/db", "postgres://app:[REDACTED]@127.0.0.1/db"},
		{"redis://localhost:6379", "redis://localhost:6379"},
		{"redis://:s3cret@127.0.0.1:6379", "redis://:[REDACTED]@127.0.0.1:6379"},
		{"https://user:p@ss@host/db", "https://user:[REDACTED]@host/db"},
		{"ssh://git@host:22/repo", "ssh://git@host:22/repo"},
		{"http://127.0.0.1:3031/health", "http://127.0.0.1:3031/health"},
		{"a://u:p@h/x b://c:d@e/y", "a://u:[REDACTED]@h/x b://c:[REDACTED]@e/y"},
		{"postgres: joe tour_agent 127.0.0.1(5000) idle", "postgres: joe tour_agent 127.0.0.1(5000) idle"},
		{"--token-file=/etc/secret", "--token-file=[REDACTED]"},
		{"secretary=ok", "secretary=[REDACTED]"},
		{"tokenize", "tokenize"},
	}
	for _, tc := range cases {
		if got := Cmdline(tc.in); got != tc.want {
			t.Errorf("Cmdline(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestCmdlineTruncatesAfterRedaction(t *testing.T) {
	prefix := strings.Repeat("a", 1000)
	in := prefix + " password=supersecret " + strings.Repeat("b", 100)
	got := Cmdline(in)
	if utf8.RuneCountInString(got) != 1024 {
		t.Fatalf("len=%d", utf8.RuneCountInString(got))
	}
	if strings.Contains(got, "supersecret") {
		t.Fatal("secret survived")
	}
	if !strings.Contains(got, "password=[REDACTED]") {
		t.Fatalf("redaction missing: %q", got[990:])
	}
}

func TestCmdlineDropsInvalidUTF8(t *testing.T) {
	got := Cmdline("ok\xffpassword=x")
	if strings.Contains(got, "\xff") || strings.Contains(got, "password=x") {
		t.Fatalf("%q", got)
	}
}

func TestJournal(t *testing.T) {
	long := strings.Repeat("a", JournalLineMax+100)
	lines, cut := Journal([]string{"password=hunter2", long})
	if !cut || len(lines) != 2 {
		t.Fatalf("cut %v lines %d", cut, len(lines))
	}
	if lines[0] != "password=[REDACTED]" {
		t.Fatalf("redact %q", lines[0])
	}
	if utf8.RuneCountInString(lines[1]) != JournalLineMax {
		t.Fatalf("clip %d", utf8.RuneCountInString(lines[1]))
	}
	old := strings.Repeat("O", 1024)
	newest := strings.Repeat("N", 1024)
	var batch []string
	batch = append(batch, "OLDEST")
	for len(old)*len(batch) <= JournalBytes {
		batch = append(batch, old)
	}
	batch = append(batch, newest)
	got, cut := Journal(batch)
	if !cut {
		t.Fatal("expected drop")
	}
	total := 0
	for _, line := range got {
		total += len(line)
		if line == "OLDEST" {
			t.Fatal("kept oldest")
		}
	}
	if total > JournalBytes || len(got) == 0 || got[len(got)-1] != newest {
		t.Fatalf("total %d last %q", total, got[len(got)-1])
	}
}
