package dlna

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// maxConcurrentMSearchResponders bounds the number of in-flight M-SEARCH
// responder goroutines. Each one may sleep up to `mxResponseCeilingSeconds`
// (the spec-mandated randomized reply delay) before sending, so without a
// cap a burst of `ssdp:all` searches — accidental or hostile — could spawn
// unbounded goroutines. When the pool is full, additional searches are
// dropped (the control point re-searches; SSDP discovery is best-effort).
const maxConcurrentMSearchResponders = 16

// defaultMSearchJitter is the reply spread applied when an M-SEARCH omits
// MX (or sends MX:0). Still randomized so simultaneous searches don't
// reply in a synchronized burst.
const defaultMSearchJitter = 100 * time.Millisecond

// ssdpLogger is the SSDP-specific package logger — distinct subcomponent
// tag from `dlna` proper so operators grepping for the multicast /
// NOTIFY path can filter narrowly. Per CodeRabbit Major on PR #303.
var ssdpLogger = logging.Component("dlna-ssdp")

// SSDPConfig configures the SSDP advertiser runtime — the goroutines
// that send periodic NOTIFY ssdp:alive packets AND respond unicast to
// incoming M-SEARCH requests on the SSDP multicast address.
//
// All fields except Logger are required. The advertiser does NOT
// validate inputs; pass in well-formed values (Location must be an
// absolute URL reachable by renderers on the local network; UDN must
// have the `uuid:` prefix; ServerToken should come from
// `SSDPServerToken(productVersion)`).
type SSDPConfig struct {
	// UDN is the device's stable unique identifier with the `uuid:`
	// prefix (e.g. "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c").
	// Determines the USN values across all NotifyTargets.
	UDN string

	// Location is the absolute URL of the bridge's DLNA device
	// description XML, as reachable by renderers on the local
	// network (e.g. "http://192.168.0.14:7790/dlna/description.xml").
	Location string

	// ServerToken is the SSDP SERVER header value. Build via
	// `SSDPServerToken(productVersion)`.
	ServerToken string

	// Interface is the LAN-eligible interface to bind the multicast
	// listener on. nil = the OS picks (Go default; works for most
	// single-interface hosts). For multi-interface hosts (e.g., Mac
	// with Ethernet + WiFi), pass the specific interface chosen via
	// `IsLANEligibleInterface`.
	Interface *net.Interface

	// AdvertiseInterval controls how often the periodic NOTIFY
	// ssdp:alive announcements fire. Default 14 minutes (slightly
	// under SSDPMaxAge/2 = 15 min) to ensure we re-announce well
	// before our previous entry would time out of renderer caches.
	// Set to 0 to use the default; values < 1 minute are clamped up
	// (defensive against test misconfig).
	AdvertiseInterval time.Duration

	// Logger is the structured logger for SSDP events. nil collapses
	// to `logging.Component("dlna-ssdp")` at Start() time.
	Logger *slog.Logger
}

// defaultAdvertiseInterval is the canonical NOTIFY ssdp:alive cadence:
// slightly under SSDPMaxAge/2 to ensure re-announcement before any
// renderer would purge the prior entry.
const defaultAdvertiseInterval = 14 * time.Minute

// SSDPAdvertiser runs the SSDP NOTIFY + M-SEARCH response goroutines.
// Use `NewSSDPAdvertiser` to create, `Start` to begin, `Stop` to
// gracefully tear down (sends ssdp:byebye for every NotifyTarget on
// the way out so renderers purge our entry from their discovery
// tables immediately rather than waiting for SSDPMaxAge).
//
// Concurrent operation: Start spawns 2 goroutines (one for periodic
// NOTIFY, one for M-SEARCH listen-and-respond). Both honor the
// internal context for cancellation. Stop cancels the context and
// blocks on the WaitGroup until both goroutines have exited.
//
// **Restart safety:** Stop() is NOT safe to call concurrently with
// Start(), nor before Start() has returned. Stop() may be called once,
// after which the advertiser is single-use — call NewSSDPAdvertiser
// for a fresh lifecycle.
type SSDPAdvertiser struct {
	cfg     SSDPConfig
	targets []NotifyTarget
	log     *slog.Logger

	// Lifecycle guards. `mu` serializes the Start / Stop pair so
	// double-Start can't spawn duplicate goroutine pairs and so a
	// concurrent Stop during a half-Start can't tear down state the
	// other side hasn't published yet. `started` is the explicit
	// state-machine flag (false → not running → Start allowed →
	// true → running → Start refused). Stop clears it back to false.
	// Per CodeRabbit Major on PR #303.
	mu      sync.Mutex
	started bool

	// Runtime state — nil until Start() is called. `listener` / `sender`
	// are accessed ONLY under `s.mu` (Start writes them; Stop closes +
	// nils them). The background goroutines + send helpers receive their
	// own captured copies as parameters and never read these fields, so
	// Stop's nil-set races nothing. Do NOT reintroduce a struct-field
	// read on `s.listener`/`s.sender` from any goroutine.
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	listener *net.UDPConn // multicast listener for incoming M-SEARCH
	sender   *net.UDPConn // unicast sender for outgoing NOTIFY

	// searchSem bounds concurrent M-SEARCH responder goroutines (see
	// `maxConcurrentMSearchResponders`). Allocated once in the
	// constructor; immutable thereafter, so it's safe to read without
	// the lock.
	searchSem chan struct{}
}

// NewSSDPAdvertiser constructs an advertiser with the given config.
// Does NOT bind sockets or spawn goroutines — call Start() to begin.
// Returned advertiser is safe to discard without calling Start() (no
// resources acquired yet).
func NewSSDPAdvertiser(cfg SSDPConfig) *SSDPAdvertiser {
	if cfg.AdvertiseInterval < time.Minute {
		cfg.AdvertiseInterval = defaultAdvertiseInterval
	}
	return &SSDPAdvertiser{
		cfg:       cfg,
		targets:   NotifyTargetsFor(cfg.UDN),
		searchSem: make(chan struct{}, maxConcurrentMSearchResponders),
	}
}

// Start binds the multicast listener and unicast sender, sends an
// initial NOTIFY ssdp:alive burst (one packet per NotifyTarget), then
// spawns the periodic NOTIFY and M-SEARCH-response goroutines.
//
// Returns an error if socket binding fails. On success, the
// advertiser is running until Stop() is called.
func (s *SSDPAdvertiser) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Refuse double-Start. Without this guard, repeated calls would
	// spawn additional listener / sender / goroutine pairs that
	// each tried to bind the same multicast address — first call
	// succeeds, subsequent calls error from socket binding, but the
	// log line would be misleading ("SSDP advertiser started" with
	// no indication that we're now on the second instance). Per
	// CodeRabbit Major on PR #303.
	if s.started {
		return errors.New("dlna: SSDP advertiser already started")
	}

	if s.log == nil {
		s.log = s.cfg.Logger
		if s.log == nil {
			s.log = ssdpLogger
		}
	}

	addr, err := net.ResolveUDPAddr("udp4", SSDPMulticastAddr)
	if err != nil {
		return err
	}

	// Multicast listener (joined to the SSDP group)
	listener, err := net.ListenMulticastUDP("udp4", s.cfg.Interface, addr)
	if err != nil {
		return err
	}

	// Unicast sender (dials the multicast address for outgoing NOTIFY).
	// `net.DialUDP` alone doesn't bind the outgoing multicast interface —
	// the OS routes via its default multicast interface, which may differ
	// from `s.cfg.Interface` on a multi-homed host (LAN + Tailscale +
	// Ethernet). When `s.cfg.Interface` is non-nil, we wrap the sender
	// in `ipv4.PacketConn` and explicitly set the multicast interface
	// so the NOTIFY ssdp:alive bursts land on the LAN where renderers
	// live. Per Gemini Medium on PR #303.
	sender, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		listener.Close()
		return err
	}
	if s.cfg.Interface != nil {
		// Pin outgoing multicast to the operator-chosen interface.
		// `SetMulticastInterface` is a connection-level option set
		// before any packets are sent; it applies to subsequent
		// `WriteTo` / `Write` calls on the underlying socket. The
		// `ipv4.NewPacketConn` wrap is non-destructive — the
		// underlying `*net.UDPConn` continues to function for
		// direct Write calls, which is how `sendAlive` / `sendByebye`
		// use it.
		if err := ipv4.NewPacketConn(sender).SetMulticastInterface(s.cfg.Interface); err != nil {
			// Soft-fail: log + continue. A failure here means
			// multicast goes via the OS default — degraded but
			// not broken; renderers on that interface still
			// discover us. Refusing to start over an unbindable
			// multicast option would punish the typical happy
			// path (single-NIC host where the OS default IS the
			// LAN).
			s.log.Warn("SSDP multicast interface bind failed (falling back to OS default)",
				slog.String("interface", s.cfg.Interface.Name),
				slog.String("err", err.Error()))
		}
	}

	s.listener = listener
	s.sender = sender

	// Internal context layered on top of the caller's so Stop() can
	// cancel even if the caller passed a long-lived context.
	innerCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	// Initial NOTIFY ssdp:alive burst. Pass the local `sender` (not
	// `s.sender`) — the goroutines below capture their own socket
	// copies too, so no background path reads the struct fields. This
	// is what makes `Stop()` nil-ing `s.listener`/`s.sender` race-free:
	// every concurrent reader holds a local copy, and the only struct-
	// field access (Start write, Stop close+nil) is serialized under
	// `s.mu`.
	s.sendAliveAll(sender)

	// Periodic NOTIFY goroutine — captures `sender` locally.
	s.wg.Add(1)
	go s.runPeriodicNotify(innerCtx, sender)

	// M-SEARCH response goroutine — captures `listener` locally.
	s.wg.Add(1)
	go s.runMSearchListener(innerCtx, listener)

	// Flip the state-machine flag AFTER all socket binds + goroutine
	// spawns succeed. Set on the happy path only; a partial-init
	// failure above returns early WITHOUT flipping the flag, so a
	// subsequent retry call sees `started == false` and is allowed.
	s.started = true

	s.log.Info("SSDP advertiser started",
		slog.String("location", s.cfg.Location),
		slog.String("interface", interfaceName(s.cfg.Interface)),
		slog.Duration("advertiseInterval", s.cfg.AdvertiseInterval),
		slog.Int("targetCount", len(s.targets)),
	)
	return nil
}

// Stop sends a final NOTIFY ssdp:byebye burst (one packet per target),
// cancels the goroutine context, closes the listener (which unblocks
// the M-SEARCH ReadFromUDP), and waits for both goroutines to exit.
//
// Safe to call exactly once after Start(). Calling Stop() before Start()
// or twice is a no-op (defensive — won't panic).
func (s *SSDPAdvertiser) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return // never started, or already stopped
	}
	// Flip the flag early — even if subsequent teardown panics or
	// hangs, a follow-up Start() call should see a fresh slate.
	s.started = false

	if s.cancel == nil {
		return // defensive — shouldn't happen given the started flag
	}

	// Send byebye burst BEFORE cancelling so renderers see our farewell.
	// Pass `s.sender` explicitly (we hold `s.mu` and `started == true`,
	// so it's non-nil here). Best-effort cleanup.
	s.sendByebyeAll(s.sender)

	s.cancel()
	s.cancel = nil

	// Close the listener + sender to unblock the goroutines' reads
	// (ReadFromUDP / Write on a closed socket return an error, which
	// the goroutines observe alongside the cancelled ctx and exit).
	// Close errors are intentionally swallowed — we're tearing down.
	//
	// Nil-ing the struct fields here is race-free: the goroutines read
	// their captured local copies, never `s.listener`/`s.sender`, so
	// the only struct-field access is this lock-held Stop and the
	// lock-held Start. We close-then-nil BEFORE `wg.Wait()` (closing
	// is what unblocks the reads — waiting first would deadlock the
	// M-SEARCH goroutine parked in `ReadFromUDP`).
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
	if s.sender != nil {
		_ = s.sender.Close()
		s.sender = nil
	}

	s.wg.Wait()
	s.log.Info("SSDP advertiser stopped")
}

// runPeriodicNotify sends a NOTIFY ssdp:alive burst every
// `cfg.AdvertiseInterval`, until context cancels. Takes the `sender`
// socket as a parameter (the local copy captured at Start) so it never
// reads `s.sender` — that's what keeps `Stop()`'s nil-set race-free.
func (s *SSDPAdvertiser) runPeriodicNotify(ctx context.Context, sender *net.UDPConn) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.AdvertiseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sendAliveAll(sender)
		}
	}
}

// runMSearchListener blocks on the multicast listener, parsing each
// incoming packet for M-SEARCH and responding unicast if the ST
// matches one of our NotifyTargets. Takes the `listener` socket as a
// parameter (the local copy captured at Start) so it never reads
// `s.listener` — that's what keeps `Stop()`'s nil-set race-free.
func (s *SSDPAdvertiser) runMSearchListener(ctx context.Context, listener *net.UDPConn) {
	defer s.wg.Done()
	buf := make([]byte, 2048) // SSDP packets are small; 2KB is plenty
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// ReadFromUDP blocks; the Stop() path closes the listener
		// to wake us up. Read errors are expected at teardown time.
		n, src, err := listener.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return // expected — Stop() closed the listener
			default:
			}
			s.log.Debug("SSDP listener read error",
				slog.String("err", err.Error()))
			continue
		}
		// Offload the response to a bounded worker goroutine so the
		// listener returns to `ReadFromUDP` immediately — `handleMSearch`
		// sleeps (the spec-mandated MX delay) and does blocking socket
		// I/O, which must not stall the receive loop (a slow / full send
		// buffer would otherwise drop concurrent searches from other
		// renderers). `buf` is reused on the next iteration, so the
		// packet MUST be copied before it crosses the goroutine boundary.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		select {
		case s.searchSem <- struct{}{}:
			// This inner s.wg.Add(1) is safe to run concurrently with
			// Stop()'s s.wg.Wait(), and does NOT risk the "WaitGroup
			// misuse: Add called concurrently with Wait" panic. The panic
			// only fires when a positive Add raises the counter UP FROM
			// ZERO while a Wait is in progress. That can't happen here:
			// this listener goroutine holds its OWN wg slot (the
			// `defer s.wg.Done()` at the top of runMSearchListener) for
			// its entire loop lifetime, so the counter is always ≥1 at
			// this point — this Add takes it ≥1→≥2, never 0→1. And
			// Stop()'s Wait() cannot return until this listener returns
			// (after the socket is closed and the loop exits), by which
			// time no further Adds are issued. Verified false-positive
			// against an external review (2026-05-31); do not "fix" by
			// moving the Add under s.mu or behind an atomic gate.
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() { <-s.searchSem }()
				s.handleMSearch(ctx, pkt, src)
			}()
		default:
			// Responder pool full — drop. The control point re-searches;
			// SSDP discovery is best-effort by design.
			s.log.Debug("M-SEARCH responder pool full; dropping",
				slog.String("src", src.String()))
		}
	}
}

// handleMSearch parses an incoming packet for M-SEARCH, waits the
// spec-mandated random [0, MX] delay, then sends one unicast response
// per matching NotifyTarget back to the source over a SINGLE transient
// socket. Runs on a bounded worker goroutine (see `runMSearchListener`).
// Pure routing logic; the packet builders + target matcher + MX parser
// are tested in isolation in ssdp_packet_test.go.
func (s *SSDPAdvertiser) handleMSearch(ctx context.Context, packet []byte, src *net.UDPAddr) {
	st := ParseMSearchST(packet)
	if st == "" {
		return // not an M-SEARCH or missing ST — silently drop
	}
	matches := MSearchTargets(st, s.targets)
	if len(matches) == 0 {
		return // ST didn't match anything we advertise
	}

	// Spec-mandated randomized reply delay in [0, MX]. Missing/zero MX
	// falls back to a small fixed jitter so simultaneous searches still
	// don't reply in a synchronized burst. The sleep is `select`-
	// cancellable so `Stop()` unparks a sleeping responder instantly
	// rather than leaking it for up to MX seconds.
	var window time.Duration
	if mx := ParseMSearchMX(packet); mx > 0 {
		window = time.Duration(mx) * time.Second
	} else {
		window = defaultMSearchJitter
	}
	delay := time.Duration(rand.Int64N(int64(window) + 1))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	// Dial the transient unicast socket ONCE (not per target). An
	// `ssdp:all` search matches every NotifyTarget; re-dialing the same
	// `src` per match is pure socket churn. One datagram per target/USN
	// is still correct UPnP — only the socket is shared.
	conn, err := net.DialUDP("udp4", nil, src)
	if err != nil {
		s.log.Debug("M-SEARCH response dial failed",
			slog.String("src", src.String()),
			slog.String("err", err.Error()))
		return
	}
	defer conn.Close()

	now := time.Now()
	for _, target := range matches {
		response := BuildMSearchResponse(s.cfg.Location, s.cfg.ServerToken, target.NT, target.USN, now)
		if _, err := conn.Write(response); err != nil {
			s.log.Debug("M-SEARCH response write failed",
				slog.String("src", src.String()),
				slog.String("nt", target.NT),
				slog.String("err", err.Error()))
		}
	}
}

// sendAliveAll sends one NOTIFY ssdp:alive packet per NotifyTarget,
// in order. Takes the `sender` socket explicitly — callers pass their
// thread-local copy so this never touches `s.sender` (race-free with
// `Stop()`'s nil-set). Logs errors at Debug level (transient send
// failures are not actionable; the next periodic tick retries). The
// nil guard is on the PARAMETER (defensive — non-nil in practice on
// every live caller).
func (s *SSDPAdvertiser) sendAliveAll(sender *net.UDPConn) {
	if sender == nil {
		return
	}
	for _, target := range s.targets {
		pkt := BuildNotifyAlive(s.cfg.Location, s.cfg.ServerToken, target)
		if _, err := sender.Write(pkt); err != nil {
			s.log.Debug("NOTIFY alive send failed",
				slog.String("nt", target.NT),
				slog.String("err", err.Error()))
		}
	}
}

// sendByebyeAll sends one NOTIFY ssdp:byebye packet per NotifyTarget.
// Takes the `sender` socket explicitly (parameter nil guard is
// defensive — Stop() passes a non-nil `s.sender` under the lock).
func (s *SSDPAdvertiser) sendByebyeAll(sender *net.UDPConn) {
	if sender == nil {
		return
	}
	for _, target := range s.targets {
		pkt := BuildNotifyByeBye(s.cfg.Location, s.cfg.ServerToken, target)
		_, _ = sender.Write(pkt) // best-effort; we're shutting down
	}
}

// interfaceName returns the interface's Name field, or "(any)" if nil
// (the OS-pick case). Slog helper — keeps the Start log message tidy.
func interfaceName(iface *net.Interface) string {
	if iface == nil {
		return "(any)"
	}
	return iface.Name
}
