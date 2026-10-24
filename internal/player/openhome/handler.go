package openhome

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// handler is what every socket of the surface serves: /openhome/ alone.
//
//	GET, HEAD  /openhome/description.xml       the device description
//	GET, HEAD  /openhome/<service>/scpd.xml    the service's description
//	POST       /openhome/<service>/control     the SOAP layer (soap.go)
//	SUBSCRIBE, UNSUBSCRIBE
//	           /openhome/<service>/event       501, never a subscription
//
// Anything else is 404; there is no admin, /v1, media or CORS. A request
// first passes row 1 (admission at accept, then its Host), then, while the
// surface stops, is 503 and its connection closed.
type handler struct {
	device Device
	// engine is what every action answers from (actions.go, writes.go); nil
	// is no engine.
	engine Engine
	// library and hosts confine an Insert's URI (writes.go); nil takes none.
	library Library
	hosts   Hosts
	scpds   map[string][]byte
	lim     Limits
	seams   *Seams

	// slots are the requests answered at once, taken once a request's body
	// is read; a request past them is 503 at once.
	slots chan struct{}
	// writes are the write actions but Stop answered at once, each holding a
	// slot too, and bySource the ones each source holds (Limits.WriteSlots,
	// WritesPerSource); a write past either is 503 at once.
	writes   chan struct{}
	writesMu sync.Mutex
	bySource map[netip.Addr]int
	// stopping is set by Stop: every request after it is 503.
	stopping atomic.Bool
	// flight counts the handlers running, which Stop joins.
	flight inflight
}

func newHandler(d Device, eng Engine, lim Limits, seams *Seams) *handler {
	h := &handler{device: d, engine: eng, scpds: map[string][]byte{}, lim: lim, seams: seams, slots: make(chan struct{}, lim.Slots),
		writes: make(chan struct{}, lim.WriteSlots), bySource: map[netip.Addr]int{}}
	for _, s := range services {
		h.scpds[s.path] = s.scpd()
	}
	return h
}

// ServeHTTP is the surface's one handler.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A write's budget runs from here (writes.go): net/http calls the
	// handler as soon as it has read the request's headers, when it sets the
	// response's write deadline (Limits.Write).
	arrived := time.Now()
	// Every reply carries UPnP's empty EXT header, in the case UDA writes
	// it: a direct assignment, since Header.Set would write it Ext. It
	// comes first, so the refusal of a request that arrives after the
	// stop's join (one net/http read just before its connection closed)
	// carries it too.
	w.Header()["EXT"] = []string{""}
	if !h.flight.enter() {
		refuse(w, http.StatusServiceUnavailable, true)
		return
	}
	defer h.flight.leave()
	local, ok := arrivedOn(r)
	if !ok || r.Host != local.String() {
		refuse(w, http.StatusBadRequest, true)
		return
	}
	if h.stopping.Load() {
		refuse(w, http.StatusServiceUnavailable, true)
		return
	}
	if r.URL.Path == descriptionPath {
		h.serveDocument(w, r, func() []byte { return h.device.description(local) })
		return
	}
	s, leaf, ok := servicePath(r.URL.Path)
	if !ok {
		refuse(w, http.StatusNotFound, false)
		return
	}
	switch leaf {
	case scpdName:
		h.serveDocument(w, r, func() []byte { return h.scpds[s.path] })
	case controlName:
		h.control(w, r, s, arrived)
	case eventName:
		event(w, r)
	}
}

// arrivedOn is the address and port the request's connection arrived on:
// the local address of the connection the listener admitted.
func arrivedOn(r *http.Request) (netip.AddrPort, bool) {
	tcp, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}, false
	}
	ap := tcp.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
}

// servicePath is the service and the leaf (scpd.xml, control or event) a
// path names under /openhome/<service>/, exactly.
func servicePath(path string) (*service, string, bool) {
	rest, ok := strings.CutPrefix(path, pathPrefix)
	if !ok {
		return nil, "", false
	}
	name, leaf, ok := strings.Cut(rest, "/")
	if !ok {
		return nil, "", false
	}
	s, ok := servicesByPath[name]
	if !ok {
		return nil, "", false
	}
	switch leaf {
	case scpdName, controlName, eventName:
		return s, leaf, true
	}
	return nil, "", false
}

// serveDocument answers a GET or a HEAD of a description, built by body,
// holding a slot while it does.
func (h *handler) serveDocument(w http.ResponseWriter, r *http.Request, body func() []byte) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		refuse(w, http.StatusMethodNotAllowed, false)
		return
	}
	if !h.takeSlot() {
		refuse(w, http.StatusServiceUnavailable, false)
		return
	}
	defer h.releaseSlot()
	h.handling(r)
	b := body()
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(b)
	}
}

// control is the SOAP layer: rows 2 to 9 of the table in soap.go, then the
// action's answer (act): a read from one engine snapshot, a write by one
// engine command. A write but Stop holds a slot of the write pool as well,
// taken with the request's slot.
func (h *handler) control(w http.ResponseWriter, r *http.Request, s *service, arrived time.Time) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		refuse(w, http.StatusMethodNotAllowed, true)
		return
	}
	if _, present := r.Header["Origin"]; present {
		refuse(w, http.StatusForbidden, true)
		return
	}
	if !mediaTypeIsXMLUTF8(r.Header) {
		refuse(w, http.StatusUnsupportedMediaType, true)
		return
	}
	st, name, ok := soapAction(r.Header)
	var a *action
	if ok && st == s.serviceType() {
		a = s.action(name)
	}
	if a == nil {
		writeFault(w, faultReply{code: errInvalidAction}, true)
		return
	}
	// Row 6: the body, bounded, under its own deadline, which a trickling
	// client meets holding no slot.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(h.lim.BodyRead))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		var ne net.Error
		switch {
		case errors.As(err, &tooBig):
			refuse(w, http.StatusRequestEntityTooLarge, true)
		case errors.As(err, &ne) && ne.Timeout():
			refuse(w, http.StatusRequestTimeout, true)
		default:
			refuse(w, http.StatusBadRequest, true)
		}
		return
	}
	if !h.takeSlot() {
		refuse(w, http.StatusServiceUnavailable, false)
		return
	}
	defer h.releaseSlot()
	if pooled(s, a) {
		src := sourceOf(r)
		if !h.takeWrite(src) {
			refuse(w, http.StatusServiceUnavailable, false)
			return
		}
		defer h.releaseWrite(src)
	}
	h.handling(r)
	root, err := parseRequestXML(body, textLimit(a))
	if err != nil {
		refuse(w, http.StatusBadRequest, true)
		return
	}
	env := checkEnvelope(root, s, a)
	switch {
	case env.tooBig:
		refuse(w, http.StatusBadRequest, true)
		return
	case env.fault != nil:
		writeFault(w, *env.fault, false)
		return
	}
	if _, ok := checkArguments(env.action, s, a); !ok {
		writeFault(w, faultReply{code: errInvalidArgs}, false)
		return
	}
	h.act(w, r, s, a, env.action, arrived)
}

// pooled reports whether a is a write that holds a slot of the write pool:
// every write but Stop, which waits on the engine at most its release, so a
// controller can always stop.
func pooled(s *service, a *action) bool {
	return reads[s.name][a.name] == nil && !(s.name == "Playlist" && a.name == "Stop")
}

// sourceOf is the address a request's connection came from: the source
// admission approved (the zero Addr for none, which counts as one source).
func sourceOf(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// takeWrite takes a slot of the write pool for a write from src, or reports
// that the pool, or src's share of it, is full.
func (h *handler) takeWrite(src netip.Addr) bool {
	h.writesMu.Lock()
	defer h.writesMu.Unlock()
	if h.bySource[src] >= h.lim.WritesPerSource {
		return false
	}
	select {
	case h.writes <- struct{}{}:
	default:
		return false
	}
	h.bySource[src]++
	return true
}

// releaseWrite gives src's slot of the write pool back.
func (h *handler) releaseWrite(src netip.Addr) {
	h.writesMu.Lock()
	defer h.writesMu.Unlock()
	<-h.writes
	if h.bySource[src]--; h.bySource[src] <= 0 {
		delete(h.bySource, src)
	}
}

// event answers GENA at a service's eventSubURL. SUBSCRIBE and UNSUBSCRIBE
// are 501 Not Implemented: UDA 1.1 (§4.1.2) has a publisher that cannot
// accept a subscription answer a 5xx, and 501, unlike 503, says it is not a
// passing condition, so a controller is not asked to try again; never 200,
// so no controller holds a subscription it would wait on events for, and
// no UNSUBSCRIBE finds one to cancel. Any other method is 405.
func event(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "SUBSCRIBE", "UNSUBSCRIBE":
		refuse(w, http.StatusNotImplemented, false)
	default:
		w.Header().Set("Allow", "SUBSCRIBE, UNSUBSCRIBE")
		refuse(w, http.StatusMethodNotAllowed, false)
	}
}

// handling runs the test's seam, once a request holds its slot.
func (h *handler) handling(r *http.Request) {
	if h.seams != nil && h.seams.Handling != nil {
		h.seams.Handling(r)
	}
}

func (h *handler) takeSlot() bool {
	select {
	case h.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *handler) releaseSlot() { <-h.slots }

// refuse answers status with its text and nothing of the request's;
// closing asks for the connection to be closed after it, for a request
// refused before its body was read (net/http would otherwise read what is
// left of it to keep the connection).
func refuse(w http.ResponseWriter, status int, closing bool) {
	if closing {
		w.Header().Set("Connection", "close")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, http.StatusText(status)+"\n")
}

// writeFault answers f as a SOAP fault, HTTP 500.
func writeFault(w http.ResponseWriter, f faultReply, closing bool) {
	if closing {
		w.Header().Set("Connection", "close")
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(faultEnvelope(f))
}

// inflight counts the handlers running; once closed it admits none, and
// its zero channel closes when the last one leaves.
type inflight struct {
	mu     sync.Mutex
	n      int
	closed bool
	zero   chan struct{}
}

func (f *inflight) enter() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.n++
	return true
}

func (f *inflight) leave() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n--
	if f.closed && f.n == 0 && f.zero != nil {
		close(f.zero)
		f.zero = nil
	}
}

// close admits no more handlers and returns a channel closed once none
// runs, and how many run now.
func (f *inflight) close() (<-chan struct{}, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	done := make(chan struct{})
	if f.n == 0 {
		close(done)
		return done, 0
	}
	f.zero = done
	return done, f.n
}

// running is how many handlers run now.
func (f *inflight) running() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}
