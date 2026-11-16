package runner

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// portUse is one port the edge needs, with its protocol.
type portUse struct {
	proto string
	port  int
}

func (p portUse) String() string {
	if p.proto == "udp" {
		return fmt.Sprintf("udp port %d", p.port)
	}
	return fmt.Sprintf("port %d", p.port)
}

// edgeUses are the sockets the edge binds itself in host network mode:
// TCP 80 and 443, and UDP 443 (HTTP/3).
var edgeUses = []portUse{{"tcp", 80}, {"tcp", 443}, {"udp", 443}}

// errSockets is a socket table the helper could not read. Unknown is not
// free: the caller refuses rather than start an edge over a holder.
var errSockets = errors.New("sockets")

// edgePortHolder reports the first edge port that is held by anything but
// the edge container (agent-protocol v1.15 item 4, port_in_use), and the
// holder's name when known.
//
//  1. A running container other than the edge that publishes the TCP port:
//     its name.
//  2. A host socket on the port, on any address (TCP listening, UDP bound):
//     it is mapped to a process through /proc/<pid>/fd. A process in the edge
//     container's cgroup is the edge. Any other process is the holder, named
//     by comm. A socket no process can be found for is the edge's when the
//     edge is running, and an unnamed holder otherwise.
//
// /proc/net/tcp and /proc/net/udp must be readable; tcp6 and udp6 may be
// missing (IPv6 off). Any other read failure is errSockets.
func edgePortHolder(sys Sys, rows []listed, edgeID string, edgeRunning bool) (portUse, string, bool, error) {
	for _, use := range edgeUses {
		if use.proto != "tcp" {
			continue
		}
		for _, c := range rows {
			if c.ID == edgeID || len(c.Names) == 0 || (c.State != "running" && c.State != "restarting") {
				continue
			}
			for _, p := range c.Ports {
				if p.HostPort == use.port {
					return use, c.Names[0], true, nil
				}
			}
		}
	}
	socks := map[string][]listenRow{}
	for _, proto := range []string{"tcp", "udp"} {
		rows, err := readSocketTables(sys.Proc, proto)
		if err != nil {
			return portUse{}, "", false, err
		}
		socks[proto] = rows
	}
	want := map[uint64]bool{}
	for _, use := range edgeUses {
		for _, l := range socks[use.proto] {
			if l.port == use.port {
				want[l.inode] = true
			}
		}
	}
	if len(want) == 0 {
		return portUse{}, "", false, nil
	}
	pids := socketPIDs(sys.Proc, want)
	for _, use := range edgeUses {
		for _, l := range socks[use.proto] {
			if l.port != use.port {
				continue
			}
			pid, ok := pids[l.inode]
			if !ok {
				if edgeRunning {
					continue
				}
				return use, "", true, nil
			}
			if edgeID != "" && inContainer(sys.Proc, pid, edgeID) {
				continue
			}
			return use, procComm(sys.Proc, pid), true, nil
		}
	}
	return portUse{}, "", false, nil
}

// readSocketTables reads /proc/net/<proto> and <proto>6, any address. TCP
// keeps listening sockets (state 0A), UDP unconnected bound ones (07).
func readSocketTables(procRoot, proto string) ([]listenRow, error) {
	state := "0A"
	if proto == "udp" {
		state = "07"
	}
	var out []listenRow
	for _, name := range []string{proto, proto + "6"} {
		b, err := os.ReadFile(filepath.Join(procRoot, "net", name))
		if err != nil {
			if name != proto && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, errSockets
		}
		out = append(out, parseProcNetFor(string(b), state, func(net.IP) bool { return true })...)
	}
	return out, nil
}

// socketPIDs maps each wanted socket inode to the first process holding it.
func socketPIDs(procRoot string, want map[uint64]bool) map[uint64]int {
	out := map[uint64]int{}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return out
	}
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid < 1 {
			continue
		}
		fdDir := filepath.Join(procRoot, ent.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if ino, ok := socketIno(target); ok && want[ino] {
				if _, seen := out[ino]; !seen {
					out[ino] = pid
				}
			}
		}
		if len(out) == len(want) {
			break
		}
	}
	return out
}

// inContainer reports whether pid's cgroup path names the container id
// (docker-<id>.scope with the systemd driver, /docker/<id> with cgroupfs).
func inContainer(procRoot string, pid int, id string) bool {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), id)
}

func procComm(procRoot string, pid int) string {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
