package dlna

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/upnpproxy"
)

// genaInitialNotifyTimeout bounds the single best-effort initial NOTIFY
// POST sent to a control point's callback URL after a successful
// SUBSCRIBE. One attempt, no retries.
const genaInitialNotifyTimeout = 5 * time.Second

const (
	// How many initial NOTIFY deliveries may be in flight at once.
	genaNotifyPool = 8

	// How long the listener waits for request headers.
	dlnaReadHeaderTimeout = 10 * time.Second

	// How long the listener waits for a whole request, body included.
	// WriteTimeout stays unset: a renderer streams for as long as the
	// file takes.
	dlnaReadTimeout = 60 * time.Second

	// How long a keep-alive connection may sit idle after a response.
	dlnaIdleTimeout = 120 * time.Second

	// Extra bytes net/http reads past MaxHeaderBytes. Go 1.26.6's
	// initialReadLimitSize adds this bufio lookahead.
	dlnaHeaderReadSlop = 4096

	// Largest header block the listener reads. A SOAP or GENA request
	// is a few kilobytes; the net/http default of 1 MiB is not.
	dlnaMaxHeaderRead = 16 << 10

	// MaxHeaderBytes that makes dlnaMaxHeaderRead the read limit,
	// once dlnaHeaderReadSlop is added.
	dlnaMaxHeaderBytes = dlnaMaxHeaderRead - dlnaHeaderReadSlop

	// Longest client-supplied string this package stores or logs, in runes.
	dlnaLoggedFieldRunes = 100
)

// packageLogger is the package-scoped slog handler. Mirrors the
// convention from `internal/admin`, `internal/api`, `internal/auth`,
// `internal/config`, `internal/enrich`, `internal/integrity` (CLAUDE.md
// logging-convention invariant) so log lines from the DLNA package
// land under `component=dlna` regardless of whether the caller passed
// a custom logger via `ServerConfig.Logger`. Per CodeRabbit Major on
// PR #303.
var packageLogger = logging.Component("dlna")

// serveGoroutineHookForTests, when non-nil, is invoked by the HTTP serve
// goroutine BEFORE it touches the *http.Server captured at spawn time. It
// receives a channel that the same goroutine closes when it returns, so a
// test can join the goroutine deterministically.
//
// Test-only seam — nil in production, where the cost is one nil check per
// Start. It exists because the window the capture above guards (Start spawns
// the goroutine, every SSDP advertiser then fails, and the caller's defensive
// Stop() lands before the goroutine is first scheduled) is otherwise
// reachable only by scheduler luck, and a test that cannot pin the interleave
// pins nothing. Production code MUST NOT set it; only tests, which restore it
// via t.Cleanup. Same convention as afterExtractHookForTests in
// internal/manifest and renameFunc in internal/manifest/extractors.go.
var serveGoroutineHookForTests func(exited <-chan struct{})

// ServerConfig configures a DLNAServer. All fields are required EXCEPT
// where noted. The caller (cmd/bridge/main.go via PR 1 task #12) is
// responsible for computing the ServerURL from the live interface IP
// + ListenAddress, picking a LAN-eligible Interface, providing a
// stable UDN, etc.
type ServerConfig struct {
	// Library is the data source the ContentDirectory + file handlers
	// query. Required.
	Library LibrarySource

	// UPnPRouting + UPnPProxy wire the UPnP-routed file handler
	// fast-path — when both are non-nil, the file handler proxies
	// upstream MediaServer bytes (e.g. a Chord 2Go's microSD card)
	// instead of trying to open a non-existent local file. Pre-this-
	// pkg the `/dlna/file/{trackID}` path returned 404 for UPnP-
	// routed tracks, so a user casting a 2Go-sourced track to any
	// DLNA renderer through the bridge got a silent decline. Pair
	// these two fields together — wiring just one disables the
	// fast-path. Both `nil` keeps the legacy filesystem-only
	// behaviour.
	UPnPRouting upnpproxy.RoutingLookup
	UPnPProxy   *upnpproxy.Proxy

	// Artwork, when non-nil, mounts `/dlna/artwork/{key}` (ArtworkHandler)
	// and makes the ContentDirectory emit `<upnp:albumArtURI>` on items and
	// on the folder containers that hold them. nil keeps both off: no route,
	// and no URI pointing at a route that is not there — a strict renderer
	// that 404s an albumArtURI may decline the whole item, so the emission is
	// gated on the SAME field as the mount rather than on the track carrying
	// a key. Optional; the wiring layer passes the api server.
	Artwork ArtworkSource

	// VariantLocator, when non-nil, gets one chance to say where a
	// variant sidecar moved to before a missing one becomes a 410 —
	// the index carries the path the row RECORDED, which a moved
	// variants directory invalidates wholesale. Consulted only on the
	// open failure, so it costs nothing when the file is where the
	// index says. Optional; the wiring layer passes an adapter over the
	// manifest store. See the interface's docblock in
	// content_directory.go.
	VariantLocator VariantLocator

	// UDN is the device's stable unique identifier WITH the `uuid:`
	// prefix (e.g. "uuid:f1b3a5c2-..."). Required. Should remain stable
	// across bridge restarts so renderers don't re-add us on every
	// boot — derive from a persisted random UUID or from a hash of
	// the bridge's stable identity (host + LibraryRoots).
	UDN string

	// FriendlyName, Manufacturer, ManufacturerURL, ModelDescription,
	// ModelName, ModelNumber — vendor identity fields surfaced in the
	// device description XML. FriendlyName is the only one users see
	// in renderer pickers; others are diagnostic.
	FriendlyName     string
	Manufacturer     string
	ManufacturerURL  string
	ModelDescription string
	ModelName        string
	ModelNumber      string

	// ListenAddress is the bind address for the HTTP listener (e.g.
	// "0.0.0.0:7790" or "192.168.0.14:7790"). The bridge picks LAN-only
	// interfaces — public-mode deployments REFUSE DLNA per
	// `shouldEnableDLNA` (PR 1 task #4 invariant).
	ListenAddress string

	// ServerURL is the absolute URL renderers should use for file
	// fetches + service control. Surfaced in:
	//   - SSDP NOTIFY LOCATION header
	//   - DIDL-Lite <res> file URLs (via the per-request serverURLFunc
	//     in ContentDirectoryHandler — defaults to "http://" + r.Host
	//     which composes naturally with this)
	//
	// Typically `"http://" + LAN-IP + ":" + port`. Required.
	ServerURL string

	// Interface is the LAN-eligible interface for SSDP multicast
	// (joined to 239.255.255.250:1900). nil = OS picks (works on
	// single-interface hosts). Multi-interface hosts should pick via
	// `IsLANEligibleInterface`.
	//
	// Used only for the single-advertiser fallback path — when
	// AdvertiseEndpoints is non-empty, the per-endpoint Interface wins
	// and this field is ignored.
	Interface *net.Interface

	// AdvertiseEndpoints, when non-empty, makes the server start ONE SSDP
	// advertiser per listed endpoint — each binding the multicast
	// listener on its own interface and announcing a per-interface
	// LOCATION URL. This is the multi-interface path: a renderer on any
	// LAN subnet receives a description URL reachable from its own subnet
	// (a single static LOCATION would hand secondary-subnet renderers a
	// URL on the primary interface's IP, which fails when cross-subnet
	// routing is restricted).
	//
	// When empty, the server falls back to a single advertiser using
	// Interface + ServerURL — the original single-interface behaviour,
	// preserved for callers that pin a specific bind host.
	AdvertiseEndpoints []AdvertiseEndpoint

	// TelemetryStore receives per-request entries via the telemetry
	// middleware. nil = telemetry disabled (middleware passes
	// through). Bridge config (PR 1 task #12) maps
	// `cfg.DLNA.TelemetryEnabled` to either NewTelemetryStore(0) or nil.
	TelemetryStore *TelemetryStore

	// Logger is the structured logger for server-level events
	// (start, stop, panics). nil → slog.Default() with the dlna
	// component tag.
	Logger *slog.Logger
}

// AdvertiseEndpoint pairs a LAN interface with the absolute base URL
// (scheme+host+port, NO trailing path) that the SSDP advertiser on that
// interface announces as its LOCATION. The host MUST be reachable by
// renderers on that interface's subnet — typically the interface's own
// IPv4 literal.
type AdvertiseEndpoint struct {
	Interface *net.Interface
	ServerURL string
}

// Server is the DLNA MediaServer runtime — orchestrates the HTTP
// listener (DLNA endpoints) + SSDP advertiser + telemetry under one
// start/stop unit. Wraps the project's standard goroutine-and-
// WaitGroup lifecycle pattern.
type Server struct {
	cfg        ServerConfig
	mux        *http.ServeMux
	httpServer *http.Server
	ssdps      []*SSDPAdvertiser // one per advertise endpoint (≥1 once started)
	log        *slog.Logger

	// GENA initial-NOTIFY lifecycle. `notifyCtx` is cancelled by Stop so
	// an in-flight best-effort NOTIFY POST can't outlive the server;
	// `notifyWG` lets Stop drain in-flight notifies (Adds happen inside
	// the SUBSCRIBE handler, which `httpServer.Shutdown` drains before
	// the Wait). `notifyClient` is the shared client newNotifyClient
	// builds: timeout-bounded, no redirect, no proxy, every connect
	// checked against the SUBSCRIBE's source.
	notifyCtx    context.Context
	notifyCancel context.CancelFunc
	notifyWG     sync.WaitGroup
	notifyClient *http.Client
	// notifySlots bounds how many initial NOTIFYs are in flight. A full
	// pool drops that NOTIFY. The SUBSCRIBE has already answered 200.
	notifySlots     chan struct{}
	notifySlotsOnce sync.Once
	// readTimeout and idleTimeout replace the production deadlines when
	// a test sets them before Start. Zero keeps the production value.
	readTimeout time.Duration
	idleTimeout time.Duration

	// Observation state for the callback-vs-source divergence warning
	// (see callbackHostMatchesSource) and the host-local refusal warning
	// (noteCallbackRefusal), a set for each under one mutex. One Warn per
	// distinct (callbackHost, sourceIP) pair, bounded, so a control point
	// that re-subscribes on a timer produces one line rather than one per
	// renewal — this project has been bitten by a per-tick log before
	// (199,078 of 200,000 lines from one ticker). Two sets, because a peer
	// reaches the refusal at will: sharing one bound let refused pairs use
	// it up and silence the divergence lines, which are the evidence the
	// private half of step two waits for.
	callbackDivergeMu   sync.Mutex
	callbackDivergeSeen map[string]struct{}
	callbackRefusedSeen map[string]struct{}

	// interfaceAddrs lists this host's addresses for ownHostOnly; nil is
	// net.InterfaceAddrs. A per-server seam, set before Start by a test
	// that stands in a host of its own.
	interfaceAddrs func() ([]net.Addr, error)

	// hostRefusedSeen holds the Host names ownHostOnly has refused, so
	// each is logged once (noteForeignHost), bounded by
	// hostRefusedSeenCap.
	hostRefusedMu   sync.Mutex
	hostRefusedSeen map[string]struct{}
}

// callbackDivergeSeenCap bounds each observation set. A LAN has a handful
// of control points; anything past this is either a fuzzer or a bug, and
// silently not warning further is the right failure mode for a
// diagnostic.
const callbackDivergeSeenCap = 64

// NewServer constructs a Server with the given config. Does NOT bind
// sockets or spawn goroutines — call Start() to begin. Validates
// required config fields and returns an error if any are missing.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Library == nil {
		return nil, errors.New("dlna: ServerConfig.Library required")
	}
	if cfg.UDN == "" {
		return nil, errors.New("dlna: ServerConfig.UDN required")
	}
	// UPnP spec requires the UDN to carry the `uuid:` prefix. A bare
	// hash without the prefix surfaces as an InvalidUDN error on
	// strict renderers (Sony's older firmwares blacklist the device
	// for the rest of the session on a single bad SUBSCRIBE / NOTIFY
	// exchange). Per CodeRabbit Minor on PR #303 — the wiring layer
	// in `cmd/bridge/dlna_wiring.go::deriveDLNAUDN` already produces
	// the correct shape; this is defence-in-depth against any future
	// caller passing a raw hash.
	if !strings.HasPrefix(cfg.UDN, "uuid:") {
		return nil, errors.New(`dlna: ServerConfig.UDN must carry the "uuid:" prefix (e.g. "uuid:f1b3a5c2-...")`)
	}
	if cfg.ListenAddress == "" {
		return nil, errors.New("dlna: ServerConfig.ListenAddress required")
	}
	if cfg.ServerURL == "" {
		return nil, errors.New("dlna: ServerConfig.ServerURL required")
	}
	log := cfg.Logger
	if log == nil {
		log = packageLogger
	}
	return &Server{cfg: cfg, log: log}, nil
}

// Start binds the HTTP listener, mounts handlers, starts serving in
// a goroutine, then starts the SSDP advertiser. Returns an error if
// listener binding fails OR if SSDP setup fails (in which case the
// HTTP listener is cleanly torn down before returning).
//
// `ctx` controls the SSDP advertiser's lifetime. The HTTP server
// runs independently via its own shutdown (Stop()) — ctx cancellation
// alone doesn't stop the HTTP server, the caller must call Stop().
func (s *Server) Start(ctx context.Context) error {
	// GENA initial-NOTIFY lifecycle — derived from the caller's ctx so
	// ctx cancellation OR Stop() both unpark in-flight notifies. Set up
	// before mountHandlers so the GENA handlers (which read these lazily
	// at request time) always observe non-nil state once serving begins.
	s.notifyCtx, s.notifyCancel = context.WithCancel(ctx)
	s.notifyClient = newNotifyClient()

	// On any error return BEFORE the happy tail hands ownership of the
	// cancel to Stop(), cancel the derived context so the child isn't
	// leaked until the parent ctx ends. A caller that saw Start fail
	// typically won't call Stop() (the site where notifyCancel normally
	// fires), so without this the notify context lingers for the parent
	// ctx's whole lifetime. `started = true` on the success tail transfers
	// ownership to Stop(); notifyCancel is idempotent (a second call is a
	// no-op), so even a caller that DOES call Stop() after a failed Start
	// can't double-cancel.
	started := false
	defer func() {
		if !started {
			s.notifyCancel()
		}
	}()

	handler := s.handler()

	readTO := dlnaReadTimeout
	idleTO := dlnaIdleTimeout
	if s.readTimeout > 0 {
		readTO = s.readTimeout
	}
	if s.idleTimeout > 0 {
		idleTO = s.idleTimeout
	}
	// WriteTimeout stays unset: a renderer streams for as long as the file takes.
	s.httpServer = &http.Server{
		Addr:              s.cfg.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: dlnaReadHeaderTimeout,
		ReadTimeout:       readTO,
		IdleTimeout:       idleTO,
		MaxHeaderBytes:    dlnaMaxHeaderBytes,
	}

	// Eagerly bind the listener so config errors (e.g. port already
	// in use) surface as a Start() error rather than as a silent
	// goroutine crash later.
	listener, err := net.Listen("tcp", s.cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("dlna: bind %s: %w", s.cfg.ListenAddress, err)
	}

	// Run HTTP server in a goroutine. Serve() blocks until Shutdown()
	// is called externally; the returned ErrServerClosed is expected
	// and silently consumed by the goroutine.
	//
	// The server is captured into a local BEFORE the `go` statement and the
	// goroutine MUST NOT read s.httpServer. Stop() writes that field
	// (`s.httpServer = nil`) from another goroutine with no synchronization,
	// and the window is real rather than theoretical: when EVERY SSDP
	// advertiser fails to bind — the documented "no multicast permission on
	// that NIC" case, and the class of environment behind the 2026-05-27
	// Windows+Tailscale incident — Start returns an error and
	// cmd/bridge/dlna_wiring.go's defensive srv.Stop(...) runs, possibly
	// before this goroutine has been scheduled at all. A field read would
	// then hand Serve a nil *http.Server, which nil-derefs inside
	// shouldConfigureHTTP2ForServe — a panic in a bare goroutine, so the
	// whole bridge process dies instead of DLNA merely degrading. Even when
	// the goroutine wins the race, the read/write pair is an unsynchronized
	// data race by the Go memory model; the capture removes both problems,
	// because httpSrv is written and read on this goroutine before the spawn.
	//
	// s.httpServer is the ONLY field Stop writes that a spawned goroutine
	// could observe: s.ssdps is touched solely by Start/Stop on the caller's
	// goroutine, and Stop never writes notifyCtx / notifyClient (it calls
	// notifyCancel, it does not reassign the fields).
	httpSrv := s.httpServer
	go func() {
		if hook := serveGoroutineHookForTests; hook != nil {
			exited := make(chan struct{})
			defer close(exited)
			hook(exited)
		}
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("DLNA HTTP server failed", slog.String("err", err.Error()))
		}
	}()

	// Start one SSDP advertiser per advertise endpoint. On a
	// multi-interface host each interface gets its own multicast
	// listener + per-interface LOCATION; single-interface (or
	// pinned-bind) hosts fall back to one advertiser.
	//
	// Failure policy: a per-interface bind failure (no multicast
	// permission on that NIC, transient flap) is logged and skipped —
	// the bridge keeps advertising on whatever interfaces DID bind. Only
	// if EVERY advertiser fails do we treat SSDP as down: tear the HTTP
	// listener back down and return an error (preserves the
	// single-interface "SSDP failed → Start fails" contract, since that
	// case has exactly one endpoint).
	endpoints := s.advertiseEndpoints()
	for _, ep := range endpoints {
		adv := NewSSDPAdvertiser(SSDPConfig{
			UDN:         s.cfg.UDN,
			Location:    ep.ServerURL + "/dlna/description.xml",
			ServerToken: SSDPServerToken(s.cfg.ModelNumber),
			Interface:   ep.Interface,
			Logger:      s.log,
		})
		if err := adv.Start(ctx); err != nil {
			ifaceName := "default"
			if ep.Interface != nil {
				ifaceName = ep.Interface.Name
			}
			s.log.Warn("DLNA SSDP advertiser failed on interface — skipping",
				slog.String("interface", ifaceName),
				slog.String("location", ep.ServerURL),
				slog.String("err", err.Error()))
			continue
		}
		s.ssdps = append(s.ssdps, adv)
	}
	if len(s.ssdps) == 0 {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownCtx)
		return fmt.Errorf("dlna: SSDP start: no advertiser could bind (tried %d endpoint(s))", len(endpoints))
	}

	s.log.Info("DLNA server started",
		slog.String("listenAddress", s.cfg.ListenAddress),
		slog.String("serverURL", s.cfg.ServerURL),
		slog.Int("ssdpInterfaces", len(s.ssdps)),
		slog.String("udn", s.cfg.UDN),
		slog.String("friendlyName", s.cfg.FriendlyName),
		slog.Bool("telemetryEnabled", s.cfg.TelemetryStore != nil),
	)
	started = true
	return nil
}

// advertiseEndpoints returns the SSDP advertise set: the configured
// per-interface endpoints when present, else a single fallback derived
// from Interface + ServerURL (the original single-advertiser behaviour).
func (s *Server) advertiseEndpoints() []AdvertiseEndpoint {
	if len(s.cfg.AdvertiseEndpoints) > 0 {
		return s.cfg.AdvertiseEndpoints
	}
	return []AdvertiseEndpoint{{Interface: s.cfg.Interface, ServerURL: s.cfg.ServerURL}}
}

// Stop gracefully shuts down both subsystems. The SSDP advertiser
// sends NOTIFY ssdp:byebye for every NotifyTarget so renderers
// purge our entry immediately. The HTTP server is given `ctx`'s
// deadline (or 5 seconds default if ctx has no deadline) to drain
// in-flight requests before being force-closed.
//
// Safe to call exactly once after Start(). Calling before Start()
// or twice is a no-op (defensive — won't panic).
func (s *Server) Stop(ctx context.Context) error {
	for _, adv := range s.ssdps {
		adv.Stop()
	}
	s.ssdps = nil
	// Cancel any in-flight GENA initial-NOTIFY POSTs so they don't
	// outlive the server.
	if s.notifyCancel != nil {
		s.notifyCancel()
	}

	if s.httpServer == nil {
		// Still drain any notify goroutines spawned before this point.
		s.notifyWG.Wait()
		return nil
	}

	// If caller passed a ctx without deadline, set a sensible default
	// so in-flight handlers don't block teardown indefinitely.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	err := s.httpServer.Shutdown(ctx)
	s.httpServer = nil

	// Shutdown drains in-flight SUBSCRIBE handlers — and each handler
	// does its `notifyWG.Add(1)` BEFORE returning — so once Shutdown
	// returns no new notify goroutines can be spawned. Now it's safe to
	// wait for the already-spawned (cancelled) ones to finish.
	s.notifyWG.Wait()
	s.log.Info("DLNA server stopped")
	return err
}

// handler builds the handler tree Start serves: the route map
// (mountHandlers), behind the Host check (ownHostOnly), inside the
// telemetry middleware, which a nil store makes a pass-through. The
// check sits inside telemetry so a refused request is recorded with its
// 421, which is what a report of "the renderer cannot play" needs to see.
func (s *Server) handler() http.Handler {
	s.mux = http.NewServeMux()
	s.mountHandlers()
	return TelemetryMiddleware(s.cfg.TelemetryStore, s.ownHostOnly(s.mux))
}

// mountHandlers wires up the HTTP route map. Each route corresponds
// to one of the spec-required UPnP endpoints:
//
//	GET  /dlna/description.xml      device description XML
//	GET  /dlna/cds.xml              ContentDirectory SCPD
//	GET  /dlna/cm.xml               ConnectionManager SCPD
//	POST /dlna/cds/control          ContentDirectory SOAP control
//	POST /dlna/cm/control           ConnectionManager SOAP control
//	GET/HEAD /dlna/file/{trackID}   file serve (with Range support)
//	GET/HEAD /dlna/silence.wav      1s PCM silence asset (decoder-reset flush)
//	GET/HEAD /dlna/silence/dsd/<fs>.dsf  60s stereo DSD silence (DSD pause)
//	SUBSCRIBE/UNSUBSCRIBE /dlna/cds/event  GENA stub (no-op success)
//	SUBSCRIBE/UNSUBSCRIBE /dlna/cm/event   GENA stub (no-op success)
//
// GENA eventing is minimal: we accept the subscription (200 OK with a
// synthesized SID + SERVER header) and send exactly ONE best-effort
// initial NOTIFY so strict control points (Linn / Naim) see their
// callback tested. No ongoing change-notifications are sent — the
// evented state is intentionally static (SystemUpdateID = 1; fixed
// *ProtocolInfo), so there's nothing to push. Refusing SUBSCRIBE with
// 404 is NOT an option (some renderers blacklist the service).
func (s *Server) mountHandlers() {
	deviceOpts := DeviceDescriptionOpts{
		UDN:              s.cfg.UDN,
		FriendlyName:     s.cfg.FriendlyName,
		Manufacturer:     s.cfg.Manufacturer,
		ManufacturerURL:  s.cfg.ManufacturerURL,
		ModelDescription: s.cfg.ModelDescription,
		ModelName:        s.cfg.ModelName,
		ModelNumber:      s.cfg.ModelNumber,
	}
	s.mux.Handle("/dlna/description.xml", DeviceDescriptionHandler(deviceOpts))
	s.mux.Handle("/dlna/cds.xml", SCPDHandler(ContentDirectorySCPDXML))
	s.mux.Handle("/dlna/cm.xml", SCPDHandler(ConnectionManagerSCPDXML))

	var cdsOpts []CDSOption
	if s.cfg.Artwork != nil {
		cdsOpts = append(cdsOpts, WithAlbumArtURIs())
	}
	cdsHandler := ContentDirectoryHandler(s.cfg.Library, func(r *http.Request) string {
		// Per-request serverURL — PREFER the request's Host header so the
		// DIDL <res> file URLs are reachable from whichever interface /
		// subnet the renderer actually used to reach us. On a
		// multi-interface host the SSDP LOCATION is now per-interface
		// (each advertiser announces its own IP), so the renderer's
		// subsequent SOAP request carries that same host — echoing it
		// back keeps every <res> URL self-consistent with the discovery
		// path. A static ServerURL would hand a secondary-subnet renderer
		// URLs pointing at the primary interface's IP, which fails when
		// cross-subnet routing is restricted. Fall back to the static
		// ServerURL only when Host is empty OR carries characters that
		// would corrupt the constructed URL (path / query / fragment
		// separators — a Host-header-injection guard; net/http normally
		// rejects these, but defense-in-depth is cheap on a URL we embed
		// verbatim in every <res>). Per gemini-code-assist on PR #328.
		if r.Host != "" && !strings.ContainsAny(r.Host, "/\\?#") {
			return "http://" + r.Host
		}
		return s.cfg.ServerURL
	}, cdsOpts...)
	s.mux.Handle("/dlna/cds/control", cdsHandler)
	s.mux.Handle("/dlna/cm/control", ConnectionManagerHandler())

	s.mux.Handle(FilePathPrefix, FileHandler(s.cfg.Library, s.cfg.UPnPRouting, s.cfg.UPnPProxy, s.cfg.VariantLocator))

	// Cover bytes for third-party control points, under `/dlna/file/`'s
	// posture (LAN-only, unauthenticated, opaque key, GET/HEAD). Mounted
	// only when the wiring handed in a source — the CDS's albumArtURI
	// emission keys on the same field (see ServerConfig.Artwork).
	if s.cfg.Artwork != nil {
		s.mux.Handle(ArtworkPathPrefix, ArtworkHandler(s.cfg.Artwork))
	}

	// Silence-flush asset: served as a static 1-second PCM WAV at
	// `/dlna/silence.wav`. iOS dispatches `SetAVTransportURI(<base>
	// /dlna/silence.wav)` between the post-pause Stop and the final
	// Stop to force boutique DAC pipelines (Chord 2Go observed
	// 2026-05-28) through a DSD→PCM clock relock — the relock
	// flushes the FPGA delta-sigma accumulators, killing the
	// residual ringing that a bare Stop leaves behind. Per Gemini
	// consult 2026-05-28. Mounted under the public unauthed
	// listener (same posture as `/dlna/file/`), serves regardless
	// of whether SOAP control is wired.
	s.mux.Handle(SilenceWAVPath, SilenceWAVHandler())

	// DSD silence: a 60 s stereo DSF of 0x69 at one of the eight rates,
	// generated as the request is read. The app switches MPD onto it
	// instead of stopping a DSD cast, which is what makes a docked Hugo 2
	// ring. Same unauthenticated listener as the WAV above. The handler
	// caps concurrent bodies; this mount does not add a log line.
	s.mux.Handle(DSDSilencePathPrefix, DSDSilenceHandler())

	// GENA event handlers — accept SUBSCRIBE / UNSUBSCRIBE, set the
	// SERVER header, and fire one best-effort initial NOTIFY. The
	// HandlerFunc inspects r.Method rather than relying on the mux
	// (which doesn't dispatch on SUBSCRIBE/UNSUBSCRIBE method names).
	s.mux.HandleFunc("/dlna/cds/event", s.genaHandler("cds"))
	s.mux.HandleFunc("/dlna/cm/event", s.genaHandler("cm"))
}

// genaHandler returns the SUBSCRIBE/UNSUBSCRIBE handler for one GENA
// service ("cds" or "cm"). It accepts the subscription with a
// synthesized SID + the mandatory UPnP SERVER header, then sends a
// single best-effort initial NOTIFY (see `fireInitialNotify`). No
// ongoing eventing — the evented state is static by design.
func (s *Server) genaHandler(label string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// SERVER header on EVERY GENA response — UPnP UDA mandates it,
		// and Go's net/http doesn't set it. Strict renderers reject GENA
		// responses lacking it.
		w.Header().Set("Server", SSDPServerToken(s.cfg.ModelNumber))
		switch r.Method {
		case "SUBSCRIBE":
			sid := fmt.Sprintf("uuid:dlna-%s-%d", label, time.Now().UnixNano())
			w.Header().Set("SID", sid)
			w.Header().Set("TIMEOUT", "Second-1800")
			w.WriteHeader(http.StatusOK)
			callback := r.Header.Get("CALLBACK")
			s.log.Debug("GENA SUBSCRIBE accepted",
				slog.String("service", label),
				slog.String("sid", sid),
				slog.String("callback", callback))
			// Best-effort initial NOTIFY AFTER the 200 response so the
			// control point already has the SID when the NOTIFY lands.
			s.fireInitialNotify(label, sid, callback, r.RemoteAddr)
		case "UNSUBSCRIBE":
			w.WriteHeader(http.StatusOK)
			s.log.Debug("GENA UNSUBSCRIBE accepted",
				slog.String("service", label),
				slog.String("sid", r.Header.Get("SID")))
		default:
			http.Error(w, "SUBSCRIBE or UNSUBSCRIBE only", http.StatusMethodNotAllowed)
		}
	}
}

// fireInitialNotify sends a single best-effort GENA initial NOTIFY to
// the control point's callback URL, carrying the service's evented
// state. Timeout-bounded, no retries, and cancellable via the server's
// notify context so Stop() unparks it. The callback is an
// unauthenticated LAN peer's say-so, so the NOTIFY is guarded against
// being turned into a relay twice, as the SSDP clients' fetches are:
//
//   - callbackHostAllowed judges the callback's host before anything is
//     sent: an IP literal (names are refused outright), and on this
//     machine or a link-local address only when it is the address the
//     SUBSCRIBE came from. A refused callback is skipped without a word
//     to the control point; the SUBSCRIBE already returned 200.
//   - newNotifyClient's client follows no redirect and checks every
//     connect against that same source, which rides in the request
//     context. Until backlog B39 the NOTIFY followed the callback's 3xx:
//     a 307/308 re-sent it to any URL, and a 301/302/303 turned it into
//     a GET, which the console's csrfGuard passes, so a peer whose
//     callback was its own address could still aim the bridge anywhere.
//
// **#818's step two is half done.** Its host-local half landed with B39
// (callbackHostAllowed). The other half, refusing a private address
// other than the source (callbackHostMatchesSource), stays held until
// noteCallbackDivergence has watched a bridge with the DLNA listener up.
func (s *Server) fireInitialNotify(service, sid, callbackHeader, remoteAddr string) {
	target := firstCallbackURL(callbackHeader)
	if target == "" {
		return
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return
	}
	host := u.Hostname()
	if !callbackHostAllowed(host, remoteAddr) {
		if callbackNamesThisHostOrLink(host) {
			s.noteCallbackRefusal(service, host, remoteAddr)
			return
		}
		s.log.Debug("GENA initial NOTIFY skipped — callback host not allowed",
			slog.String("service", service),
			slog.String("callbackHost", host))
		return
	}
	s.noteCallbackDivergence(service, host, remoteAddr)
	if s.notifyCtx == nil {
		// Not started (a handler tree mounted without Start): nothing can
		// send, as a nil context always made NewRequestWithContext fail.
		return
	}
	if !s.acquireNotifySlot() {
		return
	}

	body := initialNotifyBody(service)
	ctx := discovery.WithDialApproval(s.notifyCtx, discovery.SubscribedFrom(subscriberAddr(remoteAddr)))
	s.notifyWG.Add(1)
	go func() {
		defer s.notifyWG.Done()
		defer func() { <-s.notifySlots }()
		req, err := http.NewRequestWithContext(ctx, "NOTIFY", target, strings.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("NT", "upnp:event")
		req.Header.Set("NTS", "upnp:propchange")
		req.Header.Set("SID", sid)
		req.Header.Set("SEQ", "0")
		resp, err := s.notifyClient.Do(req)
		if err != nil {
			s.log.Debug("GENA initial NOTIFY failed",
				slog.String("service", service),
				slog.String("err", err.Error()))
			return
		}
		_ = resp.Body.Close()
	}()
}

// acquireNotifySlot takes one of the genaNotifyPool in-flight slots.
// A full pool returns false and the caller drops the NOTIFY.
func (s *Server) acquireNotifySlot() bool {
	s.notifySlotsOnce.Do(func() {
		s.notifySlots = make(chan struct{}, genaNotifyPool)
	})
	select {
	case s.notifySlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// firstCallbackURL extracts the first `<...>`-delimited URL from a GENA
// CALLBACK header. The header may carry multiple space-separated
// `<url>` entries; we use the first.
func firstCallbackURL(header string) string {
	header = strings.TrimSpace(header)
	start := strings.IndexByte(header, '<')
	if start < 0 {
		// UPnP requires <...> delimiters, but some cheap IoT control points
		// omit the opening bracket. Fall back to the trimmed header so a
		// bracket-less but otherwise-valid callback still reaches url.Parse +
		// the callbackHostAllowed SSRF guard rather than being silently
		// dropped. Safe: downstream scheme/host validation can't be bypassed
		// — a non-URL like "no-brackets" still fails there. (review r3)
		return header
	}
	rest := header[start+1:]
	end := strings.IndexByte(rest, '>')
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// callbackHostAllowed is the callback guard for the initial NOTIFY. The
// subscriber here is a CONTROL POINT (BubbleUPnP, mconnect, Kazoo) — not
// a renderer; this is the bridge's own MediaServer GENA handler, and the
// docblock said "renderer" for its whole life. The callback's host must
// be an IP literal (a name is refused outright, and so is an IPv6
// literal with a zone), and then it asks, first, what the NOTIFY's dial
// check will ask at the connect (discovery.SubscribedFrom(...).Permits),
// so the two cannot disagree:
//
//   - a loopback or link-local address, only when it is the address the
//     SUBSCRIBE came from (backlog B39). A loopback address names the
//     host that SENDS the NOTIFY, so a subscriber at another address
//     cannot mean itself by it: until B39 any LAN peer could aim the
//     NOTIFY at the bridge's own loopback services, the loopback-only,
//     unauthenticated console among them. The subscriber's own address,
//     never "any address like it", as for an SSDP LOCATION (#1069): a
//     control point on this host that subscribes over loopback calls
//     back on its own loopback address, and a zero-configuration one on
//     its own link-local address;
//   - the unspecified address and a cloud metadata address (#1074's
//     list, some of them private or public), never.
//
// Then, of what the approval admits:
//
//   - a private (RFC 1918 / ULA) address, from any source: held, see
//     callbackHostMatchesSource;
//   - any other address, only when it is the SUBSCRIBE's source.
func callbackHostAllowed(host, remoteAddr string) bool {
	cb, ok := callbackAddr(host)
	if !ok {
		return false
	}
	from := subscriberAddr(remoteAddr)
	if !discovery.SubscribedFrom(from).Permits(cb) {
		return false
	}
	if cb.IsLoopback() || cb.IsLinkLocalUnicast() || cb.IsPrivate() {
		// A loopback or link-local one IS the source here: nothing else
		// of that kind passed the approval.
		return true
	}
	return cb == from
}

// callbackNamesThisHostOrLink reports whether a callback host is a
// loopback or link-local literal: the kind callbackHostAllowed admits only
// from the subscriber's own address (and a cloud metadata address among
// them never), whose refusal noteCallbackRefusal reports.
func callbackNamesThisHostOrLink(host string) bool {
	cb, ok := callbackAddr(host)
	return ok && (cb.IsLoopback() || cb.IsLinkLocalUnicast())
}

// callbackAddr parses a callback URL's host (url.URL.Hostname) as the
// address the NOTIFY would connect to, unmapped. ok is false for a name and
// for an IPv6 literal with a zone, both of which the guard has always
// refused (net.ParseIP, which it used until B39, takes neither).
func callbackAddr(host string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(host)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// subscriberAddr is the address a SUBSCRIBE came from as the guard compares
// it: RemoteAddr's host (sourceIPOf), unmapped and without a zone. The zero
// Addr when it does not parse, which equals no callback.
func subscriberAddr(remoteAddr string) netip.Addr {
	a, err := netip.ParseAddr(sourceIPOf(remoteAddr))
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap().WithZone("")
}

// newNotifyClient returns the client the GENA initial NOTIFY is sent with,
// discovery.NewDeviceFetchClient: the one the SSDP clients fetch a device
// with. It follows no redirect (a 3xx comes back as the answer), uses no
// proxy and keeps no connection alive, and every connect goes through its
// dial check, which allows this machine or a link-local address only as the
// approval in the request's context permits; fireInitialNotify puts the
// SUBSCRIBE's there (discovery.WithDialApproval, discovery.SubscribedFrom).
// A callback is an IP literal, so the connect targets the address
// callbackHostAllowed judged by that same approval, and the two agree; the
// dial check is the one that still holds if a later change lets a name or
// a redirect through the first.
func newNotifyClient() *http.Client {
	return discovery.NewDeviceFetchClient(genaInitialNotifyTimeout)
}

// callbackHostMatchesSource is the NARROWER predicate #818 planned to move
// callbackHostAllowed to: the callback host must be the same IP that sent
// the SUBSCRIBE. A control point's event sink is, by definition, the device
// that subscribed, so this is the whole of the legitimate case — while the
// blanket private-address acceptance lets a subscriber nominate any other
// host on the network as the NOTIFY target, which is what CodeQL's
// request-forgery alert flags.
//
// Since B39 callbackHostAllowed applies it to loopback and link-local
// callbacks; it is deliberately NOT yet the gate for a private one. Some
// control points are reported to bind their outgoing SUBSCRIBE socket to
// one interface while requesting callbacks on another. This project cannot
// verify that claim about specific hardware from here, so #818 held the
// swap for a release of observation (noteCallbackDivergence). The observer
// shipped, but when B39 landed it had run on no bridge with the DLNA
// listener up (the record is in ops/engineering-log.md, 2026-09-28), so the
// hold stands until one has. Step two then swaps the private arm for this
// and deletes the observer.
func callbackHostMatchesSource(host, remoteAddr string) bool {
	cb, ok := callbackAddr(host)
	return ok && cb == subscriberAddr(remoteAddr)
}

// noteCallbackDivergence logs once per distinct (callbackHost, sourceIP)
// pair when a callback was accepted by the wide predicate but would be
// refused by the narrow one. This is the evidence the held half of step two
// is gated on: a release of silence on a bridge with the DLNA listener up
// means the narrowing is safe; a line here names the exact addresses a real
// device needs. Since B39 only a private callback can reach it.
//
// Deduped and bounded on purpose — a control point that renews its
// subscription on a timer must not turn a diagnostic into a log flood.
func (s *Server) noteCallbackDivergence(service, callbackHost, remoteAddr string) {
	if callbackHostMatchesSource(callbackHost, remoteAddr) {
		return
	}
	// Key on the source IP, NOT the raw remoteAddr. A GENA subscription
	// renewal opens a NEW TCP connection with a new EPHEMERAL PORT, so
	// keying on host:port makes every renewal a fresh key and the dedup
	// does nothing in production — the exact flood this function exists to
	// avoid. (Caught in review; the first version's test used one fixed
	// port and therefore passed against the bug.)
	if !s.firstSighting(&s.callbackDivergeSeen, callbackHost+"|"+sourceIPOf(remoteAddr)) {
		return
	}
	s.log.Warn("GENA callback host differs from the SUBSCRIBE source — accepted for now, will be refused in a future release",
		slog.String("service", service),
		slog.String("callbackHost", callbackHost),
		// The IP, not host:port: the port is ephemeral noise that changes
		// on every renewal, and the address is what a field report needs.
		slog.String("subscribeSource", sourceIPOf(remoteAddr)))
}

// noteCallbackRefusal logs, once per (callbackHost, sourceIP) pair and
// within a bound of its own, a callback refused because it names this machine
// or a link-local address that is not the subscriber's own, or a cloud
// metadata address among them: B39's rule.
// Most such lines are a peer aiming the bridge's NOTIFY at this host or the
// link. A real control point that needs one names itself here, which is
// what a change to the rule would have to see, and it is why this is a
// Warn: a strict control point (Linn, Naim) waits for its initial NOTIFY,
// and nothing else tells anyone why it never came.
func (s *Server) noteCallbackRefusal(service, callbackHost, remoteAddr string) {
	if !s.firstSighting(&s.callbackRefusedSeen, callbackHost+"|"+sourceIPOf(remoteAddr)) {
		return
	}
	s.log.Warn("GENA callback on this machine or a link-local address refused — the NOTIFY goes only to the subscriber's own, never to a cloud metadata address",
		slog.String("service", service),
		slog.String("callbackHost", callbackHost),
		slog.String("subscribeSource", sourceIPOf(remoteAddr)))
}

// firstSighting records key in one warning's observation set (a pointer to
// the Server field, allocated here on first use) and reports whether it is
// new. It answers false for a key already seen AND for every new key once
// the set holds callbackDivergeSeenCap entries: suppress rather than log
// every unseen key forever, so a host manufacturing unique addresses cannot
// turn a diagnostic into a flood by exhausting the cap.
func (s *Server) firstSighting(set *map[string]struct{}, key string) bool {
	s.callbackDivergeMu.Lock()
	defer s.callbackDivergeMu.Unlock()
	if *set == nil {
		*set = make(map[string]struct{}, 8)
	}
	if _, seen := (*set)[key]; seen {
		return false
	}
	if len(*set) >= callbackDivergeSeenCap {
		return false
	}
	(*set)[key] = struct{}{}
	return true
}

// sourceIPOf strips the ephemeral port from a net/http RemoteAddr,
// returning the bare host. Falls back to the input when it carries no
// port (a reverse proxy rewrote it, or a test passed a bare host) —
// callbackHostMatchesSource makes the same allowance.
func sourceIPOf(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// initialNotifyBody returns the GENA `<e:propertyset>` for a service's
// evented state. Minimal but spec-valid: every `sendEvents="yes"` state
// variable from the SCPD appears in its own `<e:property>`. The values
// are intentionally static (the only purpose is to test the callback).
func initialNotifyBody(service string) string {
	const head = `<?xml version="1.0" encoding="utf-8"?>` +
		`<e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0">`
	const tail = `</e:propertyset>`
	switch service {
	case "cm":
		return head +
			`<e:property><SourceProtocolInfo>` + escapeXMLText(SourceProtocolInfo) + `</SourceProtocolInfo></e:property>` +
			`<e:property><SinkProtocolInfo>` + escapeXMLText(SinkProtocolInfo) + `</SinkProtocolInfo></e:property>` +
			`<e:property><CurrentConnectionIDs>0</CurrentConnectionIDs></e:property>` +
			tail
	default: // "cds"
		return head +
			`<e:property><SystemUpdateID>1</SystemUpdateID></e:property>` +
			`<e:property><ContainerUpdateIDs></ContainerUpdateIDs></e:property>` +
			`<e:property><TransferIDs></TransferIDs></e:property>` +
			tail
	}
}

// PickLANEligibleInterface walks the host's interfaces and returns the
// one of those passing `IsLANEligibleInterface` that is most likely the
// LAN: not point-to-point first, then one with a private IPv4, then one
// with any other usable address, then link-local only, and the first in
// OS enumeration order among equals (pickLANInterface). The mDNS
// responder and the DLNA server's single-advertiser fallback bind it.
//
// It returned the FIRST eligible interface until 2026-09-27, and on a
// Mac that is utun0, a system tunnel with only an fe80 address that
// macOS enumerates ahead of en0: the responder listened there and the
// bridge could not be discovered from the LAN.
//
// Returns nil + error if no eligible interface found (caller can
// fall back to "OS-pick" with nil interface or refuse to start
// DLNA — bridge config-time decision).
func PickLANEligibleInterface(opts EligibilityOpts) (*net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("net.Interfaces: %w", err)
	}
	return pickLANInterface(ifaces, hostInterfaceAddrs, opts)
}

// PickAllLANEligibleInterfaces walks the host's interfaces and returns
// EVERY one that passes `IsLANEligibleInterface`, in OS enumeration
// order, except a point-to-point interface with only link-local
// addresses whenever anything else is eligible (pickAllLANInterfaces).
// This is the multi-interface counterpart to PickLANEligibleInterface:
// on a host with both Ethernet and Wi-Fi (or a bridged setup) renderers
// on each subnet need an advertiser bound to their interface, otherwise
// the unselected adapter's renderers never see the server. A tunnel it
// leaves out reaches nothing on the LAN; on the Mac this was measured on
// those were the system utuns, each with only an fe80 address, where an
// SSDP client cannot even send IPv4.
//
// Returns an empty slice (never errors) when no eligible interface
// exists — the caller decides whether to fall back to an OS-pick / single
// advertiser or skip DLNA entirely.
func PickAllLANEligibleInterfaces(opts EligibilityOpts) []*net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	return pickAllLANInterfaces(ifaces, hostInterfaceAddrs, opts)
}
