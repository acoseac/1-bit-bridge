package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oversizedValidJSON serves a WELL-FORMED JSON document larger than
// maxJSONResponseBytes: the caller's `first` element (which every client's
// matcher accepts) followed by enough `pad` elements to blow past the cap.
//
// Valid and oversized is what makes these negative-controllable. Without
// the cap the decoder consumes the whole document and every client returns
// a clean hit; with it the read stops short and the decode fails. An
// UNterminated body would error either way and prove nothing.
//
// This is what a hostile or misconfigured upstream looks like to a decoder:
// the bytes arrive at a healthy rate, so http.Client{Timeout} — which
// bounds DURATION, not memory — never fires. enrich.musicbrainzBaseURL is
// operator-configurable (Atlas, or any self-hosted mirror), which is why
// the package already regex-validates search-result MBIDs before they reach
// a URL or a cache path; the JSON decoders were the one unbounded read left.
func oversizedValidJSON(t *testing.T, open, first, pad, closing string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, open+first); err != nil {
			return
		}
		for written := 0; written < maxJSONResponseBytes; written += len(pad) + 1 {
			if _, err := io.WriteString(w, ","+pad); err != nil {
				return // client stopped reading — the pass condition
			}
		}
		_, _ = io.WriteString(w, closing)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// filler pads an element out so the loop reaches the cap in few iterations.
var filler = strings.Repeat("x", 4096)

func TestMusicBrainzGetCapsResponseBody(t *testing.T) {
	srv := oversizedValidJSON(t,
		`{"releases":[`,
		`{"id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}`,
		`{"id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","score":1,"title":"`+filler+`","artist-credit":[{"name":"Nobody"}]}`,
		`]}`)
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	c.minInterval = 0
	if _, err := c.SearchRelease(context.Background(), "Artist", "Album"); err == nil {
		t.Fatal("decoded an oversized body without error — the cap did not fire")
	}
}

func TestITunesGetCapsResponseBody(t *testing.T) {
	srv := oversizedValidJSON(t,
		`{"resultCount":1,"results":[`,
		`{"collectionId":1,"collectionName":"Album","artworkUrl100":"http://x/100x100bb.jpg","wrapperType":"collection"}`,
		`{"collectionId":0,"collectionName":"`+filler+`","artworkUrl100":"","wrapperType":"collection"}`,
		`]}`)
	c := NewITunesClient(srv.URL, "test", nil)
	if _, err := c.SearchAlbum(context.Background(), "Artist", "Album"); err == nil {
		t.Fatal("decoded an oversized body without error — the cap did not fire")
	}
}

func TestDeezerSearchArtistCapsResponseBody(t *testing.T) {
	srv := oversizedValidJSON(t,
		`{"data":[`,
		`{"id":1,"name":"Artist","picture_xl":"http://x/a.jpg","picture_big":""}`,
		`{"id":2,"name":"`+filler+`","picture_xl":"","picture_big":""}`,
		`]}`)
	c := NewDeezerClient(srv.URL, "test", nil)
	if _, err := c.SearchArtist(context.Background(), "Artist"); err == nil {
		t.Fatal("decoded an oversized body without error — the cap did not fire")
	}
}

// TestJSONBodyCapLeavesRealResponsesAlone is the control for the cap itself:
// a normal search response is orders of magnitude under it (the largest is
// an artist search at artistSearchLimit=25, tens of KiB) and must decode
// exactly as before.
func TestJSONBodyCapLeavesRealResponsesAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"artists":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","score":100,"name":"Artist"}]}`)
	}))
	defer srv.Close()
	c := NewMusicBrainzClient(srv.URL, "test", nil)
	c.minInterval = 0
	got, err := c.SearchArtist(context.Background(), "Artist")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if got == nil || got.Title != "Artist" {
		t.Fatalf("got %+v, want the decoded artist", got)
	}
}
