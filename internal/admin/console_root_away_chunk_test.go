package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestAChunkIntoARootThatReadsAsNotMountedIsRefused: a session the console can
// still see is no proof its root reads as it did. A root that keeps its
// staging folder (a dot folder, which is no library content) while its
// library content goes and the index holds its rows reads as a volume not
// mounted, and so does an old session's staging left on a bare mount point;
// a chunk found its session there and was written. A chunk asks the root its
// staging is in first: refused, nothing written, the offset where it was; the
// same chunk once the content is back is taken.
func TestAChunkIntoARootThatReadsAsNotMountedIsRefused(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)
	h := srv.Handler()
	root := cfg.LibraryRoots[0]
	seedLibraryFile(t, root, "Artist/Album/01.flac", "not a real flac")
	indexLibrary(t, srv)

	body := []byte("fresh audio bytes for the volume")
	half := int64(len(body) / 2)
	sess := createSession(t, srv, []map[string]any{{"path": "New Album/01.flac", "size": len(body)}}, nil)
	sid, _ := sess["id"].(string)
	fid, _ := firstFile(t, sess)["id"].(string)
	if code, out := putChunk(t, h, sid, fid, 0, body[:half], nil); code != http.StatusOK {
		t.Fatalf("the first chunk = %d %v", code, out)
	}

	// The library content goes with its volume; the staging folder stays.
	aside := filepath.Join(t.TempDir(), "volume")
	if err := os.MkdirAll(aside, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range entriesOf(root) {
		if name == ".bridge-upload" {
			continue
		}
		if err := os.Rename(filepath.Join(root, name), filepath.Join(aside, name)); err != nil {
			t.Fatal(err)
		}
	}
	part := filepath.Join(root, ".bridge-upload", sid, fid+".part")
	if fi, err := os.Stat(part); err != nil || fi.Size() != half {
		t.Fatalf("premise: the staged file after the first chunk: %v %v", fi, err)
	}

	code, out := putChunk(t, h, sid, fid, half, body[half:], nil)
	mustRefuseAsNotMounted(t, code, out, root, notMountedWords)
	if fi, err := os.Stat(part); err != nil || fi.Size() != half {
		t.Errorf("the refused chunk was written: the staged file is %v (%v), want %d bytes", fi, err, half)
	}
	var view map[string]any
	if code := doJSON(t, h, "GET", "/api/upload/sessions/"+sid, nil, &view); code != http.StatusOK || firstFile(t, view)["offset"] != float64(half) {
		t.Errorf("the session after the refused chunk = %d %v, want its offset at %d", code, view, half)
	}

	for _, name := range entriesOf(aside) {
		if err := os.Rename(filepath.Join(aside, name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := putChunk(t, h, sid, fid, half, body[half:], nil); code != http.StatusOK || out["offset"] != float64(len(body)) {
		t.Fatalf("the same chunk once the content is back = %d %v, want 200 at offset %d", code, out, len(body))
	}
}
