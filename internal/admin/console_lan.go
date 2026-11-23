package admin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/consolelan"
	"github.com/acoseac/1-bit-bridge/internal/handshakelog"
)

// The console on the home network (backlog B989; ops/plan-b989-lan-console-
// design.md). A loopback install with adminLANAddress set serves the one
// console on a second socket, bound to that address at adminAddress's port
// and held by the kernel to the interface the address is on
// (internal/consolelan). Its policy is one reading of the box's interfaces;
// every consoleLANInterval the plan reads them again, and a reading that
// differs, or fails in any part, retires the socket: from the retirement's
// first step nothing more is admitted under its policy, every request
// admitted before has its context ended, its connections are closed, and a
// new socket binds only on a reading taken after all of that.

// consoleLANInterval is the retry's and the reconciliation's one interval,
// OpenHome's listing cadence (the plan's §3.4).
const consoleLANInterval = 5 * time.Second

// consoleLANRetireWait bounds a retirement's wait for the handlers its
// policy admitted, the console's shutdown window.
const consoleLANRetireWait = 5 * time.Second

// The states of the home-network socket.
const (
	ConsoleLANOff       = consolelan.StateOff
	ConsoleLANListening = consolelan.StateListening
	ConsoleLANWaiting   = consolelan.StateWaiting
)

// What a retirement says changed (the plan's §3.5), at WARN once each.
const (
	causeAddressGone      = "address_gone"
	causeAddressMoved     = "address_moved"
	causePrefixChanged    = "prefix_changed"
	causeInterfaceChanged = "interface_changed"
	causeInterfaceDown    = "interface_down"
	causeHostModelChanged = "host_model_changed"
	causeNameChanged      = "name_changed"
	causeInterfacesUnread = "interfaces_unreadable"
	causeStoppedServing   = "stopped_serving"
	causeShutdown         = "shutdown"
)

// consoleLANRefusedMessage is the fence's 503: a request that reached a
// socket whose policy is retired. It names no address.
const consoleLANRefusedMessage = "this console's home-network socket is being reopened: try again in a moment"

// ConsoleLANState is the home-network socket's state as /api/console, the
// menu, the banner and the doctor read it: the socket's and the boot's,
// never the file's now.
type ConsoleLANState = consolelan.State

// ConsoleLANPlan is serve's plan for the home-network socket, built once from
// the boot snapshot (cmd/bridge): the address and port, where the address
// came from, what the file's key held at boot, and the seams that read the
// box and hold the socket. An Address that is not valid is a plan with no
// socket, kept for what the file's key held at the start (an empty key costs
// the rollback all the same, D16).
type ConsoleLANPlan struct {
	// Address is A, the address the plan binds; Port is P, adminAddress's.
	Address netip.Addr
	Port    int
	// Source is config.ConsoleLANSource of the boot snapshot, and FileKey
	// what the file's key held at boot (config.LANKey), apart from Address.
	Source  string
	FileKey config.LANKey

	// Read reads the box's interfaces, whole or failing (lan.ReadInterfaces).
	Read func() ([]consolelan.Interface, error)
	// HostTable reads Windows' interface table of A's family
	// (consolelan.HostModel); Decide's strong_host hold asks it.
	HostTable func(ipv6 bool) (consolelan.Table, error)
	// Hold is the Control that holds a socket to an interface before its
	// bind (consolelan.Hold).
	Hold func(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error
	// Listen binds the socket through lc; nil is lc.Listen.
	Listen func(ctx context.Context, lc *net.ListenConfig, network, address string) (net.Listener, error)
	// LocalName is the .local name the socket answers to now, "" for none:
	// the bridge's mDNS responder answers it only while it runs (I10).
	LocalName func() string
	// GOOS is the platform Decide judges, "" for runtime.GOOS.
	GOOS string
	// Tick, when set, drives the plan's interval in place of a ticker: a
	// test sends on it, and never sleeps.
	Tick <-chan time.Time
	// RetireWait and ShutdownWindow, when positive, replace the 5 s bounds
	// of a retirement's wait and of the console's shutdown, for a test.
	RetireWait     time.Duration
	ShutdownWindow time.Duration
	// Seams for tests, nil in production.
	Seams *ConsoleLANSeams
}

// ConsoleLANSeams are a test's holds on the plan's ordering, each called on
// the goroutine that reaches it.
type ConsoleLANSeams struct {
	// InAdmission runs inside the fence's one critical section, between the
	// check of a request's policy and its count (I4).
	InAdmission func()
	// RetirementMarked runs once a retirement's critical section has marked
	// the policy retired and cancelled its context, before its server's
	// Close.
	RetirementMarked func()
	// RetirementClosed runs once its server's Close has returned.
	RetirementClosed func()
	// BeforeFreshReading runs once a retirement's wait has ended, before
	// the reading a rebind takes.
	BeforeFreshReading func()
	// Rebound runs once a reading after a retirement has bound or not.
	Rebound func(ConsoleLANState)
	// Released runs once a request has counted itself out, with the count
	// its policy holds after it.
	Released func(left int)
	// Ticked runs once the loop has done what a tick asked, whatever it was.
	Ticked func()
	// RetirementEnded runs once a retirement's wait has ended and it no
	// longer counts as under way.
	RetirementEnded func()
}

// socketKind tells the loopback socket from the home-network one.
type socketKind int

const (
	socketLoopback socketKind = iota
	socketLAN
)

// socketDesc is the description of the socket a request arrived on, which
// its server's BaseContext stamps on every request (the plan's §3.1). A
// request with none is the loopback socket's, today's behaviour.
type socketDesc struct {
	kind   socketKind
	policy *lanPolicy
}

type socketDescKey struct{}

// descOf is the socket a request arrived on.
func descOf(r *http.Request) socketDesc {
	if d, ok := r.Context().Value(socketDescKey{}).(socketDesc); ok {
		return d
	}
	return socketDesc{kind: socketLoopback}
}

// onLANSocket reports whether r arrived on the home-network socket.
func onLANSocket(r *http.Request) bool { return descOf(r).kind == socketLAN }

// lanPolicyParts is the reading a home-network socket was bound on: each
// part is compared at every reading, and a structural test holds every
// field of it to a case (the plan's §3.5).
type lanPolicyParts struct {
	Index     int
	Name      string
	Flags     net.Flags
	Prefix    netip.Prefix
	HostTable consolelan.Table
	LocalName string
}

// lanPolicy is one home-network socket's policy and its fence's books, the
// latter under consoleLAN.mu.
type lanPolicy struct {
	version uint64
	parts   lanPolicyParts
	addr    netip.Addr
	port    int
	class   netip.Prefix
	hold    string

	ctx    context.Context
	cancel context.CancelFunc
	server *http.Server
	served chan struct{}

	// Under consoleLAN.mu.
	retired  bool
	inflight int
}

// consoleLAN is the plan running: the fence's mutex, the current policy, the
// state the surfaces read, and the loop.
type consoleLAN struct {
	s    *Server
	plan ConsoleLANPlan
	hold string // Decide's answer for the platform, "" where refused

	mu      sync.Mutex // the fence: versions, retirements, counts; the state's writes
	cond    *sync.Cond
	current *lanPolicy
	version uint64
	// closing is set by the stop, which no bind follows; closeDeadline is
	// the stop's one deadline, which bounds a retirement's wait under way.
	closing       bool
	closeDeadline time.Time
	lastSaid      string
	// retiring counts the retirements under way, from a retirement's first
	// section until its wait for what it admitted has ended: no tick binds
	// while one runs, and the stop joins each within its one deadline.
	retiring int

	state atomic.Pointer[ConsoleLANState]

	stop     chan struct{}
	loopDone chan struct{}
	started  bool
	// first is closed once the plan's first attempt has run, or would make
	// none: the loop takes no tick before it (BeginConsoleLAN).
	first     chan struct{}
	firstOnce sync.Once
}

// OpenConsoleLAN opens the plan and makes its first attempt synchronously
// (PrepareConsoleLAN, then BeginConsoleLAN), and returns the state it left.
// The loop that retries and reads again starts with ServeListener, which
// stops it, retires the socket and shuts both sockets down under one
// deadline. A nil plan is no posture. Call once, before ServeListener.
func (s *Server) OpenConsoleLAN(ctx context.Context, plan *ConsoleLANPlan) ConsoleLANState {
	if plan == nil {
		return s.ConsoleLAN()
	}
	s.PrepareConsoleLAN(ctx, plan)
	return s.BeginConsoleLAN(ctx)
}

// PrepareConsoleLAN opens the plan and decides its hold, and makes no
// attempt: serve prepares it before the console's goroutine starts, so the
// loop ServeListener starts is the plan's, and makes the first attempt once
// the mDNS responder's start has answered (BeginConsoleLAN), so the first
// reading holds the .local name exactly while the responder runs and the
// loop's first tick finds no change in it. A nil plan is no posture. Call
// once, before ServeListener.
func (s *Server) PrepareConsoleLAN(ctx context.Context, plan *ConsoleLANPlan) ConsoleLANState {
	if plan == nil {
		return s.ConsoleLAN()
	}
	c := &consoleLAN{s: s, plan: *plan, stop: make(chan struct{}), loopDone: make(chan struct{}),
		first: make(chan struct{})}
	c.cond = sync.NewCond(&c.mu)
	s.consoleLAN = c
	if !plan.Address.IsValid() {
		st := c.base(ConsoleLANOff, "")
		c.state.Store(&st)
		c.firstOnce.Do(func() { close(c.first) })
		return st
	}
	goos := plan.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	hold, err := consolelan.Decide(goos, plan.Address.Is6())
	if err != nil {
		c.setWaiting(reasonOf(err), err)
		c.firstOnce.Do(func() { close(c.first) })
		return c.State()
	}
	c.hold = hold
	// Until the first attempt, the surfaces say the bridge is starting.
	st := c.base(ConsoleLANWaiting, consolelan.ReasonStarting)
	c.mu.Lock()
	c.state.Store(&st)
	c.mu.Unlock()
	return st
}

// BeginConsoleLAN makes the prepared plan's first attempt, once, and returns
// the state it left; the loop takes its first tick after it. It attempts
// nothing once the console's stop has begun (AbortConsoleLAN, or
// ServeListener's shutdown), and nothing where no plan runs.
func (s *Server) BeginConsoleLAN(ctx context.Context) ConsoleLANState {
	c := s.consoleLAN
	if c == nil {
		return s.ConsoleLAN()
	}
	c.firstOnce.Do(func() {
		defer close(c.first)
		c.mu.Lock()
		closing := c.closing
		c.mu.Unlock()
		if !closing {
			c.attempt(ctx)
		}
	})
	return c.State()
}

// AbortConsoleLAN retires the home-network socket on a way out of serve that
// ServeListener's shutdown does not take: the loopback socket's bind failed,
// a listener serve was handed was refused, or the loopback socket stopped
// serving. It stops the plan within the console's one shutdown window, as
// that shutdown does (I7). Nothing where no plan runs.
func (s *Server) AbortConsoleLAN() {
	if c := s.consoleLAN; c != nil {
		c.stopAndRetire(time.Now().Add(s.shutdownWindow()))
	}
}

// ConsoleLAN is the home-network socket's state, off where no plan runs
// (or no console exists yet: a nil s).
func (s *Server) ConsoleLAN() ConsoleLANState {
	if s == nil || s.consoleLAN == nil {
		return ConsoleLANState{State: ConsoleLANOff, FileKeyAtBoot: consolelan.FileKeyAbsent}
	}
	return s.consoleLAN.State()
}

// State is the plan's state now.
func (c *consoleLAN) State() ConsoleLANState {
	if st := c.state.Load(); st != nil {
		return *st
	}
	return c.base(ConsoleLANWaiting, "")
}

// base is a state with the plan's boot facts filled.
func (c *consoleLAN) base(state, reason string) ConsoleLANState {
	st := ConsoleLANState{State: state, Reason: reason, FileKeyAtBoot: string(c.plan.FileKey.State)}
	if c.plan.Address.IsValid() {
		st.Address = c.plan.Address.String()
		st.Source = c.plan.Source
	}
	if st.FileKeyAtBoot == "" {
		st.FileKeyAtBoot = consolelan.FileKeyAbsent
	}
	if c.plan.FileKey.State == config.LANKeyAddress {
		st.FileAddressAtBoot = c.plan.FileKey.Address
	}
	return st
}

// setWaiting records a waiting state and says it at WARN once per reason.
func (c *consoleLAN) setWaiting(reason string, err error) {
	st := c.base(ConsoleLANWaiting, reason)
	var re *consolelan.RefusalError
	if errors.As(err, &re) {
		st.Interface = re.Interface
	}
	c.mu.Lock()
	c.state.Store(&st)
	said := reason == c.lastSaid
	c.lastSaid = reason
	c.mu.Unlock()
	if !said {
		logger.Warn("console: not listening on the home network",
			"reason", reason, "words", consolelan.ReasonWords(reason, st.Address, c.plan.Port, st.Interface),
			"address", st.Address, "detail", errText(err))
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// reasonOf is the reason code an attempt's error carries.
func reasonOf(err error) string {
	var re *consolelan.RefusalError
	if errors.As(err, &re) {
		return re.Reason
	}
	var le *lanError
	if errors.As(err, &le) {
		return le.reason
	}
	return consolelan.ReasonBindFailed
}

// lanError is a reading's or a bind's refusal of the plan's own.
type lanError struct {
	reason string
	err    error
}

func (e *lanError) Error() string {
	if e.err != nil {
		return e.reason + ": " + e.err.Error()
	}
	return e.reason
}

func (e *lanError) Unwrap() error { return e.err }

// read takes one reading of the box: whole or failing, A on exactly one
// interface with a network, that interface up, running, neither loopback nor
// point-to-point, and on Windows the strong host model on every row of A's
// family's table, every interface holding an address of the family having a
// row (consolelan.JudgeStrongHost).
func (c *consoleLAN) read() (lanPolicyParts, error) {
	if c.plan.Read == nil {
		return lanPolicyParts{}, &consolelan.RefusalError{Reason: consolelan.ReasonInterfacesUnreadable}
	}
	ifs, err := c.plan.Read()
	if err != nil {
		return lanPolicyParts{}, &consolelan.RefusalError{Reason: consolelan.ReasonInterfacesUnreadable, Err: err}
	}
	a := c.plan.Address
	var parts lanPolicyParts
	found := 0
	var addressed []int
	names := map[int]string{}
	for _, in := range ifs {
		names[in.Index] = in.Name
		holdsFamily := false
		for _, p := range in.Addrs {
			pa := p.Addr().Unmap().WithZone("")
			if pa.Is6() == a.Is6() {
				holdsFamily = true
			}
			if pa == a {
				found++
				parts.Index, parts.Name, parts.Flags = in.Index, in.Name, in.Flags
				parts.Prefix = netip.PrefixFrom(a, p.Bits()).Masked()
			}
		}
		if holdsFamily {
			addressed = append(addressed, in.Index)
		}
	}
	switch {
	case found == 0:
		return lanPolicyParts{}, &lanError{reason: consolelan.ReasonAddressNotOnBox}
	case found > 1:
		return lanPolicyParts{}, &lanError{reason: consolelan.ReasonAddressOnTwo}
	case parts.Prefix.Bits() < 0 || parts.Prefix.Bits() >= a.BitLen():
		return lanPolicyParts{}, &lanError{reason: consolelan.ReasonAddressNoNetwork}
	case parts.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0:
		return lanPolicyParts{}, &lanError{reason: consolelan.ReasonAddressOnTunnel}
	case parts.Flags&net.FlagUp == 0 || parts.Flags&net.FlagRunning == 0:
		return lanPolicyParts{}, &lanError{reason: consolelan.ReasonInterfaceDown}
	}
	if c.hold == consolelan.HoldStrongHost {
		if c.plan.HostTable == nil {
			return lanPolicyParts{}, &consolelan.RefusalError{Reason: consolelan.ReasonHostModelUnreadable}
		}
		t, err := c.plan.HostTable(a.Is6())
		if err != nil {
			var re *consolelan.RefusalError
			if errors.As(err, &re) {
				return lanPolicyParts{}, err
			}
			return lanPolicyParts{}, &consolelan.RefusalError{Reason: consolelan.ReasonHostModelUnreadable, Err: err}
		}
		if err := consolelan.JudgeStrongHost(t, addressed, parts.Index, names); err != nil {
			return lanPolicyParts{}, err
		}
		parts.HostTable = slices.Clone(t)
		slices.SortFunc(parts.HostTable, func(x, y consolelan.Row) int { return x.Index - y.Index })
	}
	if c.plan.LocalName != nil {
		if n, ok := lanName(c.plan.LocalName()); ok {
			parts.LocalName = n
		}
	}
	return parts, nil
}

// changeCause is why parts no longer reads as the policy's, "" for none.
func changeCause(was, now lanPolicyParts) string {
	switch {
	case now.Index != was.Index || now.Name != was.Name:
		return causeAddressMoved
	case now.Flags != was.Flags:
		return causeInterfaceChanged
	case now.Prefix != was.Prefix:
		return causePrefixChanged
	case !slices.Equal(now.HostTable, was.HostTable):
		return causeHostModelChanged
	case now.LocalName != was.LocalName:
		return causeNameChanged
	}
	return ""
}

// attempt reads the box and binds where the reading admits A; the state
// says which. On the loop's goroutine, or OpenConsoleLAN's before it starts.
func (c *consoleLAN) attempt(ctx context.Context) {
	parts, err := c.read()
	if err != nil {
		c.setWaiting(reasonOf(err), err)
		return
	}
	if err := c.bind(ctx, parts); err != nil {
		c.setWaiting(reasonOf(err), err)
	}
}

// bind holds a socket to the reading's interface before its bind, binds
// A:P, and serves the console on it with the reading as its policy. Nothing
// binds once the stop has begun.
func (c *consoleLAN) bind(ctx context.Context, parts lanPolicyParts) error {
	a := c.plan.Address
	if c.plan.Hold == nil {
		return &consolelan.RefusalError{Reason: consolelan.ReasonNotSupportedHere}
	}
	lc := &net.ListenConfig{Control: c.plan.Hold(parts.Index, a.Is6())}
	network := "tcp4"
	if a.Is6() {
		network = "tcp6"
	}
	addr := net.JoinHostPort(a.String(), strconv.Itoa(c.plan.Port))
	listen := c.plan.Listen
	if listen == nil {
		listen = func(ctx context.Context, lc *net.ListenConfig, network, address string) (net.Listener, error) {
			return lc.Listen(ctx, network, address)
		}
	}
	lis, err := listen(ctx, lc, network, addr)
	if err != nil {
		var re *consolelan.RefusalError
		if errors.As(err, &re) {
			return re
		}
		switch {
		case errors.Is(err, syscall.EADDRNOTAVAIL):
			return &lanError{reason: consolelan.ReasonAddressNotOnBox, err: err}
		case errors.Is(err, syscall.EADDRINUSE):
			return &lanError{reason: consolelan.ReasonPortInUse, err: err}
		case errors.Is(err, syscall.EACCES):
			return &lanError{reason: consolelan.ReasonPortNotPermitted, err: err}
		}
		return &lanError{reason: consolelan.ReasonBindFailed, err: err}
	}
	class, _ := config.ConsoleLANClass(a)
	pctx, cancel := context.WithCancel(ctx)
	p := &lanPolicy{parts: parts, addr: a, port: c.plan.Port, class: class, hold: c.hold,
		ctx: pctx, cancel: cancel, served: make(chan struct{})}
	base := context.WithValue(pctx, socketDescKey{}, socketDesc{kind: socketLAN, policy: p})
	p.server = &http.Server{
		Handler:           c.fence(p, c.s.Handler()),
		ErrorLog:          handshakelog.ErrorLog(),
		BaseContext:       func(net.Listener) context.Context { return base },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	st := c.base(ConsoleLANListening, "")
	st.URL = consolelan.URLOf(a, c.plan.Port)
	if parts.LocalName != "" {
		st.NameURL = "http://" + net.JoinHostPort(parts.LocalName, strconv.Itoa(c.plan.Port))
	}
	st.Network, st.Interface, st.Hold = parts.Prefix.String(), parts.Name, c.hold
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		cancel()
		_ = lis.Close()
		return &lanError{reason: consolelan.ReasonStopping}
	}
	c.version++
	p.version = c.version
	c.current = p
	c.state.Store(&st)
	c.lastSaid = ""
	c.mu.Unlock()
	go func() {
		defer close(p.served)
		err := p.server.Serve(lis)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("console: the home-network socket stopped serving", "reason", consolelan.ReasonStoppedServing, "err", err)
			c.retireForFailure(p)
		}
	}()
	logger.Info("console listening on the home network", "url", st.URL, "network", st.Network,
		"interface", st.Interface, "hold", st.Hold)
	return nil
}

// fence is the first middleware of the home-network chain (F1, F9): in one
// critical section of the plan's mutex a request reads whether its policy is
// current and, if it is, counts itself in; a request whose policy is not
// current is refused, 503 with Connection: close, and nothing else runs. The
// count is given back in a deferred call, so a panic gives it back too. Every
// response of the socket carries framing protection (I6).
func (c *consoleLAN) fence(p *lanPolicy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		if !c.admit(p) {
			h.Set("Connection", "close")
			http.Error(w, "admin refused: "+consoleLANRefusedMessage, http.StatusServiceUnavailable)
			return
		}
		defer c.release(p)
		next.ServeHTTP(w, r)
	})
}

// admit is the fence's one critical section: the check and the count, under
// the mutex a retirement takes to mark the policy retired (F9).
func (c *consoleLAN) admit(p *lanPolicy) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.retired || c.current != p {
		return false
	}
	if sm := c.plan.Seams; sm != nil && sm.InAdmission != nil {
		sm.InAdmission()
	}
	p.inflight++
	return true
}

func (c *consoleLAN) release(p *lanPolicy) {
	c.mu.Lock()
	p.inflight--
	left := p.inflight
	if p.retired && left == 0 {
		c.cond.Broadcast()
	}
	c.mu.Unlock()
	if sm := c.plan.Seams; sm != nil && sm.Released != nil {
		sm.Released(left)
	}
}

// retireWait is the plan's bound on a retirement's wait.
func (c *consoleLAN) retireWait() time.Duration {
	if c.plan.RetireWait > 0 {
		return c.plan.RetireWait
	}
	return consoleLANRetireWait
}

// retire retires p (F1, I2, I8), in this order: in one critical section it
// marks the policy retired, no longer current, the state waiting with the
// retirement's reason where p was current, counts the retirement under way
// and cancels the policy's context, so a request admitted before has its
// context ended (an event stream, waiting on its context or on a snapshot it
// waits for on that context, returns) and one after is refused; then it
// closes the server, which closes its listener, waits for its Serve and
// closes every connection it tracks, so a stream blocked in a write ends
// too; then it waits for the handlers its policy admitted until deadline,
// and only then counts itself out. A second retirement of p does it again,
// which changes nothing.
func (c *consoleLAN) retire(p *lanPolicy, cause string, deadline time.Time) {
	c.mu.Lock()
	p.retired = true
	if c.current == p {
		c.current = nil
		// Nothing is served under p from here on: the surfaces read the
		// reason the socket is closing for until an attempt says otherwise.
		st := c.base(ConsoleLANWaiting, retiredReason(cause))
		c.state.Store(&st)
	}
	c.retiring++
	p.cancel()
	c.mu.Unlock()
	if sm := c.plan.Seams; sm != nil && sm.RetirementMarked != nil {
		sm.RetirementMarked()
	}
	if cause != causeShutdown {
		logger.Warn("console: the home-network socket is retired", "cause", cause, "network", p.parts.Prefix.String())
	}
	if p.server != nil {
		_ = p.server.Close()
	}
	if sm := c.plan.Seams; sm != nil && sm.RetirementClosed != nil {
		sm.RetirementClosed()
	}
	c.waitHandlers(p, deadline)
	c.mu.Lock()
	c.retiring--
	c.cond.Broadcast()
	c.mu.Unlock()
	if sm := c.plan.Seams; sm != nil && sm.RetirementEnded != nil {
		sm.RetirementEnded()
	}
}

// retiredReason is the waiting state's reason while a retirement for cause
// runs.
func retiredReason(cause string) string {
	switch cause {
	case causeShutdown:
		return consolelan.ReasonStopping
	case causeStoppedServing:
		return consolelan.ReasonStoppedServing
	}
	return consolelan.ReasonReopening
}

// waitHandlers waits until p's count is 0 or deadline has passed, the stop's
// deadline too where the stop came meanwhile (I7), the latter said at WARN: a
// handler that outlives it keeps its count on a policy nothing admits by.
func (c *consoleLAN) waitHandlers(p *lanPolicy, deadline time.Time) {
	timer := time.AfterFunc(time.Until(deadline), func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer timer.Stop()
	c.mu.Lock()
	for p.inflight > 0 {
		now := time.Now()
		if !now.Before(deadline) || (c.closing && !now.Before(c.closeDeadline)) {
			break
		}
		c.cond.Wait()
	}
	left := p.inflight
	c.mu.Unlock()
	if left > 0 {
		logger.Warn("console: a request of a retired home-network socket outlived its wait", "count", left)
	}
}

// retireForFailure retires a socket whose Serve failed, from its goroutine,
// as the loop retires one (the plan's §3.6): the retirement's section says
// stopped_serving, and the loop binds again at its first tick once the
// retirement has ended, whose attempt says what it found. Nothing is stored
// after the retirement, so no newer reason is replaced by it.
func (c *consoleLAN) retireForFailure(p *lanPolicy) {
	c.retire(p, causeStoppedServing, time.Now().Add(c.retireWait()))
}

// tick is one interval of the loop: a waiting plan attempts again; a
// listening one reads the box again and retires its socket on any change, or
// on a reading that fails, then takes a fresh reading and binds where it
// admits A (F2).
func (c *consoleLAN) tick(ctx context.Context) {
	if c.hold == "" {
		return // not_supported_here: nothing at run time changes the answer
	}
	c.mu.Lock()
	p, closing, retiring := c.current, c.closing, c.retiring
	c.mu.Unlock()
	if closing {
		return
	}
	if p == nil {
		// A retirement under way, a failure's on its Serve's goroutine, still
		// has connections to close and handlers to wait for: no socket binds
		// before it has ended (the plan's §3.6); a tick after it binds.
		if retiring > 0 {
			return
		}
		c.attempt(ctx)
		return
	}
	parts, err := c.read()
	cause := ""
	if err != nil {
		switch reasonOf(err) {
		case consolelan.ReasonAddressNotOnBox:
			cause = causeAddressGone
		case consolelan.ReasonInterfaceDown, consolelan.ReasonAddressOnTunnel:
			cause = causeInterfaceDown
		case consolelan.ReasonAddressOnTwo, consolelan.ReasonAddressNoNetwork:
			cause = causePrefixChanged
		case consolelan.ReasonInterfaceNotStrongHost, consolelan.ReasonHostModelUnreadable:
			cause = causeHostModelChanged
		default:
			cause = causeInterfacesUnread
		}
	} else {
		cause = changeCause(p.parts, parts)
	}
	if cause == "" {
		return
	}
	c.retire(p, cause, time.Now().Add(c.retireWait()))
	if sm := c.plan.Seams; sm != nil && sm.BeforeFreshReading != nil {
		sm.BeforeFreshReading()
	}
	c.mu.Lock()
	closing = c.closing
	c.lastSaid = ""
	c.mu.Unlock()
	if closing {
		return
	}
	c.attempt(ctx)
	if st := c.State(); st.State == ConsoleLANListening {
		logger.Info("console: the home-network socket is bound again", "cause", cause, "url", st.URL)
	}
	if sm := c.plan.Seams; sm != nil && sm.Rebound != nil {
		sm.Rebound(c.State())
	}
}

// startLoop runs the plan's interval on a goroutine of its own, joined by
// stopAndRetire. A plan with no address runs none.
func (c *consoleLAN) startLoop(ctx context.Context) {
	if !c.plan.Address.IsValid() || c.started {
		return
	}
	c.started = true
	go func() {
		defer close(c.loopDone)
		select {
		case <-c.first:
		case <-c.stop:
			return
		case <-ctx.Done():
			return
		}
		var tick <-chan time.Time
		if c.plan.Tick != nil {
			tick = c.plan.Tick
		} else {
			t := time.NewTicker(consoleLANInterval)
			defer t.Stop()
			tick = t.C
		}
		for {
			select {
			case <-c.stop:
				return
			case <-ctx.Done():
				return
			case <-tick:
				c.tick(ctx)
				if sm := c.plan.Seams; sm != nil && sm.Ticked != nil {
					sm.Ticked()
				}
			}
		}
	}()
}

// stopAndRetire stops the loop and joins it, so no bind follows the stop,
// then retires the socket through the fence and joins every retirement
// under way, a failure's on its Serve's goroutine included, all within
// deadline, the one deadline the console's shutdown shares (I7): a
// retirement under way at the stop waits no longer than it.
func (c *consoleLAN) stopAndRetire(deadline time.Time) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.closing, c.closeDeadline = true, deadline
	c.cond.Broadcast()
	c.mu.Unlock()
	wake := time.AfterFunc(time.Until(deadline), func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer wake.Stop()
	if c.started {
		close(c.stop)
		select {
		case <-c.loopDone:
		case <-time.After(time.Until(deadline)):
			logger.Warn("console: the home-network socket's loop did not stop in time")
		}
	}
	c.mu.Lock()
	p := c.current
	c.mu.Unlock()
	if p != nil {
		c.retire(p, causeShutdown, deadline)
	}
	c.mu.Lock()
	for c.retiring > 0 && time.Now().Before(deadline) {
		c.cond.Wait()
	}
	left := c.retiring
	c.mu.Unlock()
	if left > 0 {
		logger.Warn("console: a retirement of the home-network socket outlived the stop's window", "count", left)
	}
	if c.plan.Address.IsValid() {
		st := c.base(ConsoleLANWaiting, consolelan.ReasonStopping)
		c.mu.Lock()
		c.state.Store(&st)
		c.mu.Unlock()
	}
}

// The source policy's refusal and its log's kinds.
const (
	errMsgLANSource     = "admin refused: this console's home-network address answers only to devices on its own network"
	lanSourceOutsideNet = "outside_the_network"
	lanSourceOutsideCls = "outside_the_class"
	lanSourceTailscale  = "tailscale"
)

// lanSourceOK is the home-network socket's source policy (the plan's §4.1,
// I9): the peer, unmapped and without a zone, in no Tailscale range whatever
// the prefix says, and inside A's private class and A's on-link network.
// ok false with no kind is a peer address that does not read.
func lanSourceOK(p *lanPolicy, remoteAddr string) (ok bool, kind string) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false, ""
	}
	peer := ap.Addr().Unmap().WithZone("")
	switch {
	case config.IsTailscaleAddr(peer):
		return false, lanSourceTailscale
	case !p.class.IsValid() || !p.class.Contains(peer):
		return false, lanSourceOutsideCls
	case !p.parts.Prefix.Contains(peer):
		return false, lanSourceOutsideNet
	}
	return true, ""
}

// lanOnly is the home-network socket's boundary, where loopbackOnly is the
// loopback socket's: the source policy, then the Host through the one
// parser (lanHostOK).
func (s *Server) lanOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := descOf(r).policy
		if p == nil {
			http.Error(w, errMsgLANSource, http.StatusForbidden)
			return
		}
		if ok, kind := lanSourceOK(p, r.RemoteAddr); !ok {
			if kind == "" {
				http.Error(w, errMsgBadRemoteAddr, http.StatusForbidden)
				return
			}
			s.noteLANSourceRefusal(kind, p)
			http.Error(w, errMsgLANSource, http.StatusForbidden)
			return
		}
		if !lanHostOK(p, r.Host) {
			s.noteForeignHostOn(socketLAN, r.Host)
			http.Error(w, lanForeignHostMessage(p), http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// lanAuthorityMatches compares an authority the one parser read with the
// socket's two: A with P, and the .local name with P.
func lanAuthorityMatches(p *lanPolicy, a lanAuthority) bool {
	if a.port != p.port {
		return false
	}
	if a.addr.IsValid() {
		return a.addr == p.addr
	}
	return a.name != "" && p.parts.LocalName != "" && a.name == p.parts.LocalName
}

// lanHostOK is the home-network socket's Host check.
func lanHostOK(p *lanPolicy, host string) bool {
	a, ok := parseLANAuthority(host)
	return ok && lanAuthorityMatches(p, a)
}

// lanOriginOK is the home-network socket's Origin check, the same parser
// and comparison after the Origin's exact "http://".
func lanOriginOK(p *lanPolicy, origin string) bool {
	a, ok := lanOriginAuthority(origin)
	return ok && lanAuthorityMatches(p, a)
}

// originMatches is the one Origin rule csrfGuard and the event stream's gate
// call (the plan's §4.3): the home-network socket's own on that socket, and
// originMatchesAdmin on the loopback one, unchanged.
func (s *Server) originMatches(r *http.Request, origin string) bool {
	if d := descOf(r); d.kind == socketLAN {
		return d.policy != nil && lanOriginOK(d.policy, origin)
	}
	return s.originMatchesAdmin(origin)
}

// lanURL is the home-network socket's URL, the address's.
func lanURL(p *lanPolicy) string { return consolelan.URLOf(p.addr, p.port) }

// lanForeignHostMessage is the home-network socket's 421 body.
func lanForeignHostMessage(p *lanPolicy) string {
	msg := "admin refused: this address answers only as " + lanURL(p)
	if p.parts.LocalName != "" {
		msg += " or http://" + net.JoinHostPort(p.parts.LocalName, strconv.Itoa(p.port))
	}
	return msg + " (a reverse proxy in front of it must send that Host)"
}

// lanSourceNoted holds the kinds of source refusal already logged, once each.
type lanSourceNoted struct {
	mu   sync.Mutex
	seen map[string]bool
}

// noteLANSourceRefusal logs a kind of source refusal once, naming A's
// network and never the peer, as the DLNA listener's refusals do.
func (s *Server) noteLANSourceRefusal(kind string, p *lanPolicy) {
	s.lanSourceNoted.mu.Lock()
	if s.lanSourceNoted.seen == nil {
		s.lanSourceNoted.seen = map[string]bool{}
	}
	seen := s.lanSourceNoted.seen[kind]
	s.lanSourceNoted.seen[kind] = true
	s.lanSourceNoted.mu.Unlock()
	if seen {
		return
	}
	logger.Warn("console refused a home-network connection from outside its network", "kind", kind,
		"network", p.parts.Prefix.String())
}

// The page data's socket kinds.
const (
	pageSocketLoopback = "loopback"
	pageSocketLAN      = "lan"
)

// pageSocket is the kind of the socket a page was served on, which decides
// the pairing dialog's and the rotation's warnings (the plan's §6.2), never
// the browser's isSecureContext.
func pageSocket(r *http.Request) string {
	if r != nil && onLANSocket(r) {
		return pageSocketLAN
	}
	return pageSocketLoopback
}

// consoleLANLine is the menu's line from the socket's state now (the plan's
// §5.1), "" for none.
func (s *Server) consoleLANLine() string {
	c := s.consoleLAN
	if c == nil {
		return ""
	}
	return consolelan.Line(c.State(), c.plan.Port)
}

// apiConsole answers GET /api/console: the home-network socket's state.
func (s *Server) apiConsole(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]ConsoleLANState{"lan": s.ConsoleLAN()})
}
