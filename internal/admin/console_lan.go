package admin

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/consolelan"
)

// Names-only shim (backlog B989's tests-first commit): the plan's type and
// its seams, the socket's description and the policy, each body inert until
// the change gives it its own. ServeConsole serves loopback alone, the plan
// ignored, and GET /api/console has no route.

const (
	ConsoleLANOff       = consolelan.StateOff
	ConsoleLANListening = consolelan.StateListening
	ConsoleLANWaiting   = consolelan.StateWaiting
)

type ConsoleLANState = consolelan.State

type ConsoleLANPlan struct {
	Address        netip.Addr
	Port           int
	Source         string
	FileKey        config.LANKey
	Read           func() ([]consolelan.Interface, error)
	HostTable      func(ipv6 bool) (consolelan.Table, error)
	Hold           func(index int, ipv6 bool) func(network, address string, c syscall.RawConn) error
	Listen         func(ctx context.Context, lc *net.ListenConfig, network, address string) (net.Listener, error)
	LocalName      func() string
	GOOS           string
	Tick           <-chan time.Time
	RetireWait     time.Duration
	ShutdownWindow time.Duration
	Seams          *ConsoleLANSeams
}

type ConsoleLANSeams struct {
	InAdmission        func()
	RetirementMarked   func()
	RetirementClosed   func()
	BeforeFreshReading func()
	Rebound            func(ConsoleLANState)
	Released           func(left int)
	Ticked             func()
}

type socketKind int

const (
	socketLoopback socketKind = iota
	socketLAN
)

type socketDesc struct {
	kind   socketKind
	policy *lanPolicy
}

type socketDescKey struct{}

type lanPolicyParts struct {
	Index     int
	Name      string
	Flags     net.Flags
	Prefix    netip.Prefix
	HostTable consolelan.Table
	LocalName string
}

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

	retired  bool
	inflight int
}

type consoleLAN struct {
	s    *Server
	plan ConsoleLANPlan
	hold string

	mu       sync.Mutex
	cond     *sync.Cond
	current  *lanPolicy
	version  uint64
	state    atomic.Pointer[ConsoleLANState]
	stop     chan struct{}
	loopDone chan struct{}
	started  bool
}

// OpenConsoleLAN opens nothing.
func (s *Server) OpenConsoleLAN(ctx context.Context, plan *ConsoleLANPlan) ConsoleLANState {
	return ConsoleLANState{State: ConsoleLANOff}
}

// ConsoleLAN is off.
func (s *Server) ConsoleLAN() ConsoleLANState { return ConsoleLANState{State: ConsoleLANOff} }

func (c *consoleLAN) State() ConsoleLANState {
	if st := c.state.Load(); st != nil {
		return *st
	}
	return ConsoleLANState{}
}

func (c *consoleLAN) base(state, reason string) ConsoleLANState {
	return ConsoleLANState{State: state, Reason: reason}
}

func (s *Server) consoleLANLine() string { return "" }

type lanSourceNoted struct {
	mu   sync.Mutex
	seen map[string]bool
}

func lanHostOK(p *lanPolicy, host string) bool { return false }

func lanOriginOK(p *lanPolicy, origin string) bool { return false }

func (s *Server) originMatches(r *http.Request, origin string) bool {
	return s.originMatchesAdmin(origin)
}

func (s *Server) apiConsole(w http.ResponseWriter, _ *http.Request) {}
