package dlna

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// genaInitialNotifyTimeout bounds the single best-effort initial NOTIFY
// POST sent to a control point's callback URL after a successful
// SUBSCRIBE. One attempt, no retries.
const genaInitialNotifyTimeout = 5 * time.Second

// packageLogger is the package-scoped slog handler. Mirrors the
// convention from `internal/admin`, `internal/api`, `internal/auth`,
// `internal/config`, `internal/enrich`, `internal/integrity` (CLAUDE.md
// logging-convention invariant) so log lines from the DLNA package
// land under `component=dlna` regardless of whether the caller passed
// a custom logger via `ServerConfig.Logger`. Per CodeRabbit Major on
// PR #303.
var packageLogger = logging.Component("dlna")

// ServerConfig configures a DLNAServer. All fields are required EXCEPT
// where noted. The caller (cmd/bridge/main.go via PR 1 task #12) is
// responsible for computing the ServerURL from the live interface IP
// + ListenAddress, picking a LAN-eligible Interface, providing a
// stable UDN, etc.
type ServerConfig struct {
	// Library is the data source the ContentDirectory + file handlers
	// query. Required.
	Library LibrarySource

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
	// the Wait). `notifyClient` is the shared timeout-bounded HTTP client.
	notifyCtx    context.Context
	notifyCancel context.CancelFunc
	notifyWG     sync.WaitGroup
	notifyClient *http.Client
}

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
	s.notifyClient = &http.Client{Timeout: genaInitialNotifyTimeout}

	s.mux = http.NewServeMux()
	s.mountHandlers()

	// Telemetry middleware wraps the entire mux. nil-store passes
	// through transparently, so this works whether telemetry is
	// enabled or not.
	handler := TelemetryMiddleware(s.cfg.TelemetryStore, s.mux)

	s.httpServer = &http.Server{
		Addr:              s.cfg.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second, // defensive against slowloris-style attacks
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
	go func() {
		if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
		// ServerURL only when Host is somehow empty.
		if r.Host != "" {
			return "http://" + r.Host
		}
		return s.cfg.ServerURL
	})
	s.mux.Handle("/dlna/cds/control", cdsHandler)
	s.mux.Handle("/dlna/cm/control", ConnectionManagerHandler())

	s.mux.Handle("/dlna/file/", FileHandler(s.cfg.Library))

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
// state. Guarded against being turned into an SSRF relay: the callback
// host must be an on-LAN IP literal (loopback / RFC1918 / link-local) OR
// match the SUBSCRIBE request's source IP; hostnames and arbitrary
// public IPs are rejected (the NOTIFY is silently skipped — the
// SUBSCRIBE already returned 200). Timeout-bounded, no retries, and
// cancellable via the server's notify context so Stop() unparks it.
func (s *Server) fireInitialNotify(service, sid, callbackHeader, remoteAddr string) {
	target := firstCallbackURL(callbackHeader)
	if target == "" {
		return
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return
	}
	if !callbackHostAllowed(u.Hostname(), remoteAddr) {
		s.log.Debug("GENA initial NOTIFY skipped — callback host not on LAN",
			slog.String("service", service),
			slog.String("callbackHost", u.Hostname()))
		return
	}

	body := initialNotifyBody(service)
	s.notifyWG.Add(1)
	go func() {
		defer s.notifyWG.Done()
		req, err := http.NewRequestWithContext(s.notifyCtx, "NOTIFY", target, strings.NewReader(body))
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

// firstCallbackURL extracts the first `<...>`-delimited URL from a GENA
// CALLBACK header. The header may carry multiple space-separated
// `<url>` entries; we use the first. Returns "" if malformed.
func firstCallbackURL(header string) string {
	header = strings.TrimSpace(header)
	start := strings.IndexByte(header, '<')
	if start < 0 {
		return ""
	}
	rest := header[start+1:]
	end := strings.IndexByte(rest, '>')
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// callbackHostAllowed is the SSRF guard for the initial NOTIFY. A
// renderer's event sink is always on-LAN, so we allow only IP literals
// that are loopback / RFC1918-private / link-local, OR that match the
// SUBSCRIBE request's source IP (covers a CGNAT renderer whose callback
// host equals its own source address). Hostnames and arbitrary public
// IPs are rejected.
func callbackHostAllowed(host, remoteAddr string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false // reject hostnames outright
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	srcHost, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// remoteAddr may lack a port (a reverse proxy rewrote it, or a
		// custom test setup) — fall back to treating it as a bare host.
		// A non-IP value still fails the ParseIP check below.
		srcHost = remoteAddr
	}
	srcIP := net.ParseIP(srcHost)
	return srcIP != nil && srcIP.Equal(ip)
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

// PickLANEligibleInterface walks the host's interfaces and returns
// the first one that passes `IsLANEligibleInterface`. Pure helper
// for cmd/bridge/main.go (PR 1 task #12) so the wiring code doesn't
// have to reimplement the interface-walk pattern.
//
// Returns nil + error if no eligible interface found (caller can
// fall back to "OS-pick" with nil interface or refuse to start
// DLNA — bridge config-time decision).
func PickLANEligibleInterface(opts EligibilityOpts) (*net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("net.Interfaces: %w", err)
	}
	for i := range ifaces {
		iface := ifaces[i]
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		if IsLANEligibleInterface(iface, addrs, opts) {
			return &iface, nil
		}
	}
	return nil, errors.New("no LAN-eligible interface found")
}

// PickAllLANEligibleInterfaces walks the host's interfaces and returns
// EVERY one that passes `IsLANEligibleInterface`, in OS enumeration
// order. This is the multi-interface counterpart to
// PickLANEligibleInterface: on a host with both Ethernet and Wi-Fi (or a
// bridged setup) renderers on each subnet need an advertiser bound to
// their interface, otherwise the unselected adapter's renderers never
// see the server.
//
// Returns an empty slice (never errors) when no eligible interface
// exists — the caller decides whether to fall back to an OS-pick / single
// advertiser or skip DLNA entirely.
func PickAllLANEligibleInterfaces(opts EligibilityOpts) []*net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []*net.Interface
	for i := range ifaces {
		iface := ifaces[i]
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		if IsLANEligibleInterface(iface, addrs, opts) {
			out = append(out, &iface)
		}
	}
	return out
}
