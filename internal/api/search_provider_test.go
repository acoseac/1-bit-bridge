package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestProviderSatisfiesTheSearchSurface is the guard whose absence let
// /v1/search ship INERT.
//
// The api reaches for search via a type assertion on its
// ManifestProvider, and what cmd/bridge actually passes is
// *manifest.Provider — a wrapper — not *manifest.Store. The assertion
// failed on every bridge: the `search` feature flag never appeared in
// /v1/health and the endpoint answered 503 to everyone.
//
// It shipped because the handler tests used a stub that implemented the
// methods DIRECTLY. A test double more capable than production proves
// nothing about production, and no amount of handler testing would have
// caught this. So this asserts against the CONCRETE type the wiring
// uses.
func TestProviderSatisfiesTheSearchSurface(t *testing.T) {
	var p any = (*manifest.Provider)(nil)

	if _, ok := p.(ServedSearcher); !ok {
		t.Error("*manifest.Provider does not implement ServedSearcher — " +
			"GET /v1/search would answer 503 on every bridge")
	}
	// The health flag's probe is a separate anonymous-interface assertion,
	// so it can regress independently of the one above.
	if _, ok := p.(interface {
		SearchAvailable(ctx context.Context) (bool, error)
	}); !ok {
		t.Error("*manifest.Provider does not implement SearchAvailable — " +
			"the `search` feature flag would never be advertised")
	}
	// And it must still be a ManifestProvider, since that is the field
	// the assertions are made against.
	if _, ok := p.(ManifestProvider); !ok {
		t.Error("*manifest.Provider no longer satisfies ManifestProvider")
	}
}

// TestSearchWorksThroughTheRealProvider drives the endpoint end-to-end
// against a real store behind a real Provider — no stub anywhere — so
// the wiring the previous test asserts structurally is also exercised.
func TestSearchWorksThroughTheRealProvider(t *testing.T) {
	// The REAL provider, exactly as cmd/bridge constructs it.
	provider, _ := newRealSearchProvider(t)
	s := newRouteRegistryTestServer(t)
	s.manifest = provider

	rr := httptest.NewRecorder()
	s.health(rr, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	var h HealthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.Features, "search") {
		t.Fatalf("`search` not advertised through the real provider: %v", h.Features)
	}

	rr = httptest.NewRecorder()
	s.search(rr, httptest.NewRequest(http.MethodGet, "/v1/search?q=xtal", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("search through the real provider: status %d, want 200; body=%s",
			rr.Code, rr.Body.String())
	}
	var got searchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tracks) != 1 || got.Tracks[0].Title != "Xtal" {
		t.Errorf("tracks = %+v, want the seeded track", got.Tracks)
	}
}

// TestASearchStoreFailureThroughTheRouterSendsAFixedMessage is backlog
// B425 end to end: the real router, the real Provider and a real store
// whose database is closed under the search, so the query fails the way a
// live store fails, with its own text ("sql: database is closed", under
// the tracks_fts query's prefix). The client gets 500 `internal` and the
// fixed message. The request-failed line carries the error, and it and
// the access line are both Error at 500: a server failure, by B364's rule.
func TestASearchStoreFailureThroughTheRouterSendsAFixedMessage(t *testing.T) {
	buf := withTestSlog(t)
	provider, store := newRealSearchProvider(t)
	router, token := newSearchRouter(t, provider)
	search := func() *httptest.ResponseRecorder {
		return searchWith(context.Background(), router, token)
	}

	// The route, the token and the store all work before the failure.
	if rr := search(); rr.Code != http.StatusOK {
		t.Fatalf("search before the store failed: status %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	rr := search()

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	er := decodeErrorResponse(t, rr.Body.Bytes())
	if er.Error != "internal" || er.Message != searchFailedMessage {
		t.Errorf("body = {%q, %q}, want {internal, %q}", er.Error, er.Message, searchFailedMessage)
	}
	for _, leak := range []string{"tracks_fts", "database is closed", "sql:"} {
		if strings.Contains(rr.Body.String(), leak) {
			t.Errorf("the 500 body carries %q from the store's error: %s", leak, rr.Body.String())
		}
	}

	logged := buf.String()
	failed := linesContaining(logged, `msg="request failed"`)
	if len(failed) != 1 {
		t.Fatalf("want one request-failed line, got %d; log:\n%s", len(failed), logged)
	}
	for _, want := range []string{"level=ERROR", "status=500", "database is closed"} {
		if !strings.Contains(failed[0], want) {
			t.Errorf("request-failed line lacks %q: %s", want, failed[0])
		}
	}
	access := linesContaining(logged, "msg=http ")
	if len(access) != 1 {
		t.Fatalf("want one access line, got %d; log:\n%s", len(access), logged)
	}
	if !strings.Contains(access[0], "level=ERROR") || !strings.Contains(access[0], "status=500") {
		t.Errorf("access line is not Error at 500: %s", access[0])
	}
}
