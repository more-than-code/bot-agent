package run

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/redact"
)

func TestFilesHashDecidesUnchanged(t *testing.T) {
	content := "plain-config-v1"
	files := []images.TaskFile{{Name: "app.conf", Target: "/etc/app.conf", Content: content}}
	want := images.FilesHash(images.AsMount(files))
	t.Run("match", func(t *testing.T) {
		a, _, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = imgHash
		seq.preFiles = want
		res := mustImage(t, a, "image_deploy", fileTask(t, false, files), 120)
		assertDeploy(t, res, "unchanged", "succeeded")
		if seq.deployed {
			t.Fatal("recreated")
		}
	})
	t.Run("content changed", func(t *testing.T) {
		a, _, _, seq := imageRun(t)
		seq.pre = runningStatus(0)
		seq.preHash = imgHash
		seq.preFiles = strings.Repeat("ab", 32)
		seq.post = runningStatus(0)
		res := mustImage(t, a, "image_deploy", fileTask(t, false, files), 120)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		if m["outcome"] == "unchanged" || !seq.deployed {
			t.Fatal("stayed")
		}
		if !strings.Contains(string(seq.deployBody), content) {
			t.Fatal("files not passed")
		}
		if strings.Contains(string(seq.deployBody), `"secret"`) {
			t.Fatal("secret flag on the helper request")
		}
	})
}

func TestSecretFileRedactedInTail(t *testing.T) {
	line, header, doc := throwawayAccount(t)
	plain := "plain-config-line-not-secret"
	// client_email is a JSON string in the secret file, not a PEM line.
	email := "throwaway@example.invalid"
	if len(email) < 16 || strings.Contains(email, "BEGIN") || !strings.Contains(doc, `"client_email":"`+email+`"`) {
		t.Fatal("fixture")
	}
	spec := fileTask(t, true, []images.TaskFile{
		{Name: "key.json", Target: "/run/secrets/key.json", Content: doc, Secret: true},
		{Name: "app.conf", Target: "/etc/app.conf", Content: plain},
	})
	a, _, _, seq := imageRun(t)
	seq.pre = missingStatus()
	seq.post = runningStatus(0)
	seq.logs = []string{line, header, plain, email}
	res := mustImage(t, a, "image_deploy", spec, 120)
	raw := marshalPayload(t, res.Payload)
	if strings.Contains(string(raw), line) || strings.Contains(string(raw), "BEGIN RSA") || strings.Contains(string(raw), doc) || strings.Contains(string(raw), email) {
		t.Fatalf("secret kept %s", raw)
	}
	if !strings.Contains(string(raw), plain) {
		t.Fatalf("plain redacted %s", raw)
	}
	if len(seq.logReqs) != 1 {
		t.Fatalf("log calls %d", len(seq.logReqs))
	}
	got := map[string]bool{}
	total := 0
	for _, v := range seq.logReqs[0].Redact {
		got[v] = true
		total += len(v)
		if len(v) < images.MinRedactBytes || len(v) > images.MaxRedactBytes {
			t.Fatalf("value %d", len(v))
		}
	}
	if !got[line] || !got[header] || !got[email] || got[plain] {
		t.Fatalf("redact list missing the pem line or client_email")
	}
	if len(seq.logReqs[0].Redact) > images.MaxRedactCount || total > images.MaxRedactTotal {
		t.Fatalf("redact %d %d", len(seq.logReqs[0].Redact), total)
	}
	if strings.Contains(string(seq.deployBody), `"secret"`) {
		t.Fatal("secret flag on the helper request")
	}
	var sent images.DeployRequest
	if err := json.Unmarshal(seq.deployBody, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Files) != 2 || sent.Files[0].Content != doc || sent.Files[1].Content != plain || sent.Files[0].Name != "key.json" {
		t.Fatalf("deploy files %+v", sent.Files)
	}
}

func TestFileTaskRefused(t *testing.T) {
	// Decide refuses a bad image spec as task_reject bad_spec, the same path
	// as every other bad image spec. runImage is not reached, so the helper
	// is not called. A helper bad_spec still becomes outcome forbidden_spec.
	proc := fileTask(t, false, []images.TaskFile{{Name: "a", Target: "/proc/x", Content: "x"}})
	for _, spec := range [][]byte{proc, fileTaskMismatch(t)} {
		a, h, _, _ := imageRun(t)
		rej, _, ok := a.execute(context.Background(), imageTask("image_deploy", spec, 120))
		if ok || rej.ErrorCode != "bad_spec" {
			t.Fatalf("ok %v code %s", ok, rej.ErrorCode)
		}
		if len(h.Calls()) != 0 {
			t.Fatalf("helper called %v", h.Calls())
		}
	}
}

func TestRedactListIsClipped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < images.MaxRedactCount+52; i++ {
		fmt.Fprintf(&b, "%016d\n", i)
	}
	content := strings.TrimSuffix(b.String(), "\n")
	secrets := redact.FileSecrets(content)
	if len(secrets) <= images.MaxRedactCount {
		t.Fatalf("fixture %d", len(secrets))
	}
	// The value just past the helper list still appears in the container
	// output. The in-process set redacts it in the result.
	dropped := secrets[images.MaxRedactCount]
	spec := fileTask(t, true, []images.TaskFile{{
		Name: "lines.txt", Target: "/etc/lines.txt", Content: content, Secret: true,
	}})
	a, _, _, seq := imageRun(t)
	seq.pre = missingStatus()
	seq.post = runningStatus(0)
	seq.logs = []string{"leak " + dropped}
	res := mustImage(t, a, "image_deploy", spec, 120)
	if len(seq.logReqs) != 1 {
		t.Fatalf("log calls %d", len(seq.logReqs))
	}
	total := 0
	for _, v := range seq.logReqs[0].Redact {
		if len(v) < images.MinRedactBytes || len(v) > images.MaxRedactBytes || strings.IndexByte(v, 0) >= 0 {
			t.Fatalf("value %d", len(v))
		}
		if v == dropped {
			t.Fatalf("clipped value was sent %q", dropped)
		}
		total += len(v)
	}
	if len(seq.logReqs[0].Redact) == 0 || len(seq.logReqs[0].Redact) > images.MaxRedactCount || total > images.MaxRedactTotal {
		t.Fatalf("redact %d total %d", len(seq.logReqs[0].Redact), total)
	}
	if len(seq.logReqs[0].Redact) >= len(secrets) {
		t.Fatal("list was not clipped")
	}
	raw := marshalPayload(t, res.Payload)
	if strings.Contains(string(raw), dropped) || !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("in-process set missed %s", raw)
	}
}

func fileTask(t *testing.T, force bool, files []images.TaskFile, specFiles ...images.SpecFile) []byte {
	t.Helper()
	if specFiles == nil {
		specFiles = make([]images.SpecFile, len(files))
		for i, f := range files {
			specFiles[i] = images.SpecFile{File: f.Name, Target: f.Target}
		}
	}
	body := map[string]any{
		"project": "acme",
		"entry":   "web",
		"spec": map[string]any{
			"kind":              "service",
			"image":             "ghcr.io/acme/web:1",
			"ports":             []map[string]any{{"host": 3031, "container": 3031, "public": false}},
			"healthy_after_sec": 5,
			"files":             specFiles,
		},
		"env":           map[string]string{},
		"registry_auth": nil,
		"force":         force,
		"spec_hash":     imgHash,
		"files":         files,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSecretKeysLeaveStatus(t *testing.T) {
	const (
		plain = "mohiguide"
		key   = "s3cret-value-1"
	)
	logs := []string{"db " + plain + " ready", "key " + key}
	run := func(t *testing.T, present bool, keys []string) (container, lines string, redactList []string) {
		t.Helper()
		a, _, _, seq := imageRun(t)
		a.policy.Projects = []string{"acme", "mohiguide"}
		st := runningStatus(0)
		name := "mohiguide-web"
		st.Container = &name
		seq.pre = missingStatus()
		seq.post = st
		seq.logs = logs
		res := mustImage(t, a, "image_deploy", secretKeyTask(t, present, keys), 120)
		m := assertDeploy(t, res, "succeeded", "succeeded")
		status, _ := m["status"].(map[string]any)
		container, _ = status["container"].(string)
		var b strings.Builder
		for _, line := range m["lines"].([]any) {
			b.WriteString(line.(string))
			b.WriteByte('\n')
		}
		if len(seq.logReqs) != 1 {
			t.Fatalf("log calls %d", len(seq.logReqs))
		}
		return container, b.String(), seq.logReqs[0].Redact
	}
	t.Run("listed", func(t *testing.T) {
		container, lines, list := run(t, true, []string{"API_KEY"})
		if container != "mohiguide-web" {
			t.Fatalf("container %q", container)
		}
		if !strings.Contains(lines, plain) || strings.Contains(lines, key) {
			t.Fatalf("lines %q", lines)
		}
		got := map[string]bool{}
		for _, v := range list {
			got[v] = true
		}
		if got[plain] || !got[key] {
			t.Fatalf("redact %v", list)
		}
	})
	t.Run("absent", func(t *testing.T) {
		container, lines, _ := run(t, false, nil)
		if container != "mohiguide-web" {
			t.Fatalf("container %q", container)
		}
		if strings.Contains(lines, plain) || strings.Contains(lines, key) {
			t.Fatalf("lines %q", lines)
		}
	})
	t.Run("empty", func(t *testing.T) {
		container, lines, list := run(t, true, []string{})
		if container != "mohiguide-web" {
			t.Fatalf("container %q", container)
		}
		if !strings.Contains(lines, plain) || !strings.Contains(lines, key) {
			t.Fatalf("lines %q", lines)
		}
		for _, v := range list {
			if v == plain || v == key {
				t.Fatalf("redact %v", list)
			}
		}
	})
}

func secretKeyTask(t *testing.T, present bool, keys []string) []byte {
	t.Helper()
	body := map[string]any{
		"project": "mohiguide",
		"entry":   "web",
		"spec": map[string]any{
			"kind":              "service",
			"image":             "ghcr.io/acme/web:1",
			"ports":             []map[string]any{{"host": 3031, "container": 3031, "public": false}},
			"healthy_after_sec": 5,
		},
		"env":           map[string]string{"DB_NAME": "mohiguide", "API_KEY": "s3cret-value-1"},
		"registry_auth": nil,
		"force":         true,
		"spec_hash":     imgHash,
	}
	if present {
		if keys == nil {
			keys = []string{}
		}
		body["secret_keys"] = keys
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fileTaskMismatch(t *testing.T) []byte {
	t.Helper()
	return fileTask(t, false,
		[]images.TaskFile{{Name: "b.conf", Target: "/etc/app.conf", Content: "x"}},
		images.SpecFile{File: "a.conf", Target: "/etc/app.conf"},
	)
}

func throwawayAccount(t *testing.T) (line, header, doc string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	body, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"private_key":  string(block),
		"client_email": "throwaway@example.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	header = "-----BEGIN RSA PRIVATE KEY-----"
	for _, raw := range strings.Split(string(block), "\n") {
		trimmed := strings.TrimSpace(raw)
		if len(trimmed) >= 16 && !strings.Contains(trimmed, "BEGIN") && !strings.Contains(trimmed, "END") {
			return trimmed, header, string(body)
		}
	}
	t.Fatal("no pem line")
	return "", "", ""
}
