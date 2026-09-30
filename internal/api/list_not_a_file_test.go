//go:build unix

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// listWithin requests /v1/list?path=rel from srv as a paired device, within
// fsutiltest.ServeBound, playing the writer on fifos if the handler is still
// running then.
func listWithin(t *testing.T, srv *Server, tok, rel string, fifos ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/list?path="+rel, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	return fsutiltest.ServeWithin(t, srv.Handler(), req, fifos...)
}

// TestListingOfADirectoryReplacedByANamedPipeAnswersAtOnce: the listing opens
// the directory the resolver's stat called one, and a directory replaced by a
// named pipe between that stat and the open (renamed away, a pipe made at its
// path) is refused at once, as a directory that could not be opened.
//
// Until 2026-09-30 the listing opened it with os.Open, which waits for a
// writer on a named pipe with nothing that can cancel the wait, so the
// request stayed until something wrote to the pipe. The window is between two
// system calls, so the test replaces the directory through the listing's own
// opener (Server.openDir), just before the real open runs.
func TestListingOfADirectoryReplacedByANamedPipeAnswersAtOnce(t *testing.T) {
	srv, tok, root := fileFixtureServer(t)
	album := filepath.Join(root, "Artist", "Album")
	parked := filepath.Join(t.TempDir(), "Album")
	open := srv.openDir
	srv.openDir = func(name string) (*os.File, error) {
		if err := os.Rename(album, parked); err != nil {
			t.Errorf("move the album aside: %v", err)
		} else if err := syscall.Mkfifo(album, 0o644); err != nil {
			t.Errorf("mkfifo %s: %v", album, err)
		}
		return open(name)
	}

	rec := listWithin(t, srv, tok, "Artist/Album", album)
	requireRefusal(t, "a directory replaced by a named pipe", rec, http.MethodGet,
		http.StatusInternalServerError, "internal", "couldn't open this directory")
}

// TestListingListsWhatIsNotAFileAsAnEntry pins the listing's answer for a
// named pipe, a link to one, a link to a device and a socket in a directory:
// each is listed, as an entry that is not a directory, at once, and nothing is
// opened to list it. The listing names what is in a directory; a client that
// follows such an entry gets the byte routes' 400, which names the kind
// (TestByteRoutesRefuseWhatIsNotAFile). Leaving them out would change what
// the app sees for a folder: the phone's browser shows every listed entry,
// and the rows it syncs come from the manifest, which holds none of them
// (#1070), so nothing it keeps depends on this listing.
func TestListingListsWhatIsNotAFileAsAnEntry(t *testing.T) {
	srv, tok, root := fileFixtureServer(t)
	kinds, pipe := fsutiltest.PlantNotAFiles(t, filepath.Join(root, "Artist", "Album"))

	rec := listWithin(t, srv, tok, "Artist/Album", pipe)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var entries []Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	listed := map[string]Entry{}
	for _, e := range entries {
		listed[e.Name] = e
	}
	for name, kind := range kinds {
		e, ok := listed[name]
		switch {
		case !ok:
			t.Errorf("the %s %s is not listed", kind, name)
		case e.IsDir:
			t.Errorf("the %s %s is listed as a directory", kind, name)
		}
	}
	if _, ok := listed["01 Track.flac"]; !ok {
		t.Error("the track beside them is not listed")
	}
}
