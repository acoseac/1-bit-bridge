package admin

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestUploadOctetStreamAllowlistContents pins the allowlist exactly.
//
// The point is not that the current entry is correct — it is that ADDING one is
// a deliberate edit. csrfGuard wraps the whole mux, so a route quietly joining
// this list relaxes the CSRF posture for a surface nobody reviewed.
func TestUploadOctetStreamAllowlistContents(t *testing.T) {
	want := []struct{ method, prefix string }{
		{http.MethodPut, "/api/upload/sessions/"},
	}
	if len(uploadOctetStreamRoutes) != len(want) {
		t.Fatalf("allowlist has %d entries, want %d — adding one must be deliberate: %+v",
			len(uploadOctetStreamRoutes), len(want), uploadOctetStreamRoutes)
	}
	for i, w := range want {
		if uploadOctetStreamRoutes[i] != w {
			t.Errorf("entry %d = %+v, want %+v", i, uploadOctetStreamRoutes[i], w)
		}
	}
}

func TestAllowsOctetStreamBody(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"upload chunk", http.MethodPut, "/api/upload/sessions/abc/files/def", true},
		{"upload chunk with query-free deep path", http.MethodPut, "/api/upload/sessions/a/files/b", true},

		{"wrong method on upload path", http.MethodPost, "/api/upload/sessions/abc/files/def", false},
		{"delete on upload path", http.MethodDelete, "/api/upload/sessions/abc", false},
		{"session create is json, not a chunk", http.MethodPost, "/api/upload/sessions", false},
		{"prefix boundary — sessions collection itself", http.MethodPut, "/api/upload/sessions", false},

		{"settings", http.MethodPatch, "/api/settings", false},
		{"scan", http.MethodPost, "/api/scan", false},
		{"root", http.MethodPut, "/", false},

		// Traversal: anything that changes under path.Clean never matches,
		// mirroring sessionMiddleware's bypass rule.
		{"traversal out of the prefix", http.MethodPut, "/api/upload/sessions/../../settings", false},
		{"traversal that lands back inside", http.MethodPut, "/api/upload/sessions/x/../y", false},
		{"double slash", http.MethodPut, "/api/upload//sessions/x", false},
		{"trailing dot segment", http.MethodPut, "/api/upload/sessions/x/.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := allowsOctetStreamBody(c.method, c.path); got != c.want {
				t.Errorf("allowsOctetStreamBody(%q, %q) = %v, want %v", c.method, c.path, got, c.want)
			}
		})
	}
}

// csrfProbe drives the real csrfGuard.
func csrfProbe(t *testing.T, method, path, contentType, body string) int {
	t.Helper()
	s := &Server{}
	h := s.csrfGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestCSRFGuardRejectsSimpleContentTypesOnUploadRoutes is the load-bearing half
// of the allowlist. multipart/form-data, form-urlencoded and text/plain are the
// three CORS SIMPLE content types — the only ones a drive-by cross-origin page
// can send without a preflight — so they must stay refused on the upload routes
// too. Relaxing to multipart is the mistake this test exists to prevent.
func TestCSRFGuardRejectsSimpleContentTypesOnUploadRoutes(t *testing.T) {
	const p = "/api/upload/sessions/abc/files/def"
	for _, ct := range []string{
		"multipart/form-data; boundary=x",
		"application/x-www-form-urlencoded",
		"text/plain",
		"text/plain; charset=utf-8",
	} {
		t.Run(ct, func(t *testing.T) {
			if got := csrfProbe(t, http.MethodPut, p, ct, "payload"); got != http.StatusUnsupportedMediaType {
				t.Errorf("Content-Type %q on an upload route = %d, want 415", ct, got)
			}
		})
	}
}

func TestCSRFGuardAcceptsOctetStreamOnlyOnUploadRoutes(t *testing.T) {
	if got := csrfProbe(t, http.MethodPut, "/api/upload/sessions/a/files/b", "application/octet-stream", "bytes"); got != http.StatusNoContent {
		t.Errorf("octet-stream on an upload route = %d, want 204", got)
	}
	if got := csrfProbe(t, http.MethodPost, "/api/settings", "application/octet-stream", "bytes"); got != http.StatusUnsupportedMediaType {
		t.Errorf("octet-stream off the upload routes = %d, want 415", got)
	}
	// The pre-existing contract is untouched.
	if got := csrfProbe(t, http.MethodPatch, "/api/settings", "application/json", `{}`); got != http.StatusNoContent {
		t.Errorf("json on a normal route = %d, want 204", got)
	}
	if got := csrfProbe(t, http.MethodPost, "/api/scan", "", ""); got != http.StatusNoContent {
		t.Errorf("bodyless POST = %d, want 204 (csrfGuard exempts empty bodies)", got)
	}
}

// trickleBody writes total bytes in chunks, pausing between them, so the
// request takes longer than the server's ReadTimeout while never stalling.
func trickleBody(total, chunk int, pause time.Duration) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		buf := make([]byte, chunk)
		for i := range buf {
			buf[i] = 'x'
		}
		for sent := 0; sent < total; sent += chunk {
			time.Sleep(pause)
			if _, err := pw.Write(buf); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		_ = pw.Close()
	}()
	return pr
}

// TestUploadBodyReaderSurvivesPastReadTimeout is the ReadTimeout fix.
//
// It runs the SAME slow request twice against the same server: once reading the
// body directly (the pre-fix path) and once through uploadBodyReader. The direct
// read is the negative control — it must fail, or the test proves nothing about
// the wrapper.
func TestUploadBodyReaderSurvivesPastReadTimeout(t *testing.T) {
	const (
		readTimeout = 300 * time.Millisecond
		totalBytes  = 8192
		chunkBytes  = 512
		pause       = 40 * time.Millisecond // 16 chunks ≈ 640ms > readTimeout
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/direct", func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestTimeout)
			return
		}
		fmt.Fprintf(w, "%d", n)
	})
	mux.HandleFunc("/wrapped", func(w http.ResponseWriter, r *http.Request) {
		// A window comfortably longer than the inter-chunk pause but far
		// shorter than the whole transfer, so only the rolling extension
		// can carry it to completion.
		body := newUploadBodyReaderTuned(w, r.Body, 250*time.Millisecond, 80*time.Millisecond)
		n, err := io.Copy(io.Discard, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestTimeout)
			return
		}
		fmt.Fprintf(w, "%d", n)
	})

	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ReadTimeout = readTimeout
	srv.Start()
	defer srv.Close()

	post := func(path string) (int, string, error) {
		req, err := http.NewRequest(http.MethodPut, srv.URL+path, trickleBody(totalBytes, chunkBytes, pause))
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := srv.Client().Do(req)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}

	// Negative control: without the wrapper the server's ReadTimeout kills it.
	code, _, err := post("/direct")
	if err == nil && code == http.StatusOK {
		t.Fatal("CONTROL INVALID: the unwrapped read completed, so ReadTimeout never bit and this test proves nothing about the wrapper")
	}

	// The fix: the same slow request completes.
	code, body, err := post("/wrapped")
	if err != nil {
		t.Fatalf("wrapped upload failed: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("wrapped upload status = %d body=%q, want 200", code, body)
	}
	if body != fmt.Sprint(totalBytes) {
		t.Errorf("wrapped upload read %s bytes, want %d", body, totalBytes)
	}
}

// TestUploadBodyReaderStillKillsAStalledClient is the other half of the
// contract: the deadline is EXTENDED by progress, not removed. A client that
// stops sending must still be torn down.
func TestUploadBodyReaderStillKillsAStalledClient(t *testing.T) {
	const window = 200 * time.Millisecond

	done := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := newUploadBodyReaderTuned(w, r.Body, window, window/2)
		_, err := io.Copy(io.Discard, body)
		done <- err
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout = time.Hour // only our window can fire
	srv.Start()
	defer srv.Close()

	// Announce a body, send a little, then go silent forever.
	//
	// pw is closed by a defer registered AFTER srv.Close's, so it runs FIRST
	// (LIFO). Without that, a failing run leaves the handler blocked in
	// io.Copy and srv.Close waits on it — the test would hang the binary
	// instead of reporting the failure it just detected.
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	go func() {
		_, _ = pw.Write([]byte("hello"))
		// deliberately not closed here — the defer above owns it
	}()
	req, err := http.NewRequest(http.MethodPut, srv.URL, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	go func() {
		resp, derr := srv.Client().Do(req)
		if derr == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled client was NOT torn down — the rolling deadline has become an absence of one")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler never returned; the stall deadline did not fire")
	}
}

// TestUploadBodyReaderSurvivesATrickleUnderAnyByteThreshold is the case a
// byte-based extension starves.
//
// The first shape of this extended after 256 KiB. A client sending 4 KiB/s
// delivers 240 KiB in a 60s window — never reaching the threshold — so an
// ACTIVELY transferring upload was torn down as though it had stalled. Time-
// based extension means any progress at all keeps it alive, which is the
// property the whole helper exists for.
func TestUploadBodyReaderSurvivesATrickleUnderAnyByteThreshold(t *testing.T) {
	// The budget is deliberately GENEROUS in absolute terms while keeping
	// the structural property intact: the upload must outlive several
	// windows (or the roll-forward is never exercised and the test proves
	// nothing), and a single pause must sit far inside one window (or an
	// ordinary scheduling stall fails a healthy upload).
	//
	// It was window=200ms / pause=25ms, which satisfied the structure with
	// only 200ms of absolute slack — and a >200ms stall on a loaded CI
	// runner is common. It failed once on macOS as
	// `read tcp …: i/o timeout`, a false failure about the runner rather
	// than the code. Same 3-window shape now, 5x the slack: a gap has to
	// exceed a FULL SECOND to fail.
	const (
		window      = 1 * time.Second
		total       = 600 // bytes — far below any byte threshold worth having
		chunk       = 20
		pause       = 100 * time.Millisecond // 30 chunks ≈ 3s, i.e. 3 windows
		readTimeout = 500 * time.Millisecond // < window, so the override is what is proven
	)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := newUploadBodyReaderTuned(w, r.Body, window, window/2)
		n, err := io.Copy(io.Discard, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestTimeout)
			return
		}
		fmt.Fprintf(w, "%d", n)
	})
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout = readTimeout
	srv.Start()
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPut, srv.URL, trickleBody(total, chunk, pause))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("a steadily-trickling upload was torn down: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != fmt.Sprint(total) {
		t.Fatalf("status %d body %q, want 200 and %d bytes", resp.StatusCode, b, total)
	}
}
