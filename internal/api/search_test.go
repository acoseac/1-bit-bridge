package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// stubSearcher lets the handler tests drive every branch without a real
// FTS5 database. The STORE-level served-set restriction is pinned in
// internal/manifest (TestSearchServedTracksExcludesSuppressedCopies,
// negative-control-verified against the unjoined query); these tests are
// about the handler's shape, bounds and error mapping.
type stubSearcher struct {
	hits      []manifest.TrackHit
	err       error
	available bool
	gotQuery  string
	gotLimit  int
}

// The two ManifestProvider methods /v1/search never calls. Present so
// the stub satisfies the interface the Server field is typed as.
func (s *stubSearcher) WriteManifest(context.Context, io.Writer, time.Time) error { return nil }

func (s *stubSearcher) BuildManifestPage(context.Context, string, int) (*ManifestPage, error) {
	return &ManifestPage{}, nil
}
func (s *stubSearcher) IsScanning() bool                       { return false }
func (s *stubSearcher) IsScanStalled() bool                    { return false }
func (s *stubSearcher) LastFullScan() time.Time                { return time.Time{} }
func (s *stubSearcher) TracksIndexed(context.Context) int      { return 0 }
func (s *stubSearcher) PendingDeletions(context.Context) int64 { return 0 }

var _ ManifestProvider = (*stubSearcher)(nil)

func (s *stubSearcher) SearchServedTracks(ctx context.Context, q string, limit int) ([]manifest.TrackHit, error) {
	s.gotQuery, s.gotLimit = q, limit
	return s.hits, s.err
}

func (s *stubSearcher) SearchAvailable(ctx context.Context) (bool, error) { return s.available, nil }

// searchServer builds a Server with only the pieces /v1/search touches,
// so these tests do not depend on the rest of the wiring.
func searchServer(t *testing.T, st *stubSearcher) *Server {
	t.Helper()
	s := newRouteRegistryTestServer(t)
	s.manifest = st
	return s
}

func doSearch(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	s.search(rr, httptest.NewRequest(http.MethodGet, target, nil))
	return rr
}

func TestSearchReturnsTheWireShape(t *testing.T) {
	st := &stubSearcher{available: true, hits: []manifest.TrackHit{
		{Path: "Artist/Album/01.flac", Title: "Xtal", Artist: "Aphex Twin", Album: "SAW"},
	}}
	rr := doSearch(t, searchServer(t, st), "/v1/search?q=xtal")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got searchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if len(got.Tracks) != 1 || got.Tracks[0].Path != "Artist/Album/01.flac" {
		t.Fatalf("tracks = %+v", got.Tracks)
	}
	if got.Tracks[0].Title != "Xtal" || got.Tracks[0].Artist != "Aphex Twin" {
		t.Errorf("display context missing: %+v", got.Tracks[0])
	}

	// Paths must be in the manifest's own form — slashless — so the
	// client can use them as a join key without normalising.
	if got.Tracks[0].Path[0] == '/' {
		t.Errorf("path %q is slash-prefixed; /v1/manifest emits the slashless form", got.Tracks[0].Path)
	}

	// `tracks` must serialise as [] rather than null on a miss: a client
	// decoding into a non-optional array would fail on null.
	st.hits = nil
	rr = doSearch(t, searchServer(t, st), "/v1/search?q=nothing")
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["tracks"] == nil {
		t.Errorf("empty result serialised `tracks` as null, want []; body=%s", rr.Body.String())
	}
}

func TestSearchRejectsAShortQuery(t *testing.T) {
	st := &stubSearcher{available: true}
	for _, q := range []string{"", "a", "%20"} {
		rr := doSearch(t, searchServer(t, st), "/v1/search?q="+q)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("q=%q: status %d, want 400", q, rr.Code)
		}
	}
	// Counted in RUNES: a two-character CJK query is long enough, and a
	// byte-length check would wrongly reject it.
	rr := doSearch(t, searchServer(t, st), "/v1/search?q=%E9%9F%B3%E6%A5%BD") // 音楽
	if rr.Code != http.StatusOK {
		t.Errorf("two-rune CJK query: status %d, want 200 — the minimum is counted in runes", rr.Code)
	}
}

func TestSearchBoundsTheLimit(t *testing.T) {
	st := &stubSearcher{available: true}
	s := searchServer(t, st)

	doSearch(t, s, "/v1/search?q=abc")
	if st.gotLimit != searchDefaultLimit {
		t.Errorf("default limit = %d, want %d", st.gotLimit, searchDefaultLimit)
	}

	doSearch(t, s, "/v1/search?q=abc&limit=1000000")
	if st.gotLimit != searchMaxLimit {
		t.Errorf("limit=1000000 passed %d to the store, want it clamped to %d", st.gotLimit, searchMaxLimit)
	}

	for _, bad := range []string{"0", "-1", "abc"} {
		rr := doSearch(t, s, "/v1/search?q=abc&limit="+bad)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("limit=%q: status %d, want 400", bad, rr.Code)
		}
	}
}

// TestSearchUsesSafeQuery pins the `+` trap: url.Values form-decodes a
// literal `+` to a space, so a query for a track whose title contains one
// would silently search for something else. Every path-bearing handler in
// this package routes through safeQuery for exactly this reason.
func TestSearchUsesSafeQuery(t *testing.T) {
	st := &stubSearcher{available: true}
	doSearch(t, searchServer(t, st), "/v1/search?q=Sigur+R%C3%B3s")
	if st.gotQuery != "Sigur+Rós" {
		t.Errorf("query reached the store as %q; safeQuery must preserve a literal +", st.gotQuery)
	}
}

func TestSearchMapsUnavailableTo503(t *testing.T) {
	st := &stubSearcher{available: false, err: manifest.ErrSearchUnavailable}
	rr := doSearch(t, searchServer(t, st), "/v1/search?q=abc")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "search_unavailable") {
		t.Errorf("body lacks the typed code: %s", rr.Body.String())
	}
}

// TestSearchFeatureFlagTracksTheRuntimeFact is the contract that keeps
// /v1/health and the endpoint from disagreeing: both read the same probe,
// so a bridge whose FTS5 probe failed never advertises search.
func TestSearchFeatureFlagTracksTheRuntimeFact(t *testing.T) {
	st := &stubSearcher{available: true}
	s := searchServer(t, st)
	if !s.searchAvailable() {
		t.Error("searchAvailable false with FTS5 present")
	}
	st.available = false
	if s.searchAvailable() {
		t.Error("searchAvailable true with FTS5 absent — health would advertise an endpoint that 503s")
	}

	// A provider with no search capability at all must also not advertise.
	s.manifest = nil
	if s.searchAvailable() {
		t.Error("searchAvailable true with no manifest provider")
	}
}

// TestSearchIsAdvertisedInHealth is the test whose ABSENCE let the
// feature flag ship missing: an earlier edit to the health handler
// silently failed, the endpoint shipped unadvertised, and nothing caught
// it because the only assertion was on the searchAvailable HELPER —
// which proves nothing about whether anything CALLS it.
//
// So it drives the real health handler and reads the real response,
// against a manifest that actually supports search.
func TestSearchIsAdvertisedInHealth(t *testing.T) {
	st := &stubSearcher{available: true}
	s := searchServer(t, st)

	rr := httptest.NewRecorder()
	s.health(rr, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got HealthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if !slices.Contains(got.Features, "search") {
		t.Fatalf("`search` missing from /v1/health features: %v\n"+
			"The endpoint exists but nothing advertises it, so no client will call it.",
			got.Features)
	}

	// And it must disappear when FTS5 is absent — advertising a capability
	// the endpoint would 503 is worse than not advertising it at all.
	st.available = false
	rr = httptest.NewRecorder()
	s.health(rr, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.Features, "search") {
		t.Errorf("`search` advertised with FTS5 absent: %v", got.Features)
	}
}

// TestSearchSwallowsAClientCancellation — this endpoint is called per
// keystroke, so an abandoned request is the normal case. Reporting it as
// 500 would bury real failures in the logs and error metrics.
func TestSearchSwallowsAClientCancellation(t *testing.T) {
	st := &stubSearcher{available: true, err: context.Canceled}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/search?q=abc", nil)
	searchServer(t, st).search(rr, req)

	if rr.Code >= 500 {
		t.Errorf("a cancelled request answered %d; it must not read as a server fault", rr.Code)
	}
	if strings.Contains(rr.Body.String(), `"error"`) {
		t.Errorf("a cancelled request wrote an error body: %s", rr.Body.String())
	}
}
