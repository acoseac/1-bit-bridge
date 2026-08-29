package admin

import (
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

	// uploadDeadlineExtendEvery bounds the syscall rate. At 256 KiB a 4 MiB
	// chunk costs 16 extensions, which is noise against the write path, and
	// staying alive needs only ~34 kbps sustained.
	uploadDeadlineExtendEvery = 256 << 10
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
	src         io.Reader
	rc          *http.ResponseController
	window      time.Duration
	extendEvery int64
	sinceExtend int64
	warnOnce    sync.Once
}

// newUploadBodyReader arms the deadline immediately. The server's ReadTimeout
// starts counting when the request begins, not when the handler is entered, so
// deferring the first extension until after the first Read would leave a slow
// first chunk racing a clock that is already running.
func newUploadBodyReader(w http.ResponseWriter, body io.Reader) *uploadBodyReader {
	return newUploadBodyReaderTuned(w, body, uploadStallWindow, uploadDeadlineExtendEvery)
}

// newUploadBodyReaderTuned is the constructor tests drive so a stall can be
// observed in milliseconds instead of a minute. The tuning lives on the
// INSTANCE rather than in package-level vars, matching transcode.Pool.jobTimeout
// — a package var would race across parallel tests.
func newUploadBodyReaderTuned(w http.ResponseWriter, body io.Reader, window time.Duration, extendEvery int64) *uploadBodyReader {
	u := &uploadBodyReader{
		src:         body,
		rc:          http.NewResponseController(w),
		window:      window,
		extendEvery: extendEvery,
	}
	u.extend()
	return u
}

func (u *uploadBodyReader) extend() {
	if err := u.rc.SetReadDeadline(time.Now().Add(u.window)); err != nil {
		u.warnOnce.Do(func() {
			logger.Error("upload read deadline not settable — uploads are capped by the server ReadTimeout; a ResponseWriter is being wrapped without Unwrap", "err", err)
		})
	}
}

func (u *uploadBodyReader) Read(p []byte) (int, error) {
	n, err := u.src.Read(p)
	if n > 0 {
		u.sinceExtend += int64(n)
		if u.sinceExtend >= u.extendEvery {
			u.sinceExtend = 0
			u.extend()
		}
	}
	return n, err
}
