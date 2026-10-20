// Package discover maps listening TCP sockets to systemd units or processes.
package discover

import (
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/collect"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// Result is one discovery pass.
type Result struct {
	Services []protocol.Discovered
	Unmapped int
	// ContainerHealth is the helper health of a container service chosen in
	// this pass, keyed by the discovered name. It is not a wire field.
	// Values are healthy, unhealthy, or starting. A null health is absent.
	ContainerHealth map[string]string
	byPID           map[int]string
	byUnit          map[string]string
}

// ServiceFor returns the discovered service name for pid, or for any
// process whose cgroup names a discovered unit.
func (r Result) ServiceFor(pid int, cgroup string) string {
	if name, ok := r.byPID[pid]; ok {
		return name
	}
	unit, _, ok := ParseCgroup(cgroup)
	if !ok {
		return ""
	}
	return r.byUnit[unit]
}

// Scan reads procRoot/net/tcp, tcp6, and per-pid fd links.
// passwdPath names socket uids. An empty or unreadable file leaves the uid
// numeric. Unreadable fd directories are skipped. They never fail the scan.
func Scan(procRoot, passwdPath string) (Result, error) {
	listeners, err := readListeners(procRoot)
	if err != nil {
		return Result{}, err
	}
	owners := mapInodes(procRoot)
	return build(listeners, owners, procRoot, readPasswd(passwdPath)), nil
}

func readPasswd(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

type listener struct {
	port   int
	inode  uint64
	uid    string
	hasUID bool
	pid    int
	known  bool
}

func readListeners(procRoot string) ([]listener, error) {
	var out []listener
	for _, name := range []string{"tcp", "tcp6"} {
		body, err := os.ReadFile(filepath.Join(procRoot, "net", name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		rows, err := ParseNetTCP(string(body))
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// ParseNetTCP parses one /proc/net/tcp or tcp6 table.
// Only state 0A on 0.0.0.0, 127.0.0.1, or :: is returned.
// The uid column is kept so the caller can name the socket owner.
func ParseNetTCP(data string) ([]listener, error) {
	var out []listener
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || !strings.Contains(f[1], ":") {
			continue
		}
		if !strings.EqualFold(f[3], "0A") {
			continue
		}
		ipHex, portHex, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		ip, err := parseProcIP(ipHex)
		if err != nil || !wanted(ip) {
			continue
		}
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil || port == 0 {
			continue
		}
		inode, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			continue
		}
		uid, hasUID := socketUID(f)
		out = append(out, listener{port: int(port), inode: inode, uid: uid, hasUID: hasUID})
	}
	return out, nil
}

// socketUID reads the uid column. The kernel prints it between retrnsmt and
// timeout, which is field 7 once the line is split on whitespace.
func socketUID(fields []string) (string, bool) {
	if len(fields) < 10 {
		return "", false
	}
	n, err := strconv.ParseUint(fields[7], 10, 32)
	if err != nil {
		return "", false
	}
	return strconv.FormatUint(n, 10), true
}

func parseProcIP(s string) (net.IP, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	switch len(b) {
	case 4:
		return net.IPv4(b[3], b[2], b[1], b[0]), nil
	case 16:
		ip := make(net.IP, 16)
		for i := 0; i < 4; i++ {
			ip[i*4+0] = b[i*4+3]
			ip[i*4+1] = b[i*4+2]
			ip[i*4+2] = b[i*4+1]
			ip[i*4+3] = b[i*4+0]
		}
		return ip, nil
	default:
		return nil, errors.New("address")
	}
}

func wanted(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		return v4.Equal(net.IPv4zero) || v4.Equal(net.IPv4(127, 0, 0, 1))
	}
	return ip.Equal(net.IPv6unspecified)
}

func mapInodes(procRoot string) map[uint64]int {
	out := map[uint64]int{}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join(procRoot, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			inode, ok := socketInode(target)
			if !ok {
				continue
			}
			if prev, exists := out[inode]; !exists || pid < prev {
				out[inode] = pid
			}
		}
	}
	return out
}

func socketInode(target string) (uint64, bool) {
	if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
		return 0, false
	}
	n, err := strconv.ParseUint(target[len("socket:["):len(target)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

type group struct {
	kind     string
	unit     string
	scope    string
	name     string
	pids     []int
	comm     string
	ports    map[int]bool
	portUser map[int]string
}

func build(listeners []listener, owners map[uint64]int, procRoot, passwd string) Result {
	res := Result{byPID: map[int]string{}, byUnit: map[string]string{}}
	groups := map[string]*group{}
	var order []string
	for i := range listeners {
		ln := &listeners[i]
		if pid, ok := owners[ln.inode]; ok {
			ln.pid = pid
			ln.known = true
		}
	}
	for _, ln := range listeners {
		if !ln.known {
			res.Unmapped++
			key := "u:" + strconv.Itoa(ln.port)
			g := groups[key]
			if g == nil {
				name := "port-" + strconv.Itoa(ln.port)
				g = &group{kind: "process", name: name, ports: map[int]bool{}}
				groups[key] = g
				order = append(order, key)
			}
			g.addListener(ln, passwd)
			continue
		}
		comm := readComm(procRoot, ln.pid)
		cgroup := readFile(filepath.Join(procRoot, strconv.Itoa(ln.pid), "cgroup"))
		unit, scope, hasUnit := ParseCgroup(cgroup)
		var key string
		if hasUnit {
			base := strings.TrimSuffix(unit, ".service")
			name, ok := Sanitize(base)
			if !ok {
				hasUnit = false
			} else {
				key = "s:" + unit
				g := groups[key]
				if g == nil {
					g = &group{kind: "systemd", unit: unit, scope: scope, name: name, ports: map[int]bool{}}
					groups[key] = g
					order = append(order, key)
				}
				g.pids = append(g.pids, ln.pid)
				g.addListener(ln, passwd)
				if g.comm == "" {
					g.comm = comm
				}
				continue
			}
		}
		name, ok := Sanitize(comm)
		if !ok {
			name = "port-" + strconv.Itoa(ln.port)
		}
		key = "p:" + name
		g := groups[key]
		if g == nil {
			g = &group{kind: "process", name: name, ports: map[int]bool{}}
			groups[key] = g
			order = append(order, key)
		}
		g.pids = append(g.pids, ln.pid)
		g.addListener(ln, passwd)
		if g.comm == "" {
			g.comm = comm
		}
		_ = key
	}
	used := map[string]bool{}
	for _, key := range order {
		g := groups[key]
		if len(g.pids) > 0 {
			sort.Ints(g.pids)
			if comm := readComm(procRoot, g.pids[0]); comm != "" {
				g.comm = comm
			}
		}
		name := g.name
		if used[name] {
			name = uniqueName(name, used)
		}
		used[name] = true
		g.name = name
		res.Services = append(res.Services, g.discovered())
		for _, pid := range g.pids {
			if _, ok := res.byPID[pid]; !ok {
				res.byPID[pid] = name
			}
		}
		if g.kind == "systemd" {
			res.byUnit[g.unit] = name
		}
	}
	sort.Slice(res.Services, func(i, j int) bool {
		return res.Services[i].Name < res.Services[j].Name
	})
	return res
}

func (g *group) discovered() protocol.Discovered {
	ports := make([]int, 0, len(g.ports))
	for p := range g.ports {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	d := protocol.Discovered{
		Name:  g.name,
		Kind:  g.kind,
		Ports: ports,
	}
	// Several sockets merge into one service. The lowest port owns the user.
	// A repeated port keeps the first uid that parsed.
	if len(ports) > 0 {
		if u, ok := g.portUser[ports[0]]; ok {
			d.User = protocol.Str(u)
		}
	}
	if g.kind == "systemd" {
		d.SystemdUnit = protocol.Str(g.unit)
		d.SystemdScope = protocol.Str(g.scope)
	}
	if len(g.pids) == 0 {
		return d.NormalizePorts()
	}
	sort.Ints(g.pids)
	d.PID = protocol.Int(g.pids[0])
	if g.comm != "" {
		d.ProcessName = protocol.Str(g.comm)
	}
	return d.NormalizePorts()
}

func (g *group) addListener(ln listener, passwd string) {
	g.ports[ln.port] = true
	if !ln.hasUID {
		return
	}
	if g.portUser == nil {
		g.portUser = map[int]string{}
	}
	if _, seen := g.portUser[ln.port]; seen {
		return
	}
	g.portUser[ln.port] = collect.LookupUser(passwd, ln.uid)
}

func uniqueName(base string, used map[string]bool) string {
	for i := 2; i < 1000; i++ {
		suffix := "-" + strconv.Itoa(i)
		name := base
		if len(name)+len(suffix) > 63 {
			name = name[:63-len(suffix)]
		}
		name += suffix
		if protocol.NamePattern.MatchString(name) && !used[name] {
			return name
		}
	}
	return "svc"
}

// Sanitize lowercases and replaces characters outside the contract name set.
func Sanitize(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	for len(out) > 0 && !isAlnum(out[0]) {
		out = out[1:]
	}
	if len(out) > 63 {
		out = out[:63]
	}
	if !protocol.NamePattern.MatchString(out) {
		return "", false
	}
	return out, true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

var (
	userAt   = regexp.MustCompile(`(?:^|/)user@\d+\.service(?:/|$)`)
	userUnit = regexp.MustCompile(`^user@\d+$`)
)

// ParseCgroup returns the service unit (including the .service suffix) and
// the scope. scope is "user" when the path contains user@<uid>.service.
// ok is false when no unit other than user@<uid>.service is present.
func ParseCgroup(text string) (unit, scope string, ok bool) {
	var v2, fallback string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && v2 == "" {
			v2 = parts[2]
		}
		if fallback == "" {
			fallback = parts[2]
		}
	}
	path := v2
	if path == "" {
		path = fallback
	}
	if path == "" {
		return "", "", false
	}
	scope = "system"
	if userAt.MatchString(path) {
		scope = "user"
	}
	segs := strings.Split(path, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		seg := segs[i]
		if !strings.HasSuffix(seg, ".service") {
			continue
		}
		base := strings.TrimSuffix(seg, ".service")
		if userUnit.MatchString(base) {
			continue
		}
		return seg, scope, true
	}
	return "", scope, false
}

func readComm(procRoot string, pid int) string {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
