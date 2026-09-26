package doctor

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"maps"
	"strings"
	"testing"
)

// The datagrams below are CAPTURED from a real kernel (Linux 7.0, x86_64,
// 2026-09-26), not written by hand, so the parser is held to the layout the
// kernel writes rather than to a second copy of the offsets. They were taken
// inside a container, in its own network namespace, holding three loopback
// listeners of the capturing process: 127.0.0.1:7788 (inode 1944195),
// [::1]:7788 (inode 1944200) and 127.0.0.1:7789 (inode 1944202), all in the
// container's cgroup, whose directory's inode was 207946. The request was a
// SOCK_DIAG_BY_FAMILY dump of TCP listeners with no extension asked for.
const (
	// capturedSockDiagV4 is the AF_INET dump's first datagram: the two
	// IPv4 listeners, each with the attributes SHUTDOWN (8),
	// INET_DIAG_CGROUP_ID (21) and SOCKOPT (22).
	capturedSockDiagV4 = "74000000140002000700000001000000020a00001e6c00007f000001000000000000000000000000000000000000000000000000000000000000" +
		"00000f200000000000000000000000000000001000000000000083aa1d0005000800000000000c0015004a2c030000000000060016005200000074" +
		"000000140002000700000001000000020a00001e6d00007f0000010000000000000000000000000000000000000000000000000000000000000000" +
		"1020000000000000000000000000000000100000000000008aaa1d0005000800000000000c0015004a2c0300000000000600160052000000"
	// capturedSockDiagV6 is the AF_INET6 dump's first datagram: the IPv6
	// listener, which also carries SKV6ONLY (11).
	capturedSockDiagV6 = "7c0000001400020007000000010000000a0a00001e6c0000000000000000000000000000000000010000000000000000000000000000000000000000" +
		"11200000000000000000000000000000001000000000000088aa1d00050008000000000005000b00010000000c0015004a2c0300000000000600160012000000"
	// capturedSockDiagDone is each dump's last datagram: NLMSG_DONE, status 0.
	capturedSockDiagDone = "1400000003000200070000000100000000000000"
	// capturedSockDiagRefused is the kernel's whole answer to the same dump
	// asked for IP protocol 250, which no inet_diag handler serves: NLMSG_DONE
	// carrying -ENOENT, not an NLMSG_ERROR.
	capturedSockDiagRefused = "14000000030002000700000001000000feffffff"
)

// captured decodes one of the captured datagrams. They are little-endian, as
// netlink is host-endian and they came from x86_64; every target this repo
// builds for (amd64, arm64) is little-endian too, and a test run elsewhere
// could not read them, so it says so rather than failing.
func captured(t *testing.T, h string) []byte {
	t.Helper()
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the captured datagrams are little-endian and this host is not")
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestParseSockDiagReadsTheCgroupOfEachListenerOnThePort reads the captured
// dumps: each socket listening on the port asked about, by the fd-link text
// /proc/<pid>/fd gives it, mapped to the cgroup the kernel says created it,
// and nothing for another port. The first datagram of a dump is not its
// end; the DONE that follows is.
func TestParseSockDiagReadsTheCgroupOfEachListenerOnThePort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		datagram string
		port     int
		want     map[string]uint64
		wantDone bool
	}{
		{"IPv4, port 7788", capturedSockDiagV4, 7788, map[string]uint64{"socket:[1944195]": 207946}, false},
		{"IPv4, port 7789", capturedSockDiagV4, 7789, map[string]uint64{"socket:[1944202]": 207946}, false},
		{"IPv4, a port nothing listens on", capturedSockDiagV4, 8080, map[string]uint64{}, false},
		{"IPv6, port 7788", capturedSockDiagV6, 7788, map[string]uint64{"socket:[1944200]": 207946}, false},
		{"the dump's end", capturedSockDiagDone, 7788, map[string]uint64{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]uint64{}
			done, err := parseSockDiag(captured(t, tc.datagram), tc.port, got)
			if err != nil || done != tc.wantDone {
				t.Fatalf("done %v, err %v; want done %v, no error", done, err, tc.wantDone)
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseSockDiagFailsARefusedDump: the kernel ends a dump it refuses with
// NLMSG_DONE carrying a negative errno (captured), and answers a malformed
// request with NLMSG_ERROR; both are errors, never an empty answer, since
// an empty answer is "no listener has a cgroup", which the census would
// then read as nothing to account for rather than nothing known.
func TestParseSockDiagFailsARefusedDump(t *testing.T) {
	refused := captured(t, capturedSockDiagRefused)
	if done, err := parseSockDiag(refused, 7788, map[string]uint64{}); !done || err == nil || !strings.Contains(err.Error(), "errno 2") {
		t.Errorf("DONE carrying -ENOENT: done %v, err %v; want done, and an error naming errno 2", done, err)
	}
	asError := append([]byte(nil), refused...)
	binary.NativeEndian.PutUint16(asError[4:], nlmsgError)
	if _, err := parseSockDiag(asError, 7788, map[string]uint64{}); err == nil || !strings.Contains(err.Error(), "errno 2") {
		t.Errorf("NLMSG_ERROR carrying -ENOENT: err %v; want an error naming errno 2", err)
	}
}

// TestParseSockDiagLeavesOutWhatItCannotRead edits the captured datagrams:
// a socket that carries no cgroup attribute (a kernel before 5.8 sends
// none) or is not listening is left out, and a datagram that does not
// parse is an error rather than a partial answer.
func TestParseSockDiagLeavesOutWhatItCannotRead(t *testing.T) {
	v4 := captured(t, capturedSockDiagV4)
	// The first message is 116 bytes: the netlink header (16), struct
	// inet_diag_msg (72), then SHUTDOWN (8 bytes, padded), then the cgroup
	// attribute, whose type is at byte 16+72+8+2.
	const cgroupAttrType, stateByte = 16 + 72 + 8 + 2, 16 + 1
	if got := binary.NativeEndian.Uint16(v4[cgroupAttrType:]); got != inetDiagCgroupAttr {
		t.Fatalf("the capture's first cgroup attribute is not where this test edits it (type %d)", got)
	}
	noCgroup := append([]byte(nil), v4...)
	binary.NativeEndian.PutUint16(noCgroup[cgroupAttrType:], 23)
	notListening := append([]byte(nil), v4...)
	notListening[stateByte] = 1 // TCP_ESTABLISHED
	for _, tc := range []struct {
		name     string
		datagram []byte
	}{
		{"no cgroup attribute", noCgroup},
		{"not listening", notListening},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]uint64{}
			if _, err := parseSockDiag(tc.datagram, 7788, got); err != nil || len(got) != 0 {
				t.Errorf("got %v, err %v; want nothing, no error", got, err)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		datagram []byte
	}{
		{"cut inside a message", v4[:100]},
		{"cut inside a header", v4[:10]},
		{"a message claiming more than the datagram holds", append([]byte{0xff, 0, 0, 0}, v4[4:]...)},
		{"a message shorter than struct inet_diag_msg", append([]byte{40, 0, 0, 0}, v4[4:40]...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSockDiag(tc.datagram, 7788, map[string]uint64{}); !errors.Is(err, errSockDiagMalformed) {
				t.Errorf("err %v; want errSockDiagMalformed", err)
			}
		})
	}
}
