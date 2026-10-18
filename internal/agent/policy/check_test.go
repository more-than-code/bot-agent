package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSkipsOwnershipAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.toml")
	body := []byte("enabled = true\n")
	if err := os.WriteFile(path, body, 0o666); err != nil {
		t.Fatal(err)
	}
	loaded := Load(Files{Policy: path, Passwd: filepath.Join(dir, "passwd"), Group: filepath.Join(dir, "group")})
	if loaded.Valid || loaded.Reason == "" {
		t.Fatalf("Load accepted a non-root file: %+v", loaded)
	}
	got := Check(path, filepath.Join(dir, "passwd"), filepath.Join(dir, "group"))
	if !got.Valid || !got.Enabled || got.Reason != "" {
		t.Fatalf("Check %+v", got)
	}
}

func TestCheckResolvesRunAs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, []byte("enabled = true\nrun_as = [\"root\", \"joe\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/bash\njoe:x:1000:1000:Joe:/home/joe:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\njoe:x:1000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Check(path, passwd, group)
	if !got.Valid || len(got.RunAs) != 1 || got.RunAs[0] != "joe" {
		t.Fatalf("kept %+v", got)
	}
	if len(got.Dropped) != 1 || got.Dropped[0].Name != "root" || got.Dropped[0].Reason != "root" {
		t.Fatalf("dropped %+v", got.Dropped)
	}
}

func TestCheckRunAsExistingAndMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.toml")
	if err := os.WriteFile(path, []byte("enabled = true\nrun_as = [\"joe\", \"nosuch\", \"root\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/bash\njoe:x:1000:1000:Joe:/home/joe:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\njoe:x:1000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Check(path, passwd, group)
	if !got.Valid || len(got.RunAs) != 1 || got.RunAs[0] != "joe" {
		t.Fatalf("kept %+v", got)
	}
	dropped := map[string]string{}
	for _, d := range got.Dropped {
		dropped[d.Name] = d.Reason
	}
	if dropped["nosuch"] != "missing" || dropped["root"] != "root" || len(dropped) != 2 {
		t.Fatalf("dropped %+v", got.Dropped)
	}
}
