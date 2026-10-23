package openhome

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/handshakelog"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// Limits are the surface's bounds. A field's zero value is DefaultLimits'.
type Limits struct {
	// MaxConns and MaxConnsPerSource cap the connections open at once, all
	// of them and from one source, counted once a connection is admitted.
	MaxConns, MaxConnsPerSource int
	// Slots are the requests answered at once.
	Slots int
	// ReadHeader is the header's read deadline; BodyRead a control body's,
	// set once its headers have passed; Read the whole request's; Write the
	// response's, from when its headers were read; Idle a kept-alive
	// connection's between requests.
	ReadHeader, BodyRead, Read, Write, Idle time.Duration
	// ShutdownGrace is how long Stop lets the requests under way finish
	// before it closes their connections; Join how long it then waits for
	// the goroutines it started.
	ShutdownGrace, Join time.Duration
	// Rescan is how often the surface lists the interfaces again.
	Rescan time.Duration
}

// DefaultLimits are production's. 1-bit opens a handful of connections to
// a player (URLSession's six per host) and polls about once a second.
var DefaultLimits = Limits{
	MaxConns: 32, MaxConnsPerSource: 8, Slots: 8,
	ReadHeader: 5 * time.Second, BodyRead: 5 * time.Second, Read: 10 * time.Second,
	Write: 10 * time.Second, Idle: 30 * time.Second,
	ShutdownGrace: 2 * time.Second, Join: time.Second,
	Rescan: 5 * time.Second,
}

// maxHeaderRead is the most of a request's headers the surface reads;
// net/http reads MaxHeaderBytes and 4 KiB of lookahead (initialReadLimitSize,
// Go 1.27.2), as the DLNA listener's bound does.
const maxHeaderRead = 16 << 10

// StopBound is the longest Stop takes with these limits (their defaults
// where unset): the listing loop's join, the grace, then the join of the
// goroutines it started.
func (l Limits) StopBound() time.Duration {
	l = l.orDefaults()
	return l.Join + l.ShutdownGrace + l.Join
}

func (l Limits) orDefaults() Limits {
	d := DefaultLimits
	pick := func(v, def int) int {
		if v > 0 {
			return v
		}
		return def
	}
	dur := func(v, def time.Duration) time.Duration {
		if v > 0 {
			return v
		}
		return def
	}
	return Limits{
		MaxConns: pick(l.MaxConns, d.MaxConns), MaxConnsPerSource: pick(l.MaxConnsPerSource, d.MaxConnsPerSource),
		Slots:      pick(l.Slots, d.Slots),
		ReadHeader: dur(l.ReadHeader, d.ReadHeader), BodyRead: dur(l.BodyRead, d.BodyRead), Read: dur(l.Read, d.Read),
		Write: dur(l.Write, d.Write), Idle: dur(l.Idle, d.Idle),
		ShutdownGrace: dur(l.ShutdownGrace, d.ShutdownGrace), Join: dur(l.Join, d.Join),
		Rescan: dur(l.Rescan, d.Rescan),
	}
}

// ListenFunc binds the surface's socket for one served address: at is the
// address and the configured port, a the address with its prefix and
// interface. It returns the listener and what restricts it to its
// interface (Listen's words), and is cancelled by ctx.
type ListenFunc func(ctx context.Context, at netip.AddrPort, a lan.Address) (net.Listener, string, error)

// Seams are a test's holds on the surface; nil in production.
type Seams struct {
	// Handling runs once a request holds its slot, before it is answered.
	Handling func(*http.Request)
	// Stopping runs once Stop refuses requests, before it shuts the
	// listeners down.
	Stopping func()
	// Forcing runs once Shutdown's grace has run out, before Stop closes
	// the connections still open.
	Forcing func()
}

// Config is the surface's.
type Config struct {
	Device Device
	// Port is output.player.openhomePort: 0 for a port the kernel picks for
	// each address.
	Port int
	// Addresses lists what the surface serves and the box's own addresses;
	// nil is the host's (lan.Look over lan.HostInterfaces). A test passes a
	// box of its own (lan/lantest), so no test reads the host's interfaces.
	Addresses func() (lan.Inventory, error)
	// Listen binds one address; nil is Listen, the platform's listener
	// restricted to its interface. A test passes loopback listeners.
	Listen ListenFunc
	Limits Limits
	// Log is where the surface logs; nil is the player component's.
	Log   *slog.Logger
	Seams *Seams
}

// Status is the surface at one look.
type Status struct {
	// Listening are the sockets bound, in the order they were bound.
	Listening []Listening
	// Failed are the served addresses that could not be bound, each with its
	// error, tried again at the next listing.
	Failed []BindFailure
	// Open counts the connections admitted and open; BySource by source.
	Open     int
	BySource map[netip.Addr]int
	// Refused counts the connections refused at accept, by why: an
	// admission refusal or a cap.
	Refused map[string]int
	// Reconciles counts the listings the surface followed; ListFailure is
	// the last listing's error, "" when it read.
	Reconciles  int
	ListFailure string
	Stopped     bool
}

// Listening is one socket: the served address it is for, the address and
// port it is bound on, and what restricts it to its interface.
type Listening struct {
	Address     lan.Address
	Bound       netip.AddrPort
	Restriction string
}

// BindFailure is a served address that could not be bound.
type BindFailure struct {
	Address lan.Address
	Err     string
}

// StopReport is what Stop did.
type StopReport struct {
	// Forced is a grace that ran out: the connections still open were
	// closed under their requests.
	Forced bool
	// Abandoned counts what was still running past the join: handlers, and
	// the listing loop when it did not return.
	Abandoned int
	Took      time.Duration
}

// Surface is the player's LAN surface (B470 PR 5a2a): a socket of its own
// on each address internal/player/lan serves, followed as they come and go,
// each connection admitted at accept, every request bounded, and a stop
// that refuses, shuts down within a bound, then closes. It advertises
// nothing.
type Surface struct {
	cfg  Config
	lim  Limits
	h    *handler
	gate *gate
	log  *slog.Logger

	ctx      context.Context
	cancel   context.CancelFunc
	poke     chan struct{}
	loopDone chan struct{}
	serving  sync.WaitGroup

	stopOnce sync.Once
	report   StopReport

	mu         sync.Mutex
	sockets    []*socket
	failed     map[lan.Address]string
	reconciles int
	listErr    string
	noAddress  bool
	stopped    bool
	changed    chan struct{}
}

// socket is one bound address.
type socket struct {
	key   lan.Address
	srv   *http.Server
	ln    net.Listener
	bound netip.AddrPort
	restr string
}

// close closes the socket's listener and every connection its server
// holds. The listener is closed here as well as by srv.Close, which closes
// only the listeners Serve has begun to track: one whose Serve goroutine
// has not run yet would stay open until it did.
func (k *socket) close() {
	_ = k.ln.Close()
	_ = k.srv.Close()
}

// Start starts the surface: its loop binds every served address at once,
// then follows the listing every Limits.Rescan. It returns at once; a bind
// that fails fails that address alone, and is said once and tried again.
func Start(cfg Config) (*Surface, error) {
	if cfg.Device.UUID == "" {
		return nil, errors.New("openhome: the device has no UUID")
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return nil, errors.New("openhome: the port is not 0 to 65535")
	}
	if cfg.Addresses == nil {
		cfg.Addresses = func() (lan.Inventory, error) { return lan.Look(lan.HostInterfaces) }
	}
	if cfg.Listen == nil {
		cfg.Listen = Listen
	}
	log := cfg.Log
	if log == nil {
		log = logging.Component("player")
	}
	lim := cfg.Limits.orDefaults()
	s := &Surface{cfg: cfg, lim: lim, log: log, h: newHandler(cfg.Device, lim, cfg.Seams),
		poke: make(chan struct{}, 1), loopDone: make(chan struct{}), failed: map[lan.Address]string{},
		changed: make(chan struct{})}
	s.gate = newGate(lim, log, s.publish)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go s.loop()
	return s, nil
}

// Rescan has the surface list the interfaces again now.
func (s *Surface) Rescan() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Changed is closed at the surface's next change of Status.
func (s *Surface) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// Status is the surface now.
func (s *Surface) Status() Status {
	s.mu.Lock()
	st := Status{Reconciles: s.reconciles, ListFailure: s.listErr, Stopped: s.stopped}
	for _, k := range s.sockets {
		st.Listening = append(st.Listening, Listening{Address: k.key, Bound: k.bound, Restriction: k.restr})
	}
	for a, err := range s.failed {
		st.Failed = append(st.Failed, BindFailure{Address: a, Err: err})
	}
	s.mu.Unlock()
	slices.SortFunc(st.Failed, func(a, b BindFailure) int { return a.Address.Prefix.Addr().Compare(b.Address.Prefix.Addr()) })
	st.Open, st.BySource, st.Refused = s.gate.counts()
	return st
}

// publish wakes whoever waits on Changed.
func (s *Surface) publish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.changed)
	s.changed = make(chan struct{})
}

// loop follows the listing until Stop.
func (s *Surface) loop() {
	defer close(s.loopDone)
	t := time.NewTicker(s.lim.Rescan)
	defer t.Stop()
	for {
		s.reconcile()
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.poke:
		}
	}
}

// reconcile lists the interfaces once and follows the listing: the
// admission snapshot first, then each socket whose address went away,
// changed its prefix or interface, or stopped being eligible closed, then
// each new address bound. A listing that fails keeps everything as it was.
func (s *Surface) reconcile() {
	inv, err := s.cfg.Addresses()
	if err != nil {
		s.mu.Lock()
		first := s.listErr == ""
		s.listErr = err.Error()
		s.reconciles++
		s.mu.Unlock()
		if first {
			s.log.Warn("player: the OpenHome surface could not list the interfaces; it keeps the addresses it serves and lists them again", "err", err)
		}
		s.publish()
		return
	}
	want := servedOf(inv)
	s.gate.set(want, inv.Own)
	s.mu.Lock()
	s.listErr = ""
	var retire []*socket
	kept := s.sockets[:0]
	for _, k := range s.sockets {
		if slices.Contains(want, k.key) {
			kept = append(kept, k)
		} else {
			retire = append(retire, k)
		}
	}
	s.sockets = kept
	for a := range s.failed {
		if !slices.Contains(want, a) {
			delete(s.failed, a)
		}
	}
	var bind []lan.Address
	for _, a := range want {
		if !slices.ContainsFunc(s.sockets, func(k *socket) bool { return k.key == a }) {
			bind = append(bind, a)
		}
	}
	none := len(want) == 0
	wasNone := s.noAddress
	s.noAddress = none
	s.mu.Unlock()
	for _, k := range retire {
		k.close()
		s.log.Info("player: the OpenHome surface closed an address that went away, changed or is no longer served",
			"address", k.bound.String(), "interface", k.key.Interface)
	}
	for _, a := range bind {
		s.bind(a)
	}
	if none && !wasNone {
		s.log.Info("player: the OpenHome surface has no LAN address to listen on yet (lan_unavailable); it listens once one comes")
	}
	s.mu.Lock()
	s.reconciles++
	s.mu.Unlock()
	s.publish()
}

// servedOf is inv's served IPv4 addresses, each once, in order.
func servedOf(inv lan.Inventory) []lan.Address {
	var out []lan.Address
	for _, a := range inv.Served {
		if !a.Prefix.IsValid() || !a.Prefix.Addr().Is4() || slices.Contains(out, a) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// bind binds the socket for a and serves it, or records why it could not.
func (s *Surface) bind(a lan.Address) {
	at := netip.AddrPortFrom(a.Prefix.Addr(), uint16(s.cfg.Port))
	ln, restr, err := s.cfg.Listen(s.ctx, at, a)
	if err != nil {
		s.mu.Lock()
		was, seen := s.failed[a]
		s.failed[a] = err.Error()
		s.mu.Unlock()
		if !seen || was != err.Error() {
			s.log.Warn("player: the OpenHome surface could not listen on an address: the surface fails there alone, the player, the library and the rest of serve are unaffected, and it tries again at the next listing",
				"address", at.String(), "interface", a.Interface, "err", err)
		}
		return
	}
	bound := at
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		ap := tcp.AddrPort()
		bound = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	srv := &http.Server{
		Handler:           s.h,
		ReadHeaderTimeout: s.lim.ReadHeader,
		ReadTimeout:       s.lim.Read,
		WriteTimeout:      s.lim.Write,
		IdleTimeout:       s.lim.Idle,
		MaxHeaderBytes:    maxHeaderRead - 4096,
		ErrorLog:          handshakelog.ErrorLog(),
	}
	admit := &admitting{Listener: ln, key: a, gate: s.gate}
	k := &socket{key: a, srv: srv, ln: admit, bound: bound, restr: restr}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		_ = ln.Close()
		return
	}
	delete(s.failed, a)
	s.sockets = append(s.sockets, k)
	s.serving.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.serving.Done()
		_ = srv.Serve(admit)
	}()
	s.log.Info("player: the OpenHome surface listens on an address (not advertised yet)",
		"address", bound.String(), "interface", a.Interface, "restricted", restr)
}

// Stop stops the surface, once: every request after it is 503 and its
// connection closed; the listing loop is cancelled and joined; every
// socket is shut down within Limits.ShutdownGrace, so the requests under
// way finish; then the connections still open are closed; then the
// goroutines it started are joined within Limits.Join. Nothing it started
// outlives it but what the report counts abandoned.
func (s *Surface) Stop() StopReport {
	s.stopOnce.Do(func() { s.report = s.stop() })
	return s.report
}

func (s *Surface) stop() StopReport {
	began := time.Now()
	var rep StopReport
	s.h.stopping.Store(true)
	if s.cfg.Seams != nil && s.cfg.Seams.Stopping != nil {
		s.cfg.Seams.Stopping()
	}
	s.cancel()
	loopTimer := time.NewTimer(s.lim.Join)
	select {
	case <-s.loopDone:
	case <-loopTimer.C:
		rep.Abandoned++
		s.log.Warn("player: the OpenHome surface's listing did not return within its bound; it is abandoned, and closes what it binds")
	}
	loopTimer.Stop()
	s.mu.Lock()
	s.stopped = true
	sockets := slices.Clone(s.sockets)
	s.mu.Unlock()
	// No new connection from here: each listener closed (Shutdown closes
	// only those Serve has begun to track), then each server shut down.
	for _, k := range sockets {
		_ = k.ln.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.lim.ShutdownGrace)
	var wg sync.WaitGroup
	late := make([]bool, len(sockets))
	for i, k := range sockets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			late[i] = k.srv.Shutdown(ctx) != nil
		}()
	}
	wg.Wait()
	cancel()
	if slices.Contains(late, true) {
		rep.Forced = true
		if s.cfg.Seams != nil && s.cfg.Seams.Forcing != nil {
			s.cfg.Seams.Forcing()
		}
	}
	for i, k := range sockets {
		if late[i] {
			k.close()
		}
	}
	// One deadline for both joins: the Serve goroutines, which return once
	// their listeners close, and the handlers, which return once their
	// connections do.
	joined := make(chan struct{})
	go func() {
		s.serving.Wait()
		close(joined)
	}()
	join := time.NewTimer(s.lim.Join)
	defer join.Stop()
	expired := false
	select {
	case <-joined:
	case <-join.C:
		expired = true
		rep.Abandoned++
	}
	idle, running := s.h.flight.close()
	if running > 0 {
		if expired {
			rep.Abandoned += running
		} else {
			select {
			case <-idle:
			case <-join.C:
				rep.Abandoned += s.h.flight.running()
			}
		}
	}
	rep.Took = time.Since(began)
	if rep.Forced || rep.Abandoned > 0 {
		s.log.Warn("player: the OpenHome surface stopped, closing what had not finished within its grace",
			"forced", rep.Forced, "abandoned", rep.Abandoned, "took", rep.Took.Round(time.Millisecond))
	} else {
		s.log.Info("player: the OpenHome surface stopped", "took", rep.Took.Round(time.Millisecond))
	}
	s.publish()
	return rep
}

// gate is the surface's admission and its caps, shared by every socket.
type gate struct {
	lim     Limits
	log     *slog.Logger
	changed func()

	mu      sync.Mutex
	served  []lan.Address
	own     map[netip.Addr]struct{}
	open    int
	bySrc   map[netip.Addr]int
	refused map[string]int
	noted   map[string]struct{}
}

// The admission refusals and the caps, as Status counts them.
const (
	refusedDestination   = "destination_not_served"
	refusedNotIPv4       = "source_not_ipv4"
	refusedLoopback      = "source_loopback"
	refusedOwnAddress    = "source_own_address"
	refusedOutsidePrefix = "source_outside_prefix"
	refusedNotLAN        = "source_not_lan"
	refusedGlobalCap     = "over_connection_cap"
	refusedSourceCap     = "over_source_cap"
)

// maxNoted bounds the refusals logged, once each per kind and source.
const maxNoted = 32

func newGate(lim Limits, log *slog.Logger, changed func()) *gate {
	return &gate{lim: lim, log: log, changed: changed, own: map[netip.Addr]struct{}{}, bySrc: map[netip.Addr]int{},
		refused: map[string]int{}, noted: map[string]struct{}{}}
}

// set is a listing's admission snapshot: what is served, and the box's own
// addresses.
func (g *gate) set(served []lan.Address, own []netip.Addr) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.served = slices.Clone(served)
	g.own = make(map[netip.Addr]struct{}, len(own))
	for _, a := range own {
		g.own[a.Unmap().WithZone("")] = struct{}{}
	}
}

// admit is admission at accept, then the caps: the connection c arrived on
// the socket for key. It returns the source and nil when c is admitted and
// counted, or why not, having counted nothing.
//
// Admitted is a connection whose local address is key's, while key is
// served, from an approved source: an IPv4 address inside the on-link
// prefix of the address it arrived on (not another address's or
// interface's), of the LAN's classes (lan.IsLANAddress, so a prefix wider
// than its address's class admits no public address), never loopback and
// never one of the box's own addresses. Then neither the connections open
// nor the source's may be at their caps.
func (g *gate) admit(key lan.Address, c net.Conn) (netip.Addr, string) {
	local, lok := tcpAddr(c.LocalAddr())
	src, sok := tcpAddr(c.RemoteAddr())
	why, log := g.judge(key, local, lok, src, sok)
	if log {
		g.log.Warn("player: the OpenHome surface refused a connection at accept",
			"why", why, "address", local.String(), "from", src.String())
	}
	return src, why
}

// judge is admit's decision, under g.mu: why the connection is refused, ""
// when it is admitted and counted, and whether its refusal is one to log.
func (g *gate) judge(key lan.Address, local netip.Addr, lok bool, src netip.Addr, sok bool) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	why := ""
	switch {
	case !lok || local != key.Prefix.Addr() || !slices.Contains(g.served, key):
		why = refusedDestination
	case !sok || !src.Is4():
		why = refusedNotIPv4
	case src.IsLoopback():
		why = refusedLoopback
	case g.isOwn(src):
		why = refusedOwnAddress
	case !key.Prefix.Masked().Contains(src):
		why = refusedOutsidePrefix
	case !lan.IsLANAddress(src):
		why = refusedNotLAN
	case g.open >= g.lim.MaxConns:
		why = refusedGlobalCap
	case g.bySrc[src] >= g.lim.MaxConnsPerSource:
		why = refusedSourceCap
	}
	if why != "" {
		g.refused[why]++
		return why, g.firstOf(why, src)
	}
	g.open++
	g.bySrc[src]++
	return "", false
}

// isOwn reports whether a is one of the box's addresses; g.mu is held.
func (g *gate) isOwn(a netip.Addr) bool {
	_, ok := g.own[a]
	return ok
}

// release counts a connection from src closed.
func (g *gate) release(src netip.Addr) {
	g.mu.Lock()
	g.open--
	if g.bySrc[src]--; g.bySrc[src] <= 0 {
		delete(g.bySrc, src)
	}
	g.mu.Unlock()
	g.changed()
}

// firstOf reports whether a refusal of this kind from src is one to log:
// once per kind and source, at most maxNoted of them, since a peer chooses
// its source; g.mu is held.
func (g *gate) firstOf(why string, src netip.Addr) bool {
	key := why + "|" + src.String()
	if _, seen := g.noted[key]; seen || len(g.noted) >= maxNoted {
		return false
	}
	g.noted[key] = struct{}{}
	return true
}

// counts are the connections open, by source, and the refusals.
func (g *gate) counts() (int, map[netip.Addr]int, map[string]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open, maps(g.bySrc), maps(g.refused)
}

func maps[K comparable](m map[K]int) map[K]int {
	out := make(map[K]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// tcpAddr is a's IP, unmapped, when a is a TCP address.
func tcpAddr(a net.Addr) (netip.Addr, bool) {
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	ip := tcp.AddrPort().Addr().Unmap()
	return ip, ip.IsValid()
}

// admitting is a socket's listener as net/http sees it: Accept hands over
// only a connection the gate admits, counted until it closes, and closes
// every other at once, unread, counted against nothing.
type admitting struct {
	net.Listener
	key  lan.Address
	gate *gate
}

// Accept returns the next connection the gate admits.
func (l *admitting) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		src, why := l.gate.admit(l.key, c)
		if why != "" {
			if lc, ok := c.(interface{ SetLinger(int) error }); ok {
				_ = lc.SetLinger(0)
			}
			_ = c.Close()
			l.gate.changed()
			continue
		}
		l.gate.changed()
		return &counted{Conn: c, gate: l.gate, src: src}, nil
	}
}

// counted is an admitted connection, released from the gate's counts once,
// when it closes.
type counted struct {
	net.Conn
	gate *gate
	src  netip.Addr
	once sync.Once
}

// Close closes the connection and releases it from the counts, once.
func (c *counted) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.gate.release(c.src) })
	return err
}

// CloseWrite half-closes the connection where it can, so net/http sends
// what it answered before it closes a connection it refused.
func (c *counted) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Listen binds the surface's socket at at, IPv4 alone, restricted to a's
// interface where an unprivileged process can (restrictToInterface), and
// says what restricts it: "IP_BOUND_IF" (macOS), "SO_BINDTOIFINDEX" (Linux,
// where the kernel lets an unprivileged process set it), or "none: " and
// why. A restriction the kernel refuses leaves the socket unrestricted,
// never unbound: admission refuses a source outside the prefix either way.
func Listen(ctx context.Context, at netip.AddrPort, a lan.Address) (net.Listener, string, error) {
	restr := ""
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		restr = restrictToInterface(c, a.Index)
		return nil
	}}
	ln, err := lc.Listen(ctx, "tcp4", at.String())
	if err != nil {
		return nil, "", err
	}
	return ln, restr, nil
}
