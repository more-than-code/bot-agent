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
		"outcome": secret,
		"digest":  secret,
		"status":  map[string]any{"state": secret, "container": "name-" + secret},
		"lines":   []any{b64(base64.StdEncoding, token)},
		"message": secret,
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["outcome"] != secret || body["digest"] != secret {
		t.Fatalf("structured value redacted %s", raw)
	}
	st, _ := body["status"].(map[string]any)
	if st["state"] != secret || st["container"] != "name-"+secret {
		t.Fatalf("status redacted %s", raw)
	}
	msg, _ := body["message"].(string)
	if strings.Contains(msg, secret) || strings.Contains(string(raw), b64(base64.StdEncoding, token)) {
		t.Fatalf("free text kept %s", raw)
	}
	if !strings.Contains(string(raw), `"status"`) {
		t.Fatalf("key redacted: %s", raw)
	}
	kept := NewExact("status").Payload(map[string]any{
		"status": map[string]any{"state": "status"},
		"lines":  []any{"status"},
	})
	raw, err = json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	st, _ = body["status"].(map[string]any)
	text, _ := body["lines"].([]any)
	if st["state"] != "status" || len(text) != 1 || strings.Contains(text[0].(string), "status") {
		t.Fatalf("key walk %s", raw)
	}
	// The v1.0.1 regex follows the same rule: a status field is not matched.
	regexed := NewExact().Payload(map[string]any{
		"status": map[string]any{"state": "password=hunter2xx"},
		"lines":  []any{"password=hunter2xx"},
	})
	raw, err = json.Marshal(regexed)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	st, _ = body["status"].(map[string]any)
	text, _ = body["lines"].([]any)
	if st["state"] != "password=hunter2xx" || len(text) != 1 || text[0] != "password=[REDACTED]" {
		t.Fatalf("regex walk %s", raw)
	}
}

func TestExactEnvPreviewAndFileRedacted(t *testing.T) {
	const (
		envLong  = "sekret-env-VALUE-9f3"
		envShort = "abcd1234"
		envJSON  = "ab<cd-0123456789"
		fileVal  = "file-piece-xyz123"
		short    = "abcde"
	)
	if len(envLong) < 6 || len(envShort) < 6 || len(envJSON) < 6 || len(fileVal) < 6 || len(short) >= 6 {
		t.Fatal("fixtures")
	}
	longPreview := SecretPreview(envLong)
	shortPreview := SecretPreview(envShort)
	jsonPreview := SecretPreview(envJSON)
	if longPreview != "sekr\u2026" || shortPreview != "\u2022\u2022\u2022\u2022" || jsonPreview != "ab<c\u2026" {
		t.Fatalf("previews %q %q %q", longPreview, shortPreview, jsonPreview)
	}
	ex := Merge(NewExact(fileVal, short), NewExactEnv(envLong, envShort, envJSON, short))
	for _, form := range formsOf(envLong) {
		assertForm(t, ex, form, longPreview)
	}
	for _, form := range formsOf(envShort) {
		assertForm(t, ex, form, shortPreview)
	}
	for _, form := range formsOf(envJSON) {
		assertForm(t, ex, form, jsonPreview)
	}
	for _, form := range formsOf(fileVal) {
		assertForm(t, ex, form, "[REDACTED]")
	}
	if got := ex.Text("note " + short + " end"); got != "note "+short+" end" {
		t.Fatalf("short value changed: %q", got)
	}
	if got := ex.Text("password=" + envLong); got != "password=[REDACTED]" {
		t.Fatalf("keyword after preview: %q", got)
	}
	if got := Merge(NewExact(envLong), NewExactEnv(envLong)).Text("x " + envLong); got != "x [REDACTED]" {
		t.Fatalf("file wins %q", got)
	}
	if got := Merge(NewExactEnv(envLong), NewExact(envLong)).Text("x " + envLong); got != "x [REDACTED]" {
		t.Fatalf("file wins second %q", got)
	}
}

func formsOf(v string) []string {
	return []string{
		v,
		b64(base64.StdEncoding, v),
		b64(base64.RawStdEncoding, v),
		b64(base64.URLEncoding, v),
		b64(base64.RawURLEncoding, v),
		jsonBodyOf(v, false),
		jsonBodyOf(v, true),
	}
}

func assertForm(t *testing.T, ex Exact, form, want string) {
	t.Helper()
	got := ex.Text("x " + form + " y")
	if got != "x "+want+" y" {
		t.Fatalf("form %q → %q, want %q", form, got, "x "+want+" y")
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
