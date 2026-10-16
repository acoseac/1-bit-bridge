package api

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// hostSideDir names a directory above the library root in these tests.
// Only an absolute host path holds it, and it holds no separator, so a body
// or a log line that names it has leaked a host path on every platform:
// JSON and the text log handler double a Windows path's backslashes, which
// a search for the whole path would miss.
const hostSideDir = "host-side"

// upscaleRouter is the API router over the one library root, with the
// upscale gate open and a stub enqueuer, after configure has set the
// Server's seams, and a token it accepts. A test serves it through a
// recorder, so the log it reads is settled when the request returns.
func upscaleRouter(t *testing.T, root string, configure func(*Server)) (http.Handler, string) {
	t.Helper()
	tokens, err := auth.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{root}, ListenAddress: ":7788", LibraryName: "Test"}
	srv := New(cfg, tokens, nil, "fp").
		WithUpscaleEnqueuer(newStubEnqueuer()).
		WithUpscale(func() bool { return true }, nil)
	if configure != nil {
		configure(srv)
	}
	return srv.Handler(), raw
}

// postUpscale sends POST /v1/upscale for folder through h and returns the
// recorded answer.
func postUpscale(t *testing.T, h http.Handler, token, folder string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(UpscaleRequest{Path: folder})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/upscale", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	return rr
}

// logAttr reports whether a text-handler log line carries key=value as a
// whole attribute.
func logAttr(line, key, value string) bool {
	return regexp.MustCompile(`(?:^| )` + regexp.QuoteMeta(key+"="+value) + `(?: |$)`).MatchString(line)
}

// assertFolderWalkFailure checks a folder walk's 500 and its log. The body
// is `internal` with the walk's redacted reason, wantMsg. One request-failed
// line at Error at 500 carries the error as wantErr and names folder, the
// path the client asked for. Neither the body nor the log names
// hostSideDir, so neither carries the host path.
func assertFolderWalkFailure(t *testing.T, rr *httptest.ResponseRecorder, logged, folder, wantMsg, wantErr string) {
	t.Helper()
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), hostSideDir) {
		t.Errorf("the body names the host path: %s", rr.Body.String())
	}
	if strings.Contains(logged, hostSideDir) {
		t.Errorf("the log names the host path:\n%s", logged)
	}
	failed := linesContaining(logged, `msg="request failed"`)
	if len(failed) != 1 {
		t.Errorf("want one request-failed line, got %d; log:\n%s", len(failed), logged)
	} else {
		line := failed[0]
		for _, want := range []string{"level=ERROR", "status=500", "code=internal", `err="` + wantErr + `"`} {
			if !strings.Contains(line, want) {
				t.Errorf("request-failed line lacks %s: %s", want, line)
			}
		}
		if !logAttr(line, "path", folder) {
			t.Errorf("request-failed line does not name the folder %q: %s", folder, line)
		}
	}
	er := decodeErrorResponse(t, rr.Body.Bytes())
	if er.Error != "internal" || er.Message != wantMsg {
		t.Errorf("body = {%q, %q}, want {internal, %q}", er.Error, er.Message, wantMsg)
	}
}

// TestAFolderWalkThatCannotStartLogsItsError is backlog B435's first site,
// the one that logged nothing. A folder request for a library root whose
// walk cannot start (fsutil.WalkableRoot fails, here with the error it gives
// a root the separator does not enter) answers 500 `internal` with the
// error's redacted reason, the wire text #185 chose and #1076 copied for the
// root's walk. The error goes to a request-failed line at Error that names
// the folder the client asked for, never the host path.
func TestAFolderWalkThatCannotStartLogsItsError(t *testing.T) {
	buf := withTestSlog(t)
	root := filepath.Join(t.TempDir(), hostSideDir, "Music")
	seedUpscaleTree(t, root)
	h, token := upscaleRouter(t, root, func(s *Server) {
		s.walkableRootForTest = func(abs string) (string, error) {
			return "", &fs.PathError{Op: "walk", Path: abs, Err: fsutil.ErrRootNotEntered}
		}
	})

	buf.Reset()
	rr := postUpscale(t, h, token, ".")
	reason := "walk: " + fsutil.ErrRootNotEntered.Error()
	assertFolderWalkFailure(t, rr, buf.String(), ".", "walk folder: "+reason, reason)
}

// TestAFolderWalkThatFailsPartWayLogsItsError is the second site: the walk
// starts and fails below the folder, at a directory it cannot read. The
// answer is the same 500 with the redacted reason, and the request-failed
// line is new. The walk's own line, which names the entry that failed, was
// already written and stays.
func TestAFolderWalkThatFailsPartWayLogsItsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory's mode does not stop Windows reading it")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	buf := withTestSlog(t)
	root := filepath.Join(t.TempDir(), hostSideDir, "Music")
	seedUpscaleTree(t, root)
	locked := filepath.Join(root, "Artist", "Locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	// Registered after TempDir, so the directory is readable again before
	// it is removed.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	h, token := upscaleRouter(t, root, nil)

	buf.Reset()
	rr := postUpscale(t, h, token, "Artist")
	logged := buf.String()
	reason := "open: " + syscall.EACCES.Error()
	assertFolderWalkFailure(t, rr, logged, "Artist", "walk folder: "+reason, reason)
	walk := linesContaining(logged, `msg="upscale folder walk"`)
	if len(walk) != 1 || !strings.Contains(walk[0], "level=ERROR") || !logAttr(walk[0], "path", "Artist/Locked") {
		t.Errorf("want the walk's own Error line naming Artist/Locked, got %q", walk)
	}
}
