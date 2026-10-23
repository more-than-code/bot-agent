package protocol

import "testing"

func TestImageEntryReservedSuffixes(t *testing.T) {
	cases := map[string]bool{
		"web": true, "old": true, "webold": true, "web-older": true, "prev": true,
		"web-old": false, "web-prev": false, "web-": false, "a-old": false,
	}
	for entry, want := range cases {
		if got := validImageEntry(entry); got != want {
			t.Fatalf("%s: got %v", entry, got)
		}
	}
}
