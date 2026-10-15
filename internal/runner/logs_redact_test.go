package runner

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
)

func TestLogsExactBeforeCut(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.add("mohiguide-web", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
	space := "correct horse"
	urlSecret := "p@ssword extra"
	long := "SECRETPREFIX" + strings.Repeat("q", 3000)
	encoded := base64.StdEncoding.EncodeToString([]byte(space))
	var logged strings.Builder
	e.sys.Logf = func(f string, args ...any) {
		logged.WriteString(f)
	}
	e.fake.logBody = append(frame(1, "password="+space+"\n"), frame(1, "mongodb://app:"+urlSecret+"@db.internal/prod\n")...)
	e.fake.logBody = append(e.fake.logBody, frame(1, encoded+"\n")...)
	e.fake.logBody = append(e.fake.logBody, frame(1, long+"\n")...)
	raw := e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{
		Project: "mohiguide", Entry: "web", Lines: 20,
		Redact: []string{space, urlSecret, long},
	}))
	var logs images.LogsResponse
	if err := json.Unmarshal(raw, &logs); err != nil || len(logs.Lines) != 4 {
		t.Fatalf("%s", raw)
	}
	blob := string(raw) + logged.String()
	for _, frag := range []string{space, "horse", urlSecret, "@ssword", encoded, "SECRETPREFIX", long[:64]} {
		if strings.Contains(blob, frag) {
			t.Fatalf("kept %q in %s", frag, blob)
		}
	}
	if strings.Contains(blob, `"redact"`) {
		t.Fatal("redact list returned")
	}
}

func TestLogsRedactRejectedBeforeDocker(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.fake.add("mohiguide-web", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
	e.fake.logBody = frame(1, "hello\n")
	cases := [][]string{
		{"short"},
		{strings.Repeat("a", 4097)},
		{"sec\x00ret"},
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = strings.Repeat("d", 6) + itoa(i)
	}
	cases = append(cases, many)
	for _, redact := range cases {
		raw := e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{
			Project: "mohiguide", Entry: "web", Lines: 5, Redact: redact,
		}))
		if imgCode(t, raw) != "bad_spec" {
			t.Fatal(string(raw))
		}
	}
	raw := e.call(images.VerbLogs, []byte(`{"project":"mohiguide","entry":"web","lines":5,"redact":["abcdef"],"extra":1}`))
	if imgCode(t, raw) != "bad_spec" {
		t.Fatal(string(raw))
	}
	for _, call := range e.fake.Calls() {
		if strings.HasSuffix(call.Path, "/logs") {
			t.Fatal("docker logs ran")
		}
	}
}
