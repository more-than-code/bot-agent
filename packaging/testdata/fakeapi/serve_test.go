// Package fakeapi is a test-only process for the installer systemd test.
// It is not part of a release bundle and it is not a cmd/ binary.
// go test ./... skips it unless BOT_AGENT_FAKE_URL_FILE and
// BOT_AGENT_FAKE_COUNT_FILE are set. The systemd test runs the compiled
// binary until it is killed.
package fakeapi

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/fakeserver"
)

func TestServe(t *testing.T) {
	urlPath := os.Getenv("BOT_AGENT_FAKE_URL_FILE")
	countPath := os.Getenv("BOT_AGENT_FAKE_COUNT_FILE")
	if urlPath == "" || countPath == "" {
		t.Skip("fake api binary only")
	}
	s := fakeserver.Start(t)
	if err := os.WriteFile(urlPath, []byte(s.URL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for {
		n := 0
		for _, f := range s.Frames() {
			if f.Type == "hello" {
				n++
			}
		}
		body := []byte(strconv.Itoa(n) + "\n")
		tmp := countPath + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, countPath); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
