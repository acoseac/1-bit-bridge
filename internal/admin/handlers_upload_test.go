package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upload"
)

// enableUploads flips the live config the handlers read.
func enableUploads(t *testing.T, srv *Server) {
	t.Helper()
	cfg := config.Clone(srv.deps.CfgHolder.Load())
	cfg.Upload.Enabled = true
	srv.deps.CfgHolder.Store(cfg)
}

func putChunk(t *testing.T, h http.Handler, sid, fid string, offset int64, body []byte, digest []byte) (int, map[string]any) {
	t.Helper()
	url := "/api/upload/sessions/" + sid + "/files/" + fid + "?offset=" + strconv.FormatInt(offset, 10)
	req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/octet-stream")
	if digest != nil {
		req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest)+":")
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	out := map[string]any{}
	if rw.Body.Len() > 0 {
		_ = json.Unmarshal(rw.Body.Bytes(), &out)
	}
	return rw.Code, out
}

func createSession(t *testing.T, srv *Server, files []map[string]any, extra map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"files": files}
	for k, v := range extra {
		body[k] = v
	}
	var out map[string]any
	code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", body, &out)
	if code != http.StatusCreated {
		t.Fatalf("create session = %d, body %v", code, out)
	}
	return out
}

func firstFile(t *testing.T, sess map[string]any) map[string]any {
	t.Helper()
	files, _ := sess["files"].([]any)
	if len(files) == 0 {
		t.Fatal("session carries no files")
	}
	f, _ := files[0].(map[string]any)
	return f
}

// TestUploadDisabledByDefault — every route refuses until the operator opts in.
// Upload is an open WRITE endpoint; it holds the same line as every other
// feature that commits the operator to something.
func TestUploadDisabledByDefault(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/upload/sessions"},
		{"GET", "/api/upload/sessions"},
		{"GET", "/api/upload/sessions/abc"},
		{"POST", "/api/upload/sessions/abc/commit"},
		{"DELETE", "/api/upload/sessions/abc"},
	} {
		var out map[string]any
		var body any
		if tc.method == "POST" && tc.path == "/api/upload/sessions" {
			body = map[string]any{"files": []any{}}
		}
		code := doJSON(t, srv.Handler(), tc.method, tc.path, body, &out)
		if code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 while uploads are off", tc.method, tc.path, code)
		}
	}
	code, _ := putChunk(t, srv.Handler(), "abc", "def", 0, []byte("x"), nil)
	if code != http.StatusForbidden {
		t.Errorf("PUT chunk = %d, want 403 while uploads are off", code)
	}
}

func TestUploadRoundTripCommitsIntoTheLibrary(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)

	body := []byte("some audio bytes")
	sess := createSession(t, srv, []map[string]any{
		{"path": "Pink Floyd/Dark Side/01 Speak to Me.flac", "size": len(body)},
	}, nil)
	if hint, _ := sess["chunkBytes"].(float64); int64(hint) != upload.DefaultChunkBytes {
		t.Errorf("chunkBytes hint = %v, want %d", sess["chunkBytes"], upload.DefaultChunkBytes)
	}
	f := firstFile(t, sess)
	sid, _ := sess["id"].(string)
	fid, _ := f["id"].(string)

	code, chunk := putChunk(t, srv.Handler(), sid, fid, 0, body, nil)
	if code != http.StatusOK {
		t.Fatalf("chunk = %d, body %v", code, chunk)
	}
	if off, _ := chunk["offset"].(float64); int(off) != len(body) {
		t.Fatalf("offset = %v, want %d", chunk["offset"], len(body))
	}
	if done, _ := chunk["complete"].(bool); !done {
		t.Error("file not reported complete after the final byte")
	}
	sum := sha256.Sum256(body)
	if got, _ := chunk["sha256"].(string); got != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want %q", got, hex.EncodeToString(sum[:]))
	}

	var commit map[string]any
	code = doJSON(t, srv.Handler(), "POST", "/api/upload/sessions/"+sid+"/commit", nil, &commit)
	if code != http.StatusOK {
		t.Fatalf("commit = %d, body %v", code, commit)
	}
	if n, _ := commit["committed"].(float64); int(n) != 1 {
		t.Fatalf("committed = %v, want 1 (%v)", commit["committed"], commit)
	}
	if full, _ := commit["fullScan"].(bool); full {
		t.Error("a single-album commit escalated to a full scan")
	}
	dirs, _ := commit["scanDirs"].([]any)
	if len(dirs) != 1 || dirs[0] != "Pink Floyd/Dark Side" {
		t.Errorf("scanDirs = %v, want [Pink Floyd/Dark Side]", dirs)
	}

	dest := filepath.Join(cfg.LibraryRoots[0], "Pink Floyd", "Dark Side", "01 Speak to Me.flac")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("committed file missing: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("committed bytes = %q, want %q", got, body)
	}
}

func TestUploadOffsetMismatchReturns409WithTheActualOffset(t *testing.T) {
	srv, _, _ := newTestServer(t)
	enableUploads(t, srv)
	sess := createSession(t, srv, []map[string]any{{"path": "x.flac", "size": 100}}, nil)
	sid, _ := sess["id"].(string)
	fid, _ := firstFile(t, sess)["id"].(string)

	if code, out := putChunk(t, srv.Handler(), sid, fid, 0, make([]byte, 40), nil); code != http.StatusOK {
		t.Fatalf("first chunk = %d (%v)", code, out)
	}
	code, out := putChunk(t, srv.Handler(), sid, fid, 99, []byte("x"), nil)
	if code != http.StatusConflict {
		t.Fatalf("mismatched offset = %d, want 409 (%v)", code, out)
	}
	if off, _ := out["offset"].(float64); int(off) != 40 {
		t.Errorf("409 body offset = %v, want 40 — without it the client has to guess", out["offset"])
	}
}

func TestContentDigestMismatchDoesNotAdvanceOffset(t *testing.T) {
	srv, _, _ := newTestServer(t)
	enableUploads(t, srv)
	sess := createSession(t, srv, []map[string]any{{"path": "x.flac", "size": 8}}, nil)
	sid, _ := sess["id"].(string)
	fid, _ := firstFile(t, sess)["id"].(string)

	wrong := sha256.Sum256([]byte("not what we sent"))
	code, _ := putChunk(t, srv.Handler(), sid, fid, 0, []byte("abcd"), wrong[:])
	if code != http.StatusBadRequest {
		t.Fatalf("digest mismatch = %d, want 400", code)
	}
	var st map[string]any
	doJSON(t, srv.Handler(), "GET", "/api/upload/sessions/"+sid, nil, &st)
	if off, _ := firstFile(t, st)["offset"].(float64); off != 0 {
		t.Fatalf("offset advanced to %v after a rejected chunk; the client cannot recover by re-sending", off)
	}
	right := sha256.Sum256([]byte("abcd"))
	if code, out := putChunk(t, srv.Handler(), sid, fid, 0, []byte("abcd"), right[:]); code != http.StatusOK {
		t.Fatalf("re-send after a rejected chunk = %d (%v)", code, out)
	}
}

func TestUploadDeclaredBytesOverSessionCapReturns413(t *testing.T) {
	srv, _, _ := newTestServer(t)
	enableUploads(t, srv)
	var out map[string]any
	code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
		"maxBytes": 1000,
		"files": []any{
			map[string]any{"path": "a.flac", "size": 600},
			map[string]any{"path": "b.flac", "size": 600},
		},
	}, &out)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap session = %d, want 413 (%v)", code, out)
	}
}

// TestUploadInsufficientSpaceReturns507WithReclaimable — the 507 carries what a
// purge could return, which is what lets the UI offer "empty trash and resume"
// instead of reporting a dead end.
func TestUploadInsufficientSpaceReturns507WithReclaimable(t *testing.T) {
	srv, _, _ := newTestServer(t)
	enableUploads(t, srv)
	srv.deps.Upload = upload.NewManager(upload.Config{}, srv.deps.Scanner.Roots,
		upload.WithFreeBytes(func(string) (int64, error) { return 1 << 20, nil }),
		upload.WithReclaimable(func(string) int64 { return 7 << 20 }),
	)
	var out map[string]any
	code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
		"files": []any{map[string]any{"path": "a.flac", "size": 1 << 20}},
	}, &out)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("full disk = %d, want 507 (%v)", code, out)
	}
	if got, _ := out["reclaimableBytes"].(float64); int64(got) != 7<<20 {
		t.Errorf("reclaimableBytes = %v, want %d", out["reclaimableBytes"], 7<<20)
	}
}

// TestSessionCreateFlagsExistingBasenameSizeMatches is the pre-flight for the
// overlapping-upload case: an album folder uploaded earlier, then the artist
// folder containing it. The paths differ so nothing collides, both copies land,
// and the shallower one wins the duplicate election — so warning up front is
// the only place it can be prevented.
func TestSessionCreateFlagsExistingBasenameSizeMatches(t *testing.T) {
	srv, _, _ := newTestServer(t)
	enableUploads(t, srv)

	body := []byte("existing audio")
	existing := "Dark Side/01 Speak to Me.flac"
	if err := srv.deps.Manifest.UpsertTrack(t.Context(), &manifest.Track{
		Path: existing, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}

	sess := createSession(t, srv, []map[string]any{
		{"path": "Pink Floyd/Dark Side/01 Speak to Me.flac", "size": len(body)},
		{"path": "Pink Floyd/Dark Side/02 Breathe.flac", "size": 999},
	}, nil)
	files, _ := sess["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	f0, _ := files[0].(map[string]any)
	if got, _ := f0["duplicateOf"].(string); got != existing {
		t.Errorf("duplicateOf = %q, want %q — the operator gets no warning about the overlap", got, existing)
	}
	f1, _ := files[1].(map[string]any)
	if got, _ := f1["duplicateOf"].(string); got != "" {
		t.Errorf("an unrelated file was flagged as a duplicate: %q", got)
	}
}

// TestPreflightHintDoesNotBlockTheUpload — the pre-flight WARNS, it does not
// decide. A track legitimately on both an album and a compilation is a real
// library, and that case belongs to serve-time duplicate suppression.
func TestPreflightHintDoesNotBlockTheUpload(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)
	body := []byte("dupe")
	if err := srv.deps.Manifest.UpsertTrack(t.Context(), &manifest.Track{
		Path: "Other/x.flac", Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	sess := createSession(t, srv, []map[string]any{{"path": "New/x.flac", "size": len(body)}}, nil)
	sid, _ := sess["id"].(string)
	f := firstFile(t, sess)
	if got, _ := f["duplicateOf"].(string); got == "" {
		t.Fatal("fixture wrong: the file was not flagged, so this test proves nothing")
	}
	fid, _ := f["id"].(string)
	if code, out := putChunk(t, srv.Handler(), sid, fid, 0, body, nil); code != http.StatusOK {
		t.Fatalf("a flagged file could not be uploaded: %d (%v)", code, out)
	}
	var commit map[string]any
	doJSON(t, srv.Handler(), "POST", "/api/upload/sessions/"+sid+"/commit", nil, &commit)
	if n, _ := commit["committed"].(float64); int(n) != 1 {
		t.Fatalf("a flagged file was not committed: %v", commit)
	}
	if _, err := os.Stat(filepath.Join(cfg.LibraryRoots[0], "New", "x.flac")); err != nil {
		t.Errorf("flagged file did not land: %v", err)
	}
}

// TestUploadRoutesAreNotInAuthBypassList — public installs gate every /api/*
// route behind an admin session. An upload route slipping into the bypass list
// would be an unauthenticated write endpoint on a public bridge.
func TestUploadRoutesAreNotInAuthBypassList(t *testing.T) {
	for _, p := range []string{
		"/api/upload/sessions",
		"/api/upload/sessions/abc",
		"/api/upload/sessions/abc/files/def",
		"/api/upload/sessions/abc/commit",
	} {
		if isAuthBypassPath(p) {
			t.Errorf("%s bypasses the admin session gate", p)
		}
	}
}

func TestParseContentDigestSHA256(t *testing.T) {
	sum := sha256.Sum256([]byte("hello"))
	b64 := base64.StdEncoding.EncodeToString(sum[:])

	got, err := parseContentDigestSHA256("sha-256=:" + b64 + ":")
	if err != nil || !bytes.Equal(got, sum[:]) {
		t.Errorf("well-formed digest: got %x err %v", got, err)
	}
	// Parameters on the member are ignored, per RFC 8941 §3.1.2.
	got, err = parseContentDigestSHA256("sha-256=:" + b64 + ":;q=1")
	if err != nil || !bytes.Equal(got, sum[:]) {
		t.Errorf("digest with a parameter: got %x err %v", got, err)
	}
	// Other algorithms are ignored rather than refused, per RFC 9530.
	got, err = parseContentDigestSHA256("sha-512=:" + b64 + ":")
	if err != nil || got != nil {
		t.Errorf("unknown algorithm should be ignored: got %x err %v", got, err)
	}
	if got, err := parseContentDigestSHA256(""); err != nil || got != nil {
		t.Errorf("absent header: got %x err %v", got, err)
	}
	for _, bad := range []string{
		"sha-256=" + b64,          // not a byte sequence
		"sha-256=:not-base64!!!:", // undecodable
		"sha-256=:" + base64.StdEncoding.EncodeToString([]byte("short")) + ":",
	} {
		if _, err := parseContentDigestSHA256(bad); err == nil {
			t.Errorf("malformed digest %q was accepted", bad)
		}
	}
}

// TestReadOnlyLibraryReturns503NotA500 — a deployment that cannot accept
// uploads as configured is not a server fault, and the operator needs the
// cause in the response rather than in a log they have no reason to open.
func TestReadOnlyLibraryReturns503NotA500(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.Chmod on Windows sets only the read-only ATTRIBUTE, and that does
		// not stop files being created inside a directory — so a 0o500 fixture
		// is simply writable there and MkdirAll succeeds. Reproducing this
		// would need an ACL edit via icacls, which this repo deliberately
		// avoids shelling out to. The CLASSIFICATION itself is covered on every
		// platform by TestClassifyStagingError*.
		t.Skip("Chmod cannot make a directory unwritable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this fixture relies on")
	}
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)
	root := cfg.LibraryRoots[0]
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	var out map[string]any
	code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
		"files": []any{map[string]any{"path": "A/x.flac", "size": 1}},
	}, &out)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%v)", code, out)
	}
	if got, _ := out["error"].(string); got != "library_read_only" {
		t.Errorf("error code = %q, want library_read_only", got)
	}
	if msg, _ := out["message"].(string); !strings.Contains(msg, "read-write") && !strings.Contains(msg, "cannot write") {
		t.Errorf("the message does not say what to do: %q", msg)
	}
}
