// Package openhome is the player's LAN surface (B470 PR 5a2a): what an
// OpenHome controller talks to, reachable on the LAN behind the opt-in
// (output.player.openhome, off by default, its default decided once 1-bit
// lists the bridge's player beside the box's MPD, B470 5b and 5c). Bound
// listeners advertise a Source through serve's shared SSDP subsystem (PR
// 5a2b).
//
// It is a listener of its own, never mounted on the DLNA listener, whose
// guards (servedOnlyListener, ownHostOnly) do not meet the player's LAN
// rule: they serve every LAN address to any source and pass any Host that
// names this host.
//
// # Addresses
//
// One socket per IPv4 address internal/player/lan serves (an eligible
// interface's RFC 1918 or 169.254 address: up, running, multicast, not
// point-to-point, never public or tailnet), keyed by the address, its
// on-link prefix and its interface: a listing every Limits.Rescan (5 s)
// binds an address before advertising it, and withdraws before closing the
// socket of one that goes away, changes its prefix or interface, or stops
// being eligible. A listing that fails keeps what is bound. The port is
// output.player.openhomePort (7792), or one the kernel picks per address
// (0), which every URL then carries. A bind that fails fails that address
// alone, is said once, and is tried at the next listing.
//
// # Admission, at accept
//
// Before any cap counts it, a connection is closed at once, unread, and
// counted against nothing unless its local address is the socket's, while
// that address is served, and its source is approved: an IPv4 address
// inside the on-link prefix of the address it arrived on (not another
// address's or another interface's), of the LAN's classes (so a prefix
// wider than its address's class admits no public address), never
// loopback, never one of the box's own addresses. Then a request's Host
// must be exactly the address and port it arrived on, no default port
// elided, or it is 400.
//
// IPv4 on Linux is the weak host model: an address bound on one interface
// is reached through another. A handshake from outside the prefix needs a
// route back, so source-in-prefix and a served destination refuse what
// they must in the ordinary case; besides, each socket is restricted to
// its interface where an unprivileged option exists: SO_BINDTOIFINDEX on
// Linux, where the kernel allows it unprivileged (5.7 on), the socket left
// unrestricted and saying why where it does not; IP_BOUND_IF on macOS,
// which scopes what the socket sends and does not filter what it receives
// (measured, restrict_darwin.go); nothing on Windows. The residual: a
// router on the LAN that rewrites sources into the prefix (SNAT), and
// anything on the link that holds an address in it, is admitted.
//
// # Limits
//
// Caps on the connections open, all and per source, counted after
// admission (DefaultLimits: 32 and 8); a header read deadline, the whole
// request's, a control body's own (row 6, before a slot is taken), a write
// deadline from when the request's headers were read, an idle deadline;
// 16 KiB of headers read; the body bound (maxBody); Limits.Slots requests
// answered at once, 503 at once past them; and of those, Limits.WriteSlots
// writes but Stop waiting for the engine, two slots fewer, and
// Limits.WritesPerSource of them from one source, 503 at once past either
// (PR 5a4). Stop refuses every new
// request (503, its connection closed), withdraws discovery while HTTP is
// bound, joins the listing loop, shuts each
// socket down within Limits.ShutdownGrace, closes what is still open, and
// joins the goroutines it started within Limits.Join.
//
// # The surface
//
// Only /openhome/ (handler.go): the device description, built per request
// with every URL absolute on the address and port the request arrived on;
// the four services' descriptions, the bridge's own, generated from its
// tables (services.go); the SOAP layer (soap.go); the actions (actions.go):
// every read answered from one snapshot of the player's engine, fault 501
// while no engine runs (PR 5a3), and every write one engine command, or none
// for the Product writes that change nothing and the seeks the engine cannot
// make, answered within one budget, an Insert's Uri confined to the
// canonical URI of a track the bridge serves, on a host it advertises or
// binds (writes.go, inventory.go, PR 5a4); and GENA's refusal, 501.
package openhome
