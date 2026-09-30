//go:build linux

package dlna

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// netnsChildEnv marks a run of this test binary as the child that runs
// TestSSDPAdvertisersKeepToTheirOwnInterfaces inside a network namespace of
// its own.
const netnsChildEnv = "DLNA_SSDP_NETNS_CHILD"

// netnsSide is one of the interfaces the namespace test gives the bridge:
// the near end of a veth pair whose far end sits unused beside it (a veth
// has carrier only while its peer is up), and its address.
type netnsSide struct {
	name, peer string
	ip         net.IP
}

// netnsSides are the bridge's two interfaces in the namespace test.
var netnsSides = []netnsSide{
	{"b71a0", "b71a1", net.IPv4(10, 71, 1, 1)},
	{"b71b0", "b71b1", net.IPv4(10, 71, 2, 1)},
}

// TestSSDPAdvertisersKeepToTheirOwnInterfaces pins backlog B71 end to end on
// Linux, on the real kernel: a DLNA server with two advertise endpoints, one
// per interface as on a multi-homed bridge, in a network namespace of the
// test's own. It checks the three things measured on main with the real
// binary. Every M-SEARCH arriving on one interface was answered by both
// advertisers, one naming the other subnet's LOCATION (the listener half).
// The NOTIFYs of the advertiser on the second interface carried the FIRST
// interface's address (the sender connected along the group's route before
// it was pinned). And with no route to the group, as on a host with no
// default route, no advertiser could start, so DLNA did not start at all.
//
// A network namespace needs root, or a user namespace where this host allows
// one (Ubuntu's AppArmor refuses it to an unconfined process), and a veth
// pair needs the veth module, which Docker loads; the test skips without
// them, so it runs on a host like dido and skips on CI. Everything in the
// namespace goes when the child exits: nothing it sends can reach a real
// network.
func TestSSDPAdvertisersKeepToTheirOwnInterfaces(t *testing.T) {
	if os.Getenv(netnsChildEnv) == "1" {
		runAdvertisersInANamespaceOfTheirOwn(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), netnsChildEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	if os.Geteuid() != 0 {
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err != nil && !errors.As(err, &exit):
		t.Skipf("no network namespace of this test's own here: %v", err)
	case strings.Contains(string(out), "--- SKIP: "+t.Name()):
		t.Skipf("the child in its own network namespace skipped:\n%s", out)
	case err != nil:
		t.Fatalf("the child in its own network namespace failed:\n%s", out)
	}
	t.Logf("the child in its own network namespace:\n%s", out)
}

// runAdvertisersInANamespaceOfTheirOwn is the child's side: it runs in a
// fresh network namespace, which it checks before it touches anything.
func runAdvertisersInANamespaceOfTheirOwn(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list interfaces: %v", err)
	}
	if len(ifaces) != 1 || ifaces[0].Flags&net.FlagLoopback == 0 {
		t.Fatalf("refusing to create interfaces: this is not a fresh network namespace (%d interfaces)", len(ifaces))
	}
	nl := openRouteNetlink(t)
	if err := nl.setUp(ifaces[0].Index); err != nil {
		t.Skipf("cannot configure this network namespace: %v", err)
	}
	for _, side := range netnsSides {
		if err := nl.newVethPair(side.name, side.peer); err != nil {
			t.Skipf("cannot create a veth pair here (is the veth module loaded?): %v", err)
		}
		near, far := interfaceNamed(t, side.name), interfaceNamed(t, side.peer)
		if err := nl.addIPv4(near.Index, side.ip, 24); err != nil {
			t.Fatalf("address %s: %v", side.name, err)
		}
		for _, i := range []*net.Interface{near, far} {
			if err := nl.setUp(i.Index); err != nil {
				t.Fatalf("bring %s up: %v", i.Name, err)
			}
		}
	}

	t.Run("with no route to the group", func(t *testing.T) {
		checkAdvertisersKeepToTheirInterfaces(t)
	})
	if err := nl.addRoute(net.IPv4(224, 0, 0, 0), 4, interfaceNamed(t, netnsSides[0].name).Index); err != nil {
		t.Fatalf("route the multicast range through %s: %v", netnsSides[0].name, err)
	}
	t.Run("with the group routed through the first interface", func(t *testing.T) {
		checkAdvertisersKeepToTheirInterfaces(t)
	})
}

// checkAdvertisersKeepToTheirInterfaces starts a DLNA server with one
// advertise endpoint per side and checks what it sends and answers.
func checkAdvertisersKeepToTheirInterfaces(t *testing.T) {
	notifies := listenOnEverySide(t)
	endpoints := startServerOnEverySide(t)
	checkNotifySources(t, notifies, endpoints)
	checkSearchesAnsweredPerSide(t, endpoints)
}

// listenOnEverySide opens a listener of the test's own on the SSDP group,
// joined on every side, before the server starts, so it hears every NOTIFY
// the start burst sends (the kernel loops a copy back as one that arrived
// on the interface it was sent from). ListenMulticastUDP sets SO_REUSEADDR,
// which the advertisers' listeners need on the port too.
func listenOnEverySide(t *testing.T) *net.UDPConn {
	t.Helper()
	group, err := net.ResolveUDPAddr("udp4", SSDPMulticastAddr)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenMulticastUDP("udp4", interfaceNamed(t, netnsSides[0].name), group)
	if err != nil {
		t.Fatalf("listen for NOTIFYs: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	for _, side := range netnsSides[1:] {
		if err := ipv4.NewPacketConn(l).JoinGroup(interfaceNamed(t, side.name), group); err != nil {
			t.Fatalf("join the group on %s: %v", side.name, err)
		}
	}
	return l
}

// startServerOnEverySide starts a DLNA server with one advertise endpoint
// per side, as the wiring gives a multi-homed host, stopped at the test's
// end, and returns the endpoints.
func startServerOnEverySide(t *testing.T) []AdvertiseEndpoint {
	t.Helper()
	endpoints := make([]AdvertiseEndpoint, len(netnsSides))
	for i, side := range netnsSides {
		endpoints[i] = AdvertiseEndpoint{Interface: interfaceNamed(t, side.name), ServerURL: "http://" + side.ip.String() + ":7790"}
	}
	s, err := NewServer(ServerConfig{
		Library:            newTestLib(testTrack("t1", "Test Track")),
		UDN:                "uuid:b71b71b7-1b71-4b71-8b71-b71b71b71b71",
		FriendlyName:       "B71",
		ListenAddress:      "0.0.0.0:7790",
		ServerURL:          endpoints[0].ServerURL,
		AdvertiseEndpoints: endpoints,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("the DLNA server did not start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	if got := len(s.ssdps); got != len(netnsSides) {
		t.Fatalf("%d advertisers started, want one per interface, %d", got, len(netnsSides))
	}
	return endpoints
}

// checkNotifySources reads the start burst's NOTIFYs from l: each names one
// side's LOCATION and must come from that side's address, and every side
// must be heard.
func checkNotifySources(t *testing.T, l *net.UDPConn, endpoints []AdvertiseEndpoint) {
	t.Helper()
	heard := map[string]int{}
	for range len(netnsSides) * len(NotifyTargetsFor("uuid:x")) {
		location, src, ok := readNotify(l)
		if !ok {
			break
		}
		if side, named := sideNamedIn(location); named && !src.Equal(side.ip) {
			t.Errorf("a NOTIFY for %s (LOCATION %s) came from %s, not from %s's own address", side.name, location, src, side.name)
		}
		heard[location]++
	}
	for _, e := range endpoints {
		if heard[e.ServerURL+"/dlna/description.xml"] == 0 {
			t.Errorf("heard no NOTIFY naming %s; heard %v", e.ServerURL, heard)
		}
	}
}

// readNotify reads l until a NOTIFY arrives and returns its LOCATION and
// source address; ok is false once nothing has arrived for two seconds.
func readNotify(l *net.UDPConn) (location string, src net.IP, ok bool) {
	buf := make([]byte, 4096)
	for {
		_ = l.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, err := l.ReadFromUDP(buf)
		if err != nil {
			return "", nil, false
		}
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(buf[:n])))
		if err == nil && req.Method == "NOTIFY" {
			return req.Header.Get("LOCATION"), from.IP, true
		}
	}
}

// sideNamedIn returns the side whose address a LOCATION names.
func sideNamedIn(location string) (netnsSide, bool) {
	for _, side := range netnsSides {
		if strings.Contains(location, "//"+side.ip.String()+":") {
			return side, true
		}
	}
	return netnsSide{}, false
}

// checkSearchesAnsweredPerSide sends an M-SEARCH out of each side: it must be
// answered with that side's LOCATION alone.
func checkSearchesAnsweredPerSide(t *testing.T, endpoints []AdvertiseEndpoint) {
	t.Helper()
	for i, side := range netnsSides {
		got := searchOutOf(t, interfaceNamed(t, side.name))
		want := endpoints[i].ServerURL + "/dlna/description.xml"
		if len(got) == 0 || slices.ContainsFunc(got, func(l string) bool { return l != want }) {
			t.Errorf("an M-SEARCH arriving on %s was answered with %q, want %q alone", side.name, got, want)
		}
	}
}

// searchOutOf sends one M-SEARCH for a MediaServer out of iface at TTL 0 (so
// the kernel loops it back as a datagram that arrived on iface) and returns
// the LOCATION of every answer, sorted.
func searchOutOf(t *testing.T, iface *net.Interface) []string {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatalf("open the searcher: %v", err)
	}
	defer c.Close()
	p := ipv4.NewPacketConn(c)
	if err := p.SetMulticastInterface(iface); err != nil {
		t.Fatalf("pin the searcher to %s: %v", iface.Name, err)
	}
	if err := p.SetMulticastTTL(0); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMulticastLoopback(true); err != nil {
		t.Fatal(err)
	}
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: " + SSDPMulticastAddr + "\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaServer:1\r\n\r\n"
	group, _ := net.ResolveUDPAddr("udp4", SSDPMulticastAddr)
	if _, err := c.WriteToUDP([]byte(msg), group); err != nil {
		t.Fatalf("send the M-SEARCH out of %s: %v", iface.Name, err)
	}
	var locations []string
	buf := make([]byte, 4096)
	deadline := time.Now().Add(1500 * time.Millisecond) // MX is 1 s
	for {
		_ = c.SetReadDeadline(deadline)
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			break
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		locations = append(locations, resp.Header.Get("LOCATION"))
	}
	slices.Sort(locations)
	return locations
}

// interfaceNamed looks an interface up by name.
func interfaceNamed(t *testing.T, name string) *net.Interface {
	t.Helper()
	iface, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatalf("interface %s: %v", name, err)
	}
	return iface
}

// routeNetlink is a NETLINK_ROUTE socket for the few requests the namespace
// test makes (what `ip link add`, `ip addr add`, `ip link set up` and `ip
// route add` send), so the test needs no iproute2 on the host.
type routeNetlink struct {
	fd  int
	seq uint32
}

// vethInfoPeer is VETH_INFO_PEER from the kernel's uapi linux/veth.h, which
// x/sys/unix does not carry.
const vethInfoPeer = 1

// openRouteNetlink opens a NETLINK_ROUTE socket closed at the test's end.
func openRouteNetlink(t *testing.T) *routeNetlink {
	t.Helper()
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		t.Skipf("cannot open a routing netlink socket: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return &routeNetlink{fd: fd}
}

// request sends one message and waits for the kernel's acknowledgement.
func (r *routeNetlink) request(msgType, flags uint16, body []byte) error {
	r.seq++
	msg := make([]byte, unix.SizeofNlMsghdr, unix.SizeofNlMsghdr+len(body))
	ne := binary.NativeEndian
	ne.PutUint32(msg[0:], uint32(unix.SizeofNlMsghdr+len(body)))
	ne.PutUint16(msg[4:], msgType)
	ne.PutUint16(msg[6:], flags|unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	ne.PutUint32(msg[8:], r.seq)
	msg = append(msg, body...)
	if err := unix.Sendto(r.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(r.fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Seq != r.seq || m.Header.Type != unix.NLMSG_ERROR {
				continue
			}
			if len(m.Data) < 4 {
				return errors.New("netlink: short acknowledgement")
			}
			if code := int32(ne.Uint32(m.Data[:4])); code != 0 {
				return syscall.Errno(-code)
			}
			return nil
		}
	}
}

// rtAttr encodes one netlink attribute, padded to four bytes.
func rtAttr(typ uint16, data []byte) []byte {
	b := make([]byte, (unix.SizeofRtAttr+len(data)+3)&^3)
	binary.NativeEndian.PutUint16(b[0:], uint16(unix.SizeofRtAttr+len(data)))
	binary.NativeEndian.PutUint16(b[2:], typ)
	copy(b[unix.SizeofRtAttr:], data)
	return b
}

// ifInfo encodes a struct ifinfomsg.
func ifInfo(index int, flags, change uint32) []byte {
	b := make([]byte, unix.SizeofIfInfomsg)
	binary.NativeEndian.PutUint32(b[4:], uint32(index))
	binary.NativeEndian.PutUint32(b[8:], flags)
	binary.NativeEndian.PutUint32(b[12:], change)
	return b
}

// cString is s with its terminating NUL, as the kernel takes a name.
func cString(s string) []byte { return append([]byte(s), 0) }

// newVethPair creates a veth pair, name and peer, in this namespace.
func (r *routeNetlink) newVethPair(name, peer string) error {
	peerInfo := append(ifInfo(0, 0, 0), rtAttr(unix.IFLA_IFNAME, cString(peer))...)
	linkInfo := append(rtAttr(unix.IFLA_INFO_KIND, cString("veth")),
		rtAttr(unix.IFLA_INFO_DATA, rtAttr(vethInfoPeer, peerInfo))...)
	body := append(ifInfo(0, 0, 0), rtAttr(unix.IFLA_IFNAME, cString(name))...)
	body = append(body, rtAttr(unix.IFLA_LINKINFO, linkInfo)...)
	return r.request(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, body)
}

// setUp brings the interface with this index up.
func (r *routeNetlink) setUp(index int) error {
	return r.request(unix.RTM_NEWLINK, 0, ifInfo(index, unix.IFF_UP, unix.IFF_UP))
}

// addIPv4 gives the interface with this index an IPv4 address.
func (r *routeNetlink) addIPv4(index int, ip net.IP, prefix int) error {
	b := make([]byte, unix.SizeofIfAddrmsg)
	b[0] = unix.AF_INET
	b[1] = byte(prefix)
	binary.NativeEndian.PutUint32(b[4:], uint32(index))
	b = append(b, rtAttr(unix.IFA_LOCAL, ip.To4())...)
	b = append(b, rtAttr(unix.IFA_ADDRESS, ip.To4())...)
	return r.request(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, b)
}

// addRoute routes dst/prefix out of the interface with index oif, on link.
func (r *routeNetlink) addRoute(dst net.IP, prefix, oif int) error {
	b := make([]byte, unix.SizeofRtMsg)
	b[0] = unix.AF_INET
	b[1] = byte(prefix)
	b[4] = unix.RT_TABLE_MAIN
	b[5] = unix.RTPROT_BOOT
	b[6] = unix.RT_SCOPE_LINK
	b[7] = unix.RTN_UNICAST
	oifBytes := make([]byte, 4)
	binary.NativeEndian.PutUint32(oifBytes, uint32(oif))
	b = append(b, rtAttr(unix.RTA_DST, dst.To4())...)
	b = append(b, rtAttr(unix.RTA_OIF, oifBytes)...)
	return r.request(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL, b)
}
