package collect

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// Fixtures under testdata/proc and testdata/sys are Linux shapes written
// from memory on macOS. testdata/mountinfo-pi1 is the pi1 mount table from
// the field report.

func TestGoldenHostAndProc(t *testing.T) {
	dst := t.TempDir()
	copyTree(t, "testdata", dst)
	proc := filepath.Join(dst, "proc")
	c := New(Roots{
		Proc:   proc,
		Sys:    filepath.Join(dst, "sys"),
		Passwd: filepath.Join(dst, "passwd"),
	}, func(path string) (FSStat, error) {
		switch path {
		case "/":
			return FSStat{Blocks: 10000, Bfree: 2500, Bavail: 2500, Bsize: 1}, nil
		case "/home":
			return FSStat{Blocks: 20000, Bfree: 20000, Bavail: 20000, Bsize: 1}, nil
		case "/home/user/My Files":
			return FSStat{Blocks: 5000, Bfree: 1000, Bavail: 1000, Bsize: 1}, nil
		default:
			t.Errorf("unexpected statfs %s", path)
			return FSStat{}, os.ErrNotExist
		}
	})
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	samples, view, err := c.SampleHost(t0)
	if err != nil {
		t.Fatal(err)
	}
	if sampleBy(samples, "cpu", "", "") != nil {
		t.Fatal("first host sample included cpu")
	}
	mem := mustSample(t, samples, "mem", "", "")
	if num(mem.F["total"]) != 1048576000 || num(mem.F["available"]) != 409600000 || num(mem.F["used"]) != 638976000 {
		t.Fatalf("mem %+v", mem.F)
	}
	near(t, mem.F["used_percent"].(float64), 60.9375)
	diskRoot := mustSample(t, samples, "disk", "path", "/")
	if diskRoot.Tags["fstype"] != "ext4" || num(diskRoot.F["total"]) != 10000 || num(diskRoot.F["free"]) != 2500 || num(diskRoot.F["used"]) != 7500 {
		t.Fatalf("disk / %+v %+v", diskRoot.Tags, diskRoot.F)
	}
	near(t, diskRoot.F["used_percent"].(float64), 75)
	home := mustSample(t, samples, "disk", "path", "/home")
	if num(home.F["used"]) != 0 || num(home.F["free"]) != 20000 {
		t.Fatalf("disk /home %+v", home.F)
	}
	files := mustSample(t, samples, "disk", "path", "/home/user/My Files")
	if num(files.F["total"]) != 5000 || num(files.F["used"]) != 4000 {
		t.Fatalf("unescaped mount %+v", files.F)
	}
	for _, s := range samples {
		if s.M == "disk" && (s.Tags["path"] == "/run" || s.Tags["path"] == "/proc" || s.Tags["path"] == "/sys" || s.Tags["path"] == "/dev") {
			t.Fatalf("pseudo mount kept: %s", s.Tags["path"])
		}
	}
	sda := mustSample(t, samples, "diskio", "name", "sda")
	if num(sda.F["read_bytes"]) != 5120 || num(sda.F["write_bytes"]) != 10240 {
		t.Fatalf("sda %+v", sda.F)
	}
	mmc := mustSample(t, samples, "diskio", "name", "mmcblk0")
	if num(mmc.F["read_bytes"]) != 2048 || num(mmc.F["write_bytes"]) != 4096 {
		t.Fatalf("mmc %+v", mmc.F)
	}
	nvme := mustSample(t, samples, "diskio", "name", "nvme0n1")
	if num(nvme.F["read_bytes"]) != 1024 || num(nvme.F["write_bytes"]) != 1536 {
		t.Fatalf("nvme %+v", nvme.F)
	}
	for _, name := range []string{"sda1", "mmcblk0p1", "nvme0n1p1", "dm-0", "loop0"} {
		if sampleBy(samples, "diskio", "name", name) != nil {
			t.Fatalf("kept %s", name)
		}
	}
	eth := mustSample(t, samples, "net", "interface", "eth0")
	if num(eth.F["bytes_recv"]) != 1000 || num(eth.F["bytes_sent"]) != 2000 {
		t.Fatalf("eth %+v", eth.F)
	}
	if sampleBy(samples, "net", "interface", "lo") != nil {
		t.Fatal("kept lo")
	}
	sys := mustSample(t, samples, "system", "", "")
	near(t, sys.F["load1"].(float64), 1)
	near(t, sys.F["load5"].(float64), 0.5)
	near(t, sys.F["load15"].(float64), 0.25)
	if num(sys.F["uptime"]) != 3600 || num(sys.F["n_cpus"]) != 2 {
		t.Fatalf("system %+v", sys.F)
	}
	cpuTemp := mustSample(t, samples, "temp", "sensor", "cpu_thermal")
	near(t, cpuTemp.F["celsius"].(float64), 45.123)
	soc := mustSample(t, samples, "temp", "sensor", "soc")
	near(t, soc.F["celsius"].(float64), 30)
	if view.TempC == nil || math.Abs(*view.TempC-45.123) > 1e-9 {
		t.Fatalf("snapshot temp %v", view.TempC)
	}
	if view.NCPU != 2 || !view.HaveMem || view.CPUPercent != nil || len(view.Disks) != 3 {
		t.Fatalf("view %+v", view)
	}
	mustValidate(t, samples)

	var sawCgroup string
	procs, all, err := c.SampleProcs(t0, func(pid int, cgroup string) string {
		if pid == 42 {
			sawCgroup = cgroup
			return "nginx"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sawCgroup, "nginx.service") {
		t.Fatalf("cgroup %q", sawCgroup)
	}
	nginx := mustSample(t, procs, "process", "pid", "42")
	if _, ok := nginx.F["cpu_usage"]; ok {
		t.Fatal("process cpu on first sample")
	}
	if nginx.Tags["process_name"] != "nginx" || nginx.Tags["user"] != "www-data" || nginx.Tags["service"] != "nginx" {
		t.Fatalf("tags %+v", nginx.Tags)
	}
	if nginx.Tags["cmdline"] != "nginx -g daemon off; password=[REDACTED]" {
		t.Fatalf("cmdline %q", nginx.Tags["cmdline"])
	}
	if strings.Contains(nginx.Tags["cmdline"], "hunter2") {
		t.Fatal("secret survived cmdline")
	}
	if num(nginx.F["memory_rss"]) != 2097152 || num(nginx.F["num_threads"]) != 4 {
		t.Fatalf("proc fields %+v", nginx.F)
	}
	near(t, nginx.F["memory_usage"].(float64), 0.2)
	if nginx.F["started_at"] != protocol.FormatTime(t0.Add(-3599*time.Second)) {
		t.Fatalf("started %v", nginx.F["started_at"])
	}
	kthread := mustSample(t, procs, "process", "pid", "2")
	if _, ok := kthread.Tags["service"]; ok {
		t.Fatal("kernel thread tagged with a service")
	}
	if kthread.Tags["process_name"] != "kthreadd" {
		t.Fatalf("kthread %+v", kthread.Tags)
	}
	mustValidate(t, procs)
	if len(all) != 2 {
		t.Fatalf("procs %d", len(all))
	}

	stat2, err := os.ReadFile(filepath.Join(dst, "stat-2"))
	if err != nil {
		t.Fatal(err)
	}
	pid2, err := os.ReadFile(filepath.Join(dst, "pid-stat-2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "stat"), stat2, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "42", "stat"), pid2, 0o644); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Second)
	samples2, view2, err := c.SampleHost(t1)
	if err != nil {
		t.Fatal(err)
	}
	cpu := mustSample(t, samples2, "cpu", "cpu", "cpu-total")
	near(t, cpu.F["usage_user"].(float64), 10)
	near(t, cpu.F["usage_system"].(float64), 5)
	near(t, cpu.F["usage_idle"].(float64), 80)
	near(t, cpu.F["usage_iowait"].(float64), 5)
	if _, ok := cpu.F["usage_nice"]; ok {
		t.Fatal("unexpected cpu field")
	}
	if view2.CPUPercent == nil || math.Abs(*view2.CPUPercent-20) > 1e-9 {
		t.Fatalf("cpu percent %v", view2.CPUPercent)
	}
	mustValidate(t, samples2)

	procs2, _, err := c.SampleProcs(t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	nginx2 := mustSample(t, procs2, "process", "pid", "42")
	near(t, nginx2.F["cpu_usage"].(float64), 100)
	if nginx2.F["started_at"] != protocol.FormatTime(t1.Add(-3599*time.Second)) {
		t.Fatalf("started2 %v", nginx2.F["started_at"])
	}
	mustValidate(t, procs2)
}

func TestParseMountInfoPi1(t *testing.T) {
	body, err := os.ReadFile("testdata/mountinfo-pi1")
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := ParseMountInfo(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 || mounts[0].Path != "/" || mounts[0].FSType != "ext4" || mounts[1].Path != "/boot/firmware" || mounts[1].FSType != "vfat" {
		t.Fatalf("%+v", mounts)
	}
}

func TestParseMountInfoShortest(t *testing.T) {
	// The shorter mount of a device wins even when it is listed later.
	// An equal-length second mount stays dropped. Pseudo and non-root binds do too.
	const data = "" +
		"50 1 8:1 / /var/lib/extra rw - ext4 /dev/sda1 rw\n" +
		"51 1 8:1 / / rw - ext4 /dev/sda1 rw\n" +
		"52 1 8:1 / /var/lib/extra2 rw - ext4 /dev/sda1 rw\n" +
		"53 1 8:2 / /boot rw - vfat /dev/sda2 rw\n" +
		"54 1 8:2 / /boot-alt rw - vfat /dev/sda2 rw\n" +
		"60 1 8:3 / /data rw - xfs /dev/sda3 rw\n" +
		"61 1 8:3 / /disk rw - xfs /dev/sda3 rw\n" +
		"70 1 8:4 /tmp /tmp rw - ext4 /dev/sda4 rw\n" +
		"71 1 0:1 / /run rw - tmpfs tmpfs rw\n"
	mounts, err := ParseMountInfo(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []Mount{
		{Path: "/", FSType: "ext4"},
		{Path: "/boot", FSType: "vfat"},
		{Path: "/data", FSType: "xfs"},
	}
	if len(mounts) != len(want) {
		t.Fatalf("%+v", mounts)
	}
	for i := range want {
		if mounts[i] != want[i] {
			t.Fatalf("got %+v want %+v", mounts, want)
		}
	}
}

func TestDiskDFParity(t *testing.T) {
	dir := t.TempDir()
	proc := filepath.Join(dir, "proc")
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	const info = "23 1 179:2 / / rw,noatime shared:1 - ext4 /dev/mmcblk0p2 rw\n"
	if err := os.WriteFile(filepath.Join(proc, "self", "mountinfo"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Roots{Proc: proc}, func(path string) (FSStat, error) {
		if path != "/" {
			t.Errorf("unexpected statfs %s", path)
			return FSStat{}, os.ErrNotExist
		}
		return FSStat{Blocks: 1000, Bfree: 400, Bavail: 350, Bsize: 4096}, nil
	})
	var view HostView
	samples, err := c.readDisks(time.Unix(0, 0).UTC(), &view)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || len(view.Disks) != 1 {
		t.Fatalf("samples %d disks %d", len(samples), len(view.Disks))
	}
	const b = int64(4096)
	d := samples[0]
	if num(d.F["total"]) != 1000*b || num(d.F["used"]) != 600*b || num(d.F["free"]) != 350*b {
		t.Fatalf("%+v", d.F)
	}
	near(t, d.F["used_percent"].(float64), 600.0/950.0*100)
	if view.Disks[0].Path != "/" || view.Disks[0].Total != uint64(1000*b) || view.Disks[0].Used != uint64(600*b) {
		t.Fatalf("view %+v", view.Disks[0])
	}
	if view.Disks[0].UsedPercent != d.F["used_percent"].(float64) {
		t.Fatalf("snapshot disk percent %v sample %v", view.Disks[0].UsedPercent, d.F["used_percent"])
	}
	_, _, _, pct0 := diskUsage(FSStat{Blocks: 10, Bfree: 10, Bavail: 0, Bsize: 4096})
	if pct0 != 0 {
		t.Fatalf("zero denom %v", pct0)
	}
}

func TestParsePIDStatComm(t *testing.T) {
	// Field positions follow the kernel's /proc/<pid>/stat layout.
	line := "7 (nginx (master)) S 1 7 7 0 -1 0 0 0 0 0 15 4 0 0 20 0 2 0 50 1 9 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	st, err := ParsePIDStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if st.Comm != "nginx (master)" || st.UTime != 15 || st.STime != 4 || st.Threads != 2 || st.StartTicks != 50 || st.RSSPages != 9 {
		t.Fatalf("%+v", st)
	}
}

func TestMissingThermalIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	proc := filepath.Join(dir, "proc")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"stat":      "cpu  1 0 1 1 0 0 0 0 0 0\ncpu0 1 0 1 1 0 0 0 0 0 0\n",
		"meminfo":   "MemTotal: 1024 kB\nMemAvailable: 512 kB\n",
		"loadavg":   "0.00 0.00 0.00 1/1 1\n",
		"uptime":    "10.00 0.00\n",
		"diskstats": "",
	} {
		if err := os.WriteFile(filepath.Join(proc, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(proc, "self", "mountinfo"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "net", "dev"), []byte("  eth0: 1 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(Roots{Proc: proc, Sys: filepath.Join(dir, "sys"), Passwd: filepath.Join(dir, "passwd")}, func(string) (FSStat, error) {
		return FSStat{}, os.ErrNotExist
	})
	samples, view, err := c.SampleHost(time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if view.TempC != nil || sampleBy(samples, "temp", "", "") != nil {
		t.Fatal("expected no temperature")
	}
}

func mustValidate(t *testing.T, samples []protocol.Sample) {
	t.Helper()
	frame := protocol.Metrics{
		V: protocol.Version, Type: "metrics",
		BatchID: "11111111-1111-4111-8111-111111111111",
		Samples: samples,
	}
	raw, err := protocol.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateAgent(raw); err != nil {
		t.Fatal(err)
	}
}

func mustSample(t *testing.T, samples []protocol.Sample, m, tagKey, tagVal string) protocol.Sample {
	t.Helper()
	s := sampleBy(samples, m, tagKey, tagVal)
	if s == nil {
		t.Fatalf("missing %s %s=%s", m, tagKey, tagVal)
	}
	return *s
}

func sampleBy(samples []protocol.Sample, m, tagKey, tagVal string) *protocol.Sample {
	for i := range samples {
		s := &samples[i]
		if s.M != m {
			continue
		}
		if tagKey == "" || s.Tags[tagKey] == tagVal {
			return s
		}
	}
	return nil
}

func num(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case uint64:
		return int64(n)
	default:
		panic(n)
	}
}

func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v want %v", got, want)
	}
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
