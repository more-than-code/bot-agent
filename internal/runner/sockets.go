package runner

import (
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/more-than-code/bot-agent/internal/agent/collect"
)

// listenRow is a listener from /proc/net/tcp{,6}. The discover package keeps
// its parser result private; this copy stays in the helper so the agent
// binary does not import the helper.
type listenRow struct {
	port  int
	inode uint64
	uid   string
}

func mapSockets(procRoot, sysfs, passwd, unit string) (ports []int, user *string, err error) {
	cgroup := filepath.Join(sysfs, "fs", "cgroup", "system.slice", unit, "cgroup.procs")
	body, err := os.ReadFile(cgroup)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []int{}, nil, nil
		}
		return nil, nil, err
	}
	inodes := map[uint64]bool{}
	for _, pid := range parsePIDs(string(body)) {
		fdDir := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
		ents, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
			if err != nil {
				continue
			}
			ino, ok := socketIno(target)
			if ok {
				inodes[ino] = true
			}
		}
	}
	var hits []listenRow
	for _, row := range readListeners(procRoot) {
		if inodes[row.inode] {
			hits = append(hits, row)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].port < hits[j].port })
	seen := map[int]bool{}
	ports = []int{}
	var uid string
	for _, h := range hits {
		if seen[h.port] {
			continue
		}
		seen[h.port] = true
		if len(ports) == 0 {
			uid = h.uid
		}
		ports = append(ports, h.port)
	}
	if len(ports) == 0 {
		return []int{}, nil, nil
	}
	pw, err := os.ReadFile(passwd)
	if err != nil {
		pw = nil
	}
	name := collect.LookupUser(string(pw), uid)
	if name == "" {
		return ports, nil, nil
	}
	return ports, &name, nil
}

func parsePIDs(body string) []int {
	var out []int
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 {
			continue
		}
		out = append(out, n)
	}
	return out
}

func socketIno(target string) (uint64, bool) {
	if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
		return 0, false
	}
	n, err := strconv.ParseUint(target[len("socket:["):len(target)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func readListeners(procRoot string) []listenRow {
	var out []listenRow
	for _, name := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(filepath.Join(procRoot, "net", name))
		if err != nil {
			continue
		}
		out = append(out, parseProcNet(string(b))...)
	}
	return out
}

func parseProcNet(data string) []listenRow {
	var out []listenRow
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
		if err != nil || !wantedAddr(ip) {
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
		uid, err := strconv.ParseUint(f[7], 10, 32)
		if err != nil {
			continue
		}
		out = append(out, listenRow{port: int(port), inode: inode, uid: strconv.FormatUint(uid, 10)})
	}
	return out
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

func wantedAddr(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		return v4.Equal(net.IPv4zero) || v4.Equal(net.IPv4(127, 0, 0, 1))
	}
	return ip.Equal(net.IPv6unspecified)
}
