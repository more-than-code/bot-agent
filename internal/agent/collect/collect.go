package collect

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// StatFS returns total and available bytes for a mount path.
type StatFS func(path string) (total, avail uint64, err error)

// Roots are the filesystem trees a collector reads. Tests point them at fixtures.
type Roots struct {
	Proc   string
	Sys    string
	Passwd string
}

// Collector holds the previous CPU sample so the next call can emit deltas.
type Collector struct {
	mu         sync.Mutex
	roots      Roots
	statfs     StatFS
	prevCPU    *cpuCounters
	prevProc   map[int]uint64
	prevProcAt time.Time
	memTotal   uint64
	uptime     float64
}

// New returns a collector. stat may be nil outside Linux; disk samples are then skipped.
func New(roots Roots, stat StatFS) *Collector {
	if stat == nil {
		stat = HostStatFS
	}
	if roots.Proc == "" {
		roots.Proc = "/proc"
	}
	if roots.Sys == "" {
		roots.Sys = "/sys"
	}
	if roots.Passwd == "" {
		roots.Passwd = "/etc/passwd"
	}
	return &Collector{roots: roots, statfs: stat, prevProc: map[int]uint64{}}
}

// HostView is the latest host sample, for host_snapshot.
type HostView struct {
	CPUPercent     *float64
	MemTotal       uint64
	MemUsed        uint64
	MemAvailable   uint64
	MemUsedPercent float64
	HaveMem        bool
	Disks          []DiskView
	Load           [3]float64
	HaveLoad       bool
	TempC          *float64
	Uptime         int64
	NCPU           int
}

// DiskView is one filesystem in a snapshot.
type DiskView struct {
	Path        string
	Total       uint64
	Used        uint64
	UsedPercent float64
}

// ProcView is one process. CPU is nil until a previous sample exists.
type ProcView struct {
	PID        int
	Name       string
	User       string
	Cmdline    string
	CPU        *float64
	RSS        int64
	MemPercent float64
	Threads    int
	Started    time.Time
	Service    string
}

// ServiceFor names the discovered service for a process, or "".
type ServiceFor func(pid int, cgroup string) string

// SampleHost reads host metrics. The cpu measurement is omitted until a
// previous sample exists. now is the timestamp on every sample.
func (c *Collector) SampleHost(now time.Time) ([]protocol.Sample, HostView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []protocol.Sample
	var view HostView
	var soft []string

	if cpu, ncpu, err := c.readCPU(now); err != nil {
		soft = append(soft, err.Error())
	} else {
		view.NCPU = ncpu
		if cpu != nil {
			out = append(out, *cpu)
			idle := cpu.F["usage_idle"].(float64)
			busy := 100 - idle
			view.CPUPercent = &busy
		}
	}
	if s, err := c.readMem(now, &view); err != nil {
		soft = append(soft, err.Error())
	} else if s != nil {
		out = append(out, *s)
	}
	if ss, err := c.readDisks(now, &view); err != nil {
		soft = append(soft, err.Error())
	} else {
		out = append(out, ss...)
	}
	if ss, err := c.readDiskIO(now); err != nil {
		soft = append(soft, err.Error())
	} else {
		out = append(out, ss...)
	}
	if ss, err := c.readNet(now); err != nil {
		soft = append(soft, err.Error())
	} else {
		out = append(out, ss...)
	}
	if s, err := c.readSystem(now, &view); err != nil {
		soft = append(soft, err.Error())
	} else if s != nil {
		out = append(out, *s)
	}
	if ss, temp, err := c.readTemp(now); err != nil {
		soft = append(soft, err.Error())
	} else {
		out = append(out, ss...)
		view.TempC = temp
	}
	return out, view, joinSoft(soft)
}

// SampleProcs reads processes. cpu_usage is omitted on the first call and
// for pids that were not in the previous sample. serviceOf may be nil.
func (c *Collector) SampleProcs(now time.Time, serviceOf ServiceFor) ([]protocol.Sample, []ProcView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	procs, err := c.readProcs(now, serviceOf)
	if err != nil {
		return nil, nil, err
	}
	chosen := Select(procs)
	samples := make([]protocol.Sample, 0, len(chosen))
	for _, p := range chosen {
		samples = append(samples, procSample(p, now))
	}
	return samples, procs, nil
}

func (c *Collector) readCPU(now time.Time) (*protocol.Sample, int, error) {
	body, err := os.ReadFile(filepath.Join(c.roots.Proc, "stat"))
	if err != nil {
		return nil, 0, err
	}
	cur, ncpu, err := ParseStat(string(body))
	if err != nil {
		return nil, 0, err
	}
	prev := c.prevCPU
	c.prevCPU = &cur
	if prev == nil {
		return nil, ncpu, nil
	}
	d := cur.sub(*prev)
	tot := d.total()
	if tot == 0 {
		return nil, ncpu, nil
	}
	s := protocol.NewSample("cpu", now, map[string]string{"cpu": "cpu-total"}, map[string]any{
		"usage_user":   percent(d.User, tot),
		"usage_system": percent(d.System, tot),
		"usage_idle":   percent(d.Idle, tot),
		"usage_iowait": percent(d.IOWait, tot),
	})
	return &s, ncpu, nil
}

func (c *Collector) readMem(now time.Time, view *HostView) (*protocol.Sample, error) {
	body, err := os.ReadFile(filepath.Join(c.roots.Proc, "meminfo"))
	if err != nil {
		return nil, err
	}
	total, avail, err := ParseMeminfo(string(body))
	if err != nil {
		return nil, err
	}
	c.memTotal = total
	used := uint64(0)
	if total > avail {
		used = total - avail
	}
	pct := percent(used, total)
	view.HaveMem = true
	view.MemTotal = total
	view.MemAvailable = avail
	view.MemUsed = used
	view.MemUsedPercent = pct
	s := protocol.NewSample("mem", now, nil, map[string]any{
		"total": total, "available": avail, "used": used, "used_percent": pct,
	})
	return &s, nil
}

func (c *Collector) readDisks(now time.Time, view *HostView) ([]protocol.Sample, error) {
	body, err := os.ReadFile(filepath.Join(c.roots.Proc, "mounts"))
	if err != nil {
		return nil, err
	}
	mounts, err := ParseMounts(string(body))
	if err != nil {
		return nil, err
	}
	var out []protocol.Sample
	var soft []string
	for _, m := range mounts {
		total, avail, err := c.statfs(m.Path)
		if err != nil {
			soft = append(soft, m.Path)
			continue
		}
		used := uint64(0)
		if total > avail {
			used = total - avail
		}
		pct := percent(used, total)
		view.Disks = append(view.Disks, DiskView{Path: m.Path, Total: total, Used: used, UsedPercent: pct})
		out = append(out, protocol.NewSample("disk", now, map[string]string{
			"path": m.Path, "fstype": m.FSType,
		}, map[string]any{
			"total": total, "used": used, "free": avail, "used_percent": pct,
		}))
	}
	if len(out) == 0 && len(soft) > 0 {
		return nil, errors.New("disk: statfs failed")
	}
	return out, nil
}

func (c *Collector) readDiskIO(now time.Time) ([]protocol.Sample, error) {
	body, err := os.ReadFile(filepath.Join(c.roots.Proc, "diskstats"))
	if err != nil {
		return nil, err
	}
	rows, err := ParseDiskstats(string(body))
	if err != nil {
		return nil, err
	}
	out := make([]protocol.Sample, 0, len(rows))
	for _, r := range rows {
		out = append(out, protocol.NewSample("diskio", now, map[string]string{"name": r.Name}, map[string]any{
			"read_bytes": r.ReadBytes, "write_bytes": r.WriteBytes,
		}))
	}
	return out, nil
}

func (c *Collector) readNet(now time.Time) ([]protocol.Sample, error) {
	body, err := os.ReadFile(filepath.Join(c.roots.Proc, "net", "dev"))
	if err != nil {
		return nil, err
	}
	rows, err := ParseNetDev(string(body))
	if err != nil {
		return nil, err
	}
	out := make([]protocol.Sample, 0, len(rows))
	for _, r := range rows {
		out = append(out, protocol.NewSample("net", now, map[string]string{"interface": r.Name}, map[string]any{
			"bytes_recv": r.BytesRecv, "bytes_sent": r.BytesSent,
		}))
	}
	return out, nil
}

func (c *Collector) readSystem(now time.Time, view *HostView) (*protocol.Sample, error) {
	loadBody, err := os.ReadFile(filepath.Join(c.roots.Proc, "loadavg"))
	if err != nil {
		return nil, err
	}
	l1, l5, l15, err := ParseLoadavg(string(loadBody))
	if err != nil {
		return nil, err
	}
	upBody, err := os.ReadFile(filepath.Join(c.roots.Proc, "uptime"))
	if err != nil {
		return nil, err
	}
	secs, exact, err := ParseUptime(string(upBody))
	if err != nil {
		return nil, err
	}
	c.uptime = exact
	ncpu := view.NCPU
	if ncpu == 0 {
		if body, err := os.ReadFile(filepath.Join(c.roots.Proc, "stat")); err == nil {
			if _, n, err := ParseStat(string(body)); err == nil {
				ncpu = n
			}
		}
	}
	view.HaveLoad = true
	view.Load = [3]float64{l1, l5, l15}
	view.Uptime = secs
	view.NCPU = ncpu
	s := protocol.NewSample("system", now, nil, map[string]any{
		"load1": l1, "load5": l5, "load15": l15, "uptime": secs, "n_cpus": ncpu,
	})
	return &s, nil
}

func (c *Collector) readTemp(now time.Time) ([]protocol.Sample, *float64, error) {
	dir := filepath.Join(c.roots.Sys, "class", "thermal")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var out []protocol.Sample
	var preferred *float64
	var first *float64
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "thermal_zone") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		typeB, err := os.ReadFile(filepath.Join(dir, name, "type"))
		if err != nil {
			continue
		}
		tempB, err := os.ReadFile(filepath.Join(dir, name, "temp"))
		if err != nil {
			continue
		}
		milli, err := strconv.ParseFloat(strings.TrimSpace(string(tempB)), 64)
		if err != nil {
			continue
		}
		celsius := milli / 1000
		sensor := strings.TrimSpace(string(typeB))
		if sensor == "" {
			sensor = name
		}
		out = append(out, protocol.NewSample("temp", now, map[string]string{"sensor": sensor}, map[string]any{
			"celsius": celsius,
		}))
		if first == nil {
			v := celsius
			first = &v
		}
		if sensor == "cpu_thermal" && preferred == nil {
			v := celsius
			preferred = &v
		}
	}
	if preferred != nil {
		return out, preferred, nil
	}
	return out, first, nil
}

func (c *Collector) readProcs(now time.Time, serviceOf ServiceFor) ([]ProcView, error) {
	entries, err := os.ReadDir(c.roots.Proc)
	if err != nil {
		return nil, err
	}
	passwd, _ := os.ReadFile(c.roots.Passwd)
	next := map[int]uint64{}
	var procs []ProcView
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		p, ticks, ok := c.readOneProc(pid, string(passwd), now, serviceOf)
		if !ok {
			continue
		}
		next[pid] = ticks
		if prev, ok := c.prevProc[pid]; ok && !c.prevProcAt.IsZero() {
			dt := now.Sub(c.prevProcAt).Seconds()
			if dt > 0 {
				delta := float64(0)
				if ticks >= prev {
					delta = float64(ticks - prev)
				}
				cpu := 100 * delta / (dt * float64(userHZ))
				p.CPU = &cpu
			}
		}
		procs = append(procs, p)
	}
	c.prevProc = next
	c.prevProcAt = now
	return procs, nil
}

func (c *Collector) readOneProc(pid int, passwd string, now time.Time, serviceOf ServiceFor) (ProcView, uint64, bool) {
	base := filepath.Join(c.roots.Proc, strconv.Itoa(pid))
	statB, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return ProcView{}, 0, false
	}
	st, err := ParsePIDStat(string(statB))
	if err != nil {
		return ProcView{}, 0, false
	}
	status := procStatus{}
	if b, err := os.ReadFile(filepath.Join(base, "status")); err == nil {
		status = ParseStatus(string(b))
	}
	name := status.Name
	if name == "" {
		name = st.Comm
	}
	rss := st.RSSPages * pageSize
	if status.HasRSS {
		rss = status.VMRSS
	}
	threads := st.Threads
	if status.HasThr {
		threads = status.Threads
	}
	cmd := ""
	if b, err := os.ReadFile(filepath.Join(base, "cmdline")); err == nil {
		cmd = ParseCmdline(b)
	}
	cgroup := ""
	if b, err := os.ReadFile(filepath.Join(base, "cgroup")); err == nil {
		cgroup = string(b)
	}
	svc := ""
	if serviceOf != nil {
		svc = serviceOf(pid, cgroup)
	}
	memPct := 0.0
	if c.memTotal > 0 && rss > 0 {
		memPct = 100 * float64(rss) / float64(c.memTotal)
	}
	var started time.Time
	if c.uptime > 0 {
		started = startedAt(now, c.uptime, st.StartTicks)
	}
	return ProcView{
		PID: pid, Name: name, User: LookupUser(passwd, status.UID),
		Cmdline: cmd, RSS: rss, MemPercent: memPct, Threads: threads,
		Started: started, Service: svc,
	}, st.UTime + st.STime, true
}

func procSample(p ProcView, now time.Time) protocol.Sample {
	tags := map[string]string{
		"pid":          strconv.Itoa(p.PID),
		"process_name": p.Name,
		"user":         p.User,
		"cmdline":      p.Cmdline,
	}
	if p.Service != "" {
		tags["service"] = p.Service
	}
	fields := map[string]any{
		"memory_rss":   p.RSS,
		"memory_usage": p.MemPercent,
		"num_threads":  p.Threads,
		"started_at":   protocol.FormatTime(p.Started),
	}
	if p.CPU != nil {
		fields["cpu_usage"] = *p.CPU
	}
	return protocol.NewSample("process", now, tags, fields)
}

// Select keeps the top 20 by cpu (when any cpu is known), the top 20 by
// RSS, and every process that belongs to a discovered service.
func Select(all []ProcView) []ProcView {
	if len(all) == 0 {
		return nil
	}
	keep := map[int]bool{}
	anyCPU := false
	for _, p := range all {
		if p.CPU != nil {
			anyCPU = true
		}
		if p.Service != "" {
			keep[p.PID] = true
		}
	}
	byCPU := append([]ProcView(nil), all...)
	sort.SliceStable(byCPU, func(i, j int) bool {
		return lessCPU(byCPU[i], byCPU[j])
	})
	if anyCPU {
		n := 0
		for _, p := range byCPU {
			if p.CPU == nil {
				continue
			}
			if n >= 20 {
				break
			}
			keep[p.PID] = true
			n++
		}
	}
	byRSS := append([]ProcView(nil), all...)
	sort.SliceStable(byRSS, func(i, j int) bool {
		if byRSS[i].RSS != byRSS[j].RSS {
			return byRSS[i].RSS > byRSS[j].RSS
		}
		return byRSS[i].PID < byRSS[j].PID
	})
	for i, p := range byRSS {
		if i >= 20 {
			break
		}
		keep[p.PID] = true
	}
	var out []ProcView
	for _, p := range byCPU {
		if keep[p.PID] {
			out = append(out, p)
		}
	}
	return out
}

func lessCPU(a, b ProcView) bool {
	switch {
	case a.CPU == nil && b.CPU == nil:
		return a.PID < b.PID
	case a.CPU == nil:
		return false
	case b.CPU == nil:
		return true
	case *a.CPU != *b.CPU:
		return *a.CPU > *b.CPU
	default:
		return a.PID < b.PID
	}
}

func joinSoft(msgs []string) error {
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "; "))
}
