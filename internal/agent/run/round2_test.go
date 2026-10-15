package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/spool"
)

func TestErrorCodeNewlineIsOneLogLine(t *testing.T) {
	var buf bytes.Buffer
	a := &agent{opt: Options{Log: log.New(&buf, "", 0)}}
	raw := []byte(`{"v":1,"type":"error","error_code":"bad\ncode","message":"ignored"}`)
	if err := a.serverError(raw); err == nil {
		t.Fatal("expected error")
	}
	logs := buf.String()
	if strings.Count(logs, "\n") != 1 {
		t.Fatalf("log lines %q", logs)
	}
	if !strings.Contains(logs, `bad\ncode`) {
		t.Fatalf("not quoted %q", logs)
	}
}

func TestQuoteLogCapsRunes(t *testing.T) {
	got := quoteLog(strings.Repeat("ü", 80))
	if got != `"`+strings.Repeat("ü", logQuoteMax)+`"` {
		t.Fatalf("%s", got)
	}
}

func TestMetricsFramesStayUnderLimit(t *testing.T) {
	const n = 1800
	cmd := strings.Repeat("c", 1024)
	// QueuedAt is compared with the wall clock. A fixed October 14 timestamp
	// falls out of the 24h spool window on later days and the file is deleted.
	now := time.Now().UTC()
	samples := make([]protocol.Sample, n)
	for i := range samples {
		samples[i] = protocol.Sample{
			M: "process",
			T: protocol.FormatTime(now),
			Tags: map[string]string{
				"pid":          strconv.Itoa(i + 1),
				"process_name": "node",
				"user":         "bot",
				"cmdline":      cmd,
			},
			F: map[string]any{
				"memory_rss":   4096,
				"memory_usage": 0.1,
				"num_threads":  1,
				"started_at":   protocol.FormatTime(now),
			},
		}
	}
	dir := t.TempDir()
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{
		opt:   Options{Log: log.New(io.Discard, "", 0), Now: func() time.Time { return now }},
		spool: sp,
	}
	var sent [][]byte
	if err := a.emitMetrics(context.Background(), func(_ context.Context, b []byte) error {
		sent = append(sent, append([]byte(nil), b...))
		return nil
	}, samples, now); err != nil {
		t.Fatal(err)
	}
	if len(sent) < 2 {
		t.Fatalf("frames %d", len(sent))
	}
	seen := map[string]bool{}
	total := 0
	for _, raw := range sent {
		if len(raw) > maxMetricsBytes || len(raw) > protocol.MaxFrame {
			t.Fatalf("frame %d bytes", len(raw))
		}
		var m struct {
			BatchID string            `json:"batch_id"`
			Samples []protocol.Sample `json:"samples"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m.BatchID == "" || seen[m.BatchID] {
			t.Fatalf("batch id %q", m.BatchID)
		}
		seen[m.BatchID] = true
		if len(m.Samples) == 0 || len(m.Samples) > protocol.MaxSamples {
			t.Fatalf("samples %d", len(m.Samples))
		}
		total += len(m.Samples)
		if _, err := os.Stat(filepath.Join(dir, "spool", m.BatchID+".json")); err != nil {
			t.Fatal(err)
		}
	}
	if total != n {
		t.Fatalf("kept %d samples", total)
	}
	items, err := sp.List()
	if err != nil || len(items) != len(sent) {
		t.Fatalf("spool files %d err %v", len(items), err)
	}
}

func TestMetricsSplitBySampleCount(t *testing.T) {
	samples := make([]protocol.Sample, protocol.MaxSamples+1)
	for i := range samples {
		samples[i] = protocol.Sample{
			M: "system", T: "2026-10-14T00:00:00Z",
			Tags: map[string]string{}, F: map[string]any{"uptime": 1},
		}
	}
	frames, err := buildMetricsFrames(samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 2 {
		t.Fatalf("frames %d", len(frames))
	}
	for _, fr := range frames {
		if len(fr.raw) > protocol.MaxFrame {
			t.Fatalf("frame %d", len(fr.raw))
		}
	}
}

func TestTaskQueueBusy(t *testing.T) {
	clearAgentEnv(t)
	srv := fakeserver.Start(t)
	srv.Heartbeat = 120
	srv.Config.MetricsIntervalSec = 60
	srv.Config.ProcessIntervalSec = 300
	hold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hang" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
		case <-timer.C:
		}
	}))
	defer hold.Close()
	live := portFrom(t, hold.Listener.Addr().String())
	proc, sys, passwd := fixtureProc(t, live)
	envPath, _ := writeEnv(t, srv.URL, t.TempDir())
	var started atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Options{
			EnvFile: envPath, ProcRoot: proc, SysRoot: sys, Passwd: passwd,
			StatFS:        statFS(1000, 1000),
			EUID:          func() int { return 1000 },
			DiscoverEvery: time.Hour,
			OnTaskStart:   func() { started.Add(1) },
			Sleep:         func(context.Context, time.Duration) error { return nil },
			Log:           stdLog(&bytes.Buffer{}),
		})
	}()
	defer stopRun(t, cancel, errCh)
	if !srv.Wait(4*time.Second, func(fr []fakeserver.Frame) bool { return countType(fr, "hello") >= 1 }) {
		t.Fatal(frameTypes(srv.Frames()))
	}
	exp := protocol.FormatTime(time.Now().Add(10 * time.Minute))
	spec := json.RawMessage(fmt.Sprintf(`{"protocol":"http","port":%d,"path":"/hang","expect_status_min":200,"expect_status_max":299}`, live))
	for i := 0; i < maxRunningTasks; i++ {
		if err := srv.SendTask(protocol.Task{
			V: 1, Type: "task", TaskID: fmt.Sprintf("block-%d", i), HostID: srv.HostID,
			Kind: "probe_health", Service: protocol.Str("nginx"), ExpiresAt: exp, TimeoutSec: 30, Spec: spec,
		}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for started.Load() < maxRunningTasks && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if started.Load() != int32(maxRunningTasks) {
		t.Fatalf("started %d", started.Load())
	}
	for i := 0; i < maxWaitingTasks; i++ {
		if err := srv.SendTask(protocol.Task{
			V: 1, Type: "task", TaskID: fmt.Sprintf("wait-%02d", i), HostID: srv.HostID,
			Kind: "host_snapshot", ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.SendTask(protocol.Task{
		V: 1, Type: "task", TaskID: "stale", HostID: srv.HostID, Kind: "host_snapshot",
		ExpiresAt: protocol.FormatTime(time.Now().Add(-time.Minute)), TimeoutSec: 15,
		Spec: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.SendTask(protocol.Task{
		V: 1, Type: "task", TaskID: "other-kind", HostID: srv.HostID, Kind: "docker_logs",
		ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.SendTask(protocol.Task{
		V: 1, Type: "task", TaskID: "overflow", HostID: srv.HostID, Kind: "host_snapshot",
		ExpiresAt: exp, TimeoutSec: 15, Spec: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if !srv.Wait(3*time.Second, func(fr []fakeserver.Frame) bool {
		rej, _ := taskOutcomes(fr)
		return rej["overflow"] == "busy" && rej["stale"] == "expired" && rej["other-kind"] == "forbidden"
	}) {
		rej, res := taskOutcomes(srv.Frames())
		t.Fatalf("reject %v result %v started %d", rej, res, started.Load())
	}
	rej, res := taskOutcomes(srv.Frames())
	for i := 0; i < maxWaitingTasks; i++ {
		id := fmt.Sprintf("wait-%02d", i)
		if rej[id] != "" || res[id] != "" {
			t.Fatalf("waiter %s reject %q result %q", id, rej[id], res[id])
		}
	}
	if started.Load() != int32(maxRunningTasks) {
		t.Fatalf("overflow started a task: %d", started.Load())
	}
}
