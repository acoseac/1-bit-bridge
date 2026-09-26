package doctor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
)

// This file holds the PARSER for the kernel's socket diagnostics replies
// (NETLINK_SOCK_DIAG, SOCK_DIAG_BY_FAMILY: what `ss` reads), untagged for
// portowner.go's reason: only sockdiag_linux.go can ask the kernel, but the
// byte layout is pure data work, so it is tested on every platform against
// datagrams captured from a real kernel (sockdiag_parse_test.go).
//
// The census reads one fact from these replies that /proc/net/tcp{,6} does
// not carry: the cgroup each listener was created in (cgroupsNotOf).

// Netlink and inet_diag constants, from the kernel's uapi headers
// (linux/netlink.h, linux/sock_diag.h, linux/inet_diag.h). The attribute
// number is from the enum there, INET_DIAG_CGROUP_ID, which Linux 5.8 added
// (commit 6e3a401fc8af); x/sys/unix does not name it.
const (
	nlmsgHeaderLen     = 16
	nlmsgError         = 2
	nlmsgDone          = 3
	sockDiagByFamily   = 20
	inetDiagMsgLen     = 72 // struct inet_diag_msg
	inetDiagCgroupAttr = 21 // INET_DIAG_CGROUP_ID
)

// errSockDiagMalformed is a reply that does not parse as the kernel writes
// one.
var errSockDiagMalformed = errors.New("socket diagnostics: malformed reply")

// parseSockDiag reads one datagram (b) of the kernel's reply to a
// SOCK_DIAG_BY_FAMILY dump of TCP listeners, and adds to into, for every
// socket in it listening on port, the cgroup the kernel reports it was
// created in (INET_DIAG_CGROUP_ID), keyed by its fd-link text,
// `socket:[<inode>]`, as listenerSockets keys it. A socket the kernel
// reports no cgroup for is left out: a kernel before 5.8, or one built
// without CONFIG_SOCK_CGROUP_DATA, sends none.
//
// done reports the end of the dump, NLMSG_DONE. The kernel ends a dump it
// refuses there too, with a negative errno in place of 0 (measured on
// Linux 7.0: a request for an IP protocol no handler serves ends in DONE
// carrying -ENOENT, not in an NLMSG_ERROR), and that is an error, as an
// NLMSG_ERROR is. So is a message that does not parse.
//
// Every field but the ports is in host byte order, as netlink's always are;
// the ports are big-endian, as in struct sockaddr_in.
func parseSockDiag(b []byte, port int, into map[string]uint64) (done bool, err error) {
	ne := binary.NativeEndian
	for len(b) > 0 {
		if len(b) < nlmsgHeaderLen {
			return false, errSockDiagMalformed
		}
		msgLen := int(ne.Uint32(b[0:]))
		if msgLen < nlmsgHeaderLen || msgLen > len(b) {
			return false, errSockDiagMalformed
		}
		if done, err := sockDiagMessage(ne.Uint16(b[4:]), b[nlmsgHeaderLen:msgLen], port, into); done || err != nil {
			return done, err
		}
		// Messages are padded to a 4-byte boundary (NLMSG_ALIGN); the last
		// in a datagram may end without its padding.
		next := (msgLen + 3) &^ 3
		if next >= len(b) {
			break
		}
		b = b[next:]
	}
	return false, nil
}

// sockDiagMessage reads one message of the reply, of netlink type typ, its
// body after the header: the dump's end (NLMSG_DONE, and the status there),
// an NLMSG_ERROR, or a socket (addListenerCgroup). Any other type is
// skipped.
func sockDiagMessage(typ uint16, body []byte, port int, into map[string]uint64) (done bool, err error) {
	ne := binary.NativeEndian
	switch typ {
	case nlmsgDone:
		if len(body) >= 4 {
			if status := int32(ne.Uint32(body)); status < 0 {
				return true, fmt.Errorf("socket diagnostics: the kernel ended the dump with errno %d", -status)
			}
		}
		return true, nil
	case nlmsgError:
		if len(body) < 4 {
			return false, errSockDiagMalformed
		}
		if status := int32(ne.Uint32(body)); status < 0 {
			return false, fmt.Errorf("socket diagnostics: the kernel refused the request with errno %d", -status)
		}
		// An acknowledgement, which a dump request does not ask for:
		// nothing follows it.
		return true, nil
	case sockDiagByFamily:
		return false, addListenerCgroup(body, port, into)
	}
	return false, nil
}

// addListenerCgroup reads one struct inet_diag_msg and the attributes after
// it, and records the socket's cgroup in into when it listens on port.
func addListenerCgroup(msg []byte, port int, into map[string]uint64) error {
	if len(msg) < inetDiagMsgLen {
		return errSockDiagMalformed
	}
	ne := binary.NativeEndian
	// idiag_state is TCP_LISTEN (10) for every socket a listeners-only dump
	// returns; checked anyway, since the state filter is what keeps an
	// outbound connection from this port out (eachListenRow's reason).
	if msg[1] != 10 || int(binary.BigEndian.Uint16(msg[4:])) != port {
		return nil
	}
	inode := ne.Uint32(msg[68:])
	for attrs := msg[inetDiagMsgLen:]; len(attrs) >= 4; {
		attrLen := int(ne.Uint16(attrs[0:]))
		if attrLen < 4 || attrLen > len(attrs) {
			return errSockDiagMalformed
		}
		if ne.Uint16(attrs[2:]) == inetDiagCgroupAttr {
			if attrLen < 12 {
				return errSockDiagMalformed
			}
			into["socket:["+strconv.FormatUint(uint64(inode), 10)+"]"] = ne.Uint64(attrs[4:])
		}
		next := (attrLen + 3) &^ 3
		if next >= len(attrs) {
			break
		}
		attrs = attrs[next:]
	}
	return nil
}
