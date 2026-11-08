package admin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The console's writes into a library root that reads as a volume not mounted
// (backlog B776). Where the bare mount point a volume leaves is the service
// user's to write, an upload or a restore while the volume is away leaves
// library content there, which the scans then read as the root's, reaping the
// volume's rows at the threshold, and which the volume hides once it is
// mounted again. Every request here goes through the console's real handlers,
// over the upload and trash managers wired as serve wires them (the scanner's
// root guard, newTestServer and wireTrash).
//
// What these tests rest on: lookAtRoot, the look the watcher holds a root by,
// reads a root whose volume is away as a folder holding no library content
// under rows the index holds, or as a path that is not there, and the guard
// asks it nothing else. awayVolume leaves exactly that, so no mount is needed
// for what B776 decides; the watcher's tests of what a mount does to a watch
// mount a disk image (build-ci-tests.md).

// awayVolume stands for a volume mounted at a library root's folder: while it
// is there, everything it holds is in that folder. Taken away, every entry is
// moved to a folder beside it and the folder is left empty and writable, as
// an unmount leaves a mount point the service user owns; put back, the entries
// are moved in again, which back refuses while the bare mount point holds
// anything a write left there.
type awayVolume struct {
	root, aside string
}

func newAwayVolume(t *testing.T, root string) *awayVolume {
	t.Helper()
	return &awayVolume{root: root, aside: filepath.Join(t.TempDir(), "volume")}
}

// away moves the volume's entries aside, leaving the bare mount point.
func (v *awayVolume) away() error {
	if err := os.MkdirAll(v.aside, 0o755); err != nil {
		return err
	}
	return moveEntries(v.root, v.aside)
}

// back moves the volume's entries in again, once the bare mount point is
// found to hold nothing.
func (v *awayVolume) back() error {
	if left := entriesOf(v.root); len(left) > 0 {
		return fmt.Errorf("the bare mount point %s holds %q, which the volume's mount would hide", v.root, left)
	}
	return moveEntries(v.aside, v.root)
}

func moveEntries(from, to string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// entriesOf is the names dir lists, sorted; nil where it lists nothing or is
// not there.
func entriesOf(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// mustBeBare fails the test unless the bare mount point holds nothing: a
// refused write left nothing there.
func mustBeBare(t *testing.T, dir string) {
	t.Helper()
	if left := entriesOf(dir); len(left) > 0 {
		t.Errorf("the bare mount point %s holds %q after a refused write", dir, left)
	}
}

// indexLibrary runs a full scan, so the index holds the rows of what the roots
// hold now.
func indexLibrary(t *testing.T, srv *Server) {
	t.Helper()
	if _, err := srv.deps.Scanner.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

// mustRefuseAsNotMounted fails the test unless the answer is the refusal of a
// write into the root named name: 409 root_not_mounted, in the hold's words
// (words), naming the root by its folder's name and no path.
func mustRefuseAsNotMounted(t *testing.T, code int, out map[string]any, root, words string) {
	t.Helper()
	if code != http.StatusConflict || out["error"] != "root_not_mounted" {
		t.Fatalf("answer = %d %v, want 409 root_not_mounted", code, out)
	}
	msg, _ := out["message"].(string)
	if !strings.Contains(msg, words) {
		t.Errorf("the refusal says %q, want the hold's words %q", msg, words)
	}
	if !strings.Contains(msg, fmt.Sprintf("%q", filepath.Base(root))) {
		t.Errorf("the refusal %q does not name the root %q by its folder's name", msg, filepath.Base(root))
	}
	if strings.Contains(msg, filepath.Dir(root)) {
		t.Errorf("the refusal %q names a path: a console error body names none", msg)
	}
}

const (
	notMountedWords = "reads as a volume that is not mounted, so nothing was written to it: check that its volume is mounted, then try again"
	dirLinksWords   = "holds no content but links to directories, which the scanner does not follow, so nothing was written to it"
)

// TestAnUploadIntoARootThatReadsAsNotMountedIsRefusedAndWritesNothing: an
// upload's staging lives inside its root, so a session created while the
// root's volume is away lands on the bare mount point (backlog B776 item 1),
// and the root's folder is made where it is not there at all. Each shape the
// hold reads as absent is refused before anything is written, in the hold's
// words: the bare mount point, holding nothing under the rows the index
// holds; the root's path gone with its volume; and a root holding nothing but
// a link to a directory, whose rows the scan keeps and a file written beside
// the link would let it reap.
func TestAnUploadIntoARootThatReadsAsNotMountedIsRefusedAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		away  func(t *testing.T, root string)
		words string
	}{
		{"the bare mount point, its volume away", func(t *testing.T, root string) {
			if err := newAwayVolume(t, root).away(); err != nil {
				t.Fatal(err)
			}
		}, notMountedWords},
		{"the root's path gone with its volume", func(t *testing.T, root string) {
			if err := os.Rename(root, filepath.Join(t.TempDir(), "volume")); err != nil {
				t.Fatal(err)
			}
		}, notMountedWords},
		{"a root holding only a link to a directory", func(t *testing.T, root string) {
			v := newAwayVolume(t, root)
			if err := v.away(); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(v.aside, filepath.Join(root, "Linked")); err != nil {
				t.Skipf("this host cannot make a symbolic link: %v", err)
			}
		}, dirLinksWords},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cfg, _ := newTestServer(t)
			enableUploads(t, srv)
			root := cfg.LibraryRoots[0]
			seedLibraryFile(t, root, "Artist/Album/01.flac", "not a real flac")
			indexLibrary(t, srv)
			tc.away(t, root)
			before := entriesOf(root)
			_, statErr := os.Lstat(root)
			wasThere := statErr == nil

			var out map[string]any
			code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
				"files": []map[string]any{{"path": "New Album/01.flac", "size": 4}},
			}, &out)
			mustRefuseAsNotMounted(t, code, out, root, tc.words)
			if after := entriesOf(root); !slices.Equal(after, before) {
				t.Errorf("the refused upload wrote into the root: it lists %q, it listed %q", after, before)
			}
			if _, err := os.Lstat(root); !wasThere && !os.IsNotExist(err) {
				t.Errorf("the refused upload made the root's folder where its volume is away: %v", err)
			}
		})
	}
}

// TestAnUploadSessionOnAVolumeThatIsAwayAnswersTheRefusalNotNotFound: a
// session staged on the volume is not found while the volume is away, and each
// route that looks for it answered 404, which the console reads as an upload
// lost (backlog B776 item 2). It answers the refusal: the session is on the
// volume, nothing is written meanwhile, and the commit of the same session
// once the volume is back lands its file.
func TestAnUploadSessionOnAVolumeThatIsAwayAnswersTheRefusalNotNotFound(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)
	h := srv.Handler()
	root := cfg.LibraryRoots[0]
	seedLibraryFile(t, root, "Artist/Album/01.flac", "not a real flac")
	indexLibrary(t, srv)
	vol := newAwayVolume(t, root)

	body := []byte("fresh audio bytes for the volume")
	sess := createSession(t, srv, []map[string]any{{"path": "New Album/01.flac", "size": len(body)}}, nil)
	sid, _ := sess["id"].(string)
	fid, _ := firstFile(t, sess)["id"].(string)
	if code, out := putChunk(t, h, sid, fid, 0, body, nil); code != http.StatusOK {
		t.Fatalf("chunk = %d %v", code, out)
	}
	if err := vol.away(); err != nil {
		t.Fatal(err)
	}

	for _, req := range []struct{ method, path string }{
		{"POST", "/api/upload/sessions/" + sid + "/commit"},
		{"GET", "/api/upload/sessions/" + sid},
		{"DELETE", "/api/upload/sessions/" + sid},
	} {
		var out map[string]any
		code := doJSON(t, h, req.method, req.path, nil, &out)
		t.Run(req.method+" "+req.path, func(t *testing.T) {
			mustRefuseAsNotMounted(t, code, out, root, notMountedWords)
		})
	}
	code, out := putChunk(t, h, sid, fid, int64(len(body)), []byte{}, nil)
	t.Run("PUT a chunk", func(t *testing.T) {
		mustRefuseAsNotMounted(t, code, out, root, notMountedWords)
	})
	mustBeBare(t, root)

	if err := vol.back(); err != nil {
		t.Fatal(err)
	}
	var commit map[string]any
	if code := doJSON(t, h, "POST", "/api/upload/sessions/"+sid+"/commit", nil, &commit); code != http.StatusOK || commit["committed"] != float64(1) {
		t.Fatalf("the session's commit once its volume was back = %d %v, want 200 and 1 committed", code, commit)
	}
	srvBgScansWait(t, srv)
	if got, err := os.ReadFile(filepath.Join(root, "New Album", "01.flac")); err != nil || string(got) != string(body) {
		t.Errorf("the committed file = %q, %v", got, err)
	}
}

// nestedConsole is a console over two roots, music and music/usb, the second a
// volume of its own mounted at its folder (vol), with uploads and deletes on
// and both indexed: an Artist album in music and a Band record on the volume.
func nestedConsole(t *testing.T) (srv *Server, music, usb string, vol *awayVolume) {
	t.Helper()
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)
	wireTrash(t, srv)
	enableDelete(t, srv, true)
	music = cfg.LibraryRoots[0]
	usb = filepath.Join(music, "usb")
	seedLibraryFile(t, music, "Artist/Album/01.flac", "not a real flac")
	seedLibraryFile(t, usb, "Band/Record/01.flac", "not a real flac")
	seedLibraryFile(t, usb, "Band/Record/02.flac", "not a real flac either")
	srv.deps.Scanner.SetRoots([]string{music, usb})
	srv.deps.Resolver.SetRoots([]string{music, usb})
	indexLibrary(t, srv)
	return srv, music, usb, newAwayVolume(t, usb)
}

// TestAWriteThroughAHealthyOuterRootIntoAnAbsentNestedOneIsRefused: an
// upload's paths are joined onto the root its session names, and a restore's
// onto the root its entry was trashed under, so each can reach a root nested
// in that one, whose folder is its volume's bare mount point while the volume
// is away (backlog B776 item 3). What is asked is the deepest configured root
// owning each destination: here the nested one, whatever the outer root reads.
// A session's commit is refused whole (item 4): the file bound for the outer
// root, which is there, is not committed either.
func TestAWriteThroughAHealthyOuterRootIntoAnAbsentNestedOneIsRefused(t *testing.T) {
	t.Run("an upload's create", func(t *testing.T) {
		srv, music, usb, vol := nestedConsole(t)
		if err := vol.away(); err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
			"root":  filepath.Base(music),
			"files": []map[string]any{{"path": "usb/Band/Record/03.flac", "size": 4}},
		}, &out)
		mustRefuseAsNotMounted(t, code, out, usb, notMountedWords)
		mustBeBare(t, usb)
		if slices.Contains(entriesOf(music), ".bridge-upload") {
			t.Error("the refused upload staged a session in the outer root")
		}
	})

	t.Run("an upload's commit, the whole session", func(t *testing.T) {
		srv, music, usb, vol := nestedConsole(t)
		h := srv.Handler()
		body := []byte("fresh audio bytes for the volume")
		sess := createSession(t, srv, []map[string]any{
			{"path": "Artist/Album/03.flac", "size": len(body)},
			{"path": "usb/Band/Record/03.flac", "size": len(body)},
		}, map[string]any{"root": filepath.Base(music)})
		sid, _ := sess["id"].(string)
		files, _ := sess["files"].([]any)
		for _, f := range files {
			fid, _ := f.(map[string]any)["id"].(string)
			if code, out := putChunk(t, h, sid, fid, 0, body, nil); code != http.StatusOK {
				t.Fatalf("chunk = %d %v", code, out)
			}
		}
		if err := vol.away(); err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		code := doJSON(t, h, "POST", "/api/upload/sessions/"+sid+"/commit", nil, &out)
		mustRefuseAsNotMounted(t, code, out, usb, notMountedWords)
		mustBeBare(t, usb)
		if _, err := os.Stat(filepath.Join(music, "Artist", "Album", "03.flac")); !os.IsNotExist(err) {
			t.Errorf("a refused commit committed the file bound for the outer root: %v", err)
		}
		if err := vol.back(); err != nil {
			t.Fatal(err)
		}
		var commit map[string]any
		if code := doJSON(t, h, "POST", "/api/upload/sessions/"+sid+"/commit", nil, &commit); code != http.StatusOK || commit["committed"] != float64(2) {
			t.Fatalf("the session's commit once its volume was back = %d %v, want 200 and 2 committed", code, commit)
		}
		srvBgScansWait(t, srv)
		for _, p := range []string{filepath.Join(music, "Artist", "Album", "03.flac"), filepath.Join(usb, "Band", "Record", "03.flac")} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("the committed file %s: %v", p, err)
			}
		}
	})

	t.Run("a restore", func(t *testing.T) {
		srv, music, usb, vol := nestedConsole(t)
		h := srv.Handler()
		var trashed map[string]any
		if code := doJSON(t, h, "POST", "/api/library/trash", map[string]any{
			"paths": []string{filepath.Base(music) + "/usb/Band/Record/02.flac"},
		}, &trashed); code != http.StatusOK || trashed["ok"] != float64(1) {
			t.Fatalf("trash = %d %v", code, trashed)
		}
		srvBgScansWait(t, srv)
		var listed []map[string]any
		doJSON(t, h, "GET", "/api/library/trash", nil, &listed)
		if len(listed) != 1 {
			t.Fatalf("the trash lists %v", listed)
		}
		id, _ := listed[0]["id"].(string)
		if err := vol.away(); err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		code := doJSON(t, h, "POST", "/api/library/trash/restore", map[string]any{"ids": []string{id}}, &out)
		mustRefuseAsNotMounted(t, code, out, usb, notMountedWords)
		mustBeBare(t, usb)
		if err := vol.back(); err != nil {
			t.Fatal(err)
		}
		var restored map[string]any
		if code := doJSON(t, h, "POST", "/api/library/trash/restore", map[string]any{"ids": []string{id}}, &restored); code != http.StatusOK || restored["ok"] != float64(1) {
			t.Fatalf("the restore once its volume was back = %d %v, want 200 and 1 restored", code, restored)
		}
		srvBgScansWait(t, srv)
		if _, err := os.Stat(filepath.Join(usb, "Band", "Record", "02.flac")); err != nil {
			t.Errorf("the restored file: %v", err)
		}
	})
}

// TestARestoreIsRefusedWholeBeforeItRenamesAnything: a restore runs its
// entries in turn, across roots, and one refused after another had been
// renamed back would answer an error the handler reports in place of that
// success and its scan (backlog B776 item 4). The whole request is looked at
// first: a healthy root's entry, followed by one whose root's volume is away
// (its trash with it, so the entry is in no trash the bridge can see), is a
// refusal for both, and nothing is renamed; the same request once the volume
// is back restores both.
func TestARestoreIsRefusedWholeBeforeItRenamesAnything(t *testing.T) {
	srv, music, h := openTrashConsole(t)
	nas := filepath.Join(t.TempDir(), "NAS")
	if err := os.MkdirAll(nas, 0o755); err != nil {
		t.Fatal(err)
	}
	srv.deps.Scanner.SetRoots([]string{music, nas})
	srv.deps.Resolver.SetRoots([]string{music, nas})
	seedLibraryFile(t, music, "Artist/Album/01.flac", "not a real flac")
	seedLibraryFile(t, nas, "Band/Record/01.flac", "not a real flac")
	seedLibraryFile(t, nas, "Band/Record/02.flac", "not a real flac either")
	indexLibrary(t, srv)

	healthy, away := filepath.Base(music)+"/Artist/Album/01.flac", "NAS/Band/Record/01.flac"
	var trashed map[string]any
	if code := doJSON(t, h, "POST", "/api/library/trash", map[string]any{"paths": []string{healthy, away}}, &trashed); code != http.StatusOK || trashed["ok"] != float64(2) {
		t.Fatalf("trash = %d %v", code, trashed)
	}
	srvBgScansWait(t, srv)
	var listed []map[string]any
	doJSON(t, h, "GET", "/api/library/trash", nil, &listed)
	ids := map[string]string{}
	for _, e := range listed {
		path, _ := e["originalPath"].(string)
		ids[path], _ = e["id"].(string)
	}
	if ids[healthy] == "" || ids[away] == "" {
		t.Fatalf("the trash lists %v", listed)
	}
	vol := newAwayVolume(t, nas)
	if err := vol.away(); err != nil {
		t.Fatal(err)
	}

	request := map[string]any{"ids": []string{ids[healthy], ids[away]}}
	var out map[string]any
	code := doJSON(t, h, "POST", "/api/library/trash/restore", request, &out)
	mustRefuseAsNotMounted(t, code, out, nas, notMountedWords)
	if _, err := os.Stat(filepath.Join(music, "Artist", "Album", "01.flac")); !os.IsNotExist(err) {
		t.Errorf("a refused restore renamed the healthy root's entry back: %v", err)
	}
	mustBeBare(t, nas)

	if err := vol.back(); err != nil {
		t.Fatal(err)
	}
	var restored map[string]any
	if code := doJSON(t, h, "POST", "/api/library/trash/restore", request, &restored); code != http.StatusOK || restored["ok"] != float64(2) {
		t.Fatalf("the restore once the volume was back = %d %v, want 200 and 2 restored", code, restored)
	}
	srvBgScansWait(t, srv)
	for _, p := range []string{filepath.Join(music, "Artist", "Album", "01.flac"), filepath.Join(nas, "Band", "Record", "01.flac")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the restored file %s: %v", p, err)
		}
	}
}

// TestTheFirstUploadAndRestoreIntoAFreshEmptyRootAreNotRefused: a fresh root
// on a healthy disk, an empty folder on the same device as its parent, which
// the index holds nothing of, reads as it is and is never refused (backlog
// B776 item 6): a guard that read a root holding no content as absent without
// asking the index would refuse every first upload, and pass every test of a
// volume away. A file uploaded and trashed leaves the root holding no content
// and the index no row, and its restore is not refused either.
func TestTheFirstUploadAndRestoreIntoAFreshEmptyRootAreNotRefused(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)
	wireTrash(t, srv)
	enableDelete(t, srv, true)
	h := srv.Handler()
	root := cfg.LibraryRoots[0]
	if left := entriesOf(root); len(left) > 0 {
		t.Fatalf("premise: the fresh root holds %q", left)
	}

	body := []byte("first audio")
	sess := createSession(t, srv, []map[string]any{{"path": "01.flac", "size": len(body)}}, nil)
	sid, _ := sess["id"].(string)
	fid, _ := firstFile(t, sess)["id"].(string)
	if code, out := putChunk(t, h, sid, fid, 0, body, nil); code != http.StatusOK {
		t.Fatalf("chunk = %d %v", code, out)
	}
	var commit map[string]any
	if code := doJSON(t, h, "POST", "/api/upload/sessions/"+sid+"/commit", nil, &commit); code != http.StatusOK || commit["committed"] != float64(1) {
		t.Fatalf("the first commit into a fresh root = %d %v", code, commit)
	}
	srvBgScansWait(t, srv)

	var trashed map[string]any
	if code := doJSON(t, h, "POST", "/api/library/trash", map[string]any{"paths": []string{"01.flac"}}, &trashed); code != http.StatusOK || trashed["ok"] != float64(1) {
		t.Fatalf("trash = %d %v", code, trashed)
	}
	srvBgScansWait(t, srv)
	for _, name := range entriesOf(root) {
		if !strings.HasPrefix(name, ".") {
			t.Fatalf("premise: the root holds %q beside its dot folders", name)
		}
	}
	if got, err := srv.deps.Manifest.GetTrack(context.Background(), "01.flac"); err != nil || got != nil {
		t.Fatalf("premise: the index still holds the trashed row: %v %v", got, err)
	}
	var listed []map[string]any
	doJSON(t, h, "GET", "/api/library/trash", nil, &listed)
	if len(listed) != 1 {
		t.Fatalf("the trash lists %v", listed)
	}
	var restored map[string]any
	if code := doJSON(t, h, "POST", "/api/library/trash/restore", map[string]any{"ids": []string{listed[0]["id"].(string)}}, &restored); code != http.StatusOK || restored["ok"] != float64(1) {
		t.Fatalf("the restore into a root holding no content and no row = %d %v", code, restored)
	}
	srvBgScansWait(t, srv)
	if got, err := os.ReadFile(filepath.Join(root, "01.flac")); err != nil || string(got) != string(body) {
		t.Errorf("the restored file = %q, %v", got, err)
	}
}
