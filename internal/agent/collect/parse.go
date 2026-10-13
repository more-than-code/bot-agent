package collect

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
	"github.com/more-than-code/bot-agent/internal/agent/redact"
)

// userHZ is the Linux ABI clock tick (USER_HZ). It is 100 on every
// architecture; it is not the hardware timer frequency.
const userHZ = 100

// pageSize is the fallback when /proc/<pid>/status has no VmRSS.
// Production prefers VmRSS, which is already in bytes.
const pageSize = 4096

type cpuCounters struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

func (c cpuCounters) total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

func (c cpuCounters) sub(p cpuCounters) cpuCounters {
	return cpuCounters{
		User: c.User - p.User, Nice: c.Nice - p.Nice, System: c.System - p.System,
		Idle: c.Idle - p.Idle, IOWait: c.IOWait - p.IOWait, IRQ: c.IRQ - p.IRQ,
		SoftIRQ: c.SoftIRQ - p.SoftIRQ, Steal: c.Steal - p.Steal,
	}
}

// ParseStat reads /proc/stat. ncpu is the number of per-cpu lines.
func ParseStat(data string) (agg cpuCounters, ncpu int, err error) {
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		if f[0] == "cpu" {
			agg, err = parseCPUFields(f[1:])
			if err != nil {
				return cpuCounters{}, 0, err
			}
			continue
		}
		if strings.HasPrefix(f[0], "cpu") {
			ncpu++
		}
	}
	if agg.total() == 0 && ncpu == 0 {
		return cpuCounters{}, 0, errors.New("stat: no cpu line")
	}
	return agg, ncpu, nil
}

func parseCPUFields(f []string) (cpuCounters, error) {
	nums := make([]uint64, 8)
	for i := 0; i < len(nums) && i < len(f); i++ {
		n, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return cpuCounters{}, fmt.Errorf("stat: %w", err)
		}
		nums[i] = n
	}
	return cpuCounters{
		User: nums[0], Nice: nums[1], System: nums[2], Idle: nums[3],
		IOWait: nums[4], IRQ: nums[5], SoftIRQ: nums[6], Steal: nums[7],
	}, nil
}

// ParseMeminfo returns total and available bytes from /proc/meminfo.
func ParseMeminfo(data string) (total, available uint64, err error) {
	vals := map[string]uint64{}
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		key := strings.TrimSuffix(f[0], ":")
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		if len(f) >= 3 && f[2] == "kB" {
			n *= 1024
		}
		vals[key] = n
	}
	total, okT := vals["MemTotal"]
	available, okA := vals["MemAvailable"]
	if !okT || !okA || total == 0 {
		return 0, 0, errors.New("meminfo: MemTotal and MemAvailable required")
	}
	return total, available, nil
}

// ParseLoadavg reads the three load averages.
func ParseLoadavg(data string) (l1, l5, l15 float64, err error) {
	f := strings.Fields(data)
	if len(f) < 3 {
		return 0, 0, 0, errors.New("loadavg")
	}
	if l1, err = strconv.ParseFloat(f[0], 64); err != nil {
		return 0, 0, 0, err
	}
	if l5, err = strconv.ParseFloat(f[1], 64); err != nil {
		return 0, 0, 0, err
	}
	l15, err = strconv.ParseFloat(f[2], 64)
	return l1, l5, l15, err
}

// ParseUptime returns uptime in seconds (truncated) and the exact value.
func ParseUptime(data string) (seconds int64, exact float64, err error) {
	f := strings.Fields(data)
	if len(f) < 1 {
		return 0, 0, errors.New("uptime")
	}
	exact, err = strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, 0, err
	}
	if exact < 0 {
		return 0, 0, errors.New("uptime")
	}
	return int64(exact), exact, nil
}

// NetIf is one non-loopback interface counter pair.
type NetIf struct {
	Name      string
	BytesRecv uint64
	BytesSent uint64
}

// ParseNetDev reads /proc/net/dev. The loopback interface is omitted.
func ParseNetDev(data string) ([]NetIf, error) {
	var out []NetIf
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			return nil, fmt.Errorf("net/dev %s", name)
		}
		recv, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return nil, err
		}
		sent, err := strconv.ParseUint(f[8], 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, NetIf{Name: name, BytesRecv: recv, BytesSent: sent})
	}
	return out, nil
}

// DiskIO is a whole-disk cumulative counter.
type DiskIO struct {
	Name       string
	ReadBytes  uint64
	WriteBytes uint64
}

// ParseDiskstats keeps whole disks only. Sectors are 512 bytes, per the kernel.
func ParseDiskstats(data string) ([]DiskIO, error) {
	var out []DiskIO
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		name := f[2]
		if !protocol.WholeDisk.MatchString(name) {
			continue
		}
		rsect, err := strconv.ParseUint(f[5], 10, 64)
		if err != nil {
			return nil, err
		}
		wsect, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, DiskIO{Name: name, ReadBytes: rsect * 512, WriteBytes: wsect * 512})
	}
	return out, nil
}

// Mount is a real filesystem mount.
type Mount struct {
	Path   string
	FSType string
}

// ParseMounts reads /proc/mounts and drops pseudo filesystems.
func ParseMounts(data string) ([]Mount, error) {
	var out []Mount
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		path := unescapeMount(f[1])
		fst := f[2]
		if pseudoFS[fst] || path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, Mount{Path: path, FSType: fst})
	}
	return out, nil
}

// pseudoFS are filesystems the contract excludes, plus other kernel mounts
// that are not a disk a person can fill.
var pseudoFS = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "overlay": true, "squashfs": true,
	"proc": true, "sysfs": true, "devpts": true, "cgroup": true, "cgroup2": true,
	"autofs": true, "bpf": true, "tracefs": true, "securityfs": true,
	"pstore": true, "debugfs": true, "fusectl": true, "configfs": true,
	"mqueue": true, "hugetlbfs": true, "ramfs": true, "nsfs": true,
	"binfmt_misc": true, "rpc_pipefs": true, "nfsd": true, "devfs": true,
	"iso9660": true,
}

func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			v := (s[i+1]-'0')*64 + (s[i+2]-'0')*8 + (s[i+3] - '0')
			b.WriteByte(v)
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

type procTimes struct {
	Comm       string
	UTime      uint64
	STime      uint64
	StartTicks uint64
	RSSPages   int64
	Threads    int
}

// ParsePIDStat reads /proc/<pid>/stat. comm may contain spaces and parentheses.
func ParsePIDStat(data string) (procTimes, error) {
	open := strings.IndexByte(data, '(')
	close := strings.LastIndexByte(data, ')')
	if open < 0 || close < open {
		return procTimes{}, errors.New("stat: comm")
	}
	rest := strings.Fields(data[close+1:])
	// rest[0] is state (field 3). utime is field 14 → index 11.
	if len(rest) < 22 {
		return procTimes{}, errors.New("stat: short")
	}
	ut, err := strconv.ParseUint(rest[11], 10, 64)
	if err != nil {
		return procTimes{}, err
	}
	st, err := strconv.ParseUint(rest[12], 10, 64)
	if err != nil {
		return procTimes{}, err
	}
	threads, err := strconv.Atoi(rest[17])
	if err != nil {
		return procTimes{}, err
	}
	start, err := strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		return procTimes{}, err
	}
	rss, err := strconv.ParseInt(rest[21], 10, 64)
	if err != nil {
		return procTimes{}, err
	}
	return procTimes{
		Comm: data[open+1 : close], UTime: ut, STime: st,
		StartTicks: start, RSSPages: rss, Threads: threads,
	}, nil
}

type procStatus struct {
	Name    string
	VMRSS   int64
	HasRSS  bool
	Threads int
	HasThr  bool
	UID     string
}

// ParseStatus reads the fields the agent needs from /proc/<pid>/status.
func ParseStatus(data string) procStatus {
	var st procStatus
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Name":
			st.Name = v
		case "VmRSS":
			f := strings.Fields(v)
			if len(f) > 0 {
				if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					st.VMRSS = n * 1024
					st.HasRSS = true
				}
			}
		case "Threads":
			f := strings.Fields(v)
			if len(f) == 0 {
				break
			}
			if n, err := strconv.Atoi(f[0]); err == nil {
				st.Threads = n
				st.HasThr = true
			}
		case "Uid":
			f := strings.Fields(v)
			if len(f) > 0 {
				st.UID = f[0]
			}
		}
	}
	return st
}

// ParseCmdline turns NUL-separated arguments into one redacted line.
func ParseCmdline(data []byte) string {
	data = bytes.TrimRight(data, "\x00")
	s := string(bytes.ReplaceAll(data, []byte{0}, []byte(" ")))
	return redact.Cmdline(s)
}

// LookupUser resolves a numeric uid. An unknown uid is returned as digits.
func LookupUser(passwd, uid string) string {
	if uid == "" {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(passwd))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) >= 3 && f[2] == uid && f[0] != "" {
			return f[0]
		}
	}
	return uid
}

func startedAt(now time.Time, uptime float64, ticks uint64) time.Time {
	boot := now.Add(-time.Duration(uptime * float64(time.Second)))
	delta := time.Duration(float64(ticks) / float64(userHZ) * float64(time.Second))
	return boot.Add(delta)
}

func percent(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}
