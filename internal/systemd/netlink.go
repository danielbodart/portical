package systemd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"path/filepath"
	"syscall"

	"github.com/danielbodart/portical/internal/model"
)

// Kernel is the real machine's listening sockets, asked of the kernel
// directly over NETLINK_SOCK_DIAG - the interface `ss` itself uses - so
// nothing has to be installed, run or parsed.
//
// Each socket comes with the id of the cgroup that created it (Linux 5.7 and
// later), which is the inode of that cgroup's directory in the cgroup2 file
// system - or, on a 32-bit kernel, holds it in its low half; see cgroupPath.
// So the ids are put back to paths by walking that file system, as
// ss does, and a unit's sockets are the ones at its ControlGroup or beneath.
func Kernel() SocketLister { return kernel{cgroupRoot: "/sys/fs/cgroup"} }

type kernel struct {
	// Where the cgroup2 file system is, or contains it at unified/ on a host
	// still mounting the v1 hierarchies.
	cgroupRoot string
}

// From linux/netlink.h, linux/sock_diag.h, linux/inet_diag.h and
// net/tcp_states.h; the syscall package has the netlink basics but none of
// sock_diag.
const (
	netlinkSockDiag  = 4
	sockDiagByFamily = 20

	inetDiagSkV6Only = 11
	inetDiagCgroupID = 21

	tcpListen = 10
	// An unconnected UDP socket is in TCP_CLOSE, which ss shows as UNCONN.
	tcpClose = 7

	cgroup2SuperMagic = 0x63677270

	// inet_diag_msg: family, state, timer, retrans, then inet_diag_sockid (48
	// bytes) and five u32s.
	inetDiagMsgLength = 72
)

type query struct {
	protocol model.Protocol
	family   uint8
	number   uint8
	state    uint
}

// queries is everything listed, each a dump of its own: the kernel answers a
// sock_diag request for one family and one protocol.
var queries = []query{
	{model.TCP, syscall.AF_INET, syscall.IPPROTO_TCP, tcpListen},
	{model.TCP, syscall.AF_INET6, syscall.IPPROTO_TCP, tcpListen},
	{model.UDP, syscall.AF_INET, syscall.IPPROTO_UDP, tcpClose},
	{model.UDP, syscall.AF_INET6, syscall.IPPROTO_UDP, tcpClose},
}

func (k kernel) Listening(ctx context.Context) ([]Listener, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, netlinkSockDiag)
	if err != nil {
		return nil, fmt.Errorf("opening sock_diag netlink socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("binding sock_diag netlink socket: %w", err)
	}

	var listeners []Listener
	for i, q := range queries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		found, err := dump(fd, uint32(i+1), q)
		if err != nil {
			return nil, fmt.Errorf("listing %s sockets: %w", q.protocol, err)
		}
		listeners = append(listeners, found...)
	}

	if !anyCgroup(listeners) {
		return listeners, nil
	}
	paths, err := cgroupPaths(k.cgroupRoot)
	if errors.Is(err, ErrUnknownCgroups) {
		// A host on cgroup v1 alone still sends an id - its default
		// hierarchy's root - but there is no cgroup2 tree to look it up in.
		return listeners, err
	}
	if err != nil {
		return nil, err
	}
	for i := range listeners {
		listeners[i].Cgroup = cgroupPath(paths, listeners[i].CgroupID)
	}
	return listeners, nil
}

// request is an inet_diag_req_v2 dump request: every socket of one family and
// protocol in one state, from anywhere to anywhere.
func request(seq uint32, q query) []byte {
	message := make([]byte, syscall.NLMSG_HDRLEN+56)
	order := binary.NativeEndian
	order.PutUint32(message[0:], uint32(len(message)))
	order.PutUint16(message[4:], sockDiagByFamily)
	order.PutUint16(message[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_DUMP)
	order.PutUint32(message[8:], seq)
	body := message[syscall.NLMSG_HDRLEN:]
	body[0] = q.family
	body[1] = q.number
	// idiag_ext asks only for the extensions numbered up to 8. The two read
	// here, v6only and the cgroup id, come with every socket regardless.
	order.PutUint32(body[4:], 1<<q.state)
	return message
}

func dump(fd int, seq uint32, q query) ([]Listener, error) {
	if err := syscall.Sendto(fd, request(seq, q), 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, err
	}

	var listeners []Listener
	buffer := make([]byte, 64*1024)
	for {
		n, _, err := syscall.Recvfrom(fd, buffer, 0)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return nil, err
		}
		messages, err := syscall.ParseNetlinkMessage(buffer[:n])
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			if message.Header.Seq != seq {
				continue
			}
			switch message.Header.Type {
			case syscall.NLMSG_DONE, syscall.NLMSG_ERROR:
				// Both carry an errno, negated; DONE's is 0 unless the dump
				// failed part way, and an ERROR of 0 is an acknowledgement.
				if len(message.Data) >= 4 {
					if errno := int32(binary.NativeEndian.Uint32(message.Data)); errno < 0 {
						return nil, syscall.Errno(-errno)
					}
				}
				if message.Header.Type == syscall.NLMSG_DONE {
					return listeners, nil
				}
			case sockDiagByFamily:
				listener, err := parseDiag(message.Data, q.protocol)
				if err != nil {
					return nil, err
				}
				listeners = append(listeners, listener)
			}
		}
	}
}

// parseDiag reads one inet_diag_msg and the attributes after it.
func parseDiag(data []byte, protocol model.Protocol) (Listener, error) {
	if len(data) < inetDiagMsgLength {
		return Listener{}, fmt.Errorf("short inet_diag_msg: %d bytes", len(data))
	}
	order := binary.NativeEndian
	listener := Listener{
		Protocol: protocol,
		// Ports and addresses are in network order, everything else in the
		// host's.
		Port:      int(binary.BigEndian.Uint16(data[4:6])),
		Interface: int(order.Uint32(data[40:44])),
	}
	switch data[0] {
	case syscall.AF_INET:
		listener.Address = netip.AddrFrom4([4]byte(data[8:12]))
	case syscall.AF_INET6:
		listener.Address = netip.AddrFrom16([16]byte(data[8:24]))
	default:
		return Listener{}, fmt.Errorf("unexpected address family %d", data[0])
	}

	for attributes := data[inetDiagMsgLength:]; len(attributes) >= syscall.SizeofRtAttr; {
		length := int(order.Uint16(attributes[0:2]))
		kind := order.Uint16(attributes[2:4]) & 0x3fff // without NLA_F_NESTED and NLA_F_NET_BYTEORDER
		if length < syscall.SizeofRtAttr || length > len(attributes) {
			return Listener{}, fmt.Errorf("malformed inet_diag attribute")
		}
		payload := attributes[syscall.SizeofRtAttr:length]
		switch {
		case kind == inetDiagSkV6Only && len(payload) >= 1:
			listener.V6Only = payload[0] != 0
		case kind == inetDiagCgroupID && len(payload) >= 8:
			listener.CgroupID = order.Uint64(payload)
		}
		aligned := (length + syscall.RTA_ALIGNTO - 1) &^ (syscall.RTA_ALIGNTO - 1)
		if aligned >= len(attributes) {
			break
		}
		attributes = attributes[aligned:]
	}
	return listener, nil
}

// cgroupPaths is every cgroup's path, such as /system.slice/sshd.service, by
// the inode of its directory - which on cgroup2 is its id.
func cgroupPaths(root string) (map[uint64]string, error) {
	root, err := cgroup2(root)
	if err != nil {
		return nil, err
	}
	paths := map[uint64]string{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			// A cgroup removed while walking, or one this user may not
			// list: its sockets go unmatched, as they would have been a
			// moment later or earlier.
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			paths[stat.Ino] = "/"
		} else {
			paths[stat.Ino] = "/" + filepath.ToSlash(relative)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading cgroups: %w", err)
	}
	return paths, nil
}

// cgroupPath is the path of the cgroup the kernel gave this id, or "" if it
// is not one of paths.
//
// The id is the cgroup directory's kernfs node id, and that is its inode
// number only where ino_t is 64 bits. A 32-bit kernel - armv7 on 32-bit
// Raspberry Pi OS, which the releases are built for - keeps the inode in the
// low half and a generation, starting at 1, in the high half, so no id there
// ever equals the inode a stat reports. ss sidesteps this by asking for every
// directory's file handle; falling back to the low half gives the same answer
// without a syscall per cgroup, as no two live cgroups share an inode. On a
// 64-bit kernel the id is the inode, so the whole id is what matches there.
func cgroupPath(paths map[uint64]string, id uint64) string {
	if path, ok := paths[id]; ok {
		return path
	}
	return paths[uint64(uint32(id))]
}

// cgroup2 is where the cgroup2 hierarchy is mounted: root itself on a host
// with only cgroup2, which is every current distribution, or root/unified on
// one in systemd's hybrid mode.
func cgroup2(root string) (string, error) {
	for _, candidate := range []string{root, filepath.Join(root, "unified")} {
		var stat syscall.Statfs_t
		if syscall.Statfs(candidate, &stat) == nil && stat.Type == cgroup2SuperMagic {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no cgroup2 file system at %s, so %w", root, ErrUnknownCgroups)
}
