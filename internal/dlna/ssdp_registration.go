package dlna

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// SSDPInterface is an interface as the SSDP subsystem tells interfaces
// apart: its index and name, and the caller's incarnation token. Two
// endpoints with equal SSDPInterfaces share a listener and its membership.
// The zero value is the OS default interface, which only the MediaServer's
// single-endpoint fallback uses.
type SSDPInterface struct {
	Index       int
	Name        string
	Incarnation uint64
}

// SSDPListenerCaps is what a listener's setup obtained: whether it reports
// each datagram's destination, whether it reports the interface each one
// arrived on, and whether it hears only the group's datagrams that arrive on
// its own interface. A capability that is false failed when the listener
// opened; the MediaServer answers without it as it always has, and a strict
// registration refuses an endpoint on such a listener.
type SSDPListenerCaps struct {
	DestinationReporting bool
	ArrivalReporting     bool
	ReceiveConfined      bool
}

// SSDPEndpoint is one place a registration is advertised: an interface and
// the one complete LOCATION announced there. It is never an instruction to
// enumerate the interface's addresses: a caller that wants two LOCATIONs on
// one interface gives two endpoints.
type SSDPEndpoint struct {
	// Interface is where the endpoint joins, announces and is answered.
	// nil is the OS default interface, for the MediaServer alone; a strict
	// registration's endpoint names one.
	Interface *net.Interface

	// Incarnation tells one life of an interface from the next, for a
	// caller that follows the host's interfaces (PR 5a2): an interface
	// removed and created again may come back under the same index and
	// name, and its old memberships are gone. Endpoints that share an
	// interface give the same token; the caller changes it when the
	// interface is recreated. The MediaServer's is 0, always.
	Incarnation uint64

	// Location is the absolute URL of the device description, announced
	// as LOCATION, as reachable from Interface's network.
	Location string

	// Local is this host's IPv4 address on Interface, with its prefix,
	// when the caller knows it. Nothing infers it, from Location's host or
	// otherwise. A strict registration answers a unicast search only on an
	// endpoint whose Local address the search was sent to, and its source
	// hook may judge a searcher by Local's prefix.
	Local netip.Prefix
}

// SSDPEndpointSnapshot is an endpoint as a result or a source hook sees it,
// every field a value.
type SSDPEndpointSnapshot struct {
	Interface SSDPInterface
	Location  string
	Local     netip.Prefix
}

// SSDPDatagram is what one received datagram carried beside its payload, as
// the kernel reported it, and the listener that read it. Every field is a
// value. A field the kernel did not report is not valid, and nothing stands
// in for it: Listener names the socket that read the datagram, never the
// interface it arrived on (a wildcard listener takes a unicast to any
// address, and one that could not be confined takes other interfaces' group
// datagrams).
type SSDPDatagram struct {
	Source           netip.AddrPort
	Destination      netip.Addr
	DestinationValid bool
	ArrivalIndex     int
	ArrivalValid     bool
	Listener         SSDPInterface
	ListenerCaps     SSDPListenerCaps
}

// SSDPSearch is what a strict registration's source hook judges: the
// endpoint the search would be answered from, and the datagram as received.
type SSDPSearch struct {
	Endpoint SSDPEndpointSnapshot
	Datagram SSDPDatagram
}

// SSDPAdmission is how a registration decides which searches it answers.
// SSDPStrictAdmission is the one a caller outside this package builds; the
// MediaServer's own admits by destination through the server's served
// addresses (ssdpServedAdmission).
type SSDPAdmission interface{ ssdpAdmission() }

// SSDPStrictAdmission answers only what can be proved: a search whose
// listener reports its destination and its arrival interface, and, for a
// group search, hears only its own interface; whose datagram carries both;
// that arrived on the interface of the endpoint it would be answered from;
// and, unicast, was sent to that endpoint's Local address. Then Source
// judges the searcher. Every endpoint of a strict registration needs those
// capabilities and a pinned sender to start at all, and a strict
// registration never takes the MediaServer's unarmed fallback.
type SSDPStrictAdmission struct {
	// Source reports whether a search from s.Datagram.Source may be
	// answered from s.Endpoint (PR 5a2: an on-link rule over the endpoint's
	// Local prefix). It runs on a listener's read loop, so it must be pure
	// and must not block. Required.
	Source func(s SSDPSearch) bool
}

func (SSDPStrictAdmission) ssdpAdmission() {}

// ssdpServedAdmission is the MediaServer's admission, unchanged by B470 PR
// 5a1: a search sent to the SSDP group is admitted; any other destination is
// admitted when served serves it (backlog B48), the very servedAddrs its
// HTTP listener judges by; a search whose destination was asked for and not
// reported is refused; and on a listener that could not be asked, every
// search is admitted, as before B48. The source is never judged (it is
// logged), and the arrival interface is not asked, but for one refusal no
// MediaServer alone can meet: a group search that arrived on an interface
// only another registration joined (legacySelect).
type ssdpServedAdmission struct {
	served *servedAddrs
}

func (ssdpServedAdmission) ssdpAdmission() {}

// SSDPRegistration is one device the subsystem advertises.
//
// Which endpoint answers a search: one matched target set per admitted
// registration per datagram, never one per address. A search sent to the
// group is dispatched to the registrations with an endpoint on the listener
// that read it; a unicast whose destination is reported, to every
// registration, whichever listener read it. The MediaServer answers a group
// search from its endpoint on that listener (even one delivered there by a
// listener that could not be confined: its existing fallback), and a
// unicast from its endpoint on that listener, or from its first endpoint
// when the listener is another device's; never by the destination, so with
// the MediaServer alone it answers exactly as before (backlog B119). A strict
// registration answers as SSDPStrictAdmission says.
type SSDPRegistration struct {
	// Name says what the registration is in its log lines: "DLNA" for the
	// MediaServer ("DLNA advertising is degraded").
	Name string

	// UDN, Targets and ServerToken make up every packet: Targets in the
	// order each burst sends them (NotifyTargetsFor for the MediaServer).
	UDN         string
	Targets     []NotifyTarget
	ServerToken string

	// Endpoints are tried in order, and announced in that order.
	Endpoints []SSDPEndpoint

	// Admission is required: SSDPStrictAdmission from outside the package.
	Admission SSDPAdmission

	// ResponderSlots is how many answers each endpoint may have in flight
	// (one search's burst holds one across its MX delay and its writes); 0
	// is maxConcurrentMSearchResponders.
	ResponderSlots int

	// AdvertiseInterval is how often each endpoint repeats its alive
	// burst; 0, or less than a minute, is defaultAdvertiseInterval (14
	// minutes, under SSDPMaxAge/2).
	AdvertiseInterval time.Duration

	// Logger is the registration's logger; nil is the subsystem's.
	Logger *slog.Logger
}

// SSDPEndpointResult is what became of one endpoint a Register or an
// Update was given.
type SSDPEndpointResult struct {
	// Position is the endpoint's index in the slice given.
	Position int
	Endpoint SSDPEndpointSnapshot
	// Listener is the capabilities of the endpoint's listener, when one
	// was opened or shared.
	Listener SSDPListenerCaps
	// SenderPinned is false for an endpoint whose sender could not be
	// pinned to its interface (and for the OS default, which is not
	// pinned).
	SenderPinned bool
	// Err is why the endpoint is not active; nil for an active one.
	Err error
	// Problems names each capability its setup could not obtain, active or
	// not: the MediaServer runs without them, a strict endpoint is refused.
	Problems []error
}

// SSDPResult is what a Register or an Update made of its endpoints, each
// list in the order given.
type SSDPResult struct {
	Active []SSDPEndpointResult
	Failed []SSDPEndpointResult
}

// SSDPHandle is one live registration: Update replaces its endpoints and
// Withdraw ends it. Its methods serialize with each other.
type SSDPHandle struct {
	s           *SSDPSubsystem
	name        string
	udn         string
	targets     []NotifyTarget
	serverToken string
	admission   SSDPAdmission
	slots       int
	interval    time.Duration
	log         *slog.Logger

	// ctx bounds the registration's work, under the subsystem's context
	// and the one Register was given; cancel ends it.
	ctx    context.Context
	cancel context.CancelFunc

	// opMu serializes Register's commit, Update and Withdraw.
	opMu      sync.Mutex
	withdrawn bool // under opMu

	// endpoints is the active endpoints, in the caller's order: written
	// under opMu, and read under it by Update and Withdraw.
	endpoints []*ssdpEndpoint

	// active is the list the dispatch table holds, written and read under
	// the subsystem's mu.
	active []*ssdpEndpoint

	// gate guards each endpoint's closing flag and the Add of its work: a
	// worker joins only while its endpoint is open.
	gate sync.Mutex
}

// ssdpEndpoint is one active endpoint.
type ssdpEndpoint struct {
	h        *SSDPHandle
	spec     SSDPEndpoint // Interface is a copy
	key      ssdpListenerKey
	listener *ssdpListener
	sender   ssdpSendConn
	pinned   bool
	problems []error

	// pool holds one token per answer in flight from this endpoint.
	pool chan struct{}

	// ctx is the endpoint's: cancelled when it is removed or withdrawn,
	// which stops its timer, wakes its answers' delays and closes their
	// sockets. An endpoint is never revived, so ctx is its generation.
	ctx    context.Context
	cancel context.CancelFunc

	// closing is set under the handle's gate when the endpoint starts to
	// go; work counts its answers, added only while !closing.
	closing bool
	work    sync.WaitGroup

	// notifyErrs is the alive bursts' failure log (discovery.SendFailureLog):
	// noted by the initial burst before the timer starts, then by the timer
	// goroutine alone.
	notifyErrs discovery.SendFailureLog
	kick       chan struct{} // asks the timer for one burst now
	aliveDone  chan struct{} // closed when the timer goroutine returns; nil before it starts
}

// snapshot is the endpoint as SSDPEndpointSnapshot.
func (ep *ssdpEndpoint) snapshot() SSDPEndpointSnapshot {
	return SSDPEndpointSnapshot{Interface: ep.key.public(), Location: ep.spec.Location, Local: ep.spec.Local}
}

// ssdpEndpointKeyOf is the listener identity of e.
func ssdpEndpointKeyOf(e SSDPEndpoint) ssdpListenerKey {
	if e.Interface == nil {
		return ssdpListenerKey{incarnation: e.Incarnation}
	}
	return ssdpListenerKey{index: e.Interface.Index, name: e.Interface.Name, incarnation: e.Incarnation}
}

// sameEndpoint reports whether two endpoints are the same for an Update: the
// interface's index and name, the incarnation, the local address and the
// LOCATION, never a pointer or a host.
func sameEndpoint(a, b SSDPEndpoint) bool {
	return ssdpEndpointKeyOf(a) == ssdpEndpointKeyOf(b) && a.Local == b.Local && a.Location == b.Location
}

// strict reports whether the handle's admission is strict.
func (h *SSDPHandle) strict() bool {
	_, ok := h.admission.(SSDPStrictAdmission)
	return ok
}

// advertiseIntervalOf is the cadence a registration that asked for d gets
// under a floor: d, or defaultAdvertiseInterval for 0 or anything under the
// floor (a minute in production).
func advertiseIntervalOf(d, floor time.Duration) time.Duration {
	if d <= 0 || d < floor {
		return defaultAdvertiseInterval
	}
	return d
}

// Register starts a registration: it opens each endpoint's listener (or
// shares the one its interface already has) and sender, in order, sends
// each active endpoint's initial alive burst, and then answers on them. Call
// it once the LOCATIONs' HTTP listener is up.
//
// ctx bounds the registration's work, as Start's context bounded an
// advertiser's: once it is done the registration stops announcing and
// answering, and holds its sockets until Withdraw. A Register that started
// at least one endpoint succeeds, with the rest in the result's Failed; one
// that started none returns ErrSSDPNoEndpoint, holding nothing. A MediaServer
// endpoint whose sender cannot be pinned starts anyway, sending on the OS
// default interface; a strict one does not start, and sends nothing.
func (s *SSDPSubsystem) Register(ctx context.Context, reg SSDPRegistration) (*SSDPHandle, SSDPResult, error) {
	if err := checkRegistration(reg); err != nil {
		return nil, SSDPResult{}, err
	}
	done, err := s.enter()
	if err != nil {
		return nil, SSDPResult{}, err
	}
	defer done()

	h := &SSDPHandle{
		s:           s,
		name:        reg.Name,
		udn:         reg.UDN,
		targets:     slices.Clone(reg.Targets),
		serverToken: reg.ServerToken,
		admission:   reg.Admission,
		slots:       reg.ResponderSlots,
		interval:    advertiseIntervalOf(reg.AdvertiseInterval, s.intervalFloor),
		log:         reg.Logger,
	}
	if h.name == "" {
		h.name = "SSDP"
	}
	if h.slots <= 0 {
		h.slots = maxConcurrentMSearchResponders
	}
	if h.log == nil {
		h.log = s.log
	}
	h.ctx, h.cancel = mergeContexts(ctx, s.ctx)

	h.opMu.Lock()
	defer h.opMu.Unlock()
	res, staged := h.stage(reg.Endpoints)
	if len(staged) == 0 {
		h.cancel()
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return nil, res, ErrSSDPClosed
		}
		return nil, res, ErrSSDPNoEndpoint
	}
	for _, ep := range staged {
		h.announceFirst(ep)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		// Close began while this ran: what was announced is withdrawn
		// rather than left registered, and Close is waiting for it.
		h.withdrawn = true
		err := h.retire(withdrawDeadline(context.Background()), staged)
		h.cancel()
		for _, r := range res.Active {
			r.Err = ErrSSDPClosed
			res.Failed = append(res.Failed, r)
		}
		slices.SortFunc(res.Failed, func(a, b SSDPEndpointResult) int { return a.Position - b.Position })
		return nil, SSDPResult{Failed: res.Failed}, errors.Join(ErrSSDPClosed, err)
	}
	h.endpoints = staged
	h.active = staged
	s.handles = append(s.handles, h)
	s.publishLocked()
	s.mu.Unlock()
	for _, ep := range staged {
		h.startTimer(ep)
	}
	return h, res, nil
}

// checkRegistration refuses a registration nothing could advertise.
func checkRegistration(reg SSDPRegistration) error {
	switch a := reg.Admission.(type) {
	case nil:
		return errors.New("dlna: an SSDP registration needs an admission")
	case SSDPStrictAdmission:
		if a.Source == nil {
			return errors.New("dlna: a strict SSDP registration needs a source hook")
		}
	case ssdpServedAdmission:
		if a.served == nil {
			return errors.New("dlna: the MediaServer's SSDP admission needs its served addresses")
		}
	}
	if reg.UDN == "" || len(reg.Targets) == 0 {
		return errors.New("dlna: an SSDP registration needs a UDN and its targets")
	}
	return nil
}

// mergeContexts returns a context done when either parent is, and its
// cancel.
func mergeContexts(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	stop := context.AfterFunc(b, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// stage opens each endpoint, in order, and returns the ones that opened,
// neither announced nor published. A failed one holds nothing.
func (h *SSDPHandle) stage(eps []SSDPEndpoint) (SSDPResult, []*ssdpEndpoint) {
	var res SSDPResult
	var staged []*ssdpEndpoint
	for i, e := range eps {
		ep, r := h.open(i, e)
		if r.Err != nil {
			res.Failed = append(res.Failed, r)
			continue
		}
		res.Active = append(res.Active, r)
		staged = append(staged, ep)
	}
	return res, staged
}

// open opens one endpoint: its listener (shared by identity), its sender and
// the sender's pin, checking what a strict registration needs at each step.
func (h *SSDPHandle) open(pos int, e SSDPEndpoint) (*ssdpEndpoint, SSDPEndpointResult) {
	if e.Interface != nil {
		iface := *e.Interface
		e.Interface = &iface
	}
	key := ssdpEndpointKeyOf(e)
	r := SSDPEndpointResult{Position: pos, Endpoint: SSDPEndpointSnapshot{Interface: key.public(), Location: e.Location, Local: e.Local}}
	if err := h.checkEndpoint(e); err != nil {
		r.Err = err
		return nil, r
	}
	l, err := h.s.acquireListener(key, e.Interface)
	if err != nil {
		r.Err = err
		return nil, r
	}
	r.Listener = l.setup.caps
	for _, p := range []error{l.setup.dstErr, l.setup.arrivalErr, l.setup.confineErr} {
		if p != nil {
			r.Problems = append(r.Problems, p)
		}
	}
	if h.strict() && !(r.Listener.DestinationReporting && r.Listener.ArrivalReporting && r.Listener.ReceiveConfined) {
		h.s.releaseListener(l)
		r.Err = fmt.Errorf("%w: the listener on %s reports %+v", ErrSSDPStrictCapability, interfaceName(e.Interface), r.Listener)
		h.log.Warn("SSDP endpoint refused: its listener cannot prove where a search was sent and arrived",
			slog.String("registration", h.name), slog.String("interface", interfaceName(e.Interface)),
			slog.Any("problems", r.Problems))
		return nil, r
	}
	sender, pinErr, err := h.s.transport.openSender(e.Interface)
	if err != nil {
		h.s.releaseListener(l)
		r.Err = err
		return nil, r
	}
	r.SenderPinned = e.Interface != nil && pinErr == nil
	if pinErr != nil {
		r.Problems = append(r.Problems, pinErr)
		if h.strict() {
			_ = sender.Close()
			h.s.releaseListener(l)
			r.Err = fmt.Errorf("%w: its sender could not be pinned to %s: %w", ErrSSDPStrictCapability, interfaceName(e.Interface), pinErr)
			h.log.Warn("SSDP endpoint refused: its NOTIFY sender cannot be pinned to its interface",
				slog.String("registration", h.name), slog.String("interface", interfaceName(e.Interface)),
				slog.String("err", pinErr.Error()))
			return nil, r
		}
		// Soft-fail: log + continue. A failure here means multicast goes
		// via the OS default — degraded but not broken; renderers on that
		// interface still discover us. Refusing to start over an
		// unbindable multicast option would punish the typical happy path
		// (single-NIC host where the OS default IS the LAN).
		h.log.Warn("SSDP multicast interface bind failed (falling back to OS default)",
			slog.String("interface", e.Interface.Name),
			slog.String("err", pinErr.Error()))
	}
	ep := &ssdpEndpoint{
		h:        h,
		spec:     e,
		key:      key,
		listener: l,
		sender:   sender,
		pinned:   r.SenderPinned,
		problems: r.Problems,
		pool:     make(chan struct{}, h.slots),
		kick:     make(chan struct{}, 1),
	}
	ep.ctx, ep.cancel = context.WithCancel(h.ctx)
	return ep, r
}

// checkEndpoint refuses an endpoint the registration cannot use before any
// socket is opened.
func (h *SSDPHandle) checkEndpoint(e SSDPEndpoint) error {
	if e.Location == "" {
		return errors.New("dlna: an SSDP endpoint needs a LOCATION")
	}
	if e.Local.IsValid() && !e.Local.Addr().Unmap().Is4() {
		return errors.New("dlna: an SSDP endpoint's local address is IPv4: SSDP here is IPv4 alone")
	}
	if h.strict() && (e.Interface == nil || e.Interface.Index <= 0) {
		return fmt.Errorf("%w: it names no interface", ErrSSDPStrictCapability)
	}
	return nil
}

// announceFirst sends ep's initial alive burst, noted to a failure log of its
// own (each start is a fresh streak), and logs the endpoint's start.
func (h *SSDPHandle) announceFirst(ep *ssdpEndpoint) {
	ep.notifyErrs = discovery.NewSendFailureLog(h.log, "NOTIFY", interfaceName(ep.spec.Interface),
		h.name+" advertising", h.interval)
	h.noteAlive(ep, h.sendAliveAll(ep))
	h.log.Info("SSDP advertiser started",
		slog.String("location", ep.spec.Location),
		slog.String("interface", interfaceName(ep.spec.Interface)),
		slog.Duration("advertiseInterval", h.interval),
		slog.Int("targetCount", len(h.targets)),
	)
}

// startTimer starts ep's timer goroutine, which repeats its alive burst at
// the registration's interval, and at once when kicked, until ep's context
// ends.
func (h *SSDPHandle) startTimer(ep *ssdpEndpoint) {
	ep.aliveDone = make(chan struct{})
	go func() {
		defer close(ep.aliveDone)
		ticker := time.NewTicker(h.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ep.ctx.Done():
				return
			case <-ticker.C:
			case <-ep.kick:
			}
			h.noteAlive(ep, h.sendAliveAll(ep))
		}
	}()
}

// errSSDPSendCancelled is a burst its endpoint's cancellation cut short:
// the endpoint is going, which is no failure to report.
var errSSDPSendCancelled = errors.New("dlna: SSDP burst cancelled")

// noteAlive notes one alive burst's result to ep's failure log, unless its
// cancellation cut it short.
func (h *SSDPHandle) noteAlive(ep *ssdpEndpoint, err error) {
	if errors.Is(err, errSSDPSendCancelled) {
		return
	}
	ep.notifyErrs.Note(err)
}

// sendAliveAll sends one NOTIFY ssdp:alive per target, in order, and returns
// the burst's result: nil when every packet went out, else the first
// failure. Each failed write is also logged at Debug, naming its target; the
// burst's result is what reaches the default level, through notifyErrs. The
// burst is bounded (ssdpBurstDeadline from its first write), and checks its
// endpoint's context before each write, so nothing is sent once the
// endpoint has started to go.
//
// A write that meets net.ErrClosed ends the burst without a line, and is
// what the burst returns: a withdrawal closed the sender under a burst, and
// every target left would meet the same closed socket. Only a withdrawal
// closes this socket, so that error names the stop exactly, and
// discovery.SendFailureLog drops it.
func (h *SSDPHandle) sendAliveAll(ep *ssdpEndpoint) error {
	_ = ep.sender.SetWriteDeadline(time.Now().Add(ssdpBurstDeadline))
	var first error
	for _, target := range h.targets {
		if ep.ctx.Err() != nil {
			return errSSDPSendCancelled
		}
		pkt := BuildNotifyAlive(ep.spec.Location, h.serverToken, target)
		if _, err := ep.sender.WriteToUDP(pkt, h.s.notifyTo); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			h.log.Debug("NOTIFY alive send failed",
				slog.String("nt", target.NT),
				slog.String("err", err.Error()))
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// sendByebyeAll sends one NOTIFY ssdp:byebye per target, in order, by
// deadline at the latest (and ssdpBurstDeadline from its first write), and
// returns what failed, joined. It does not ask any context: a byebye goes
// out on a shutdown whose context is already done, and a withdrawal sends it
// only once nothing else of the endpoint's is sending.
func (h *SSDPHandle) sendByebyeAll(ep *ssdpEndpoint, deadline time.Time) error {
	if d := time.Now().Add(ssdpBurstDeadline); d.Before(deadline) {
		deadline = d
	}
	_ = ep.sender.SetWriteDeadline(deadline)
	var errs []error
	for _, target := range h.targets {
		pkt := BuildNotifyByeBye(ep.spec.Location, h.serverToken, target)
		if _, err := ep.sender.WriteToUDP(pkt, h.s.notifyTo); err != nil {
			errs = append(errs, err)
			var ne net.Error
			if errors.Is(err, net.ErrClosed) || (errors.As(err, &ne) && ne.Timeout()) {
				break // every target left would fail the same way
			}
		}
	}
	return errors.Join(errs...)
}

// Withdraw ends the registration: no search is dispatched to it from the
// moment it starts; its endpoints stop, their delayed answers are cancelled
// and their sockets closed, and every send of theirs in progress is waited
// for; only then does each endpoint send its byebye burst, in order, and
// release its sender and its share of its listener (a listener another
// registration uses stays open). No alive or answer is sent once the byebye
// begins, and nothing at all once Withdraw returns; a datagram already handed
// to the kernel cannot be recalled.
//
// Its budget is the earlier of ctx's deadline and five seconds from entry,
// shared by every endpoint; ctx's cancellation does not shorten it, so a
// byebye goes out on a shutdown. When the budget runs out the sends still in
// progress are cut (their sockets closed), no byebye is sent, and everything
// is still released and joined; Withdraw then returns an error that is
// context.DeadlineExceeded, joined with any byebye that failed. Neither
// undoes the withdrawal. A second Withdraw, or one of a nil handle, returns
// nil.
func (h *SSDPHandle) Withdraw(ctx context.Context) error {
	if h == nil {
		return nil
	}
	return h.withdraw(withdrawDeadline(ctx))
}

// withdraw is Withdraw by deadline, the budget a Close shares among its
// registrations.
func (h *SSDPHandle) withdraw(deadline time.Time) error {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	if h.withdrawn {
		return nil
	}
	h.withdrawn = true
	s := h.s
	s.mu.Lock()
	if i := slices.Index(s.handles, h); i >= 0 {
		s.handles = slices.Delete(s.handles, i, i+1)
	}
	h.active = nil
	s.publishLocked()
	s.mu.Unlock()
	err := h.retire(deadline, h.endpoints)
	h.endpoints = nil
	h.cancel()
	return err
}

// retire withdraws eps (§7 of B470 PR 5a1's brief), which the dispatch table
// no longer lists: closes each one's gate so no answer joins, cancels it,
// waits for its answers and its timer, then sends each one's byebye and
// releases its sender and listener, all by deadline. Past the deadline it
// cuts what is still sending and sends no byebye, and still joins and
// releases everything.
func (h *SSDPHandle) retire(deadline time.Time, eps []*ssdpEndpoint) error {
	if len(eps) == 0 {
		return nil
	}
	h.gate.Lock()
	for _, ep := range eps {
		ep.closing = true
	}
	h.gate.Unlock()
	for _, ep := range eps {
		ep.cancel()
	}
	var errs []error
	expired := !waitUntil(deadline, func() {
		for _, ep := range eps {
			ep.join()
		}
	})
	if expired {
		// The sends still in progress are cut: closing a socket fails a
		// write blocked on it at once, so the join that follows settles.
		for _, ep := range eps {
			_ = ep.sender.Close()
		}
		for _, ep := range eps {
			ep.join()
		}
		errs = append(errs, fmt.Errorf("dlna: SSDP withdrawal of %s: %w", h.udn, context.DeadlineExceeded))
	} else {
		for _, ep := range eps {
			if err := h.sendByebyeAll(ep, deadline); err != nil {
				errs = append(errs, err)
			}
		}
		if time.Now().After(deadline) {
			errs = append(errs, fmt.Errorf("dlna: SSDP withdrawal of %s: %w", h.udn, context.DeadlineExceeded))
		}
	}
	for _, ep := range eps {
		_ = ep.sender.Close()
		h.s.releaseListener(ep.listener)
		h.log.Info("SSDP advertiser stopped",
			slog.String("location", ep.spec.Location),
			slog.String("interface", interfaceName(ep.spec.Interface)))
	}
	return errors.Join(errs...)
}

// join waits for ep's answers and its timer goroutine.
func (ep *ssdpEndpoint) join() {
	ep.work.Wait()
	if ep.aliveDone != nil {
		<-ep.aliveDone
	}
}

// waitUntil runs wait and reports whether it returned by deadline. When it
// did not, wait is still running; the caller makes it return.
func waitUntil(deadline time.Time, wait func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// Update replaces the registration's endpoints with eps, the caller's whole
// new snapshot, in order (the subsystem never enumerates the host's
// interfaces itself; PR 5a2 calls this as the Source's addresses change).
// An endpoint equal to an active one (sameEndpoint: interface index and
// name, incarnation, local address, LOCATION) is kept as it is, with its
// sender, timer, responder slots and membership, answering throughout; a
// changed index, name or incarnation is a removal and an addition.
//
// The additions are opened first, without announcing. Then the removed
// endpoints are withdrawn as Withdraw withdraws (their byebye best-effort:
// an address already gone fails it, and the removal stands), the additions
// are announced and answered on, and, when anything was removed and an
// endpoint remains, every remaining endpoint repeats its alive burst at once:
// a byebye names the device, not one LOCATION, so a control point that heard
// it on the removed interface and also hears another would drop the device
// until the next repeat. A LOCATION changed on one interface is thereby a
// byebye of the old before the alive of the new. An addition that fails is
// reported in the result and not replaced by anything, an endpoint the
// caller removed is never kept, and an Update that leaves nothing active
// parks the registration with no socket, until a later Update.
func (h *SSDPHandle) Update(ctx context.Context, eps []SSDPEndpoint) (SSDPResult, error) {
	deadline := withdrawDeadline(ctx)
	s := h.s
	done, err := s.enter()
	if err != nil {
		return SSDPResult{}, err
	}
	defer done()
	h.opMu.Lock()
	defer h.opMu.Unlock()
	if h.withdrawn {
		return SSDPResult{}, ErrSSDPWithdrawn
	}

	// Each new endpoint keeps the first active one equal to it that no
	// earlier one kept; the rest are additions, and the active ones nobody
	// kept are removals.
	kept := make([]*ssdpEndpoint, len(eps))
	taken := make([]bool, len(h.endpoints))
	var adds []int
	for i, e := range eps {
		for j, ep := range h.endpoints {
			if !taken[j] && sameEndpoint(ep.spec, e) {
				taken[j], kept[i] = true, ep
				break
			}
		}
		if kept[i] == nil {
			adds = append(adds, i)
		}
	}
	var removed, keptInOrder []*ssdpEndpoint
	for j, ep := range h.endpoints {
		if !taken[j] {
			removed = append(removed, ep)
		}
	}
	for _, ep := range kept {
		if ep != nil {
			keptInOrder = append(keptInOrder, ep)
		}
	}

	// Stage the additions, then take the removals out of dispatch and
	// withdraw them, before anything new is announced.
	var res SSDPResult
	added := map[int]*ssdpEndpoint{}
	for _, i := range adds {
		ep, r := h.open(i, eps[i])
		if r.Err != nil {
			res.Failed = append(res.Failed, r)
			continue
		}
		added[i] = ep
	}
	var errs []error
	if len(removed) > 0 {
		s.mu.Lock()
		h.active = keptInOrder
		s.publishLocked()
		s.mu.Unlock()
		h.endpoints = keptInOrder
		if err := h.retire(deadline, removed); err != nil {
			errs = append(errs, err)
		}
	}

	var next, fresh []*ssdpEndpoint
	for i := range eps {
		switch {
		case kept[i] != nil:
			next = append(next, kept[i])
			res.Active = append(res.Active, kept[i].result(i))
		case added[i] != nil:
			next = append(next, added[i])
			fresh = append(fresh, added[i])
			res.Active = append(res.Active, added[i].result(i))
		}
	}
	slices.SortFunc(res.Failed, func(a, b SSDPEndpointResult) int { return a.Position - b.Position })
	for _, ep := range fresh {
		h.announceFirst(ep)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		// Close began while this ran; it withdraws what is published, so
		// what was opened here is withdrawn here.
		errs = append(errs, h.retire(withdrawDeadline(context.Background()), fresh))
		return res, errors.Join(append(errs, ErrSSDPClosed)...)
	}
	h.active = next
	s.publishLocked()
	s.mu.Unlock()
	h.endpoints = next
	for _, ep := range fresh {
		h.startTimer(ep)
	}
	if len(removed) > 0 {
		for _, ep := range keptInOrder {
			select {
			case ep.kick <- struct{}{}:
			default:
			}
		}
	}
	return res, errors.Join(errs...)
}

// result is the SSDPEndpointResult of an active endpoint at position pos.
func (ep *ssdpEndpoint) result(pos int) SSDPEndpointResult {
	return SSDPEndpointResult{
		Position:     pos,
		Endpoint:     ep.snapshot(),
		Listener:     ep.listener.setup.caps,
		SenderPinned: ep.pinned,
		Problems:     ep.problems,
	}
}
