package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// fakeManifestProvider is a tiny ManifestProvider stand-in for tests that
// want to exercise /v1/manifest without spinning up the real scanner.
//
// `body` is the legacy non-paginated WriteManifest response — kept as
// `any` because that path encodes whatever shape the test hands it
// straight to the wire (some tests use `map[string]any{...}` to check
// arbitrary JSON keys, e.g. the legacy-path back-compat assertion).
// `pageBody` is typed via the api ManifestProvider contract so the
// paginated path is statically checked from fake to handler.
type fakeManifestProvider struct {
	body             any
	err              error
	isScanning       bool
	lastFullScan     time.Time
	tracksIndexed    int
	pendingDeletions int64
	// pageBody / pageErr drive BuildManifestPage independently so
	// pagination tests can assert against a different response than
	// the legacy WriteManifest path without clobbering full-manifest
	// coverage. nil pageBody returns an empty *manifest.Manifest so
	// tests that only care about cursor/limit forwarding don't have
	// to construct a fixture.
	pageBody *manifest.Manifest
	pageErr  error
	// lastPageCursor / lastPageLimit let pagination tests verify the
	// handler forwards query params correctly.
	lastPageCursor string
	lastPageLimit  int
	// lastSince captures the since arg passed to WriteManifest so tests
	// can assert the handler forwards `?since=` correctly.
	lastSince time.Time
}

func (f *fakeManifestProvider) WriteManifest(ctx context.Context, w io.Writer, since time.Time) error {
	f.lastSince = since
	if f.err != nil {
		return f.err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return json.NewEncoder(w).Encode(f.body)
}
func (f *fakeManifestProvider) BuildManifestPage(ctx context.Context, cursor string, limit int) (*ManifestPage, error) {
	f.lastPageCursor = cursor
	f.lastPageLimit = limit
	if f.pageErr != nil {
		return f.pageBody, f.pageErr
	}
	if f.pageBody != nil {
		return f.pageBody, nil
	}
	return &manifest.Manifest{}, nil
}
func (f *fakeManifestProvider) IsScanning() bool                           { return f.isScanning }
func (f *fakeManifestProvider) LastFullScan() time.Time                    { return f.lastFullScan }
func (f *fakeManifestProvider) TracksIndexed(ctx context.Context) int      { return f.tracksIndexed }
func (f *fakeManifestProvider) PendingDeletions(ctx context.Context) int64 { return f.pendingDeletions }

// newTestServer spins up an httptest.Server with the api handler and a
// populated auth store. Returns the server plus one valid raw token for
// driving the authed() middleware.
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	os.MkdirAll(lib, 0o755)
	cfg := &config.Config{
		LibraryRoots:  []string{lib},
		ListenAddress: ":7788",
		LibraryName:   "Test Library",
	}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.Mint("test-client")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, nil, "AB:CD:EF:01:02:03:...:FF")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw
}

func TestHealthReturns200WithExpectedShape(t *testing.T) {
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	var got HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ProtocolVersion != version.ProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", got.ProtocolVersion, version.ProtocolVersion)
	}
	if got.ServerVersion != version.ServerVersion {
		t.Errorf("serverVersion = %q, want %q", got.ServerVersion, version.ServerVersion)
	}
	if got.LibraryName != "Test Library" {
		t.Errorf("libraryName = %q", got.LibraryName)
	}
	if len(got.LibraryRoots) != 1 || got.LibraryRoots[0] != "Music" {
		t.Errorf("libraryRoots = %v, want [Music]", got.LibraryRoots)
	}
	if got.CertFingerprint == "" {
		t.Error("certFingerprint missing")
	}
	if got.StartedAt.IsZero() {
		t.Error("startedAt missing")
	}
	if got.ScanState.IsScanning {
		t.Error("scanState.isScanning should be false on fresh server")
	}
}

// TestHealthCertNotAfterIsOmittedWhenUnset locks in the omitempty
// shape: a Server constructed without WithCertExpiry (test harnesses,
// pre-PR bridges) must not emit a `0001-01-01T00:00:00Z` zero-time
// on the wire. Pre-PR-aware iOS clients treat absence as "no warn".
// Asserts on the raw JSON bytes, not the decoded struct, because
// `time.Time` zero round-trips through Go's decoder transparently
// — the wire-shape check is the only way to catch a regression
// where a typo on the field tag accidentally drops the pointer.
func TestHealthCertNotAfterIsOmittedWhenUnset(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	store, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(New(cfg, store, nil, "fp").Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "certNotAfter") {
		t.Errorf("certNotAfter should be omitted when unset, got: %s", body)
	}
}

// TestHealthCertNotAfterEmittedWhenSet: WithCertExpiry plumbs the
// NotAfter through to /v1/health so iOS can warn the operator before
// the cert actually expires.
func TestHealthCertNotAfterEmittedWhenSet(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	store, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Date(2027, 7, 1, 12, 0, 0, 0, time.UTC)
	srv := New(cfg, store, nil, "fp").WithCertExpiry(expiry)
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.CertNotAfter == nil {
		t.Fatalf("certNotAfter unset on wire (expected %v)", expiry)
	}
	if !got.CertNotAfter.Equal(expiry) {
		t.Errorf("certNotAfter = %v, want %v", *got.CertNotAfter, expiry)
	}
}

// TestHealthLECertNotAfterOmittedWithoutProvider pins the
// post-PR-#296 followup contract: a Server constructed without
// WithLECertExpiry (loopback bridges, pre-autocert deploys, test
// harnesses) must NOT emit `leCertNotAfter` on the wire — iOS
// treats absence as "no LE cert" (LAN-only bridge).
func TestHealthLECertNotAfterOmittedWithoutProvider(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	store, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(New(cfg, store, nil, "fp").Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "leCertNotAfter") {
		t.Errorf("leCertNotAfter should be omitted when no provider, got: %s", body)
	}
}

// TestHealthLECertNotAfterOmittedWhenProviderReturnsZero — autocert
// hasn't completed first mint yet (or manager is disabled). The
// provider returns the zero time and the field must stay off the
// wire. iOS treats absence the same as a loopback bridge — correct
// fallback for a public-mode bridge mid-first-handshake too.
func TestHealthLECertNotAfterOmittedWhenProviderReturnsZero(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	store, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, nil, "fp").WithLECertExpiry(func() time.Time { return time.Time{} })
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "leCertNotAfter") {
		t.Errorf("leCertNotAfter should be omitted when provider returns zero, got: %s", body)
	}
}

// TestHealthLECertNotAfterEmittedWhenSet: WithLECertExpiry plumbs
// the autocert manager's NotAfter through to /v1/health so iOS /
// operator tooling can render the ~90-day LE rotation warning
// distinctly from the self-signed cert's ~397-day cap. Distinct
// from CertNotAfter — public-mode bridges emit BOTH (different
// certs, different audiences).
func TestHealthLECertNotAfterEmittedWhenSet(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	store, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	selfSigned := time.Date(2027, 6, 25, 12, 0, 0, 0, time.UTC)
	leExpiry := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	srv := New(cfg, store, nil, "fp").
		WithCertExpiry(selfSigned).
		WithLECertExpiry(func() time.Time { return leExpiry })
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.LECertNotAfter == nil {
		t.Fatalf("leCertNotAfter unset on wire (expected %v)", leExpiry)
	}
	if !got.LECertNotAfter.Equal(leExpiry) {
		t.Errorf("leCertNotAfter = %v, want %v", *got.LECertNotAfter, leExpiry)
	}
	// CertNotAfter must STILL be emitted alongside — both surface
	// on a public-mode bridge so the operator sees both the iOS-pin
	// expiry and the public-trust expiry.
	if got.CertNotAfter == nil {
		t.Fatalf("certNotAfter dropped from wire when leCertNotAfter present — they're independent fields")
	}
	if !got.CertNotAfter.Equal(selfSigned) {
		t.Errorf("certNotAfter = %v, want %v", *got.CertNotAfter, selfSigned)
	}
}

// TestHealthLECertNotAfterIsRead Live — autocert renews in the
// background, so the provider must be called per-request (not
// stamped at WithLECertExpiry time). Drive a mutable counter
// through the closure; assert the second /v1/health probe sees
// the second value.
func TestHealthLECertNotAfterIsReadLive(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	store, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	second := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) // simulated renewal
	current := first
	srv := New(cfg, store, nil, "fp").WithLECertExpiry(func() time.Time { return current })
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	fetch := func() *time.Time {
		resp, err := http.Get(hs.URL + "/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got HealthResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got.LECertNotAfter
	}

	got1 := fetch()
	if got1 == nil || !got1.Equal(first) {
		t.Fatalf("first probe leCertNotAfter = %v, want %v", got1, first)
	}
	current = second // simulate autocert renewal between probes
	got2 := fetch()
	if got2 == nil || !got2.Equal(second) {
		t.Fatalf("second probe leCertNotAfter = %v, want %v (live-read regression)", got2, second)
	}
}

// fakeUpdater stands in for internal/updater.Updater in tests so we can
// drive UpdateInfo into /v1/health without spinning up a real poller.
type fakeUpdater struct{ info UpdateInfo }

func (f fakeUpdater) UpdateInfo() UpdateInfo { return f.info }

func TestHealthOmitsUpdateFieldsWhenNoUpdaterAttached(t *testing.T) {
	// Pre-Phase-A wire shape: when WithUpdater hasn't been called, all
	// four update-related JSON keys must be absent from the response.
	// iOS clients without the new decoder fields would still be fine
	// either way (Codable ignores unknown keys), but absent vs. zero
	// is the iOS test harness's contract for "not advertised".
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, key := range []string{"latestServerVersion", "updateAvailable", "updateReleaseNotesURL", "minClientVersion"} {
		if strings.Contains(string(body), `"`+key+`":`) {
			t.Errorf("body advertises %q without an updater attached: %s", key, body)
		}
	}
}

func TestHealthIncludesUpdateFieldsWhenUpdaterAttached(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	os.MkdirAll(lib, 0o755)
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, nil, "fp").WithUpdater(fakeUpdater{
		info: UpdateInfo{
			LatestVersion:    "9.9.9",
			UpdateAvailable:  true,
			ReleaseNotesURL:  "https://example.test/release",
			MinClientVersion: "1.2.0",
		},
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.LatestServerVersion != "9.9.9" {
		t.Errorf("LatestServerVersion = %q, want 9.9.9", got.LatestServerVersion)
	}
	if !got.UpdateAvailable {
		t.Error("UpdateAvailable = false, want true")
	}
	if got.UpdateReleaseNotesURL != "https://example.test/release" {
		t.Errorf("UpdateReleaseNotesURL = %q", got.UpdateReleaseNotesURL)
	}
	if got.MinClientVersion != "1.2.0" {
		t.Errorf("MinClientVersion = %q, want 1.2.0", got.MinClientVersion)
	}
}

func TestAuthedRecordsClientVersion(t *testing.T) {
	hs, srv, raw := newTestServerWithProbe(t)
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("X-Client-Version", "1.2.3-build42")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("probe status = %d, want 200", resp.StatusCode)
	}
	tokens := srv.store.List()
	if len(tokens) != 1 {
		t.Fatalf("List = %d, want 1", len(tokens))
	}
	if tokens[0].LastClientVersion != "1.2.3-build42" {
		t.Errorf("LastClientVersion = %q, want 1.2.3-build42", tokens[0].LastClientVersion)
	}
	if tokens[0].LastClientVersionAt.IsZero() {
		t.Error("LastClientVersionAt not stamped")
	}
}

func TestAuthedHandlesMissingClientVersionHeader(t *testing.T) {
	// Older iOS builds don't send X-Client-Version. Authed requests
	// from them must still succeed and must not stamp empty fields.
	hs, srv, raw := newTestServerWithProbe(t)
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("probe status = %d, want 200", resp.StatusCode)
	}
	tokens := srv.store.List()
	if tokens[0].LastClientVersion != "" {
		t.Errorf("LastClientVersion = %q, want empty (header absent)", tokens[0].LastClientVersion)
	}
}

func TestHealthNoAuthRequired(t *testing.T) {
	hs, _ := newTestServer(t)
	// No Authorization header — must still return 200.
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("health without auth returned %d, want 200", resp.StatusCode)
	}
}

func TestProtocolVersionHeaderOnEveryResponse(t *testing.T) {
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := resp.Header.Get("X-Bridge-Protocol")
	want := strconv.Itoa(version.ProtocolVersion)
	if got != want {
		t.Errorf("X-Bridge-Protocol = %q, want %q", got, want)
	}
}

func TestProtocolVersionHeaderOn404(t *testing.T) {
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/nothing-here")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	// Middleware must still fire on unmatched routes.
	if got := resp.Header.Get("X-Bridge-Protocol"); got != strconv.Itoa(version.ProtocolVersion) {
		t.Errorf("X-Bridge-Protocol on 404 = %q", got)
	}
}

func TestLibraryRootsBasenamesOnly(t *testing.T) {
	// The full server-side absolute path must NOT leak to clients.
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "/tmp") || strings.Contains(string(body), "/var/folders") {
		t.Errorf("health body leaks absolute server path: %s", body)
	}
}

// ---- authed() middleware tests, using a tiny probe handler ----

func newTestServerWithProbe(t *testing.T) (*httptest.Server, *Server, string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	os.MkdirAll(lib, 0o755)
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := store.Mint("test")
	s := New(cfg, store, nil, "fp")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/probe", s.authed(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "probe-ok")
	}))
	hs := httptest.NewServer(protocolHeader(mux))
	t.Cleanup(hs.Close)
	return hs, s, raw
}

func TestAuthedMissingTokenReturns401(t *testing.T) {
	hs, _, _ := newTestServerWithProbe(t)
	resp, err := http.Get(hs.URL + "/v1/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	var er ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&er)
	if er.Error != "unauthorized" {
		t.Errorf("error code = %q", er.Error)
	}
}

func TestAuthedBadTokenReturns401(t *testing.T) {
	hs, _, _ := newTestServerWithProbe(t)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthedValidTokenPassesThrough(t *testing.T) {
	hs, _, raw := newTestServerWithProbe(t)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d; body = %s", resp.StatusCode, body)
	}
	if string(body) != "probe-ok" {
		t.Errorf("body = %q", body)
	}
}

func TestAuthedBearerCaseInsensitive(t *testing.T) {
	// Clients differ: "Bearer X", "bearer X", "BEARER X" must all work.
	hs, _, raw := newTestServerWithProbe(t)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req, _ := http.NewRequest("GET", hs.URL+"/v1/probe", nil)
		req.Header.Set("Authorization", scheme+" "+raw)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("scheme %q: %v", scheme, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("scheme %q: status = %d, body = %s", scheme, resp.StatusCode, body)
		}
	}
}

func TestAuthedWrongSchemeRejected(t *testing.T) {
	hs, _, raw := newTestServerWithProbe(t)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/probe", nil)
	req.Header.Set("Authorization", "Basic "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401 (non-Bearer scheme must not fall through)", resp.StatusCode)
	}
}

// ---- integration over real TLS + fingerprint pinning ----

// TestHealthOverTLSWithPinnedCert proves the full wire works: a TLS server
// using a cert minted by internal/tls, fingerprint pinning on the client, a
// /v1/health round-trip that matches schema. This is the nearest Go-side
// analogue to what the iOS pairing flow will do.
func TestHealthOverTLSWithPinnedCert(t *testing.T) {
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	os.MkdirAll(lib, 0o755)

	certPath, keyPath := servertls.DefaultPaths(tmp)
	cert, fingerprint, err := servertls.LoadOrGenerate(certPath, keyPath, "localhost")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "TLS"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))

	s := New(cfg, store, nil, fingerprint)
	hs := httptest.NewUnstartedServer(s.Handler())
	hs.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	hs.StartTLS()
	defer hs.Close()

	var peerFP string
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				// Defensive bounds check — matches the guarded access
				// in cmd/bridge/main_test.go. A server-side TLS handshake
				// always populates PeerCertificates, but keeping the two
				// call sites symmetric avoids a latent panic if the
				// handshake behavior ever changes.
				if len(state.PeerCertificates) == 0 {
					return nil
				}
				peerFP = servertls.FingerprintFromDER(state.PeerCertificates[0].Raw)
				return nil
			},
		},
	}}

	resp, err := client.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatalf("health over TLS: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if peerFP != fingerprint {
		t.Errorf("peer fingerprint %q != server fingerprint %q", peerFP, fingerprint)
	}
	var got HealthResponse
	json.NewDecoder(resp.Body).Decode(&got)
	if got.CertFingerprint != fingerprint {
		t.Errorf("health reports fingerprint %q, expected %q", got.CertFingerprint, fingerprint)
	}
}

// stubUPnPPublicProvider is a deterministic fake of
// UPnPUpstreamPublicProvider for /v1/health wire-shape coverage; the
// production bridge-side adapter lives in cmd/bridge.
//
// Records whether the handler passed a non-nil ctx, without holding a
// reference — Go convention (S8242) is to avoid storing a
// `context.Context` on a struct. The boolean witness preserves the
// propagation assertion without the lifecycle hazard.
type stubUPnPPublicProvider struct {
	servers           []UPnPUpstreamPublicServer
	nonNilCtxObserved bool
}

func (s *stubUPnPPublicProvider) PublicServers(ctx context.Context) []UPnPUpstreamPublicServer {
	if ctx != nil {
		s.nonNilCtxObserved = true
	}
	return s.servers
}

// newUPnPHealthTestServer spins up an httptest.Server with the api
// Handler and (optionally) a wired UPnPUpstreamPublicProvider —
// shared by the three /v1/health UPnP wire-shape cases below. Returns
// the server (caller defers Close), and the provider so the
// EmittedFromProvider case can assert ctx propagation post-hit.
//
// Centralizes the tmp / cfg / auth.OpenStore / httptest.NewServer
// boilerplate the three cases were duplicating. Doesn't extend to the
// existing `newTestServer` helper because that one doesn't expose the
// UPnP-provider wiring point (and rewiring it would expand this PR's
// scope beyond the UPnP feature).
func newUPnPHealthTestServer(t *testing.T, provider UPnPUpstreamPublicProvider) (hs *httptest.Server, p *stubUPnPPublicProvider) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	store, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, nil, "fp")
	if provider != nil {
		srv = srv.WithUPnPUpstreamPublicProvider(provider)
		if stub, ok := provider.(*stubUPnPPublicProvider); ok {
			p = stub
		}
	}
	return httptest.NewServer(srv.Handler()), p
}

// fetchUPnPHealthBody hits /v1/health on the supplied test server and
// returns the raw response body. Centralizes the GET + defer + read
// boilerplate the three cases were duplicating.
func fetchUPnPHealthBody(t *testing.T, hs *httptest.Server) []byte {
	t.Helper()
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestHealth_UPnPUpstreamServers_OmittedWithoutProvider pins the
// pre-feature wire-shape: a Server without WithUPnPUpstreamPublicProvider
// MUST omit `upnpUpstreamServers` entirely so pre-feature iOS sees the
// same shape it did before the field landed. Asserts on raw JSON
// bytes (omitempty on a `[]Type` field only fires for nil-not-empty;
// the absence-on-wire is the load-bearing guarantee).
func TestHealth_UPnPUpstreamServers_OmittedWithoutProvider(t *testing.T) {
	hs, _ := newUPnPHealthTestServer(t, nil)
	defer hs.Close()
	body := fetchUPnPHealthBody(t, hs)
	if strings.Contains(string(body), "upnpUpstreamServers") {
		t.Errorf("upnpUpstreamServers should be omitted when provider is unwired, got: %s", body)
	}
}

// TestHealth_UPnPUpstreamServers_EmittedFromProvider locks the
// happy-path wire shape: a wired provider's PublicServers output flows
// through `/v1/health` byte-for-byte (field order on the wire follows
// the provider's slice order, which iOS uses for deterministic
// sub-source row ordering).
func TestHealth_UPnPUpstreamServers_EmittedFromProvider(t *testing.T) {
	provider := &stubUPnPPublicProvider{
		servers: []UPnPUpstreamPublicServer{
			{
				Name:          "Chord 2Go (cards)",
				ConfiguredUDN: "uuid:2go",
				PathPrefix:    "2go",
				FriendlyName:  "Chord 2Go:2go-ars",
				RoutedTracks:  247,
			},
			{
				Name:         "Manual MiniDLNA",
				PathPrefix:   "manual",
				RoutedTracks: 18,
				// ConfiguredUDN + FriendlyName intentionally empty
				// — manual-URL entries pre-discovery.
			},
		},
	}
	hs, p := newUPnPHealthTestServer(t, provider)
	defer hs.Close()
	body := fetchUPnPHealthBody(t, hs)
	var got HealthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.UPnPUpstreamServers) != 2 {
		t.Fatalf("got %d rows; want 2", len(got.UPnPUpstreamServers))
	}
	if got.UPnPUpstreamServers[0].Name != "Chord 2Go (cards)" {
		t.Errorf("row 0 Name = %q; want %q", got.UPnPUpstreamServers[0].Name, "Chord 2Go (cards)")
	}
	if got.UPnPUpstreamServers[0].ConfiguredUDN != "uuid:2go" {
		t.Errorf("row 0 ConfiguredUDN = %q; want %q", got.UPnPUpstreamServers[0].ConfiguredUDN, "uuid:2go")
	}
	if got.UPnPUpstreamServers[0].PathPrefix != "2go" {
		t.Errorf("row 0 PathPrefix = %q; want %q", got.UPnPUpstreamServers[0].PathPrefix, "2go")
	}
	if got.UPnPUpstreamServers[0].FriendlyName != "Chord 2Go:2go-ars" {
		t.Errorf("row 0 FriendlyName = %q; want %q", got.UPnPUpstreamServers[0].FriendlyName, "Chord 2Go:2go-ars")
	}
	if got.UPnPUpstreamServers[0].RoutedTracks != 247 {
		t.Errorf("row 0 RoutedTracks = %d; want 247", got.UPnPUpstreamServers[0].RoutedTracks)
	}
	if got.UPnPUpstreamServers[1].Name != "Manual MiniDLNA" {
		t.Errorf("row 1 Name = %q; want %q", got.UPnPUpstreamServers[1].Name, "Manual MiniDLNA")
	}
	// Context propagation: the handler MUST pass `r.Context()` so a
	// client disconnect mid-/v1/health cancels downstream SQLite
	// queries. A regression that swaps in a synthetic background
	// context would silently disable query cancellation. The stub's
	// witness is a boolean (not a stored ctx) so the assertion lives
	// on a copyable, non-lifecycle-bearing primitive. `p` is the
	// same `*stubUPnPPublicProvider` the helper returned at the top
	// of the test (typed-assert back from the interface argument).
	if !p.nonNilCtxObserved {
		t.Errorf("nonNilCtxObserved is false — handler didn't propagate context")
	}
}

// TestHealth_UPnPUpstreamServers_EmptyProviderResultOmitted pins the
// edge case: a provider wired but returning a NIL slice (feature
// enabled at config-load but no servers configured, OR a torn-down
// RuntimeConfig surfacing nil from the adapter) MUST also drop the
// field from the wire — omitempty handles nil slices but not empty
// slices, so the adapter contract is "return nil to opt out, not []".
// This test pins that contract by passing a nil-returning provider
// and asserting absence on the wire.
func TestHealth_UPnPUpstreamServers_EmptyProviderResultOmitted(t *testing.T) {
	hs, _ := newUPnPHealthTestServer(t, &stubUPnPPublicProvider{servers: nil})
	defer hs.Close()
	body := fetchUPnPHealthBody(t, hs)
	if strings.Contains(string(body), "upnpUpstreamServers") {
		t.Errorf("upnpUpstreamServers should be omitted when provider returns nil, got: %s", body)
	}
}
