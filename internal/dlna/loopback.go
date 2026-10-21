package dlna

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The loopback route is the file route of the DLNA listener, served on this
// host's loopback alone, for the MPD beside the bridge (backlog B473,
// decision 2). When the app casts one of the bridge's own tracks to that
// MPD and MPD's database has no copy at the path rule's path, MPD fetches
// http://127.0.0.1:<port>/dlna/file/<trackID> instead. It runs while
// output.mpd resolves on, whether or not the DLNA server is enabled, and
// it has no SSDP, no ContentDirectory, no artwork and no authentication:
// reach is the gate, as it is for the DLNA listener, and here the reach is
// this host.
//
// What it serves is narrower than the DLNA listener's route. The track id
// names a row of the library index (the same adapter and the same id the
// DLNA listener uses), and the file is served only when, its links
// resolved, it lies inside a configured library root. A rendition is not
// served, and neither is a track the bridge proxies from an upstream
// server. Nothing is listed.

// TrackLookup is what the loopback route reads: a track id to its file.
// The DLNA listener's LibrarySource is one.
type TrackLookup interface {
	GetTrackInfo(trackID string) (TrackInfo, bool)
}

// LoopbackFileHandler serves GET and HEAD of FilePathPrefix + trackID to a
// client on this host's loopback, and refuses everything else with a typed
// JSON refusal (`{"error": code, "message": …}`):
//
//	403 loopback_only       the request did not come from a loopback address
//	405 method_not_allowed  not GET or HEAD
//	404 not_found           no index row with that id, a path that is not
//	                        exactly the file route and one id, a rendition,
//	                        a routed track, or a file that is gone
//	404 not_a_file          the path is a directory, a pipe, a device
//	403 outside_library     the file, its links resolved, is not inside a
//	                        configured library root
//	500 internal            the file could not be opened for another reason
//
// roots is read on every request: the live library roots.
func LoopbackFileHandler(lib TrackLookup, roots func() []string) http.Handler {
	return &loopbackHandler{lib: lib, roots: roots}
}

type loopbackHandler struct {
	lib   TrackLookup
	roots func() []string
	// beforeOpen, when set, runs between the containment check and the
	// open, and afterOpen between the open and the second resolution.
	// Production leaves both nil. A test swaps a link in each window.
	beforeOpen func()
	afterOpen  func()
}

func (h *loopbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The listener binds loopback addresses only. This is the defence for a
	// listener that is ever bound otherwise: the handler itself refuses.
	if !remoteIsLoopback(r.RemoteAddr) {
		refuseLoopback(w, http.StatusForbidden, "loopback_only",
			"this route answers only on this host's loopback")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		refuseLoopback(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or HEAD only")
		return
	}
	trackID, ok := loopbackTrackID(r.URL.Path)
	if !ok {
		refuseLoopback(w, http.StatusNotFound, "not_found", "no library track has that id")
		return
	}
	info, ok := h.lib.GetTrackInfo(trackID)
	if !ok || info.AbsolutePath == "" {
		// An empty path is a track the bridge proxies from an upstream
		// server: not a file in a library root.
		refuseLoopback(w, http.StatusNotFound, "not_found", "no library track has that id")
		return
	}
	var roots []string
	if h.roots != nil {
		roots = h.roots()
	}
	resolved, err := fsutil.ResolveLinks(info.AbsolutePath)
	if err != nil {
		refuseOpen(w, err)
		return
	}
	// Judged before the open, so nothing outside the roots is opened.
	if fsutil.IsUnderAny(resolved, roots) == "" {
		refuseOutside(w)
		return
	}
	if h.beforeOpen != nil {
		h.beforeOpen()
	}
	// Opened the way every route that serves a file opens it: what is not
	// a file is refused, and a named pipe without waiting for a writer.
	f, stat, err := fsutil.OpenAsFile(info.AbsolutePath)
	if err != nil {
		refuseOpen(w, err)
		return
	}
	defer f.Close()
	// The file that opened is a file inside the roots. The path is resolved
	// again after the open: a directory on it replaced by a link out of the
	// roots after the first check sends the open and any later stat of the
	// first resolution down the same link, so only a fresh resolution sees
	// where the open went. That resolution must still be inside, and name
	// the file that opened.
	if h.afterOpen != nil {
		h.afterOpen()
	}
	again, err := fsutil.ResolveLinks(info.AbsolutePath)
	if err != nil || fsutil.IsUnderAny(again, roots) == "" {
		refuseOutside(w)
		return
	}
	if now, err := os.Stat(again); err != nil || !os.SameFile(stat, now) {
		refuseOutside(w)
		return
	}
	serveOpenedFile(w, r, f, stat, info.AbsolutePath, info.FileExtension, trackID)
}

// loopbackTrackID is the id in FilePathPrefix + id. The id is one path
// segment: a request that names anything below it (a rendition, a `..`, a
// path) names no track.
func loopbackTrackID(urlPath string) (string, bool) {
	id, ok := strings.CutPrefix(urlPath, FilePathPrefix)
	if !ok || id == "" || strings.ContainsRune(id, '/') {
		return "", false
	}
	return id, true
}

// remoteIsLoopback reports whether a request's remote address is a
// loopback address of this host (127.0.0.0/8, ::1, or 127/8 mapped into
// IPv6). Anything that does not parse as an address is not.
func remoteIsLoopback(remote string) bool {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	return ap.Addr().Unmap().IsLoopback()
}

func refuseOutside(w http.ResponseWriter) {
	refuseLoopback(w, http.StatusForbidden, "outside_library",
		"the track's file is not inside a library root")
}

// refuseOpen answers a file that could not be resolved or opened.
func refuseOpen(w http.ResponseWriter, err error) {
	switch {
	case fsutil.NotAFileKind(err) != "":
		refuseLoopback(w, http.StatusNotFound, "not_a_file",
			"the track's path is a "+fsutil.NotAFileKind(err)+", not a file")
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		refuseLoopback(w, http.StatusNotFound, "not_found", "no library track has that id")
	default:
		refuseLoopback(w, http.StatusInternalServerError, "internal", "the bridge couldn't open this file")
	}
}

// loopbackRefusal is the body of every refusal: the route's own typed
// envelope, in the shape of the /v1 API's error envelope.
type loopbackRefusal struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func refuseLoopback(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(loopbackRefusal{Error: code, Message: message})
}

// LoopbackConfig configures a LoopbackServer. Port 0 takes a port the
// kernel picks, which tests use; a real bridge passes the configured one,
// so the URLs MPD keeps in its queue survive a restart.
type LoopbackConfig struct {
	Library TrackLookup
	Roots   func() []string
	Port    int
	Logger  *slog.Logger
}

// LoopbackServer serves LoopbackFileHandler on 127.0.0.1 and, when this
// host has it, ::1, on one port, and on no other address.
type LoopbackServer struct {
	cfg LoopbackConfig
	log *slog.Logger

	mu      sync.Mutex
	srv     *http.Server
	lns     []net.Listener
	stopped bool
	serving sync.WaitGroup
}

// NewLoopbackServer returns a server that is not listening yet.
func NewLoopbackServer(cfg LoopbackConfig) *LoopbackServer {
	log := cfg.Logger
	if log == nil {
		log = packageLogger
	}
	return &LoopbackServer{cfg: cfg, log: log}
}

// Start binds 127.0.0.1 and then ::1 on the same port, and serves. A
// failure to bind 127.0.0.1 is the error; ::1 is optional, since a host
// can run without IPv6, and the base URL names 127.0.0.1.
func (s *LoopbackServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("dlna: loopback route stopped")
	}
	if s.srv != nil {
		return nil
	}
	v4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return fmt.Errorf("dlna: loopback route: %w", err)
	}
	lns := []net.Listener{v4}
	port := v4.Addr().(*net.TCPAddr).Port
	if v6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port))); err == nil {
		lns = append(lns, v6)
	} else {
		s.log.Info("loopback route: ::1 is not bound, 127.0.0.1 only", slog.String("err", err.Error()))
	}
	// The DLNA listener's deadlines (B220): a header and a request are
	// bounded, a response is not, because MPD streams for as long as the
	// track plays.
	srv := &http.Server{
		Handler:           LoopbackFileHandler(s.cfg.Library, s.cfg.Roots),
		ReadHeaderTimeout: dlnaReadHeaderTimeout,
		ReadTimeout:       dlnaReadTimeout,
		IdleTimeout:       dlnaIdleTimeout,
		MaxHeaderBytes:    dlnaMaxHeaderBytes,
	}
	s.srv, s.lns = srv, lns
	for _, ln := range lns {
		s.serving.Add(1)
		go func(ln net.Listener) {
			defer s.serving.Done()
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Warn("loopback route stopped serving", slog.String("err", err.Error()))
			}
		}(ln)
	}
	return nil
}

// Addrs are the addresses the server listens on, none before Start.
func (s *LoopbackServer) Addrs() []net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]net.Addr, 0, len(s.lns))
	for _, ln := range s.lns {
		out = append(out, ln.Addr())
	}
	return out
}

// BaseURL is http://127.0.0.1:<port> while the server serves, and empty
// before Start and after Stop. A track is BaseURL + FilePathPrefix + id.
func (s *LoopbackServer) BaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || len(s.lns) == 0 {
		return ""
	}
	return "http://" + s.lns[0].Addr().String()
}

// Stop stops the server: the listeners close at once, and a response
// still streaming when ctx ends is cut off rather than waited for. A
// stopped server does not start again.
func (s *LoopbackServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.stopped = true
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	s.serving.Wait()
	return err
}
