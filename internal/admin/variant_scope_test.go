package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedSharedDirLibrary stages two albums whose tracks sit flat in ONE
// directory — the shape that makes a folder scope unusable for an
// album. It mirrors `Peter Gabriel/Hi-Res Masters/`, where the
// reference library keeps 18 albums side by side.
func seedSharedDirLibrary(t *testing.T, st *manifest.Store) {
	t.Helper()
	rate, bits := 44100.0, 16
	mk := func(path, title, album string) *manifest.Track {
		return &manifest.Track{
			Path: path, Title: title, Album: album,
			AlbumArtist: "Peter Gabriel", Artist: "Peter Gabriel",
			Codec: "FLAC", Size: 1000, ModTime: time.Unix(1, 0),
			SampleRate: &rate, BitsPerSample: &bits,
		}
	}
	for _, tr := range []*manifest.Track{
		mk("Music/Peter Gabriel/Hi-Res Masters/So - 01.flac", "Red Rain", "So"),
		mk("Music/Peter Gabriel/Hi-Res Masters/So - 02.flac", "Sledgehammer", "So"),
		mk("Music/Peter Gabriel/Hi-Res Masters/Us - 01.flac", "Come Talk To Me", "Us"),
	} {
		if err := st.UpsertTrack(t.Context(), tr); err != nil {
			t.Fatal(err)
		}
	}
}

// albumIDByTitle resolves a catalog album id the way a browser would —
// through the same /api/player/albums response the client reads.
func albumIDByTitle(t *testing.T, srv *Server, title string) string {
	t.Helper()
	_, body := playerGet(t, srv, "/api/player/albums?limit=200")
	albums, _ := body["albums"].([]any)
	for _, raw := range albums {
		a, _ := raw.(map[string]any)
		if a["title"] == title {
			id, _ := a["id"].(string)
			return id
		}
	}
	t.Fatalf("album %q not in catalog: %v", title, albums)
	return ""
}

func artistIDByName(t *testing.T, srv *Server, name string) string {
	t.Helper()
	_, body := playerGet(t, srv, "/api/player/artists?limit=200")
	artists, _ := body["artists"].([]any)
	for _, raw := range artists {
		a, _ := raw.(map[string]any)
		if a["name"] == name {
			id, _ := a["id"].(string)
			return id
		}
	}
	t.Fatalf("artist %q not in catalog: %v", name, artists)
	return ""
}

func postBatch(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/upscale/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestBatchSubmitByAlbumIDExcludesDirectoryNeighbours is the end-to-end
// pin for the whole identity-scope feature: an album submitted by id
// must enqueue that album's tracks and NOTHING else, even though a
// neighbouring album shares its directory.
//
// The folder form is exercised alongside it to show what it would have
// done — not as a bug report against the prefix query, which does
// exactly what it promises, but so a reader can see why the two forms
// both have to exist.
func TestBatchSubmitByAlbumIDExcludesDirectoryNeighbours(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub

	soID := albumIDByTitle(t, srv, "So")

	w := postBatch(t, srv, `{"albumIds":["`+soID+`"],"kind":"optimize"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	if len(stub.submitOptimizeCalls) != 1 {
		t.Fatalf("SubmitOptimizePaths calls = %d, want 1", len(stub.submitOptimizeCalls))
	}
	got := sortedCopy(stub.submitOptimizeCalls[0].paths)
	want := []string{
		"Music/Peter Gabriel/Hi-Res Masters/So - 01.flac",
		"Music/Peter Gabriel/Hi-Res Masters/So - 02.flac",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("album scope enqueued %v, want exactly %v", got, want)
	}
	for _, p := range got {
		if strings.Contains(p, "Us - ") {
			t.Errorf("neighbouring album %q swept into an album-scoped submit", p)
		}
	}

	// The label is display only, but it should still be a value an
	// operator recognises on the Jobs page.
	if label := stub.submitOptimizeCalls[0].path; label != "Music/Peter Gabriel/Hi-Res Masters" {
		t.Errorf("batch label = %q, want the album's folder path", label)
	}
}

// TestBatchSubmitByArtistIDUnionsItsAlbums: one short id stands in for
// every track the artist has, which is the whole reason ids travel
// instead of paths.
func TestBatchSubmitByArtistIDUnionsItsAlbums(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub

	id := artistIDByName(t, srv, "Peter Gabriel")
	w := postBatch(t, srv, `{"artistId":"`+id+`","targetRate":176400,"targetBits":24}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	if len(stub.submitCalls) != 1 {
		t.Fatalf("SubmitPaths calls = %d, want 1", len(stub.submitCalls))
	}
	if n := len(stub.submitCalls[0].paths); n != 3 {
		t.Errorf("artist scope enqueued %d paths, want 3: %v", n, stub.submitCalls[0].paths)
	}
	if stub.submitCalls[0].rate != 176400 || stub.submitCalls[0].bits != 24 {
		t.Errorf("target = %d/%d, want 176400/24",
			stub.submitCalls[0].rate, stub.submitCalls[0].bits)
	}
}

// TestBatchSubmitFolderFormUnchanged is the back-compat pin: a body
// with no identity field is still the folder form, and still reaches
// the prefix-scoped coordinator method.
func TestBatchSubmitFolderFormUnchanged(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub

	w := postBatch(t, srv, `{"path":"Music/Album","kind":"optimize"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	if len(stub.submitOptimizeCalls) != 1 || stub.submitOptimizeCalls[0].path != "Music/Album" {
		t.Fatalf("folder form did not reach SubmitOptimize: %+v", stub.submitOptimizeCalls)
	}
	if stub.submitOptimizeCalls[0].paths != nil {
		t.Errorf("folder form carried a path set: %v", stub.submitOptimizeCalls[0].paths)
	}

	// The whole library is an OMITTED path, and it must stay reachable:
	// it is how the Folders view's root scope and the CLI-equivalent
	// "everything" batch are expressed. This is the counterpart to the
	// empty-scope rejection — `{}` means everything, `{"albumIds":[]}`
	// means a client bug.
	stub.submitCalls = nil
	w = postBatch(t, srv, `{"targetRate":192000,"targetBits":24}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("whole-library form: status %d, body %s", w.Code, w.Body.String())
	}
	if len(stub.submitCalls) != 1 || stub.submitCalls[0].path != "" {
		t.Fatalf("whole-library form did not reach Submit(\"\"): %+v", stub.submitCalls)
	}
}

// TestBatchSubmitScopeRejections covers every way a scope can be
// malformed. Each must be refused BEFORE the coordinator is reached —
// a request that half-means two things must never be guessed at, since
// both guesses spend disk and one of them is destructive in the delete
// twin.
func TestBatchSubmitScopeRejections(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedSharedDirLibrary(t, srv.deps.Manifest)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub

	soID := albumIDByTitle(t, srv, "So")
	manyAlbums := `{"albumIds":[` + strings.TrimSuffix(strings.Repeat(`"`+soID+`",`, maxScopeAlbumIDs+1), ",") + `]}`
	manyTracks := `{"trackPaths":[` + strings.TrimSuffix(strings.Repeat(`"a/b.flac",`, maxScopeTrackPaths+1), ",") + `]}`

	for _, tc := range []struct {
		name string
		body string
		want int
		code string
	}{
		{"album plus artist", `{"albumIds":["` + soID + `"],"artistId":"` + soID + `"}`, 400, "ambiguous-scope"},
		{"album plus path", `{"albumIds":["` + soID + `"],"path":"Music"}`, 400, "ambiguous-scope"},
		{"tracks plus path", `{"trackPaths":["a/b.flac"],"path":"Music"}`, 400, "ambiguous-scope"},
		{"malformed album id", `{"albumIds":["nope"]}`, 400, "bad-request"},
		{"unknown album id", `{"albumIds":["0123456789abcdef"]}`, 404, "not_found"},
		{"malformed artist id", `{"artistId":"nope"}`, 400, "bad-request"},
		{"traversal in trackPaths", `{"trackPaths":["../etc/passwd"]}`, 400, "bad-path"},
		{"empty trackPath", `{"trackPaths":[""]}`, 400, "bad-path"},
		// An empty selection must never widen to the whole library.
		// The folder form's own spelling for "everything" is an
		// omitted path, so a present-but-empty identity field landing
		// there would upscale the entire library from a client bug
		// that serialised a selection of nothing.
		{"empty albumIds", `{"albumIds":[]}`, 400, "empty-scope"},
		{"empty trackPaths", `{"trackPaths":[]}`, 400, "empty-scope"},
		{"empty albumIds with a target", `{"albumIds":[],"targetRate":192000,"targetBits":24}`, 400, "empty-scope"},
		{"too many albums", manyAlbums, 400, "too-many-albums"},
		{"too many tracks", manyTracks, 400, "too-many-tracks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(stub.submitCalls) + len(stub.submitOptimizeCalls)
			w := postBatch(t, srv, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if got, _ := body["error"].(string); got != tc.code {
				t.Errorf("error code %q, want %q (body %s)", got, tc.code, w.Body.String())
			}
			if after := len(stub.submitCalls) + len(stub.submitOptimizeCalls); after != before {
				t.Errorf("a rejected scope still reached the coordinator")
			}
		})
	}
}

// TestBatchSubmitTrackPathsNormalises: the one identity form whose
// members are client-supplied rather than catalog-derived still goes
// through normaliseBrowsePath, and duplicates collapse.
func TestBatchSubmitTrackPathsNormalises(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub

	w := postBatch(t, srv, `{"trackPaths":["/Music/A.flac","Music/A.flac","//Music/B.flac"]}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	got := stub.submitCalls[0].paths
	want := []string{"Music/A.flac", "Music/B.flac"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("normalised paths = %v, want %v", got, want)
	}
}
