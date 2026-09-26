//go:build linux

package doctor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// sockDiagTimeout bounds each wait for the kernel's reply. The kernel
// answers a dump at once, so this only ever fires on something broken, and
// a doctor run must not hang on it.
const sockDiagTimeout = 2 * time.Second

// listenerCgroups asks the kernel's socket diagnostics (NETLINK_SOCK_DIAG,
// SOCK_DIAG_BY_FAMILY: what `ss --cgroup` reads) which cgroup each TCP
// socket listening on port was created in, over both address families, by
// its fd-link text (parseSockDiag). It is the census's source for that fact
// (cgroupsNotOf), which /proc/net/tcp{,6} does not carry.
//
// Any user may ask, and the answer covers every socket in this network
// namespace, root's included, as the socket tables do: measured on Linux 7.0
// as uid 1000, with no attribute requested and no capability held. The
// cgroup is the id of the cgroup's directory on the cgroup2 mount, its
// inode number, fixed when socket(2) created the socket (commit
// 6e3a401fc8af, which added the attribute in 5.8, says so: it is "not
// changed when process get moved to another cgroup"). An error means the
// answer is unknown, and the census then counts nothing by cgroup.
func listenerCgroups(port int) (map[string]uint64, error) {
	cgroups := map[string]uint64{}
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := dumpListenerCgroups(family, port, cgroups); err != nil {
			return nil, err
		}
	}
	return cgroups, nil
}

// dumpListenerCgroups asks for one address family's TCP listeners and adds
// their cgroups to into (parseSockDiag).
func dumpListenerCgroups(family uint8, port int, into map[string]uint64) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return fmt.Errorf("socket diagnostics: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	tv := unix.NsecToTimeval(sockDiagTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("socket diagnostics: %w", err)
	}
	// struct nlmsghdr, then struct inet_diag_req_v2: the family, TCP, no
	// extensions requested (the cgroup comes without one), and the states
	// wanted as a bit set, TCP_LISTEN's alone. The socket id stays zero, so
	// the kernel filters nothing more; parseSockDiag matches the port.
	const seq = 1
	req := make([]byte, nlmsgHeaderLen+56)
	ne := binary.NativeEndian
	ne.PutUint32(req[0:], uint32(len(req)))
	ne.PutUint16(req[4:], sockDiagByFamily)
	ne.PutUint16(req[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	ne.PutUint32(req[8:], seq)
	req[nlmsgHeaderLen] = family
	req[nlmsgHeaderLen+1] = unix.IPPROTO_TCP
	ne.PutUint32(req[nlmsgHeaderLen+4:], 1<<10)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("socket diagnostics: %w", err)
	}
	buf := make([]byte, 64<<10)
	for {
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return fmt.Errorf("socket diagnostics: %w", err)
		}
		// Only the kernel (port id 0) answers: an unprivileged process
		// cannot send to another's NETLINK_SOCK_DIAG socket, but nothing
		// is taken on that alone.
		if nl, ok := from.(*unix.SockaddrNetlink); !ok || nl.Pid != 0 {
			continue
		}
		if n >= nlmsgHeaderLen && ne.Uint32(buf[8:]) != seq {
			continue
		}
		done, err := parseSockDiag(buf[:n], port, into)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if n == 0 {
			return errors.New("socket diagnostics: the kernel sent an empty reply")
		}
	}
}
