package conn

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	b := NewBackoff()
	d := b.Take()
	if d < 800*time.Millisecond-time.Millisecond || d > 1200*time.Millisecond+time.Millisecond {
		t.Fatalf("first %s", d)
	}
	if b.Next() != 2*time.Second {
		t.Fatalf("next %s", b.Next())
	}
	for i := 0; i < 8; i++ {
		b.Take()
	}
	if b.Next() != 30*time.Second {
		t.Fatalf("cap %s", b.Next())
	}
	b.Set(30 * time.Second)
	d = b.Take()
	lo := time.Duration(float64(30*time.Second)*0.8) - time.Millisecond
	hi := time.Duration(float64(30*time.Second)*1.2) + time.Millisecond
	if d < lo || d > hi {
		t.Fatalf("30s jitter %s", d)
	}
	if b.Next() != 30*time.Second {
		t.Fatalf("stayed %s", b.Next())
	}
	b.Reset()
	if b.Next() != time.Second {
		t.Fatal("reset")
	}
}

func TestSafe(t *testing.T) {
	if Safe(nil, "join-token-test-value") != "" {
		t.Fatal("nil")
	}
	token := "join-token-test-value"
	cred := strings.Repeat("a", 43)
	err := errors.New("dial " + token + " " + cred + " short abc")
	got := Safe(err, token, cred, "abc")
	if strings.Contains(got, token) || strings.Contains(got, cred) {
		t.Fatal(got)
	}
	if !strings.Contains(got, "abc") || !strings.Contains(got, "[REDACTED]") {
		t.Fatal(got)
	}
	long := errors.New(strings.Repeat("x", 500))
	if len(Safe(long)) != 200 {
		t.Fatalf("len %d", len(Safe(long)))
	}
}
