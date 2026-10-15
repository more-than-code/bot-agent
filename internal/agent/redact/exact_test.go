package redact

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestExactValueForms(t *testing.T) {
	secret := `sek"ret`
	token := `tok<en`
	if len(secret) < 6 || len(token) < 6 {
		t.Fatal("fixtures")
	}
	ex := NewExact(secret, token, "short")
	line := strings.Join([]string{
		"short",
		secret,
		token,
		b64(base64.StdEncoding, secret),
		b64(base64.RawStdEncoding, secret),
		b64(base64.URLEncoding, secret),
		b64(base64.RawURLEncoding, secret),
		b64(base64.StdEncoding, token),
		b64(base64.URLEncoding, token),
		jsonBodyOf(secret, false),
		jsonBodyOf(token, true),
		jsonBodyOf(token, false),
	}, " ")
	got := ex.Text(line)
	for _, form := range []string{
		secret, token,
		b64(base64.StdEncoding, secret),
		b64(base64.RawStdEncoding, secret),
		b64(base64.URLEncoding, secret),
		b64(base64.RawURLEncoding, secret),
		b64(base64.StdEncoding, token),
		b64(base64.URLEncoding, token),
		jsonBodyOf(secret, false),
		jsonBodyOf(token, true),
		jsonBodyOf(token, false),
	} {
		if strings.Contains(got, form) {
			t.Fatalf("kept %q in %s", form, got)
		}
	}
	if !strings.Contains(got, "short") {
		t.Fatalf("redacted a 5-byte value: %s", got)
	}
	lines, _ := JournalTask([]string{line}, ex)
	if len(lines) != 1 || strings.Contains(lines[0], secret) || !strings.Contains(lines[0], "short") {
		t.Fatalf("journal %v", lines)
	}
	payload := ex.Payload(map[string]any{
		"status": map[string]any{"state": secret},
		"lines":  []any{b64(base64.StdEncoding, token)},
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), b64(base64.StdEncoding, token)) {
		t.Fatalf("payload %s", raw)
	}
	if !strings.Contains(string(raw), `"status"`) {
		t.Fatalf("key redacted: %s", raw)
	}
	kept := NewExact("status").Payload(map[string]any{"status": map[string]any{"state": "status"}})
	raw, err = json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"status":`) || strings.Contains(string(raw), `"state":"status"`) {
		t.Fatalf("key walk %s", raw)
	}
}

func TestExactBeforeRegex(t *testing.T) {
	space := "correct horse"
	urlSecret := "p@ssword extra"
	ex := NewExact(space, urlSecret)
	got := ex.Text("password=" + space + " mongodb://app:" + urlSecret + "@db.internal/prod")
	for _, frag := range []string{space, "horse", urlSecret, "@ssword"} {
		if strings.Contains(got, frag) {
			t.Fatalf("kept %q in %s", frag, got)
		}
	}
}

func b64(enc *base64.Encoding, s string) string {
	return enc.EncodeToString([]byte(s))
}

func jsonBodyOf(s string, html bool) string {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(html)
	if err := enc.Encode(s); err != nil {
		return ""
	}
	raw := strings.TrimRight(buf.String(), "\n")
	return raw[1 : len(raw)-1]
}
