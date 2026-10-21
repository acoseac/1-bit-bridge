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
	"path/filepath"
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
//	403 loopback_host_only  the Host is not 127.0.0.1, [::1] or localhost
//	                        (any port or none): a name a page re-pointed at
//	                        this host (DNS rebinding), or no Host at all
//	403 forwarded_request   the request carries Forwarded, X-Forwarded-For,
//	                        X-Forwarded-Host or X-Real-IP: a proxy sent it
//	404 not_serving         output.mpd does not resolve on now: enabled
//	                        answered false
//	405 method_not_allowed  not GET or HEAD
//	404 not_found           no index row with that id, a path that is not
//	                        exactly the file route and one id, a rendition,
//	                        a routed track, or a file that is gone
//	404 not_a_file          the path is a directory, a pipe, a device
//	403 outside_library     the file, its links resolved, is not inside a
//	                        configured library root
//	500 internal            the file could not be opened for another reason
//
// roots is read on every request: the live library roots. A configured
// root's own path and its ancestors are trusted as the owner's (a root that
// is a link is followed wherever it points), and the route defends
// everything below a root. enabled is asked on every request too, before
// anything is looked up, because output.mpd can stop resolving on while
// the listener runs. It must answer at once, without I/O: serve passes the
// localOutputs gate's last answer, which never dials MPD. A nil enabled
// serves nothing.
func LoopbackFileHandler(lib TrackLookup, roots func() []string, enabled func() bool) http.Handler {
	return &loopbackHandler{lib: lib, roots: roots, enabled: enabled}
}

type loopbackHandler struct {
	lib     TrackLookup
	roots   func() []string
	enabled func() bool
	// beforeOpen, when set, runs between the containment check and the
	// open, afterOpen between the open and the second resolution, and
	// beforeConfirm between that resolution and the identity check.
	// Production leaves all three nil. A test swaps a link in each window.
	beforeOpen    func()
	afterOpen     func()
	beforeConfirm func()
}

func (h *loopbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	trackID, ok := h.admit(w, r)
	if !ok {
		return
	}
	h.serveTrack(w, r, trackID)
}

// admit answers every refusal that needs no file: who asked, whether the
// route is serving now, the method and the id. It returns the track id of
// a request that may go on.
func (h *loopbackHandler) admit(w http.ResponseWriter, r *http.Request) (string, bool) {
	// The listener binds loopback addresses only. This is the defence for a
	// listener that is ever bound otherwise: the handler itself refuses.
	if !remoteIsLoopback(r.RemoteAddr) {
		refuseLoopback(w, http.StatusForbidden, "loopback_only",
			"this route answers only on this host's loopback")
		return "", false
	}
	// A page a browser loads from a name its author controls can re-point
	// that name at 127.0.0.1 (DNS rebinding): its requests then come from
	// loopback with the page's name as the Host. MPD's name 127.0.0.1, the
	// base URL the bridge hands it.
	if !loopbackHost(r.Host) {
		refuseLoopback(w, http.StatusForbidden, "loopback_host_only",
			"this route answers only a Host of 127.0.0.1, [::1] or localhost")
		return "", false
	}
	// A request a proxy forwarded came from beyond this host, whatever its
	// remote address says. A proxy on this host that strips all four
	// headers and forwards to the port is the owner's own configuration,
	// which nothing in the request tells from MPD.
	if forwarded(r.Header) {
		refuseLoopback(w, http.StatusForbidden, "forwarded_request",
			"this route does not answer a request a proxy forwarded")
		return "", false
	}
	// Asked on every request, never only when the listener starts: the
	// listener stays up while output.mpd turns off.
	if h.enabled == nil || !h.enabled() {
		refuseLoopback(w, http.StatusNotFound, "not_serving",
			"this bridge is not serving its tracks to a player on this host now")
		return "", false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		refuseLoopback(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or HEAD only")
		return "", false
	}
	trackID, ok := loopbackTrackID(r.URL.Path)
	if !ok {
		refuseNoTrack(w)
		return "", false
	}
	return trackID, true
}

// serveTrack serves the file trackID names when it is a file inside a
// library root, and refuses it otherwise.
func (h *loopbackHandler) serveTrack(w http.ResponseWriter, r *http.Request, trackID string) {
	info, ok := h.lib.GetTrackInfo(trackID)
	if !ok || info.AbsolutePath == "" {
		// An empty path is a track the bridge proxies from an upstream
		// server: not a file in a library root.
		refuseNoTrack(w)
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
	runHook(h.beforeOpen)
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
	runHook(h.afterOpen)
	again, err := fsutil.ResolveLinks(info.AbsolutePath)
	if err != nil {
		refuseOutside(w)
		return
	}
	root := fsutil.IsUnderAny(again, roots)
	if root == "" {
		refuseOutside(w)
		return
	}
	runHook(h.beforeConfirm)
	if !sameFileInRoot(stat, root, again) {
		refuseOutside(w)
		return
	}
	serveOpenedFile(w, r, f, stat, info.AbsolutePath, info.FileExtension, trackID)
}

// runHook runs a test's hook in a race window. Production sets none.
func runHook(hook func()) {
	if hook != nil {
		hook()
	}
}

// sameFileInRoot reports whether opened is the file at path, walked from
// root with os.Root, which follows a link only while it stays inside root
// and never an absolute one. path is a resolution under root, so a
// component swapped for a link out after that resolution does not walk
// out of the root, and the file that opened is confirmed only by a path
// that stays inside. A path that does not begin with root, byte for byte,
// is not confirmed: on a volume that folds case, a link whose target
// spells the root another way is refused rather than guessed at.
func sameFileInRoot(opened fs.FileInfo, root, path string) bool {
	sep := string(filepath.Separator)
	rel, ok := strings.CutPrefix(path, strings.TrimSuffix(root, sep)+sep)
	if !ok || rel == "" {
		return false
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return false
	}
	defer r.Close()
	now, err := r.Stat(rel)
	return err == nil && os.SameFile(opened, now)
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

// loopbackHost reports whether a request's Host is one a client on this
// host sends: 127.0.0.1, [::1] or localhost (in any case), with any port
// or none. Another name, another address and an empty Host are not.
func loopbackHost(host string) bool {
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		// No port.
		name = host
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			name = host[1 : len(host)-1]
		}
	}
	switch {
	case name == "::1":
		// Only bracketed: a bare ::1 is no Host a client writes.
		return strings.HasPrefix(host, "[")
	case name == "127.0.0.1":
		return true
	default:
		return strings.EqualFold(name, "localhost")
	}
}

// forwardingHeaders are the headers a proxy adds to a request it forwards.
var forwardingHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-IP"}

// forwarded reports whether a request carries a forwarding header, even an
// empty one.
func forwarded(h http.Header) bool {
	for _, name := range forwardingHeaders {
		if len(h.Values(name)) > 0 {
			return true
		}
	}
	return false
}

// msgNoTrack is the message of every not_found refusal.
const msgNoTrack = "no library track has that id"

// refuseNoTrack answers a request that names no track the route serves.
func refuseNoTrack(w http.ResponseWriter) {
	refuseLoopback(w, http.StatusNotFound, "not_found", msgNoTrack)
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
		refuseNoTrack(w)
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
	// Enabled is asked on every request (LoopbackFileHandler's enabled):
	// the route serves while it answers true. Nil serves nothing.
	Enabled func() bool
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
		Handler:           LoopbackFileHandler(s.cfg.Library, s.cfg.Roots, s.cfg.Enabled),
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
