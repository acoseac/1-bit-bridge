package enrich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// SearchAlbum must reject a candidate carrying an empty collectionName
// rather than returning it as a match for any query.
//
// The either-direction substring filter is
//
//	!Contains(nameLower, albumLower) && !Contains(albumLower, nameLower)
//
// and Contains(x, "") is ALWAYS true in Go — so a nameless candidate
// made the second half false, the whole condition false, and the
// candidate was returned as a hit. Its artwork would then be cached
// under the MB-derived release MBID and served to iOS until the cache
// was cleared.
//
// Needs a malformed upstream payload to fire (real iTunes populates
// collectionName on every album result), so this is insurance rather
// than a live incident — but it's one guard on a line that already
// filters two other empty fields.
func TestSearchAlbum_RejectsEmptyCollectionName(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resultCount": 2,
			"results": []map[string]any{
				// Malformed: complete except for the title.
				{
					"collectionId":   int64(111),
					"collectionName": "",
					"artworkUrl100":  srv.URL + "/a/100x100bb.jpg",
				},
				// The genuine match, deliberately second so the test
				// fails loudly if the nameless one is taken first.
				{
					"collectionId":   int64(222),
					"collectionName": "Kind of Blue",
					"artworkUrl100":  srv.URL + "/b/100x100bb.jpg",
				},
			},
		})
	}))
	defer srv.Close()

	c := NewITunesClient(srv.URL, "test", nil)

	got, err := c.SearchAlbum(context.Background(), "Miles Davis", "Kind of Blue")
	if err != nil {
		t.Fatalf("SearchAlbum: %v", err)
	}
	if got.CollectionID == 111 {
		t.Fatal("returned the empty-collectionName candidate: " +
			"Contains(album, \"\") is always true, so it matches every query")
	}
	if got.CollectionID != 222 {
		t.Fatalf("CollectionID = %d, want 222 (the real match)", got.CollectionID)
	}
}

// A nameless candidate with no real match behind it must miss, not fall
// through as a hit.
func TestSearchAlbum_EmptyCollectionNameAloneIsNotFound(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resultCount": 1,
			"results": []map[string]any{{
				"collectionId":   int64(111),
				"collectionName": "   ", // whitespace-only counts as empty
				"artworkUrl100":  srv.URL + "/a/100x100bb.jpg",
			}},
		})
	}))
	defer srv.Close()

	c := NewITunesClient(srv.URL, "test", nil)

	if _, err := c.SearchAlbum(context.Background(), "Someone", "Totally Unrelated Album"); err == nil {
		t.Fatal("a nameless candidate was returned as a match for an unrelated query")
	}
}
