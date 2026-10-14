package discover

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// testdata/proc is a /proc tree written from memory (kernel table layout,
// little-endian addresses). It was not copied from a live Linux system.

func TestScan(t *testing.T) {
	dst := t.TempDir()
	copyTree(t, filepath.Join("testdata", "proc"), dst)
	fd := filepath.Join(dst, "99", "fd")
	if err := os.Chmod(fd, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fd, 0o755) })
	_, readErr := os.ReadDir(fd)
	hidden := readErr != nil

	res, err := Scan(dst, "")
	if err != nil {
		t.Fatal(err)
	}
	nginx := mustSvc(t, res, "nginx")
	if nginx.Kind != "systemd" || str(nginx.SystemdUnit) != "nginx.service" || str(nginx.SystemdScope) != "system" {
		t.Fatalf("nginx %+v", nginx)
	}
	if str(nginx.ProcessName) != "nginx" || pint(nginx.PID) != 42 || nginx.ContainerName != nil {
		t.Fatalf("nginx identity %+v", nginx)
	}
	if !slices.Equal(nginx.Ports, []int{80, 8080}) {
		t.Fatalf("nginx ports %v", nginx.Ports)
	}
	myapp := mustSvc(t, res, "myapp")
	if myapp.Kind != "systemd" || str(myapp.SystemdScope) != "user" || str(myapp.SystemdUnit) != "myapp.service" || pint(myapp.PID) != 50 {
		t.Fatalf("myapp %+v", myapp)
	}
	if !slices.Equal(myapp.Ports, []int{3000}) {
		t.Fatalf("myapp ports %v", myapp.Ports)
	}
	py := mustSvc(t, res, "python3.11")
	if py.Kind != "process" || py.SystemdUnit != nil || py.SystemdScope != nil || pint(py.PID) != 60 {
		t.Fatalf("python %+v", py)
	}
	if !slices.Equal(py.Ports, []int{9090}) || str(py.ProcessName) != "python3.11" {
		t.Fatalf("python ports %+v", py)
	}
	for _, svc := range res.Services {
		for _, p := range svc.Ports {
			if p == 443 || p == 22 || p == 81 {
				t.Fatalf("excluded port %d on %s", p, svc.Name)
			}
		}
	}
	if hidden {
		if res.Unmapped != 1 {
			t.Fatalf("unmapped %d", res.Unmapped)
		}
		port := mustSvc(t, res, "port-9")
		if port.Kind != "process" || port.PID != nil || port.ProcessName != nil || port.SystemdUnit != nil {
			t.Fatalf("port-9 %+v", port)
		}
		if !slices.Equal(port.Ports, []int{9}) {
			t.Fatalf("port-9 ports %v", port.Ports)
		}
	} else {
		t.Log("fd directory still readable; unmapped assertion skipped")
	}
	if res.ServiceFor(43, "") != "nginx" {
		t.Fatal("pid 43")
	}
	if res.ServiceFor(9, "0::/system.slice/nginx.service") != "nginx" {
		t.Fatal("unit lookup")
	}
	if res.ServiceFor(9, "0::/user.slice/user-1000.slice/user@1000.service/app.slice/myapp.service") != "myapp" {
		t.Fatal("user unit lookup")
	}
	raw, err := protocol.Marshal(protocol.Services{V: protocol.Version, Type: "services", Services: res.Services})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateAgent(raw); err != nil {
		t.Fatal(err)
	}
}

func TestNameCollision(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("net/tcp", "  sl  local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n"+
		"   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 1 1 0 100 0 0 10 0\n"+
		"   1: 0100007F:0051 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 2 1 0 100 0 0 10 0\n")
	write("1/comm", "nginx\n")
	write("1/cgroup", "0::/system.slice/Nginx.service\n")
	write("2/comm", "nginx\n")
	write("2/cgroup", "0::/user.slice/user-1000.slice/user@1000.service\n")
	if err := os.Symlink("socket:[1]", filepath.Join(dir, "1", "fd", "3")); err != nil {
		os.MkdirAll(filepath.Join(dir, "1", "fd"), 0o755)
		if err := os.Symlink("socket:[1]", filepath.Join(dir, "1", "fd", "3")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "2", "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:[2]", filepath.Join(dir, "2", "fd", "3")); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if mustSvc(t, res, "nginx").Kind != "systemd" {
		t.Fatal("unit lost its name")
	}
	if mustSvc(t, res, "nginx-2").Kind != "process" {
		t.Fatal("collision suffix")
	}
}

func TestSocketOwner(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One unit owns three sockets. The rows are ordered so the lowest port
	// is neither the first nor the last, and its uid is neither the smallest
	// nor the largest. port 80 / uid 1000 must win.
	write("net/tcp", "  sl  local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n"+
		"   0: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1234 0 1 1 0 100 0 0 10 0\n"+
		"   1: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 2 1 0 100 0 0 10 0\n"+
		"   2: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 3 1 0 100 0 0 10 0\n"+
		"   3: 00000000:0009 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 4 1 0 100 0 0 10 0\n"+
		"   4: 0100007F:0007 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1234 0 5 1 0 100 0 0 10 0\n")
	write("10/comm", "web\n")
	write("10/cgroup", "0::/system.slice/web.service\n")
	for _, fd := range []struct{ name, target string }{
		{"3", "socket:[1]"},
		{"4", "socket:[2]"},
		{"5", "socket:[3]"},
	} {
		path := filepath.Join(dir, "10", "fd", fd.name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(fd.target, path); err != nil {
			t.Fatal(err)
		}
	}
	write("passwd", "root:x:0:0:root:/root:/bin/bash\njoe:x:1000:1000:Joe:/home/joe:/bin/bash\n")

	res, err := Scan(dir, filepath.Join(dir, "passwd"))
	if err != nil {
		t.Fatal(err)
	}
	web := mustSvc(t, res, "web")
	if web.Kind != "systemd" || str(web.User) != "joe" || !slices.Equal(web.Ports, []int{80, 3000, 8080}) {
		t.Fatalf("web %+v", web)
	}
	root := mustSvc(t, res, "port-9")
	if root.Kind != "process" || root.PID != nil || str(root.User) != "root" || !slices.Equal(root.Ports, []int{9}) {
		t.Fatalf("port-9 %+v", root)
	}
	unknown := mustSvc(t, res, "port-7")
	if unknown.Kind != "process" || unknown.PID != nil || str(unknown.User) != "1234" || !slices.Equal(unknown.Ports, []int{7}) {
		t.Fatalf("port-7 %+v", unknown)
	}
	raw, err := protocol.Marshal(protocol.Services{V: protocol.Version, Type: "services", Services: res.Services})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateAgent(raw); err != nil {
		t.Fatal(err)
	}
}

func TestSanitize(t *testing.T) {
	got, ok := Sanitize("Nginx Web")
	if !ok || got != "nginx-web" {
		t.Fatalf("%s %v", got, ok)
	}
	got, ok = Sanitize("---9")
	if !ok || got != "9" {
		t.Fatalf("%s %v", got, ok)
	}
	if _, ok := Sanitize("..."); ok {
		t.Fatal("expected reject")
	}
	long := make([]byte, 80)
	for i := range long {
		long[i] = 'a'
	}
	got, ok = Sanitize(string(long))
	if !ok || len(got) != 63 {
		t.Fatalf("%d %v", len(got), ok)
	}
}

func TestParseCgroup(t *testing.T) {
	unit, scope, ok := ParseCgroup("12:cpu:/system.slice/wrong.service\n0::/system.slice/nginx.service\n")
	if !ok || unit != "nginx.service" || scope != "system" {
		t.Fatalf("%s %s %v", unit, scope, ok)
	}
	unit, scope, ok = ParseCgroup("0::/user.slice/user-1000.slice/user@1000.service/app.slice/myapp.service\n")
	if !ok || unit != "myapp.service" || scope != "user" {
		t.Fatalf("%s %s %v", unit, scope, ok)
	}
	_, _, ok = ParseCgroup("0::/user.slice/user-1000.slice/user@1000.service\n")
	if ok {
		t.Fatal("user@ alone is not a service")
	}
}

func mustSvc(t *testing.T, res Result, name string) protocol.Discovered {
	t.Helper()
	for _, s := range res.Services {
		if s.Name == name {
			return s
		}
	}
	var names []string
	for _, s := range res.Services {
		names = append(names, s.Name)
	}
	t.Fatalf("missing %s in %v", name, names)
	return protocol.Discovered{}
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func pint(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
