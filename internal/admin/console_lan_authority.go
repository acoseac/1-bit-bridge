package admin

import "net/netip"

// Names-only shim (backlog B989's tests-first commit): the one parser, which
// parses nothing until the change.

type lanAuthority struct {
	addr netip.Addr
	name string
	port int
}

func parseLANAuthority(raw string) (lanAuthority, bool) { return lanAuthority{}, false }

func lanOriginAuthority(origin string) (lanAuthority, bool) { return lanAuthority{}, false }
