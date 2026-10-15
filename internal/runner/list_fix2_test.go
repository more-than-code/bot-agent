package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
)

func TestListKeepsLabelledWithinTwoHundred(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	labelled := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		name := "z-owned-" + itoa(i)
		e.fake.add(name, "running", "alpine:latest", e.labels("web"))
		labelled = append(labelled, name)
	}
	for i := 0; i < 460; i++ {
		e.fake.add("a-other-"+itoa(i), "running", "alpine:latest", nil)
	}
	raw := e.call(images.VerbList, []byte(`{}`))
	if imgCode(t, raw) != "" {
		t.Fatal(string(raw))
	}
	if len(raw) > programs.MaxListOut {
		t.Fatalf("encoded %d over %d", len(raw), programs.MaxListOut)
	}
	var list images.ListResponse
	if json.Unmarshal(raw, &list) != nil || !list.Truncated || len(list.Items) != maxListItems {
		t.Fatalf("n %d truncated %v", len(list.Items), list.Truncated)
	}
	got := map[string]bool{}
	var prev string
	prevLab := true
	for i, item := range list.Items {
		lab := len(item.Labels) > 0
		if !lab && prevLab {
			prev = ""
		}
		if lab != prevLab && !prevLab {
			t.Fatalf("labelled row after an other row at %d", i)
		}
		if prev != "" && item.Name < prev {
			t.Fatalf("order %s then %s", prev, item.Name)
		}
		prev, prevLab = item.Name, lab
		got[item.Name] = true
	}
	for _, name := range labelled {
		if !got[name] {
			t.Fatalf("dropped labelled %s", name)
		}
	}
}

func TestListProjectionNeedsRaisedStdoutCap(t *testing.T) {
	labels := map[string]string{
		images.LabelOwner:    images.OwnerValue,
		images.LabelHost:     "0123456789abcdef",
		images.LabelProject:  strings.Repeat("p", 64),
		images.LabelEntry:    strings.Repeat("e", 48),
		images.LabelSpecHash: strings.Repeat("ab", 32),
	}
	ports := make([]portView, 16)
	for i := range ports {
		ports[i] = portView{HostIP: "255.255.255.255", HostPort: 40000 + i, ContainerPort: 40000 + i}
	}
	rows := make([]listed, maxListItems)
	for i := range rows {
		rows[i] = listed{
			ID:     padHex(i + 1),
			Names:  []string{strings.Repeat("n", 200) + itoa(i)},
			State:  "restarting",
			Image:  "ghcr.io/acme/app@sha256:" + strings.Repeat("cd", 32),
			Labels: labels,
			Ports:  ports,
		}
	}
	raw := images.Encode(projectRows(rows))
	if len(raw) <= programs.MaxHelperOut {
		t.Fatalf("200 full rows fit in %d; container_list does not need its own cap", programs.MaxHelperOut)
	}
	if len(raw) > programs.MaxListOut {
		t.Fatalf("encoded %d over list cap %d", len(raw), programs.MaxListOut)
	}
	var list images.ListResponse
	if json.Unmarshal(raw, &list) != nil || list.Truncated || len(list.Items) != maxListItems {
		t.Fatalf("n %d truncated %v", len(list.Items), list.Truncated)
	}
}

func TestStatusAndLogsLeaveNoLockFile(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	raw := e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	var st images.StatusResult
	if json.Unmarshal(raw, &st) != nil || st.Status.State != "missing" {
		t.Fatal(string(raw))
	}
	raw = e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{Project: "mohiguide", Entry: "web", Lines: 20}))
	var logs images.LogsResponse
	if json.Unmarshal(raw, &logs) != nil || len(logs.Lines) != 0 {
		t.Fatal(string(raw))
	}
	rel, ok := images.LockPath("mohiguide", "web")
	if !ok {
		t.Fatal("lock path")
	}
	if _, err := os.Stat(filepath.Join(e.sys.Root, strings.TrimPrefix(rel, "/"))); !os.IsNotExist(err) {
		t.Fatalf("entry lock: %v", err)
	}
	ents, err := os.ReadDir(filepath.Join(e.sys.Root, "run", "bot-agent"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		if strings.HasSuffix(ent.Name(), ".lock") {
			t.Fatalf("lock file %s", ent.Name())
		}
	}
}
