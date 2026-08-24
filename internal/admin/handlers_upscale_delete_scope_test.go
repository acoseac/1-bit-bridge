package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func deleteVariants(t *testing.T, srv *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/upscale/variants?"+query, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestVariantDeleteByAlbumIDScopesToThatAlbum is the destructive half
// of the neighbour-isolation contract, and the half that actually costs
// something when it is wrong: a prefix delete for an album sharing its
// directory would unlink every neighbour's sidecar, and the operator
// would only find out the next time they played one.
func TestVariantDeleteByAlbumIDScopesToThatAlbum(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	stub := &stubVariantDeleter{}
	srv.deps.VariantDeleter = stub

	soID := albumIDByTitle(t, srv, "So")
	w := deleteVariants(t, srv, "albumId="+soID+"&kind=optimize")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	got := sortedCopy(stub.gotReq.Paths)
	want := []string{
		"Music/Peter Gabriel/Hi-Res Masters/So - 01.flac",
		"Music/Peter Gabriel/Hi-Res Masters/So - 02.flac",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("delete scope = %v, want exactly %v", got, want)
	}
	if stub.gotReq.All || stub.gotReq.Prefix != "" || stub.gotReq.Path != "" {
		t.Errorf("identity delete also set another shape: %+v", stub.gotReq)
	}
	if stub.gotReq.Kind != "optimize" {
		t.Errorf("kind = %q, want optimize", stub.gotReq.Kind)
	}
}

// TestVariantDeleteByArtistIDUnionsAlbums: same expansion the submit
// side does, so an operator clearing an artist gets one request rather
// than one per track.
func TestVariantDeleteByArtistIDUnionsAlbums(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	stub := &stubVariantDeleter{}
	srv.deps.VariantDeleter = stub

	id := artistIDByName(t, srv, "Peter Gabriel")
	w := deleteVariants(t, srv, "artistId="+id)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	if n := len(stub.gotReq.Paths); n != 3 {
		t.Errorf("artist delete scope = %d paths, want 3: %v", n, stub.gotReq.Paths)
	}
}

// TestVariantDeleteIdentityNeverFallsThroughToAll is the one that
// matters most. The unscoped shape wipes the entire variant cache, and
// it is reached by a request that names no scope — so an identity
// parameter the parser failed to recognise would be silently
// reinterpreted as "delete everything" the moment a caller also passed
// confirm=true.
func TestVariantDeleteIdentityNeverFallsThroughToAll(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	stub := &stubVariantDeleter{}
	srv.deps.VariantDeleter = stub

	soID := albumIDByTitle(t, srv, "So")
	w := deleteVariants(t, srv, "albumId="+soID+"&confirm=true")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	if stub.gotReq.All {
		t.Fatal("an album-scoped delete carrying confirm=true was widened to the whole cache")
	}
	if len(stub.gotReq.Paths) != 2 {
		t.Errorf("scope = %v, want the album's 2 tracks", stub.gotReq.Paths)
	}
}

// TestVariantDeleteScopeRejections: the combinations that mean two
// things at once are refused before the deleter runs.
func TestVariantDeleteScopeRejections(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	stub := &stubVariantDeleter{}
	srv.deps.VariantDeleter = stub

	soID := albumIDByTitle(t, srv, "So")
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"album plus prefix", "albumId=" + soID + "&prefix=Music"},
		{"album plus path", "albumId=" + soID + "&path=Music/A.flac"},
		{"album plus artist", "albumId=" + soID + "&artistId=" + soID},
		{"malformed album id", "albumId=nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := stub.gotCalls
			w := deleteVariants(t, srv, tc.query)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if stub.gotCalls != before {
				t.Error("a rejected delete scope still reached the deleter")
			}
		})
	}
}

// TestVariantDeleteBlankIdentityIsRejected covers the shape that has
// no business reaching the deleter at all.
//
// A parameter that is PRESENT but blank is a malformed identity
// request, not the absence of one. Read as an absence, it leaves every
// shape unset — and a delete request with no shape is precisely the one
// that means "every variant in the manifest". RunVariantDelete's own
// guard refuses it, so this was never a wipe; it was a 500 wearing a
// guard's clothes, and the handler should never build the request in
// the first place.
//
// The two spellings take different routes on purpose: `?albumId=`
// carries a value that fails id validation, `?artistId=` carries
// nothing at all and would otherwise slip past a presence check that
// only looked at the value.
func TestVariantDeleteBlankIdentityIsRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	stub := &stubVariantDeleter{}
	srv.deps.VariantDeleter = stub

	// `confirm=true` is the load-bearing half of this table. Without
	// it a blank identity that slipped past the presence check would
	// still 400 — on the unscoped form's own confirm gate — and the
	// test would pass for a reason unrelated to what it is pinning.
	// WITH it, the same slip sets All and clears the entire variant
	// cache. A presence check that inspects the VALUE rather than the
	// PARAMETER fails exactly here and nowhere else.
	for _, query := range []string{
		"albumId=", "artistId=", "artistId=&kind=optimize", "albumId=&albumId=",
		"artistId=&confirm=true", "albumId=&confirm=true",
	} {
		t.Run(query, func(t *testing.T) {
			w := deleteVariants(t, srv, query)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if stub.gotCalls != 0 {
				t.Fatalf("a blank identity parameter reached the deleter as %+v "+
					"(All=%v would clear every variant in the manifest)",
					stub.gotReq, stub.gotReq.All)
			}
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if code, _ := body["error"].(string); code == "" {
				t.Errorf("no error code in body %s", w.Body.String())
			}
		})
	}
}
