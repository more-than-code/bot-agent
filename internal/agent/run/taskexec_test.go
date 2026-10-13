package run

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

func TestPendingDropsOldestAndLogs(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{opt: Options{Log: log.New(&buf, "", 0)}}
	for i := 0; i < maxPendingResults; i++ {
		a.storeResult(protocol.TaskResult{TaskID: fmt.Sprintf("t%03d", i), State: "succeeded"})
	}
	if strings.Contains(buf.String(), "drop unacked") {
		t.Fatalf("dropped early: %s", buf.String())
	}
	a.storeResult(protocol.TaskResult{TaskID: "t100", State: "succeeded"})
	if !strings.Contains(buf.String(), `bot-agent: drop unacked task_result id="t000"`) {
		t.Fatalf("log %s", buf.String())
	}
	if _, ok := a.pending.get("t000"); ok {
		t.Fatal("oldest still pending")
	}
	if _, ok := a.pending.get("t100"); !ok {
		t.Fatal("newest missing")
	}
	if _, ok := a.pending.get("t001"); !ok {
		t.Fatal("second missing")
	}
	a.pending.ack("t001")
	a.storeResult(protocol.TaskResult{TaskID: "t101", State: "succeeded"})
	if strings.Count(buf.String(), "drop unacked") != 1 {
		t.Fatalf("ack did not free a slot: %s", buf.String())
	}
}
