package dlna

import (
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

const (
	// servedMaxAge is how long a snapshot of the served addresses answers
	// before the next question rebuilds it.
	servedMaxAge = 10 * time.Second

	// servedMissRebuild is how old a snapshot must be before an address it
	// does not serve rebuilds it. A LAN address that comes up after Start is
	// served from its first connection a second later at most, and a flood
	// of refused connections rebuilds once a second.
	servedMissRebuild = time.Second

	// servedRefusedSeenCap bounds the refusals noteRefused logs.
	servedRefusedSeenCap = 32
)

// servedAddrs answers which local addresses the DLNA listener and its SSDP
// advertisers serve (backlog B48). Both bind wildcards: the HTTP listener
// binds dlna.listenAddress, ":7790" by default, and each advertiser's
// M-SEARCH listener 0.0.0.0:1900 (net.ListenMulticastUDP binds the
// wildcard). A wildcard socket takes a connection, or a unicast datagram, on
// every address of this host, and the eligibility rules (IsLANEligibleInterface
// and the pickers) chose only where the advertisers announce, so before B48
// every tailnet peer reached the unauthenticated listener on the host's
// tailnet address. The listener now judges each connection by the address it
// was accepted on, and each advertiser each M-SEARCH by the address it was
// sent to, and both ask this.
//
// One servedAddrs belongs to one Server, shared by its listener and its
// advertisers. It reads the host's interfaces into a snapshot: rebuilt when
// it is servedMaxAge old, and, for an address it does not serve, when it is
// servedMissRebuild old. A rebuild that cannot list the interfaces keeps the
// snapshot it would have replaced.
type servedAddrs struct {
	// interfaces and addrsOf read the host's interfaces; nil is
	// net.Interfaces and hostInterfaceAddrs. A test stands in a host of its
	// own (fakeHost).
	interfaces func() ([]net.Interface, error)
	addrsOf    interfaceAddrs

	// now is the clock the snapshot ages by; nil is time.Now.
	now func() time.Time

	// advertised holds the literal hosts the server advertises
	// (knownOwnHosts: its ServerURL, each advertise endpoint's LOCATION and
	// a pinned listen address), unmapped and without a zone.
	advertised map[netip.Addr]struct{}

	// judgeAs, when set, replaces the address admit is asked about. A
	// test's connection and datagram arrive on loopback, and this is how one
	// is judged as if it had arrived on a tailnet or a LAN address. nil in
	// production.
	judgeAs func(netip.Addr) netip.Addr

	mu      sync.Mutex
	built   bool
	builtAt time.Time
	snap    servedSnapshot

	refusedMu   sync.Mutex
	refusedSeen map[string]struct{}
}

// servedSnapshot is one reading of the host's interfaces.
type servedSnapshot struct {
	// served holds the served addresses, by servedKey.
	served map[netip.Addr]struct{}
	// holder names the interface that holds each address of the host, by
	// servedKey, for the refusal log.
	holder map[netip.Addr]string
}

// newServedAddrs returns the served addresses of this host for a server
// that advertises the literal hosts in advertised.
func newServedAddrs(advertised map[netip.Addr]struct{}) *servedAddrs {
	return &servedAddrs{advertised: advertised}
}

// admit judges a connection or a datagram that arrived on the local address
// local from the peer from (an IP literal, for the log), and logs a refusal
// (noteRefused) on log. what names it there: "a connection" or "an
// M-SEARCH".
func (s *servedAddrs) admit(log *slog.Logger, what string, local netip.Addr, from string) bool {
	if s.judgeAs != nil {
		local = s.judgeAs(local)
	}
	if s.serves(local) {
		return true
	}
	s.noteRefused(log, what, local, from)
	return false
}

// serves reports whether the listener and its advertisers serve the local
// address a. They serve:
//
//   - a loopback address: this host itself, which RFC 1122 has a host
//     discard from any other interface, and whose processes already own the
//     bridge, as the loopback console without auth has it;
//   - an address of a member of the multicast set (pickAllLANInterfaces with
//     no opt-in, the interfaces every advertiser and discovery client runs
//     on) that the eligibility check counts as a LAN address
//     (lanAddrKindOf): RFC 1918, a ULA outside Tailscale's, or link-local,
//     an IPv6 one only with that member as its zone;
//   - an address of a member that the server itself advertises, whatever
//     its kind, so it never announces an address it refuses (an interface
//     can be eligible through a ULA beside the public IPv4 its LOCATION
//     names).
//
// Nothing else: not a tailnet address (Tailscale's interface is not a member
// without the opt-in, and its 100.64/10 and ULA addresses count as public in
// any case), not another interface's address (a docker veth's fe80, awdl0's,
// a link-local tunnel's), and not a public address of a member that the
// server does not advertise, such as a dual-stack LAN's SLAAC address, which
// the router may let the internet reach.
func (s *servedAddrs) serves(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	if a.IsLoopback() {
		return true
	}
	k := servedKey(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if !s.built || now.Sub(s.builtAt) >= servedMaxAge {
		s.rebuildLocked(now)
	}
	if _, ok := s.snap.served[k]; ok {
		return true
	}
	if now.Sub(s.builtAt) < servedMissRebuild {
		return false
	}
	s.rebuildLocked(now)
	_, ok := s.snap.served[k]
	return ok
}

// clock is s.now, or time.Now.
func (s *servedAddrs) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// rebuildLocked reads the host's interfaces into s.snap. s.mu is held.
func (s *servedAddrs) rebuildLocked(now time.Time) {
	s.built = true
	s.builtAt = now
	list := s.interfaces
	if list == nil {
		list = net.Interfaces
	}
	ifaces, err := list()
	if err != nil {
		// Keep the snapshot this one would replace: one failed listing must
		// not refuse the LAN. builtAt above keeps a host whose listing keeps
		// failing from listing more than once a second.
		return
	}
	addrsOf := s.addrsOf
	if addrsOf == nil {
		addrsOf = hostInterfaceAddrs
	}
	s.snap = servedSnapshotOf(ifaces, addrsOf, s.advertised)
}

// servedSnapshotOf reads which addresses a host whose interfaces are ifaces
// serves (servedAddrs.serves). Each interface's addresses are read once, and
// the multicast set is chosen from that same reading.
func servedSnapshotOf(ifaces []net.Interface, addrsOf interfaceAddrs, advertised map[netip.Addr]struct{}) servedSnapshot {
	type reading struct {
		addrs []net.Addr
		err   error
	}
	readings := make(map[*net.Interface]reading, len(ifaces))
	snap := servedSnapshot{served: map[netip.Addr]struct{}{}, holder: map[netip.Addr]string{}}
	for i := range ifaces {
		ifi := &ifaces[i]
		addrs, err := addrsOf(ifi)
		readings[ifi] = reading{addrs: addrs, err: err}
		for _, a := range addrs {
			if k, ok := interfaceAddrKey(a, ifi.Name); ok {
				snap.holder[k] = ifi.Name
			}
		}
	}
	read := func(ifi *net.Interface) ([]net.Addr, error) {
		r := readings[ifi]
		return r.addrs, r.err
	}
	for _, member := range pickAllLANInterfaces(ifaces, read, EligibilityOpts{}) {
		for _, a := range readings[member].addrs {
			k, ok := interfaceAddrKey(a, member.Name)
			if !ok {
				continue
			}
			switch lanAddrKindOf(ipFromAddr(a)) {
			case lanAddrPrivate, lanAddrLinkLocal:
				snap.served[k] = struct{}{}
			case lanAddrPublic:
				if _, adv := advertised[k.WithZone("")]; adv {
					snap.served[k] = struct{}{}
				}
			}
		}
	}
	return snap
}

// servedKey is how an address is looked up in a snapshot: unmapped, keeping
// the zone a link-local IPv6 address needs to name its interface, and with
// no zone on any other.
func servedKey(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if a.Is6() && a.IsLinkLocalUnicast() {
		return a
	}
	return a.WithZone("")
}

// interfaceAddrKey is the servedKey of an address the interface named name
// holds. ok is false for an address that is not an IP.
func interfaceAddrKey(a net.Addr, name string) (netip.Addr, bool) {
	addr, ok := netip.AddrFromSlice(ipFromAddr(a))
	if !ok {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	if addr.Is6() && addr.IsLinkLocalUnicast() {
		return addr.WithZone(name), true
	}
	return addr, true
}

// noteRefused logs a refusal on log, once per kind and address and within
// servedRefusedSeenCap. The address is this host's own, so a peer cannot
// grow the set; the bound is for a host whose addresses churn.
func (s *servedAddrs) noteRefused(log *slog.Logger, what string, local netip.Addr, from string) {
	addr := "(not reported)"
	if local.IsValid() {
		addr = local.String()
	}
	key := what + "|" + addr
	s.refusedMu.Lock()
	if s.refusedSeen == nil {
		s.refusedSeen = make(map[string]struct{})
	}
	_, seen := s.refusedSeen[key]
	full := len(s.refusedSeen) >= servedRefusedSeenCap
	if !seen && !full {
		s.refusedSeen[key] = struct{}{}
	}
	s.refusedMu.Unlock()
	if seen || full {
		return
	}
	log.Warn("DLNA refused "+what+" to an address it does not serve: it answers only on the LAN-eligible interfaces",
		slog.String("address", addr),
		slog.String("interface", s.holderOf(local)),
		slog.String("from", from))
}

// holderOf names the interface that holds a in the current snapshot, or
// "(none)".
func (s *servedAddrs) holderOf(a netip.Addr) string {
	if !a.IsValid() {
		return "(none)"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if name, ok := s.snap.holder[servedKey(a)]; ok {
		return name
	}
	return "(none)"
}
