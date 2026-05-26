package dlna

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

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
	Interface *net.Interface

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

// Server is the DLNA MediaServer runtime — orchestrates the HTTP
// listener (DLNA endpoints) + SSDP advertiser + telemetry under one
// start/stop unit. Wraps the project's standard goroutine-and-
// WaitGroup lifecycle pattern.
type Server struct {
	cfg        ServerConfig
	mux        *http.ServeMux
	httpServer *http.Server
	ssdp       *SSDPAdvertiser
	log        *slog.Logger
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
	if cfg.ListenAddress == "" {
		return nil, errors.New("dlna: ServerConfig.ListenAddress required")
	}
	if cfg.ServerURL == "" {
		return nil, errors.New("dlna: ServerConfig.ServerURL required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default().With(slog.String("component", "dlna"))
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

	// Start the SSDP advertiser. If it fails, tear down the HTTP
	// listener so we don't leak the bind.
	s.ssdp = NewSSDPAdvertiser(SSDPConfig{
		UDN:         s.cfg.UDN,
		Location:    s.cfg.ServerURL + "/dlna/description.xml",
		ServerToken: SSDPServerToken(s.cfg.ModelNumber),
		Interface:   s.cfg.Interface,
		Logger:      s.log,
	})
	if err := s.ssdp.Start(ctx); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(shutdownCtx)
		return fmt.Errorf("dlna: SSDP start: %w", err)
	}

	s.log.Info("DLNA server started",
		slog.String("listenAddress", s.cfg.ListenAddress),
		slog.String("serverURL", s.cfg.ServerURL),
		slog.String("udn", s.cfg.UDN),
		slog.String("friendlyName", s.cfg.FriendlyName),
		slog.Bool("telemetryEnabled", s.cfg.TelemetryStore != nil),
	)
	return nil
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
	if s.ssdp != nil {
		s.ssdp.Stop()
		s.ssdp = nil
	}
	if s.httpServer == nil {
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
//	SUBSCRIBE/UNSUBSCRIBE /dlna/cds/event  GENA stub (no-op success)
//	SUBSCRIBE/UNSUBSCRIBE /dlna/cm/event   GENA stub (no-op success)
//
// GENA event subscription is stubbed — we accept the subscription
// (200 OK with synthesized SID) but never send NOTIFY events. Most
// renderers tolerate this gracefully; the alternative (refusing
// SUBSCRIBE with 404) makes some renderers blacklist the service.
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
		// Per-request serverURL — prefer the static ServerURL config
		// (matches the SSDP LOCATION renderers cached), fall back to
		// the request's Host header for non-LAN-IP-bound deployments.
		if s.cfg.ServerURL != "" {
			return s.cfg.ServerURL
		}
		return "http://" + r.Host
	})
	s.mux.Handle("/dlna/cds/control", cdsHandler)
	s.mux.Handle("/dlna/cm/control", ConnectionManagerHandler())

	s.mux.Handle("/dlna/file/", FileHandler(s.cfg.Library))

	// GENA event stubs — accept SUBSCRIBE / UNSUBSCRIBE without
	// actually delivering events. The HandlerFunc inspects r.Method
	// rather than relying on the mux (which doesn't dispatch on
	// SUBSCRIBE/UNSUBSCRIBE method names).
	s.mux.HandleFunc("/dlna/cds/event", genaStubHandler("cds", s.log))
	s.mux.HandleFunc("/dlna/cm/event", genaStubHandler("cm", s.log))
}

// genaStubHandler returns a no-op SUBSCRIBE/UNSUBSCRIBE handler that
// accepts the subscription with a synthesized SID but never sends
// NOTIFY events. The bridge has no state-change events to push (our
// library is read-only from the renderer's perspective), so the
// "subscription" is purely a courtesy to renderers that expect to
// be able to subscribe.
func genaStubHandler(label string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "SUBSCRIBE":
			sid := fmt.Sprintf("uuid:dlna-%s-stub-%d", label, time.Now().UnixNano())
			w.Header().Set("SID", sid)
			w.Header().Set("TIMEOUT", "Second-1800")
			w.WriteHeader(http.StatusOK)
			log.Debug("GENA SUBSCRIBE stubbed",
				slog.String("service", label),
				slog.String("sid", sid),
				slog.String("callback", r.Header.Get("CALLBACK")))
		case "UNSUBSCRIBE":
			w.WriteHeader(http.StatusOK)
			log.Debug("GENA UNSUBSCRIBE stubbed",
				slog.String("service", label),
				slog.String("sid", r.Header.Get("SID")))
		default:
			http.Error(w, "SUBSCRIBE or UNSUBSCRIBE only", http.StatusMethodNotAllowed)
		}
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
