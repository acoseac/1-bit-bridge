package dlna

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// extractTrackID — pure URL-path parsing
// -----------------------------------------------------------------------------

func Test_extractTrackID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/dlna/file/abc123", "abc123"},
		{"/dlna/file/abc123/", "abc123"},                      // trailing slash ignored
		{"/dlna/file/abc123/extra", "abc123"},                 // extra segments ignored
		{"/dlna/file/abc123/extra/more", "abc123"},            // multi-level extras ignored
		{"/dlna/file/", ""},                                   // empty trackID
		{"/dlna/file", ""},                                    // no trailing slash, no trackID
		{"/dlna/cds/control", ""},                             // wrong prefix
		{"/v1/download", ""},                                  // unrelated path
		{"", ""},                                              // empty
		{"/dlna/file/16-hex-chars-9999", "16-hex-chars-9999"}, // realistic hash shape
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := extractTrackID(tc.path)
			if got != tc.want {
				t.Errorf("extractTrackID(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// FileHandler — end-to-end with real temp files
// -----------------------------------------------------------------------------

// createTempFile writes contents to a temp file and returns the path
// (caller responsible for cleanup; or use t.Cleanup which we do here).
func createTempFile(t *testing.T, ext, contents string) string {
	t.Helper()
	f, err := os.CreateTemp("", "dlna-test-*"+ext)
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(contents); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func Test_FileHandler_ServesKnownTrack(t *testing.T) {
	path := createTempFile(t, ".dsf", "FAKE DSF CONTENT")
	lib := newTestLib(TrackInfo{
		TrackID: "abc123", AbsolutePath: path,
		FileExtension: ".dsf", Size: 16,
	})
	h := FileHandler(lib)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/abc123", nil)
	req.Header.Set("User-Agent", "Music Player Daemon 0.21.26")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "FAKE DSF CONTENT" {
		t.Errorf("body = %q, want %q", got, "FAKE DSF CONTENT")
	}
	// MPD UA on .dsf → audio/x-dsf
	if got := rec.Header().Get("Content-Type"); got != "audio/x-dsf" {
		t.Errorf("Content-Type = %q, want audio/x-dsf", got)
	}
	if got := rec.Header().Get("transferMode.dlna.org"); got != "Streaming" {
		t.Errorf("transferMode.dlna.org = %q, want Streaming", got)
	}
	if got := rec.Header().Get("contentFeatures.dlna.org"); got == "" {
		t.Errorf("contentFeatures.dlna.org header missing")
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
}

func Test_FileHandler_PerUserAgentMIME_Sony(t *testing.T) {
	path := createTempFile(t, ".dsf", "x")
	lib := newTestLib(TrackInfo{TrackID: "sony", AbsolutePath: path, FileExtension: ".dsf", Size: 1})
	h := FileHandler(lib)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/sony", nil)
	req.Header.Set("User-Agent", "Sony SRS-HG1/3.2")
	rec := httptest.NewRecorder()
	h(rec, req)

	// Sony vendor override → audio/dsd not audio/x-dsf
	if got := rec.Header().Get("Content-Type"); got != "audio/dsd" {
		t.Errorf("Sony UA Content-Type = %q, want audio/dsd (vendor override)", got)
	}
}

func Test_FileHandler_PerUserAgentMIME_Chord(t *testing.T) {
	path := createTempFile(t, ".dsf", "x")
	lib := newTestLib(TrackInfo{TrackID: "chord", AbsolutePath: path, FileExtension: ".dsf", Size: 1})
	h := FileHandler(lib)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/chord", nil)
	req.Header.Set("User-Agent", "Chord 2go/1.5.7")
	rec := httptest.NewRecorder()
	h(rec, req)

	if got := rec.Header().Get("Content-Type"); got != "audio/x-dsf" {
		t.Errorf("Chord UA Content-Type = %q, want audio/x-dsf", got)
	}
}

func Test_FileHandler_RangeRequestReturns206(t *testing.T) {
	const contents = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	path := createTempFile(t, ".flac", contents)
	lib := newTestLib(TrackInfo{TrackID: "rng", AbsolutePath: path, FileExtension: ".flac", Size: int64(len(contents))})
	h := FileHandler(lib)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/rng", nil)
	req.Header.Set("Range", "bytes=5-9")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 Partial Content", rec.Code)
	}
	if got := rec.Body.String(); got != "FGHIJ" {
		t.Errorf("body = %q, want %q (bytes 5-9 of ABCDEFG...)", got, "FGHIJ")
	}
	if got := rec.Header().Get("Content-Range"); got == "" {
		t.Errorf("Range request must produce Content-Range header, got empty")
	}
}

func Test_FileHandler_UnknownTrackID_Returns404(t *testing.T) {
	lib := newTestLib()
	h := FileHandler(lib)
	req := httptest.NewRequest(http.MethodGet, "/dlna/file/nonexistent", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown trackID should return 404, got %d", rec.Code)
	}
}

func Test_FileHandler_FileMissingOnDisk_Returns404(t *testing.T) {
	lib := newTestLib(TrackInfo{TrackID: "missing", AbsolutePath: "/this/path/does/not/exist.dsf", FileExtension: ".dsf", Size: 1})
	h := FileHandler(lib)
	req := httptest.NewRequest(http.MethodGet, "/dlna/file/missing", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing file should return 404 (indistinguishable from unknown trackID), got %d", rec.Code)
	}
}

func Test_FileHandler_NonGETOrHEAD_Returns405(t *testing.T) {
	lib := newTestLib()
	h := FileHandler(lib)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/dlna/file/anything", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s should return 405, got %d", method, rec.Code)
		}
	}
}

func Test_FileHandler_HEAD_ReturnsHeadersWithoutBody(t *testing.T) {
	const contents = "1234567890"
	path := createTempFile(t, ".flac", contents)
	lib := newTestLib(TrackInfo{TrackID: "headtest", AbsolutePath: path, FileExtension: ".flac", Size: int64(len(contents))})
	h := FileHandler(lib)

	req := httptest.NewRequest(http.MethodHead, "/dlna/file/headtest", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rec.Code)
	}
	// http.ServeContent honors HEAD natively — body should be empty
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD response body should be empty, got %d bytes", rec.Body.Len())
	}
	// Headers should still be set
	if got := rec.Header().Get("Content-Type"); got == "" {
		t.Errorf("HEAD must still set Content-Type, got empty")
	}
}

func Test_FileHandler_FileExtensionInferredFromPathIfBlank(t *testing.T) {
	// If TrackInfo.FileExtension is empty (bug or pre-populated nil),
	// the handler should derive it from the AbsolutePath defensively.
	path := createTempFile(t, ".flac", "x")
	lib := newTestLib(TrackInfo{TrackID: "inferred", AbsolutePath: path, FileExtension: "", Size: 1})
	h := FileHandler(lib)
	req := httptest.NewRequest(http.MethodGet, "/dlna/file/inferred", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "flac") {
		t.Errorf("Content-Type should reflect inferred .flac extension, got %q", got)
	}
}

func Test_FileHandler_EmptyTrackIDInURL_Returns404(t *testing.T) {
	lib := newTestLib()
	h := FileHandler(lib)
	req := httptest.NewRequest(http.MethodGet, "/dlna/file/", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("/dlna/file/ (no trackID) should return 404, got %d", rec.Code)
	}
}
