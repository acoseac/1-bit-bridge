package admin

import (
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// Upload transport floor.
//
// Two things stand between a chunked upload and the admin server as it is
// configured today, and both are deliberate features that have to be amended
// rather than removed:
//
//  1. csrfGuard 415s any body-bearing mutation whose Content-Type is not
//     application/json (admin.go). It wraps the ENTIRE mux, so it is not a
//     per-route decision — see uploadOctetStreamRoutes below.
//  2. The server sets ReadTimeout: 30s, which caps reading the entire request
//     body. A 200 MB file dies at 30s on anything under ~55 Mbps — see
//     newUploadBodyReader.

// uploadOctetStreamRoutes are the (method, path-prefix) pairs permitted to
// carry an application/octet-stream body past csrfGuard.
//
// WHY THIS IS SAFE, and why the shape matters more than it looks:
//
// csrfGuard's Content-Type check is a CORS simple-request defense. The simple
// content types are exactly application/x-www-form-urlencoded,
// multipart/form-data and text/plain — those three can be sent cross-origin
// with no preflight, so a drive-by page can forge them. application/octet-
// stream is NOT one of them, and PUT is not a simple METHOD either, so a
// request matching this allowlist is preflight-forced twice over and the
// bridge answers no preflight.
//
// The corollary is the part to not get wrong later: multipart/form-data must
// stay refused even here. Building the upload as a multipart form would hand
// back precisely the property csrfGuard exists to provide.
//
// The Origin allowlist in csrfGuard is unchanged and still applies.
var uploadOctetStreamRoutes = []struct{ method, prefix string }{
	{http.MethodPut, "/api/upload/sessions/"},
}

// allowsOctetStreamBody reports whether this exact (method, path) may carry an
// application/octet-stream body.
//
// The path must be its own clean form to match, mirroring sessionMiddleware's
// bypass check: a request for /api/upload/sessions/../../settings normalises to
// something outside the prefix, and refusing to match anything that changes
// under path.Clean means a traversal attempt can never reach the relaxed
// content-type branch.
func allowsOctetStreamBody(method, urlPath string) bool {
	if path.Clean(urlPath) != urlPath {
		return false
	}
	for _, rt := range uploadOctetStreamRoutes {
		if method == rt.method && strings.HasPrefix(urlPath, rt.prefix) {
			return true
		}
	}
	return false
}

const (
	// uploadStallWindow is how long an upload may make ZERO progress before
	// the connection is torn down. It is not a cap on the transfer — the
	// deadline is pushed forward as bytes arrive — so a slow-but-alive link
	// survives arbitrarily long while a genuinely stalled one still dies,
	// which is the Slowloris protection ReadTimeout was added for (PR #75).
	uploadStallWindow = 60 * time.Second

	// uploadExtendAfter is how much of the window may elapse before the next
	// successful read pushes the deadline forward.
	//
	// This is deliberately TIME-based rather than byte-based. A byte threshold
	// (the first shape here was 256 KiB) starves exactly the client it is
	// meant to protect: at 4 KiB/s a client sends 240 KiB in a 60s window,
	// never reaches the threshold, and is torn down while actively
	// transferring. Extending on elapsed time instead means ANY progress keeps
	// the connection alive, at a bounded ~2 syscalls per window.
	uploadExtendAfter = uploadStallWindow / 2
)

// uploadBodyReader wraps an upload request body and pushes the connection's
// read deadline forward as bytes arrive.
//
// It deliberately does NOT fail the request when the deadline cannot be set.
// SetReadDeadline returns ErrNotSupported only when the ResponseWriter has been
// wrapped without an Unwrap method — a programming error in middleware, not a
// runtime condition — and in that state the request may still complete inside
// the server's own ReadTimeout. So the miss is logged once, loudly, and the
// read proceeds: a degraded upload beats a refused one, and the log line is
// what leads a maintainer to the wrapper they added.
type uploadBodyReader struct {
	src        io.Reader
	rc         *http.ResponseController
	window     time.Duration
	extendGap  time.Duration
	lastExtend time.Time
	warnOnce   sync.Once
}

// newUploadBodyReader arms the deadline immediately. The server's ReadTimeout
// starts counting when the request begins, not when the handler is entered, so
// deferring the first extension until after the first Read would leave a slow
// first chunk racing a clock that is already running.
func newUploadBodyReader(w http.ResponseWriter, body io.Reader) *uploadBodyReader {
	return newUploadBodyReaderTuned(w, body, uploadStallWindow, uploadExtendAfter)
}

// newUploadBodyReaderTuned is the constructor tests drive so a stall can be
// observed in milliseconds instead of a minute. The tuning lives on the
// INSTANCE rather than in package-level vars, matching transcode.Pool.jobTimeout
// — a package var would race across parallel tests.
func newUploadBodyReaderTuned(w http.ResponseWriter, body io.Reader, window, extendGap time.Duration) *uploadBodyReader {
	if extendGap <= 0 || extendGap > window {
		extendGap = window / 2
	}
	u := &uploadBodyReader{
		src:       body,
		rc:        http.NewResponseController(w),
		window:    window,
		extendGap: extendGap,
	}
	u.extend()
	return u
}

func (u *uploadBodyReader) extend() {
	u.lastExtend = time.Now()
	err := u.rc.SetReadDeadline(u.lastExtend.Add(u.window))
	if err == nil {
		return
	}
	// ONLY ErrNotSupported means the ResponseWriter was wrapped without an
	// Unwrap method. Every other error here is ordinary connection lifecycle —
	// a client that hung up mid-upload returns "use of closed network
	// connection" — and logging the wrapper diagnosis for those is both
	// misleading and self-defeating: warnOnce means the first spurious one
	// SUPPRESSES the real diagnosis for the life of the process.
	if !errors.Is(err, http.ErrNotSupported) {
		return
	}
	u.warnOnce.Do(func() {
		logger.Error("upload read deadline not settable — uploads are capped by the server ReadTimeout; a ResponseWriter is being wrapped without Unwrap", "err", err)
	})
}

func (u *uploadBodyReader) Read(p []byte) (int, error) {
	n, err := u.src.Read(p)
	if n > 0 && time.Since(u.lastExtend) >= u.extendGap {
		u.extend()
	}
	return n, err
}
