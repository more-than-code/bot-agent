package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/dockerusage"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

// serveDF is GET /system/df (agent-protocol v1.25).
func (f *fakeDocker) serveDF(w http.ResponseWriter, r *http.Request) {
	if f.dfDelay > 0 {
		select {
		case <-time.After(f.dfDelay):
		case <-r.Context().Done():
			return
		}
	}
	if f.dfCode != 0 {
		w.WriteHeader(f.dfCode)
	}
	w.Write(f.dfBody)
}

// set changes the fake under its lock.
func (f *fakeDocker) set(fn func(*fakeDocker)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func dfEnv(t *testing.T, policy string, body []byte) *imgEnv {
	t.Helper()
	e := newImgEnv(t, policy)
	e.fake.set(func(f *fakeDocker) { f.dfBody = body })
	return e
}

func dfSummary(t *testing.T, raw []byte) []byte {
	t.Helper()
	var r struct {
		Summary json.RawMessage `json:"summary"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Summary == nil {
		t.Fatalf("not a summary: %s", raw)
	}
	if _, ok := dockerusage.DecodeReply(raw); !ok {
		t.Fatalf("agent would refuse: %s", raw)
	}
	return r.Summary
}

// The recorded answer gives exactly the pinned summary.
func TestDockerDFSummaryFromFixture(t *testing.T) {
	e := dfEnv(t, imagePolicy, readFixture(t, "system-df.json"))
	raw := e.call(dockerusage.Verb, []byte(`{}`))
	jsonEqual(t, dfSummary(t, raw), string(readFixture(t, "system-df.summary.json")))
	var got []engCall
	for _, c := range e.fake.Calls() {
		if c.Path != "/_ping" {
			got = append(got, c)
		}
	}
	if len(got) != 1 || got[0].Method != http.MethodGet || got[0].Path != "/v1.47/system/df" || got[0].Query != "" || len(got[0].Body) != 0 {
		t.Fatalf("engine calls %+v", got)
	}
}

// Labels, env, mounts, host paths, commands and digests never leave the helper.
func TestDockerDFLeaksNothing(t *testing.T) {
	e := dfEnv(t, imagePolicy, readFixture(t, "system-df.json"))
	raw := e.call(dockerusage.Verb, []byte(`{}`))
	for _, bad := range []string{"SECRETLABEL", "SECRETENV", "hunter2", "/srv", "/var/lib", "sha256", "mohiguide", "@", "/data", "Labels", "Mountpoint", "Command"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Fatalf("%q in %s", bad, raw)
		}
	}
	forbidKeys(t, raw)
}

// Each list holds the 10 largest, ties by name; totals count everything;
// strings are capped at 200 characters.
func TestDockerDFCapsAndTies(t *testing.T) {
	var imgs, ctrs, vols []map[string]any
	for i := 0; i < 15; i++ {
		imgs = append(imgs, map[string]any{
			"Id": fmt.Sprintf("sha256:%064x", i), "RepoTags": []string{fmt.Sprintf("r/%02d:1", 14-i)}, "Size": 10, "Containers": 0,
		})
		ctrs = append(ctrs, map[string]any{
			"Id": fmt.Sprintf("%064x", i), "Names": []string{fmt.Sprintf("/c%02d", 14-i)}, "State": "exited", "SizeRw": 7,
		})
		vols = append(vols, map[string]any{
			"Name": fmt.Sprintf("v%02d", 14-i), "UsageData": map[string]any{"Size": 3, "RefCount": 0},
		})
	}
	long := strings.Repeat("é", 250)
	imgs = append(imgs, map[string]any{"Id": "sha256:" + strings.Repeat("f", 64), "RepoTags": []string{long}, "Size": 11, "Containers": 1})
	body, _ := json.Marshal(map[string]any{"Images": imgs, "Containers": ctrs, "Volumes": vols, "BuildCache": nil})
	e := dfEnv(t, imagePolicy, body)
	var s dockerusage.Summary
	if err := json.Unmarshal(dfSummary(t, e.call(dockerusage.Verb, []byte(`{}`))), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Images) != 10 || len(s.Containers) != 10 || len(s.Volumes) != 10 {
		t.Fatalf("lengths %d %d %d", len(s.Images), len(s.Containers), len(s.Volumes))
	}
	if s.Images[0].Ref != strings.Repeat("é", 200) || s.Images[1].Ref != "r/00:1" || s.Images[9].Ref != "r/08:1" {
		t.Fatalf("images %+v", s.Images)
	}
	if s.Containers[0].Name != "c00" || s.Containers[9].Name != "c09" || s.Volumes[0].Name != "v00" || s.Volumes[9].Name != "v09" {
		t.Fatalf("order %+v %+v", s.Containers, s.Volumes)
	}
	want := dockerusage.Totals{
		Images:     dockerusage.Category{Count: 16, Active: 1, Size: 161, Reclaimable: 150},
		Containers: dockerusage.Category{Count: 15, Active: 0, Size: 105, Reclaimable: 105},
		Volumes:    dockerusage.Category{Count: 15, Active: 0, Size: 45, Reclaimable: 45},
	}
	if s.Totals != want {
		t.Fatalf("totals %+v", s.Totals)
	}
}

// An empty daemon answers empty lists, not null.
func TestDockerDFEmpty(t *testing.T) {
	e := dfEnv(t, imagePolicy, []byte(`{"LayersSize":0,"Images":[],"Containers":[],"Volumes":[],"BuildCache":null}`))
	jsonEqual(t, dfSummary(t, e.call(dockerusage.Verb, []byte(`{}`))), `{"totals":{
		"images":{"count":0,"active":0,"size":0,"reclaimable":0},
		"containers":{"count":0,"active":0,"size":0,"reclaimable":0},
		"volumes":{"count":0,"active":0,"size":0,"reclaimable":0},
		"build_cache":{"count":0,"active":0,"size":0,"reclaimable":0}},
		"images":[],"volumes":[],"containers":[]}`)
}

// Image totals follow docker system df: with LayersSize, reclaimable is
// LayersSize less Size-SharedSize of the images in use (unknown skipped),
// clamped to [0, LayersSize]; without it, the sums.
func TestDockerDFImageTotals(t *testing.T) {
	img := func(c string, size, shared, ctrs int) string {
		return fmt.Sprintf(`{"Id":"sha256:%s","RepoTags":["%s:1"],"Size":%d,"SharedSize":%d,"Containers":%d}`,
			strings.Repeat(c, 64), c, size, shared, ctrs)
	}
	for _, tc := range []struct {
		name string
		body string
		want dockerusage.Category
	}{
		{"no LayersSize", `{"Images":[` + img("a", 40, -1, 0) + `,` + img("b", -1, -1, 3) + `]}`,
			dockerusage.Category{Count: 2, Active: 1, Size: 40, Reclaimable: 40}},
		{"unused images own bytes only", `{"LayersSize":100,"Images":[` + img("a", 80, 10, 1) + `,` + img("b", 40, 15, 0) + `]}`,
			dockerusage.Category{Count: 2, Active: 1, Size: 100, Reclaimable: 25}},
		{"unknown shared size counts the whole image", `{"LayersSize":100,"Images":[` + img("a", 30, -1, 0) + `,` + img("b", -1, 5, 0) + `]}`,
			dockerusage.Category{Count: 2, Active: 0, Size: 100, Reclaimable: 30}},
		{"in-use images never count", `{"LayersSize":50,"Images":[` + img("a", 80, 0, 1) + `]}`,
			dockerusage.Category{Count: 1, Active: 1, Size: 50, Reclaimable: 0}},
		{"at most the size", `{"LayersSize":30,"Images":[` + img("a", 40, -1, 0) + `]}`,
			dockerusage.Category{Count: 1, Active: 0, Size: 30, Reclaimable: 30}},
		// The raw Engine answer of pi3 (Docker 29.9, 2026-11-24), where
		// docker system df printed 21 images, 15 active, 2GB, 193.5MB.
		{"pi3 Docker 29.9", pi3SystemDF(),
			dockerusage.Category{Count: 21, Active: 15, Size: 1999997165, Reclaimable: 193522176}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := dfEnv(t, imagePolicy, []byte(tc.body))
			var s dockerusage.Summary
			if err := json.Unmarshal(dfSummary(t, e.call(dockerusage.Verb, []byte(`{}`))), &s); err != nil {
				t.Fatal(err)
			}
			if s.Totals.Images != tc.want {
				t.Fatalf("%+v", s.Totals.Images)
			}
		})
	}
}

// A shared build cache record is counted but not sized, as in docker
// system df; a volume without UsageData is unknown and not in use.
func TestDockerDFBuildCacheAndVolumeUsage(t *testing.T) {
	e := dfEnv(t, imagePolicy, []byte(`{"BuildCache":[
		{"InUse":true,"Shared":true,"Size":1000},{"InUse":false,"Shared":true,"Size":500},
		{"InUse":true,"Shared":false,"Size":7},{"InUse":false,"Shared":false,"Size":-1},{"InUse":false,"Size":3}],
		"Volumes":[{"Name":"a"},{"Name":"b","UsageData":null},{"Name":"c","UsageData":{"Size":4,"RefCount":2}}]}`))
	var s dockerusage.Summary
	if err := json.Unmarshal(dfSummary(t, e.call(dockerusage.Verb, []byte(`{}`))), &s); err != nil {
		t.Fatal(err)
	}
	if s.Totals.BuildCache != (dockerusage.Category{Count: 5, Active: 2, Size: 10, Reclaimable: 3}) {
		t.Fatalf("%+v", s.Totals.BuildCache)
	}
	if s.Totals.Volumes != (dockerusage.Category{Count: 3, Active: 1, Size: 4, Reclaimable: 0}) {
		t.Fatalf("%+v", s.Totals.Volumes)
	}
	b, _ := json.Marshal(s.Volumes)
	jsonEqual(t, b, `[{"name":"c","size":4,"in_use":true},{"name":"a","size":null,"in_use":false},{"name":"b","size":null,"in_use":false}]`)
}

// The gate is container_list's: images or edge.
func TestDockerDFGate(t *testing.T) {
	fixture := readFixture(t, "system-df.json")
	for _, tc := range []struct {
		name, policy, code string
	}{
		{"images", imagePolicy, ""},
		{"edge only", "edge = true\n", ""},
		{"neither", "enabled = true\nimages = []\nprojects = []\n", dockerusage.ReasonDisabled},
		{"no file", "", dockerusage.ReasonDisabled},
		{"invalid", "edge = true\nnope = 1\n", dockerusage.ReasonDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := dfEnv(t, tc.policy, fixture)
			if tc.name == "no file" {
				e.sys.Policy = filepath.Join(t.TempDir(), "missing.toml")
			}
			raw := e.call(dockerusage.Verb, []byte(`{}`))
			if tc.code == "" {
				dfSummary(t, raw)
				return
			}
			if imgCode(t, raw) != tc.code {
				t.Fatalf("%s", raw)
			}
			if len(e.fake.Calls()) != 0 {
				t.Fatal("engine called while disabled")
			}
		})
	}
}

func TestDockerDFBadRequest(t *testing.T) {
	e := dfEnv(t, imagePolicy, readFixture(t, "system-df.json"))
	for _, in := range []string{`{"x":1}`, `null`, `[]`, ``, `{}{}`} {
		if c := imgCode(t, e.call(dockerusage.Verb, []byte(in))); c != programs.ReasonBadSpec {
			t.Fatalf("%q: %s", in, c)
		}
	}
}

func TestDockerDFEngineDown(t *testing.T) {
	e := dfEnv(t, imagePolicy, nil)
	e.sys.DockerSock = filepath.Join(t.TempDir(), "gone.sock")
	if c := imgCode(t, e.call(dockerusage.Verb, []byte(`{}`))); c != dockerusage.ReasonUnavailable {
		t.Fatal(c)
	}
}

// A 500 is logged with the scrubbed Engine message, a bad body is
// docker_unavailable, and neither reaches the answer.
func TestDockerDFEngineErrors(t *testing.T) {
	var logs bytes.Buffer
	e := dfEnv(t, imagePolicy, []byte(`{"message":"df failed at /var/lib/docker"}`))
	e.fake.set(func(f *fakeDocker) { f.dfCode = http.StatusInternalServerError })
	e.sys.Logf = func(f string, a ...any) { fmt.Fprintf(&logs, f+"\n", a...) }
	raw := e.call(dockerusage.Verb, []byte(`{}`))
	if imgCode(t, raw) != dockerusage.ReasonUnavailable || bytes.Contains(raw, []byte("var")) {
		t.Fatalf("%s", raw)
	}
	if !strings.Contains(logs.String(), "GET /system/df: status 500") {
		t.Fatalf("log %q", logs.String())
	}
	e = dfEnv(t, imagePolicy, []byte(`{"Images":"nope"}`))
	if c := imgCode(t, e.call(dockerusage.Verb, []byte(`{}`))); c != dockerusage.ReasonUnavailable {
		t.Fatal(c)
	}
}

func TestDockerDFDeadline(t *testing.T) {
	old := dfTimeout
	dfTimeout = 200 * time.Millisecond
	t.Cleanup(func() { dfTimeout = old })
	e := dfEnv(t, imagePolicy, readFixture(t, "system-df.json"))
	e.fake.set(func(f *fakeDocker) { f.dfDelay = 5 * time.Second })
	start := time.Now()
	if c := imgCode(t, e.call(dockerusage.Verb, []byte(`{}`))); c != dockerusage.ReasonTimeout {
		t.Fatal(c)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("deadline not applied")
	}
	// A caller that has gone away is not a timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c := imgCode(t, Handle(ctx, dockerusage.Verb, []byte(`{}`), e.sys)); c != dockerusage.ReasonUnavailable {
		t.Fatal(c)
	}
}

// exactDF is a valid /system/df answer of exactly n bytes.
func exactDF(n int) []byte {
	head := `{"Images":[{"Id":"sha256:` + strings.Repeat("a", 64) + `","RepoTags":["`
	tail := `"],"Size":1,"Containers":0}]}`
	return []byte(head + strings.Repeat("x", n-len(head)-len(tail)) + tail)
}

// The 32 MiB limit is for /system/df alone; every other call keeps 4 MiB.
func TestDockerDFBodyLimit(t *testing.T) {
	if maxEngineJSON != 4<<20 || maxDFJSON != 32<<20 {
		t.Fatal("limits changed")
	}
	body := exactDF(maxDFJSON)
	if len(body) != maxDFJSON {
		t.Fatalf("fixture %d", len(body))
	}
	e := dfEnv(t, imagePolicy, body)
	dfSummary(t, e.call(dockerusage.Verb, []byte(`{}`)))

	body = exactDF(maxDFJSON + 1)
	if len(body) != maxDFJSON+1 {
		t.Fatalf("fixture %d", len(body))
	}
	e = dfEnv(t, imagePolicy, body)
	if c := imgCode(t, e.call(dockerusage.Verb, []byte(`{}`))); c != dockerusage.ReasonUnavailable {
		t.Fatal(c)
	}

	e = dfEnv(t, imagePolicy, exactDF(5<<20))
	list := []byte(`[{"Id":"` + strings.Repeat("a", 64) + `","Names":["/` + strings.Repeat("x", 5<<20) + `"]}]`)
	e.fake.set(func(f *fakeDocker) { f.listBody = list })
	dfSummary(t, e.call(dockerusage.Verb, []byte(`{}`)))
	if _, err := newEngine(e.sys.DockerSock, nil).List(context.Background()); err == nil {
		t.Fatal("a 5 MiB list was read")
	}
}

// A caller that goes away while Docker is still sizing is docker_unavailable.
func TestDockerDFCallerGone(t *testing.T) {
	e := dfEnv(t, imagePolicy, readFixture(t, "system-df.json"))
	e.fake.set(func(f *fakeDocker) { f.dfDelay = 5 * time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	if c := imgCode(t, Handle(ctx, dockerusage.Verb, []byte(`{}`), e.sys)); c != dockerusage.ReasonUnavailable {
		t.Fatal(c)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancel not applied")
	}
}

// Each call re-reads runner.toml.
func TestDockerDFRereadsPolicy(t *testing.T) {
	e := dfEnv(t, imagePolicy, readFixture(t, "system-df.json"))
	dfSummary(t, e.call(dockerusage.Verb, []byte(`{}`)))
	e.setPolicy("enabled = true\nimages = []\nprojects = []\n")
	if c := imgCode(t, e.call(dockerusage.Verb, []byte(`{}`))); c != dockerusage.ReasonDisabled {
		t.Fatal(c)
	}
}

// docker_df is a read-only helper verb: known, not mutating, allowed
// during an agent upgrade.
func TestDockerDFVerbRegistry(t *testing.T) {
	if !programs.KnownVerb(dockerusage.Verb) || mutatingVerb(dockerusage.Verb) {
		t.Fatal("registry")
	}
	if programs.FixedReason(dockerusage.ReasonDisabled) != dockerusage.ReasonDisabled ||
		programs.FixedReason(dockerusage.ReasonTimeout) != dockerusage.ReasonTimeout {
		t.Fatal("reasons")
	}
}

// pi3SystemDF is the image part of pi3's /system/df on 2026-11-24 (Size,
// SharedSize, Containers per image, as ops read them; names left out).
func pi3SystemDF() string {
	rows := [][3]int64{
		{39876923, 0, 0}, {62661000, 8657177, 1}, {62562125, 8657177, 0}, {49295439, 8657177, 0},
		{13907186, 445238, 0}, {39469908, 8130724, 1}, {62660635, 0, 1}, {39471636, 8130724, 1},
		{58340818, 2110783, 1}, {570456319, 0, 1}, {134825889, 134825889, 1}, {10576526, 8822697, 2},
		{42283205, 2110783, 2}, {134825889, 134825889, 1}, {570426843, 0, 1}, {39474869, 0, 2},
		{8822697, 8822697, 1}, {49709363, 8174522, 0}, {4105254, 0, 0}, {181963586, 0, 3}, {4105262, 0, 3},
	}
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf(`{"Id":"sha256:%064x","RepoTags":[],"Size":%d,"SharedSize":%d,"Containers":%d}`, i+1, r[0], r[1], r[2])
	}
	return `{"LayersSize":1999997165,"Images":[` + strings.Join(parts, ",") + `]}`
}
