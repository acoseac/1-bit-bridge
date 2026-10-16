package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// TestAnSACDVirtualPathAnswers404NotFound: a route asked for an SACD image's
// virtual track path answers PROTOCOL.md's `404 not_found` (backlog B414).
// The image is a regular file, so the resolver's stat goes through a file and
// fails with ENOTDIR on unix, which the routes answered `500 internal` until
// ResolveChecked counted it as not found. An app from before SACD ISO support
// fetches the virtual path itself, and its missing-track affordance rests on
// the 404. The image itself, which a current app fetches and demuxes, is
// still served.
func TestAnSACDVirtualPathAnswers404NotFound(t *testing.T) {
	hs, tok, root := fileFixture(t)
	image := bytes.Repeat([]byte{0x5A}, 4096)
	if err := os.WriteFile(filepath.Join(root, "Artist", "Disc.iso"), image, 0o644); err != nil {
		t.Fatal(err)
	}
	q := func(p string) string { return "?path=" + url.QueryEscape(p) }
	const virtual = "Artist/Disc.iso/st/01.dff"
	for _, tc := range []struct{ name, target, rng string }{
		{"read", "/v1/read" + q(virtual), "bytes=0-1023"},
		{"download", "/v1/download" + q(virtual), ""},
		{"ranged download", "/v1/download" + q(virtual), "bytes=0-1023"},
		{"stat", "/v1/stat" + q(virtual), ""},
		{"list of the area", "/v1/list" + q("Artist/Disc.iso/st"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := authGetRange(t, hs, tc.target, tok, tc.rng)
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			var er ErrorResponse
			_ = json.Unmarshal(body, &er)
			if resp.StatusCode != http.StatusNotFound || er.Error != "not_found" {
				t.Fatalf("GET %s: status = %d, body = %s; want 404 not_found", tc.target, resp.StatusCode, body)
			}
		})
	}

	resp := authGetRange(t, hs, "/v1/read"+q("Artist/Disc.iso"), tok, "bytes=0-1023")
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(got, image[:1024]) {
		t.Fatalf("the image itself: status = %d, %d bytes; want 206 and its first 1024 bytes", resp.StatusCode, len(got))
	}
}
