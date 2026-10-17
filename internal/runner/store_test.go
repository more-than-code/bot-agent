package runner

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/images"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/redact"
)

func testBase(t *testing.T) string {
	t.Helper()
	// /tmp and /var are symlinks on macOS. The store walk refuses those.
	if fi, err := os.Lstat("/private/tmp"); err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		return "/private/tmp"
	}
	return "/tmp"
}

func storeRootTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(testBase(t), "bot-files-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// A directory created under /tmp can come out group 0. The process can
	// chown it to its own gid, which is the owner the test hook expects.
	if err := os.Chown(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (e *imgEnv) useStore(t *testing.T) string {
	t.Helper()
	root := storeRootTemp(t)
	e.sys.fileStore = root
	e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
	return root
}

func plantSlot(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	for n, body := range files {
		path := filepath.Join(dir, n)
		if err := os.WriteFile(path, []byte(body), 0444); err != nil {
			t.Fatal(err)
		}
		// WriteFile applies the umask. The check wants 0444 exactly.
		if err := os.Chmod(path, 0444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, os.Getuid(), os.Getgid()); err != nil {
			t.Fatal(err)
		}
	}
}

func fileMode(t *testing.T, path string) (os.FileMode, int, int, uint64) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return os.FileMode(permOf(*st)), int(st.Uid), int(st.Gid), nlinkOf(*st)
}

func fileDeploy(content string) images.DeployRequest {
	return images.DeployRequest{
		Project: "mohiguide", Entry: "web", Digest: digestA, SpecHash: hashC,
		Spec: images.ImageSpec{
			Kind: "service", Image: "ghcr.io/joe-and-his-friends/mo-webadmin:1.2",
			Files: []images.SpecFile{{File: "app.conf", Target: "/etc/app.conf"}},
		},
		Files: []images.MountFile{{Name: "app.conf", Target: "/etc/app.conf", Content: content}},
	}
}

func TestSlotModes(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	root := e.useStore(t)
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != "" {
		t.Fatal("deploy")
	}
	slot := filepath.Join(root, "mohiguide-web")
	mode, uid, gid, _ := fileMode(t, slot)
	if mode != 0700 || uid != os.Getuid() || gid != os.Getgid() {
		t.Fatalf("slot mode %o uid %d gid %d", mode, uid, gid)
	}
	fmode, fuid, fgid, nlink := fileMode(t, filepath.Join(slot, "app.conf"))
	if fmode != 0444 || fuid != os.Getuid() || fgid != os.Getgid() || nlink != 1 {
		t.Fatalf("file mode %o uid %d gid %d nlink %d", fmode, fuid, fgid, nlink)
	}
	body, err := os.ReadFile(filepath.Join(slot, "app.conf"))
	if err != nil || string(body) != "v1" {
		t.Fatalf("%s %v", body, err)
	}
}

func TestSymlinkStoreRefused(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	real := storeRootTemp(t)
	link, err := os.MkdirTemp(testBase(t), "bot-files-link-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	e.sys.fileStore = link
	e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonBadSpec {
		t.Fatal("symlink store")
	}
	if created(e) {
		t.Fatal("container created")
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced")
	}
	entries, _ := os.ReadDir(real)
	if len(entries) != 0 {
		t.Fatalf("store target gained %d entries", len(entries))
	}
}

func TestSymlinkSlotAndFileRefused(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	root := e.useStore(t)
	slot := filepath.Join(root, "mohiguide-web")
	if err := os.Symlink(root, slot); err != nil {
		t.Fatal(err)
	}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonBadSpec || created(e) {
		t.Fatal("symlink slot")
	}
	if fi, err := os.Lstat(slot); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("slot symlink removed")
	}

	e = newImgEnv(t, imagePolicy)
	root = e.useStore(t)
	plantSlot(t, root, "mohiguide-web", nil)
	// The target is a regular 0444 file owned by the test owner, link count
	// 1, outside the slot. Following the link would see a file the check
	// accepts, so only AT_SYMLINK_NOFOLLOW refuses it.
	outside, err := os.MkdirTemp(testBase(t), "bot-link-target-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	target := filepath.Join(outside, "app.conf")
	if err := os.WriteFile(target, []byte("plain"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(target, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	mode, uid, gid, nlink := fileMode(t, target)
	fi, err := os.Lstat(target)
	if err != nil || !fi.Mode().IsRegular() || mode != 0444 || uid != os.Getuid() || gid != os.Getgid() || nlink != 1 {
		t.Fatalf("target mode %o uid %d gid %d nlink %d", mode, uid, gid, nlink)
	}
	link := filepath.Join(root, "mohiguide-web", "app.conf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonBadSpec || created(e) {
		t.Fatal("symlink file")
	}
	if fi, err = os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("file symlink removed")
	}
}

func TestBadFileShapeRefused(t *testing.T) {
	t.Run("mode", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		root := e.useStore(t)
		plantSlot(t, root, "mohiguide-web", map[string]string{"app.conf": "v"})
		if err := os.Chmod(filepath.Join(root, "mohiguide-web", "app.conf"), 0644); err != nil {
			t.Fatal(err)
		}
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonBadSpec || created(e) {
			t.Fatal("mode")
		}
		mode, _, _, _ := fileMode(t, filepath.Join(root, "mohiguide-web", "app.conf"))
		if mode != 0644 {
			t.Fatalf("mode changed to %o", mode)
		}
	})
	t.Run("nlink", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		root := e.useStore(t)
		plantSlot(t, root, "mohiguide-web", map[string]string{"app.conf": "v"})
		other := filepath.Join(root, "hard")
		if err := os.Link(filepath.Join(root, "mohiguide-web", "app.conf"), other); err != nil {
			t.Fatal(err)
		}
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonBadSpec || created(e) {
			t.Fatal("nlink")
		}
		_, _, _, nlink := fileMode(t, other)
		if nlink != 2 {
			t.Fatalf("link count %d", nlink)
		}
	})
	t.Run("extra", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		root := e.useStore(t)
		plantSlot(t, root, "mohiguide-web", map[string]string{"app.conf": "v"})
		extra := filepath.Join(root, "mohiguide-web", "nested")
		if err := os.Mkdir(extra, 0700); err != nil {
			t.Fatal(err)
		}
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonBadSpec || created(e) {
			t.Fatal("extra")
		}
		if _, err := os.Lstat(extra); err != nil {
			t.Fatal("extra removed")
		}
	})
	t.Run("extra-file", func(t *testing.T) {
		rejectExtraManagedFile(t)
	})
}

func TestLeftoverNewAndOrphanSlot(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	root := e.useStore(t)
	plantSlot(t, root, "mohiguide-web.new", map[string]string{"old": "new-marker"})
	plantSlot(t, root, "mohiguide-web", map[string]string{"old": "orphan-marker"})
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v2")))) != "" {
		t.Fatal("deploy")
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web.new")); !os.IsNotExist(err) {
		t.Fatal(".new leftover kept")
	}
	body, err := os.ReadFile(filepath.Join(root, "mohiguide-web", "app.conf"))
	if err != nil || string(body) != "v2" {
		t.Fatalf("slot %s %v", body, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web", "old")); !os.IsNotExist(err) {
		t.Fatal("orphan file kept")
	}
	prev := filepath.Join(root, "mohiguide-web-prev")
	entries, err := os.ReadDir(prev)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, ent := range entries {
		body, err := os.ReadFile(filepath.Join(prev, ent.Name()))
		if err == nil && strings.Contains(string(body), "orphan-marker") {
			t.Fatal("prev holds the orphan")
		}
	}
}

func TestAuditExactRejectsExtraName(t *testing.T) {
	rejectExtraManagedFile(t)
}

// rejectExtraManagedFile plants a regular 0444 file with a managed name
// into the slot just written. The shape check passes. The name audit refuses
// it, and the partial slot is removed.
func rejectExtraManagedFile(t *testing.T) {
	t.Helper()
	root := storeRootTemp(t)
	st, err := openStore(root, os.Getuid(), os.Getgid(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(st)
	st.plant = func(dirfd int) error {
		fd, err := openat(dirfd, "extra.conf", syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_WRONLY|syscall.O_CLOEXEC, 0444)
		if err != nil {
			return err
		}
		defer syscall.Close(fd)
		if err := syscall.Fchown(fd, os.Getuid(), os.Getgid()); err != nil {
			return err
		}
		if err := syscall.Fchmod(fd, 0444); err != nil {
			return err
		}
		return nil
	}
	files := []images.MountFile{{Name: "app.conf", Target: "/etc/app.conf", Content: "v"}}
	if err := st.writeSlot("mohiguide-web.new", files); err == nil {
		t.Fatal("extra name accepted")
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web.new")); !os.IsNotExist(err) {
		t.Fatal("partial slot kept")
	}
}

func TestBindMountsAndFilesHash(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.useStore(t)
	req := images.DeployRequest{
		Project: "mohiguide", Entry: "web", Digest: digestA, SpecHash: hashC,
		Spec: images.ImageSpec{
			Kind: "service", Image: "ghcr.io/joe-and-his-friends/mo-webadmin:1.2",
			Files: []images.SpecFile{
				{File: "b.conf", Target: "/etc/b.conf"},
				{File: "a.conf", Target: "/etc/a.conf"},
			},
		},
		Files: []images.MountFile{
			{Name: "b.conf", Target: "/etc/b.conf", Content: "beta"},
			{Name: "a.conf", Target: "/etc/a.conf", Content: "alpha"},
		},
	}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != "" {
		t.Fatal("deploy")
	}
	var body map[string]any
	if json.Unmarshal(e.fake.body("POST", "/containers/create"), &body) != nil {
		t.Fatal("create body")
	}
	labels := body["Labels"].(map[string]any)
	if labels["dev.deploybot.files_hash"] != rule8Hash(req.Files) {
		t.Fatalf("hash %v", labels["dev.deploybot.files_hash"])
	}
	mounts := body["HostConfig"].(map[string]any)["Mounts"].([]any)
	if len(mounts) != 2 {
		t.Fatalf("mounts %d", len(mounts))
	}
	for i, f := range req.Files {
		m := mounts[i].(map[string]any)
		if m["Type"] != "bind" || m["ReadOnly"] != true || m["Target"] != f.Target {
			t.Fatalf("mount %v", m)
		}
		if m["Source"] != "/var/lib/bot-agent-runner/files/mohiguide-web/"+f.Name {
			t.Fatalf("source %v", m["Source"])
		}
		if _, ok := m["BindOptions"]; ok {
			t.Fatal("BindOptions")
		}
		if len(m) != 4 {
			t.Fatalf("mount keys %v", m)
		}
	}
	raw := e.call(images.VerbStatus, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))
	var st images.StatusResult
	if json.Unmarshal(raw, &st) != nil || st.FilesHash != rule8Hash(req.Files) {
		t.Fatalf("status %s", raw)
	}

	e2 := newImgEnv(t, imagePolicy)
	if imgCode(t, e2.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))) != "" {
		t.Fatal("no files")
	}
	var plain map[string]any
	if json.Unmarshal(e2.fake.body("POST", "/containers/create"), &plain) != nil {
		t.Fatal("plain body")
	}
	if _, ok := plain["Labels"].(map[string]any)["dev.deploybot.files_hash"]; ok {
		t.Fatal("files_hash without files")
	}
	for _, m := range plain["HostConfig"].(map[string]any)["Mounts"].([]any) {
		if m.(map[string]any)["Type"] == "bind" {
			t.Fatal("bind without files")
		}
	}
}

func TestVolumeAndFileBothMounted(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.useStore(t)
	req := fileDeploy("v1")
	req.Spec.Volumes = []images.Volume{{Name: "data", Target: "/var/data"}}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != "" {
		t.Fatal("deploy")
	}
	var body map[string]any
	if json.Unmarshal(e.fake.body("POST", "/containers/create"), &body) != nil {
		t.Fatal("create body")
	}
	mounts := body["HostConfig"].(map[string]any)["Mounts"].([]any)
	sawVol, sawFile := false, false
	for _, raw := range mounts {
		m := raw.(map[string]any)
		switch m["Type"] {
		case "volume":
			if m["Source"] == "mohiguide-data" && m["Target"] == "/var/data" {
				sawVol = true
			}
		case "bind":
			if m["ReadOnly"] == true && m["Target"] == "/etc/app.conf" && m["Source"] == "/var/lib/bot-agent-runner/files/mohiguide-web/app.conf" {
				sawFile = true
			}
		}
	}
	if len(mounts) != 2 || !sawVol || !sawFile {
		t.Fatalf("mounts %v", mounts)
	}
}

func TestFileSlotOrder(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	root := e.useStore(t)
	e.fake.slotDir = filepath.Join(root, "mohiguide-web")
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != "" {
		t.Fatal("first")
	}
	second := fileDeploy("v2")
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, second))) != "" {
		t.Fatal("second")
	}
	if got := readFile(t, filepath.Join(root, "mohiguide-web", "app.conf")); got != "v2" {
		t.Fatalf("current %s", got)
	}
	if got := readFile(t, filepath.Join(root, "mohiguide-web-prev", "app.conf")); got != "v1" {
		t.Fatalf("prev %s", got)
	}
	names := containerNames(e)
	if !names["mohiguide-web-prev"] || !names["mohiguide-web"] {
		t.Fatalf("containers %v", names)
	}
	if imgCode(t, e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != "" {
		t.Fatal("rollback")
	}
	if got := readFile(t, filepath.Join(root, "mohiguide-web", "app.conf")); got != "v1" {
		t.Fatalf("rolled %s", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web-prev")); !os.IsNotExist(err) {
		t.Fatal("prev slot kept")
	}
	var creates, starts []string
	for _, s := range e.fake.slotSnaps() {
		switch s.op {
		case "create":
			creates = append(creates, s.files["app.conf"])
		case "start":
			starts = append(starts, s.files["app.conf"])
		}
	}
	if len(creates) != 2 || creates[0] != "v1" || creates[1] != "v2" {
		t.Fatalf("create slot %v", creates)
	}
	if len(starts) != 3 || starts[2] != "v1" {
		t.Fatalf("start slot %v", starts)
	}

	e = newImgEnv(t, imagePolicy)
	root = e.useStore(t)
	job := jobDeploy()
	job.Spec.Files = []images.SpecFile{{File: "job.conf", Target: "/etc/job.conf"}}
	job.Files = []images.MountFile{{Name: "job.conf", Target: "/etc/job.conf", Content: "j1"}}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, job))) != "" {
		t.Fatal("job")
	}
	job.Files[0].Content = "j2"
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, job))) != "" {
		t.Fatal("job replace")
	}
	if got := readFile(t, filepath.Join(root, "mohiguide-job", "job.conf")); got != "j2" {
		t.Fatalf("job slot %s", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-job-prev")); !os.IsNotExist(err) {
		t.Fatal("job touched -prev")
	}
	if imgCode(t, e.call(images.VerbRemove, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "job"}))) != "" {
		t.Fatal("remove")
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-job")); !os.IsNotExist(err) {
		t.Fatal("slot kept after remove")
	}
}

func TestHelperFileRefusals(t *testing.T) {
	cases := []images.DeployRequest{
		fileReq("a", "/", "x"),
		fileReq("a", "/proc/x", "x"),
		fileReq("a", "/a/../b", "x"),
		fileReq("a", "/a/", "x"),
		dupTarget(),
		underVolume(),
		volumeUnderFile(),
		fileReq("A", "/etc/a", "x"),
		fileReq("a", "/etc/a", ""),
		fileReq("a", "/etc/a", "a\x00b"),
		mismatch(),
	}
	for _, req := range cases {
		e := newImgEnv(t, imagePolicy)
		e.useStore(t)
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, req))) != programs.ReasonBadSpec || len(e.fake.Calls()) != 0 {
			t.Fatalf("%s", mustJSON(t, req.Spec.Files))
		}
	}
}

func TestDeployAcceptsOneAndAHalfMiB(t *testing.T) {
	e := newImgEnv(t, imagePolicy)
	e.useStore(t)
	req := jobDeploy()
	req.Env = map[string]string{}
	for i := 0; i < 140; i++ {
		req.Env["E"+itoa(i)] = strings.Repeat("b", 4096)
	}
	req.Spec.Files = make([]images.SpecFile, 16)
	req.Files = make([]images.MountFile, 16)
	for i := 0; i < 16; i++ {
		name := "f" + itoa(i)
		target := "/opt/f" + itoa(i)
		req.Spec.Files[i] = images.SpecFile{File: name, Target: target}
		req.Files[i] = images.MountFile{Name: name, Target: target, Content: strings.Repeat("c", 65536)}
	}
	body := mustJSON(t, req)
	if len(body) <= 3*1024*1024/2 || len(body) > images.MaxFrame {
		t.Fatalf("size %d", len(body))
	}
	if imgCode(t, e.call(images.VerbDeploy, body)) != "" || !created(e) {
		t.Fatal("1.5 MiB deploy")
	}
}

func TestOwnerAndModeRefused(t *testing.T) {
	t.Run("file-owner", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		root := e.useStore(t)
		plantSlot(t, root, "mohiguide-web", map[string]string{"app.conf": "kept"})
		slot := filepath.Join(root, "mohiguide-web")
		path := filepath.Join(slot, "app.conf")
		// The file, the slot, and the store share the process ids. The
		// per-file check expects a different uid. No chown to another id:
		// a non-root Linux process cannot do that.
		e.sys.fileCheck = &fileIDs{uid: os.Getuid() + 1, gid: os.Getgid()}
		mode, _, _, nlink := fileMode(t, path)
		if mode != 0444 || nlink != 1 {
			t.Fatalf("file mode %o nlink %d", mode, nlink)
		}
		assertOwner(t, root, e.sys.fileOwner.uid, e.sys.fileOwner.gid, true)
		assertOwner(t, slot, e.sys.fileOwner.uid, e.sys.fileOwner.gid, true)
		assertOwner(t, path, e.sys.fileOwner.uid, e.sys.fileOwner.gid, true)
		if e.sys.fileCheck.uid == e.sys.fileOwner.uid && e.sys.fileCheck.gid == e.sys.fileOwner.gid {
			t.Fatal("file check matches the directory owner")
		}
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonBadSpec || created(e) || mutated(e) {
			t.Fatal("owner")
		}
		if readFile(t, path) != "kept" {
			t.Fatal("file changed")
		}
		assertOwner(t, path, e.sys.fileOwner.uid, e.sys.fileOwner.gid, true)
	})
	t.Run("slot-mode", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		root := e.useStore(t)
		plantSlot(t, root, "mohiguide-web", map[string]string{"app.conf": "kept"})
		slot := filepath.Join(root, "mohiguide-web")
		if err := os.Chmod(slot, 0755); err != nil {
			t.Fatal(err)
		}
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonBadSpec || created(e) || mutated(e) {
			t.Fatal("slot mode")
		}
		mode, _, _, _ := fileMode(t, slot)
		if mode != 0755 || readFile(t, filepath.Join(slot, "app.conf")) != "kept" {
			t.Fatalf("slot mode %o", mode)
		}
	})
	t.Run("store-mode", func(t *testing.T) {
		e := newImgEnv(t, imagePolicy)
		root := e.useStore(t)
		plantSlot(t, root, "mohiguide-web", map[string]string{"app.conf": "kept"})
		if err := os.Chmod(root, 0755); err != nil {
			t.Fatal(err)
		}
		if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonBadSpec || created(e) || mutated(e) {
			t.Fatal("store mode")
		}
		mode, _, _, _ := fileMode(t, root)
		if mode != 0755 || readFile(t, filepath.Join(root, "mohiguide-web", "app.conf")) != "kept" {
			t.Fatalf("store mode %o", mode)
		}
	})
}

func assertOwner(t *testing.T, path string, uid, gid int, match bool) {
	t.Helper()
	_, gotUID, gotGID, _ := fileMode(t, path)
	same := gotUID == uid && gotGID == gid
	if same != match {
		t.Fatalf("%s uid %d gid %d want match %v against %d:%d", path, gotUID, gotGID, match, uid, gid)
	}
}

func TestRollbackStartFailureRestoresPair(t *testing.T) {
	e, root := twoFileDeploys(t)
	e.fake.startFail = true
	before := len(e.fake.Calls())
	if imgCode(t, e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != programs.ReasonDocker {
		t.Fatal("rollback")
	}
	started := false
	for _, c := range e.fake.Calls()[before:] {
		if strings.Contains(c.Path, "/start") {
			started = true
		}
	}
	if !started {
		t.Fatal("start was not attempted")
	}
	names := containerNames(e)
	if names["mohiguide-web"] || !names["mohiguide-web-prev"] {
		t.Fatalf("containers %v", names)
	}
	if got := readFile(t, filepath.Join(root, "mohiguide-web-prev", "app.conf")); got != "v1" {
		t.Fatalf("prev slot %s", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web")); !os.IsNotExist(err) {
		t.Fatal("current slot still present")
	}
}

func TestRollbackRenameFailureRestoresSlot(t *testing.T) {
	e, root := twoFileDeploys(t)
	e.fake.renameFail = true
	before := len(e.fake.Calls())
	if imgCode(t, e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != programs.ReasonDocker {
		t.Fatal("rollback")
	}
	for _, c := range e.fake.Calls()[before:] {
		if strings.Contains(c.Path, "/start") {
			t.Fatal("started after a failed rename")
		}
	}
	names := containerNames(e)
	if names["mohiguide-web"] || !names["mohiguide-web-prev"] {
		t.Fatalf("containers %v", names)
	}
	if got := readFile(t, filepath.Join(root, "mohiguide-web-prev", "app.conf")); got != "v1" {
		t.Fatalf("prev slot %s", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "mohiguide-web")); !os.IsNotExist(err) {
		t.Fatal("current slot still present")
	}
}

func TestStoreOpenErrors(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		e := twoPlainDeploys(t)
		parent := storeRootTemp(t)
		missing := filepath.Join(parent, "files")
		e.sys.fileStore = missing
		e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
		if imgCode(t, e.call(images.VerbRollback, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != "" {
			t.Fatal("rollback")
		}
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Fatal("missing store was created")
		}
		names := containerNames(e)
		if !names["mohiguide-web"] || names["mohiguide-web-prev"] {
			t.Fatalf("containers %v", names)
		}

		e = twoPlainDeploys(t)
		parent = storeRootTemp(t)
		missing = filepath.Join(parent, "files")
		e.sys.fileStore = missing
		e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
		if imgCode(t, e.call(images.VerbRemove, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != "" {
			t.Fatal("remove")
		}
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Fatal("remove created the store")
		}
		if containerNames(e)["mohiguide-web"] {
			t.Fatal("current container kept")
		}
	})
	t.Run("missing-parent", func(t *testing.T) {
		t.Run("deploy", func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			parent := storeRootTemp(t)
			e.sys.fileStore = filepath.Join(parent, "missing-parent", "files")
			e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
			if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))) != "" {
				t.Fatal("deploy")
			}
			if _, err := os.Lstat(filepath.Join(parent, "missing-parent")); !os.IsNotExist(err) {
				t.Fatal("parent was created")
			}
			if !containerNames(e)["mohiguide-web"] {
				t.Fatal("container missing")
			}
		})
		for _, verb := range []string{images.VerbRollback, images.VerbRemove} {
			t.Run(verb, func(t *testing.T) {
				e := twoPlainDeploys(t)
				parent := storeRootTemp(t)
				e.sys.fileStore = filepath.Join(parent, "missing-parent", "files")
				e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
				if imgCode(t, e.call(verb, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != "" {
					t.Fatalf("%s", verb)
				}
				if _, err := os.Lstat(filepath.Join(parent, "missing-parent")); !os.IsNotExist(err) {
					t.Fatal("parent was created")
				}
				names := containerNames(e)
				if verb == images.VerbRollback {
					if !names["mohiguide-web"] || names["mohiguide-web-prev"] {
						t.Fatalf("containers %v", names)
					}
					return
				}
				if names["mohiguide-web"] {
					t.Fatal("current container kept")
				}
			})
		}
		t.Run("with-files", func(t *testing.T) {
			e := newImgEnv(t, imagePolicy)
			parent := storeRootTemp(t)
			e.sys.fileStore = filepath.Join(parent, "missing-parent", "files")
			e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
			if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != programs.ReasonDocker || created(e) || mutated(e) {
				t.Fatal("files")
			}
			if _, err := os.Lstat(filepath.Join(parent, "missing-parent")); !os.IsNotExist(err) {
				t.Fatal("parent was created")
			}
		})
	})
	t.Run("other", func(t *testing.T) {
		// Root ignores mode bits, so a mode-0 directory does not return EACCES.
		// The assertion below needs that permission error.
		if os.Geteuid() == 0 {
			t.Skip("root does not get EACCES from a mode-0 directory")
		}
		// A permission error is not "no store", with or without files.
		for _, withFiles := range []bool{false, true} {
			t.Run(map[bool]string{false: "no-files", true: "files"}[withFiles], func(t *testing.T) {
				var e *imgEnv
				if withFiles {
					e = newImgEnv(t, imagePolicy)
				} else {
					e = twoPlainDeploys(t)
				}
				parent := storeRootTemp(t)
				blocked := filepath.Join(parent, "blocked")
				if err := os.Mkdir(blocked, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chown(blocked, os.Getuid(), os.Getgid()); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(blocked, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(blocked, 0700) })
				e.sys.fileStore = filepath.Join(blocked, "files")
				e.sys.fileOwner = &fileIDs{uid: os.Getuid(), gid: os.Getgid()}
				before := containerNames(e)
				body := mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"})
				verb := images.VerbRollback
				if withFiles {
					body = mustJSON(t, fileDeploy("v1"))
					verb = images.VerbDeploy
				}
				if imgCode(t, e.call(verb, body)) != programs.ReasonDocker {
					t.Fatalf("%s", verb)
				}
				if !sameNames(before, containerNames(e)) {
					t.Fatalf("containers changed %v", containerNames(e))
				}
				// Mode 000 hides the child from Lstat. Put the directory
				// back, then check the helper did not create files/.
				if err := os.Chmod(blocked, 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(blocked, "files")); !os.IsNotExist(err) {
					t.Fatal("store was created")
				}
			})
		}
	})
}

func TestRollbackRemovePreflight(t *testing.T) {
	corrupt := []struct {
		name string
		fn   func(t *testing.T, root string)
		keep func(t *testing.T, root string)
	}{
		{
			name: "symlink",
			fn: func(t *testing.T, root string) {
				t.Helper()
				slot := filepath.Join(root, "mohiguide-web")
				if err := os.RemoveAll(slot); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root, slot); err != nil {
					t.Fatal(err)
				}
			},
			keep: func(t *testing.T, root string) {
				t.Helper()
				fi, err := os.Lstat(filepath.Join(root, "mohiguide-web"))
				if err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Fatal("symlink removed")
				}
			},
		},
		{
			name: "mode",
			fn: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Chmod(filepath.Join(root, "mohiguide-web", "app.conf"), 0644); err != nil {
					t.Fatal(err)
				}
			},
			keep: func(t *testing.T, root string) {
				t.Helper()
				mode, _, _, _ := fileMode(t, filepath.Join(root, "mohiguide-web", "app.conf"))
				if mode != 0644 {
					t.Fatalf("mode %o", mode)
				}
			},
		},
		{
			name: "nlink",
			fn: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Link(filepath.Join(root, "mohiguide-web", "app.conf"), filepath.Join(root, "hard")); err != nil {
					t.Fatal(err)
				}
			},
			keep: func(t *testing.T, root string) {
				t.Helper()
				_, _, _, nlink := fileMode(t, filepath.Join(root, "hard"))
				if nlink != 2 {
					t.Fatalf("nlink %d", nlink)
				}
			},
		},
	}
	for _, verb := range []string{images.VerbRollback, images.VerbRemove} {
		for _, c := range corrupt {
			t.Run(verb+"/"+c.name, func(t *testing.T) {
				e, root := twoFileDeploys(t)
				c.fn(t, root)
				before := len(e.fake.Calls())
				names := containerNames(e)
				if imgCode(t, e.call(verb, mustJSON(t, images.EntryRequest{Project: "mohiguide", Entry: "web"}))) != programs.ReasonBadSpec {
					t.Fatalf("%s", verb)
				}
				if mutatedSince(e, before) {
					t.Fatal("engine mutation")
				}
				if !sameNames(names, containerNames(e)) {
					t.Fatal("containers changed")
				}
				c.keep(t, root)
			})
		}
	}
}

func twoFileDeploys(t *testing.T) (*imgEnv, string) {
	t.Helper()
	e := newImgEnv(t, imagePolicy)
	root := e.useStore(t)
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v1")))) != "" {
		t.Fatal("v1")
	}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, fileDeploy("v2")))) != "" {
		t.Fatal("v2")
	}
	return e, root
}

func twoPlainDeploys(t *testing.T) *imgEnv {
	t.Helper()
	e := newImgEnv(t, imagePolicy)
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashC)))) != "" {
		t.Fatal("first")
	}
	if imgCode(t, e.call(images.VerbDeploy, mustJSON(t, serviceDeploy(hashD)))) != "" {
		t.Fatal("second")
	}
	return e
}

func mutated(e *imgEnv) bool {
	return mutatedSince(e, 0)
}

func mutatedSince(e *imgEnv, start int) bool {
	for _, c := range e.fake.Calls()[start:] {
		if c.Method != "GET" {
			return true
		}
	}
	return false
}

func sameNames(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for name := range a {
		if !b[name] {
			return false
		}
	}
	return true
}

func created(e *imgEnv) bool {
	for _, c := range e.fake.Calls() {
		if c.Method == "POST" && strings.Contains(c.Path, "/containers/create") {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func containerNames(e *imgEnv) map[string]bool {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	out := map[string]bool{}
	for name := range e.fake.ctrs {
		out[name] = true
	}
	return out
}

func rule8Hash(files []images.MountFile) string {
	cp := append([]images.MountFile(nil), files...)
	if len(cp) == 2 && cp[0].Name > cp[1].Name {
		cp[0], cp[1] = cp[1], cp[0]
	}
	h := sha256.New()
	for _, f := range cp {
		sum := sha256.Sum256([]byte(f.Content))
		_, _ = h.Write([]byte(f.Name + ":" + hex.EncodeToString(sum[:]) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileReq(name, target, content string) images.DeployRequest {
	req := fileDeploy(content)
	req.Spec.Files = []images.SpecFile{{File: name, Target: target}}
	req.Files = []images.MountFile{{Name: name, Target: target, Content: content}}
	return req
}

func dupTarget() images.DeployRequest {
	req := fileDeploy("x")
	req.Spec.Files = []images.SpecFile{{File: "a", Target: "/etc/a"}, {File: "b", Target: "/etc/a"}}
	req.Files = []images.MountFile{
		{Name: "a", Target: "/etc/a", Content: "x"},
		{Name: "b", Target: "/etc/a", Content: "y"},
	}
	return req
}

func underVolume() images.DeployRequest {
	req := fileDeploy("x")
	req.Spec.Volumes = []images.Volume{{Name: "data", Target: "/var/data"}}
	req.Spec.Files = []images.SpecFile{{File: "a", Target: "/var/data/a"}}
	req.Files = []images.MountFile{{Name: "a", Target: "/var/data/a", Content: "x"}}
	return req
}

func volumeUnderFile() images.DeployRequest {
	req := fileDeploy("x")
	req.Spec.Volumes = []images.Volume{{Name: "data", Target: "/var/data"}}
	req.Spec.Files = []images.SpecFile{{File: "a", Target: "/var"}}
	req.Files = []images.MountFile{{Name: "a", Target: "/var", Content: "x"}}
	return req
}

func mismatch() images.DeployRequest {
	req := fileDeploy("x")
	req.Spec.Files = []images.SpecFile{{File: "a.conf", Target: "/etc/a.conf"}}
	req.Files = []images.MountFile{{Name: "b.conf", Target: "/etc/a.conf", Content: "x"}}
	return req
}

func TestSecretFileLogsRedacted(t *testing.T) {
	line, header, doc := throwawayAccount(t)
	plain := "plain-config-line-not-secret"
	e := newImgEnv(t, imagePolicy)
	e.fake.add("mohiguide-web", "running", "ghcr.io/joe-and-his-friends/app@"+digestA, e.labels("web"))
	e.fake.logBody = append(frame(1, line+"\n"), frame(1, header+"\n")...)
	e.fake.logBody = append(e.fake.logBody, frame(1, plain+"\n")...)
	values := secretLines(doc)
	if !images.ValidRedact(values) {
		t.Fatalf("redact list %d", len(values))
	}
	raw := e.call(images.VerbLogs, mustJSON(t, images.LogsRequest{
		Project: "mohiguide", Entry: "web", Lines: 20, Redact: values,
	}))
	if strings.Contains(string(raw), line) || strings.Contains(string(raw), "BEGIN RSA") {
		t.Fatalf("secret kept %s", raw)
	}
	if !strings.Contains(string(raw), plain) {
		t.Fatalf("plain redacted %s", raw)
	}
}

// throwawayAccount builds a service-account JSON around a key generated in
// this test. Nothing here is a real credential. line is one PEM body line;
// header is the BEGIN line, so a log that prints either one is covered.
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

func secretLines(doc string) []string {
	var out []string
	for _, v := range redact.FileSecrets(doc) {
		if len(v) >= images.MinRedactBytes && len(v) <= images.MaxRedactBytes && strings.IndexByte(v, 0) < 0 {
			out = append(out, v)
		}
	}
	return out
}
