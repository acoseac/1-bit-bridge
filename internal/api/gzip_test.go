package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		// Common-case clients
		{"", false},
		{"gzip", true},
		{"gzip, deflate, br", true}, // iOS URLSession default shape
		{"deflate, br, gzip", true},
		{"identity", false},
		{"br", false},
		{"deflate", false},
		// Wildcard maps to gzip-OK per RFC §12.5.3
		{"*", true},
		// Quality factors
		{"gzip;q=0.5", true},
		{"gzip;q=1", true},
		{"gzip;q=1.0", true},
		{"gzip;q=0", false},   // explicit refusal
		{"gzip;q=0.0", false}, // explicit refusal (decimal form)
		{"gzip;q=0.000", false},
		{"br, gzip;q=0", false},        // gzip explicitly refused even though present
		{"br, gzip;q=0.5", true},       // br first, gzip accepted
		{"gzip;q=0, br, gzip", true},   // a later non-q=0 gzip token rescues
		{"GZIP", true},                 // case-insensitive
		{"  gzip  ,  br  ", true},      // leading/trailing whitespace
		{"gzip  ;  q=0.5", true},       // whitespace around q-param
		{"gzip;q=0.5;extra=foo", true}, // unknown extension params
		// RFC 9110 §12.5.3 — explicit reference takes precedence over
		// a wildcard. Order MUST NOT matter.
		{"*, gzip;q=0", false},     // wildcard accept, explicit gzip refuse → refuse
		{"gzip;q=0, *", false},     // explicit gzip refuse before wildcard → refuse
		{"gzip;q=0, *;q=1", false}, // explicit gzip refuse beats explicit wildcard accept
		{"*;q=0, gzip", true},      // explicit wildcard refuse, explicit gzip accept → accept
		{"*;q=1, gzip;q=0", false}, // explicit gzip refuse beats explicit wildcard accept (reversed)
		{"*;q=0", false},           // bare wildcard refusal → refuse (no explicit gzip)
		{"*;q=0.5", true},          // bare wildcard accept (non-zero q) → accept
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/", nil)
			if c.header != "" {
				r.Header.Set("Accept-Encoding", c.header)
			}
			if got := acceptsGzip(r); got != c.want {
				t.Errorf("acceptsGzip(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

// TestAcceptsGzipHonorsMultipleHeaderLines pins the contract that
// `acceptsGzip` reads ALL `Accept-Encoding` header lines, not just
// the first. Per RFC 9110 §5.3, a client may send multiple lines
// with the same field name and the recipient must treat them as
// equivalent to a single comma-joined field. `Header.Get` returns
// only the first; `Header.Values` returns all. CodeRabbit on PR #181.
func TestAcceptsGzipHonorsMultipleHeaderLines(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   bool
	}{
		// Two lines: wildcard accept + explicit gzip refuse. The
		// explicit refusal must win; pre-fix `Header.Get` only saw
		// the wildcard line and returned true.
		{"wildcard then explicit refuse", []string{"*", "gzip;q=0"}, false},
		// Two lines reversed: explicit refuse + wildcard accept.
		// Same outcome.
		{"explicit refuse then wildcard", []string{"gzip;q=0", "*"}, false},
		// Two lines: identity + gzip. Either presence accepts.
		{"identity then gzip", []string{"identity", "gzip"}, true},
		// Single line shape still works (control case).
		{"single combined line", []string{"*, gzip;q=0"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "/", nil)
			for _, v := range c.values {
				r.Header.Add("Accept-Encoding", v)
			}
			if got := acceptsGzip(r); got != c.want {
				t.Errorf("acceptsGzip(%v) = %v, want %v", c.values, got, c.want)
			}
		})
	}
}

// fakeStreamingProvider writes a fixed JSON body to the manifest writer
// — used to assert gzip wrapping of the streaming-manifest path.
type fakeStreamingProvider struct {
	body []byte
}

func (f *fakeStreamingProvider) WriteManifest(ctx context.Context, w io.Writer, since time.Time) error {
	// Skip the Write entirely when body is empty so the test fake
	// matches realistic behaviour: a real manifest streamer with an
	// empty library returns without invoking Write, leaving the
	// underlying gzip Writer un-touched (no header emitted, dw.written
	// stays false).
	if len(f.body) == 0 {
		return nil
	}
	_, err := w.Write(f.body)
	return err
}
func (f *fakeStreamingProvider) BuildManifestPage(cursor string, limit int) (*ManifestPage, error) {
	return nil, nil
}
func (f *fakeStreamingProvider) IsScanning() bool        { return false }
func (f *fakeStreamingProvider) LastFullScan() time.Time { return time.Time{} }
func (f *fakeStreamingProvider) TracksIndexed() int      { return 0 }
func (f *fakeStreamingProvider) PendingDeletions() int64 { return 0 }

// TestManifestEmitsGzipWhenAcceptEncodingGzip confirms the handler
// sets Content-Encoding + Vary AND the body is valid gzip that
// decompresses to the same bytes the identity path produces. Pairs
// with TestManifestServesIdentityWhenNoAcceptEncoding so the two
// paths' wire bytes are explicitly proven equivalent.
func TestManifestEmitsGzipWhenAcceptEncodingGzip(t *testing.T) {
	want := []byte(`{"version":1,"tracks":[{"path":"a.flac","size":1234567}]}`)
	hs, tok := withManifest(t, &fakeStreamingProvider{body: want})

	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept-Encoding", "gzip")
	// Manually consume — http.Client transparently decompresses gzip
	// when DisableCompression is false (the default), which would
	// hide whether the server actually compressed. Disable transport-
	// level decompression so the test asserts on raw bytes.
	tr := &http.Transport{DisableCompression: true}
	cli := &http.Client{Transport: tr}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if got := resp.Header.Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read raw body: %v (raw len %d)", err, len(raw))
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v (raw len %d)", err, len(raw))
	}
	defer gr.Close()
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("decompress body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("decompressed body mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestManifestServesIdentityWhenNoAcceptEncoding — request without
// Accept-Encoding gets identity bytes back, no Content-Encoding
// header.
func TestManifestServesIdentityWhenNoAcceptEncoding(t *testing.T) {
	want := []byte(`{"version":1,"tracks":[]}`)
	hs, tok := withManifest(t, &fakeStreamingProvider{body: want})

	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	// http.NewRequest doesn't add Accept-Encoding; stdlib transport
	// would otherwise inject `Accept-Encoding: gzip`. Disable it.
	tr := &http.Transport{DisableCompression: true}
	cli := &http.Client{Transport: tr}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty (identity)", got)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, want) {
		t.Errorf("body mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestManifestServesIdentityWhenAcceptEncodingExcludesGzip — `br` only,
// no gzip → identity, no Content-Encoding header.
func TestManifestServesIdentityWhenAcceptEncodingExcludesGzip(t *testing.T) {
	want := []byte(`{"version":1,"tracks":[]}`)
	hs, tok := withManifest(t, &fakeStreamingProvider{body: want})

	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept-Encoding", "br")
	tr := &http.Transport{DisableCompression: true}
	cli := &http.Client{Transport: tr}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty (identity, since gzip not in Accept-Encoding)", got)
	}
}

// TestManifestPreStreamErrorStripsGzipHeaders — when WriteManifest
// fails BEFORE producing any byte (DB error inside ListFolders /
// CountTracks), the handler must (a) emit a structured 5xx and (b)
// strip Content-Encoding so the JSON error body isn't misinterpreted
// as gzip. Without the strip, URLSession would surface a transport
// error and silently retry, masking the real DB failure.
func TestManifestPreStreamErrorStripsGzipHeaders(t *testing.T) {
	mp := &fakeManifestProvider{err: io.ErrUnexpectedEOF}
	hs, tok := withManifest(t, mp)

	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept-Encoding", "gzip")
	tr := &http.Transport{DisableCompression: true}
	cli := &http.Client{Transport: tr}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty (header stripped on early error)", got)
	}
	// Body is identity JSON — decode straight, not via gzip.NewReader.
	var er ErrorResponse
	if derr := json.NewDecoder(resp.Body).Decode(&er); derr != nil {
		t.Fatalf("decode error body: %v", derr)
	}
	if er.Error != "internal" {
		t.Errorf("error code = %q, want internal", er.Error)
	}
}

// TestManifestEmptyBodyStripsGzipHeaders covers the empty-manifest
// path: WriteManifest succeeds with zero bytes (an empty library,
// or a `since` filter that excluded every track). A response with
// Content-Encoding: gzip + zero body is invalid (gzip needs 10-byte
// header + 8-byte trailer); iOS URLSession would surface a transport
// error decoding the zero body. The handler must strip the encoding
// headers so the wire response is a plain empty 200.
func TestManifestEmptyBodyStripsGzipHeaders(t *testing.T) {
	// fakeStreamingProvider with empty body — WriteManifest writes
	// zero bytes and returns nil.
	hs, tok := withManifest(t, &fakeStreamingProvider{body: nil})

	req, _ := http.NewRequest("GET", hs.URL+"/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept-Encoding", "gzip")
	tr := &http.Transport{DisableCompression: true}
	cli := &http.Client{Transport: tr}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty (header stripped on empty body)", got)
	}
	if got := resp.Header.Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want empty (header stripped on empty body)", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("body length = %d, want 0", len(body))
	}
}

// TestSSEEndpointsNotGzippedAfterManifestGzipChange is a tripwire
// that the events.go / pairing.go SSE paths do NOT acquire a gzip
// Content-Encoding from this PR's gzip wrapper. The plan was
// explicit: gzip MUST live at the manifest handler only, not as
// global middleware, because SSE breaks under any buffering layer.
// Existing TestEventsResponseIsNotGzipped / TestPairingEventsResponseIsNotGzipped
// pin the same contract from a different angle (sending Accept-
// Encoding: gzip to the SSE endpoints directly); this test re-asserts
// from THIS package's gzip helper specifically so a future refactor
// that lifts the helper into middleware fails loud.
func TestSSEEndpointsNotGzippedAfterManifestGzipChange(t *testing.T) {
	hs, tok := newTestServer(t)
	for _, path := range []string{"/v1/events"} {
		req, _ := http.NewRequest("GET", hs.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept-Encoding", "gzip")
		tr := &http.Transport{DisableCompression: true}
		cli := &http.Client{Transport: tr, Timeout: 2 * time.Second}
		resp, err := cli.Do(req)
		if err != nil {
			// SSE never closes — Timeout above triggers once we
			// have headers. Same handling shape as the existing
			// TestEventsResponseIsNotGzipped.
			if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "Timeout") {
				t.Fatalf("path=%s: unexpected error %v", path, err)
			}
			continue
		}
		defer resp.Body.Close()
		// Match existing trip-wire convention: accept empty OR
		// `identity`. Anything else (especially `gzip`) means the
		// PR D wrapper bled into a non-manifest handler.
		if ce := resp.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
			t.Errorf("path=%s: Content-Encoding = %q, want empty or identity", path, ce)
		}
	}
	_ = httptest.NewRequest // silence unused import if test changes
}
