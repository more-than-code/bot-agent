package images

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestFileRules(t *testing.T) {
	base := ImageSpec{
		Kind: "service", Image: "ghcr.io/joe-and-his-friends/app:1",
		Volumes: []Volume{{Name: "data", Target: "/var/data"}},
		Files:   []SpecFile{{File: "app.conf", Target: "/etc/app.conf"}},
	}
	if base.Validate() != "" {
		t.Fatal(base.Validate())
	}
	if !ValidFileName("a") || !ValidFileName("a.conf") || !ValidFileName("a_b-c.d") {
		t.Fatal("names")
	}
	good := []SpecFile{{File: "app.conf", Target: "/etc/app.conf"}}
	if CheckResolved(good, []MountFile{{Name: "app.conf", Target: "/etc/app.conf", Content: "x"}}) != "" {
		t.Fatal("aligned")
	}
	long := strings.Repeat("a", maxFileBytes)
	if !ValidFileContent(long) || ValidFileContent(long+"b") || ValidFileContent("") || ValidFileContent("a\x00b") || ValidFileContent("\xff") {
		t.Fatal("content bounds")
	}
	// /procx is not under /proc. /var/data2 is not under /var/data.
	okTargets := ImageSpec{
		Kind: "service", Image: "ghcr.io/joe-and-his-friends/app:1",
		Volumes: []Volume{{Name: "data", Target: "/var/data"}},
		Files:   []SpecFile{{File: "p", Target: "/procx"}, {File: "q", Target: "/var/data2"}},
	}
	if okTargets.Validate() != "" {
		t.Fatal(okTargets.Validate())
	}

	bad := []struct {
		name string
		spec []SpecFile
		vols []Volume
		got  []MountFile
	}{
		{"root", []SpecFile{{File: "a", Target: "/"}}, nil, nil},
		{"proc", []SpecFile{{File: "a", Target: "/proc/x"}}, nil, nil},
		{"sys", []SpecFile{{File: "a", Target: "/sys"}}, nil, nil},
		{"dev", []SpecFile{{File: "a", Target: "/dev/null"}}, nil, nil},
		{"dotdot", []SpecFile{{File: "a", Target: "/a/../b"}}, nil, nil},
		{"trail", []SpecFile{{File: "a", Target: "/a/"}}, nil, nil},
		{"dup target", []SpecFile{{File: "a", Target: "/etc/a"}, {File: "b", Target: "/etc/a"}}, nil, nil},
		{"dup name", []SpecFile{{File: "a", Target: "/etc/a"}, {File: "a", Target: "/etc/b"}}, nil, nil},
		{"bad name", []SpecFile{{File: "A", Target: "/etc/a"}}, nil, nil},
		{"dot", []SpecFile{{File: ".", Target: "/etc/a"}}, nil, nil},
		{"dotdot name", []SpecFile{{File: "..", Target: "/etc/a"}}, nil, nil},
		{"under volume", []SpecFile{{File: "a", Target: "/var/data/x"}}, []Volume{{Name: "data", Target: "/var/data"}}, nil},
		{"volume target", []SpecFile{{File: "a", Target: "/var/data"}}, []Volume{{Name: "data", Target: "/var/data"}}, nil},
		{"volume under file", []SpecFile{{File: "a", Target: "/var"}}, []Volume{{Name: "data", Target: "/var/data"}}, nil},
		{"volume slash", []SpecFile{{File: "a", Target: "/etc/a"}}, []Volume{{Name: "data", Target: "/"}}, nil},
		{"order", []SpecFile{{File: "a", Target: "/etc/a"}, {File: "b", Target: "/etc/b"}}, nil, []MountFile{
			{Name: "b", Target: "/etc/b", Content: "x"},
			{Name: "a", Target: "/etc/a", Content: "y"},
		}},
		{"name mismatch", []SpecFile{{File: "a", Target: "/etc/a"}}, nil, []MountFile{{Name: "b", Target: "/etc/a", Content: "x"}}},
		{"empty content", []SpecFile{{File: "a", Target: "/etc/a"}}, nil, []MountFile{{Name: "a", Target: "/etc/a", Content: ""}}},
		{"nul content", []SpecFile{{File: "a", Target: "/etc/a"}}, nil, []MountFile{{Name: "a", Target: "/etc/a", Content: "a\x00b"}}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			spec := ImageSpec{Kind: "service", Image: "ghcr.io/joe-and-his-friends/app:1", Files: tc.spec, Volumes: tc.vols}
			if tc.got == nil {
				if spec.Validate() == "" {
					t.Fatal("accepted")
				}
				return
			}
			if spec.Validate() != "" {
				t.Fatal(spec.Validate())
			}
			if CheckResolved(tc.spec, tc.got) == "" {
				t.Fatal("resolved")
			}
		})
	}
	t.Run("seventeen", func(t *testing.T) {
		letters := "abcdefghijklmnopq"
		files := make([]SpecFile, 17)
		for i := range files {
			name := letters[i : i+1]
			files[i] = SpecFile{File: name, Target: "/opt/" + name}
		}
		spec := ImageSpec{Kind: "service", Image: "ghcr.io/joe-and-his-friends/app:1", Files: files}
		if spec.Validate() == "" {
			t.Fatal("accepted")
		}
	})
}

func TestFilesHashSortsByName(t *testing.T) {
	files := []MountFile{
		{Name: "b", Content: "beta"},
		{Name: "a", Content: "alpha"},
	}
	if FilesHash(nil) != "" || FilesHash(files) == "" {
		t.Fatal("empty")
	}
	if FilesHash(files) != rule8(files) {
		t.Fatalf("%s", FilesHash(files))
	}
	swapped := []MountFile{files[1], files[0]}
	if FilesHash(swapped) != FilesHash(files) {
		t.Fatal("order changed the hash")
	}
}

func rule8(files []MountFile) string {
	cp := append([]MountFile(nil), files...)
	// Same steps as the contract, written out so a change to FilesHash fails here.
	if cp[0].Name > cp[1].Name {
		cp[0], cp[1] = cp[1], cp[0]
	}
	h := sha256.New()
	for _, f := range cp {
		sum := sha256.Sum256([]byte(f.Content))
		_, _ = h.Write([]byte(f.Name + ":" + hex.EncodeToString(sum[:]) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestRedactLimits(t *testing.T) {
	if !ValidRedact(nil) || !ValidRedact([]string{"abcdef"}) {
		t.Fatal("empty")
	}
	many := make([]string, MaxRedactCount+1)
	for i := range many {
		many[i] = "abcdef"
	}
	if ValidRedact(many) {
		t.Fatal("count")
	}
	bulk := make([]string, 129)
	for i := range bulk {
		bulk[i] = strings.Repeat("a", MaxRedactBytes)
	}
	if ValidRedact(bulk) {
		t.Fatal("total")
	}
	fit := make([]string, 128)
	for i := range fit {
		fit[i] = strings.Repeat("a", MaxRedactBytes)
	}
	if !ValidRedact(fit) {
		t.Fatal("128 * 4096 is the total cap")
	}
}
