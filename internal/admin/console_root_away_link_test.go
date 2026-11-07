package admin

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Backlog B776's guard and a link (its review's round 2). Which root a path is
// compares the path's elements with the configured roots' and resolves no
// link (manifest's rootIn, which a subtree scan's owning root and the
// watcher's events take too), so a link in a healthy root that leads to
// another root's folder took a write past that root's look: the write's
// MkdirAll and rename follow the link, and the upload's and the trash's own
// containment checks resolve it and take the destination as inside the root
// the request names. On Windows the link is a directory junction, which needs
// no privilege to make, and which the bridge resolves as it resolves a
// symbolic link (fsutil.ResolveLinks).

// linkDir makes link lead to the folder target: a directory junction on
// Windows (mklink /J), a symbolic link elsewhere. A host that cannot make a
// symbolic link skips the test.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		cmd, err := exec.LookPath("cmd")
		if err != nil {
			t.Skipf("no cmd to make a directory junction with: %v", err)
		}
		if out, err := exec.Command(cmd, "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this host cannot make a symbolic link: %v", err)
	}
}

// TestAWriteThroughALinkIntoAnAbsentNestedRootIsRefused: music/alias leads to
// the folder of music/usb, a root of its own whose volume is away. An upload
// named through the link, and a restore of an entry trashed through it, land
// in that bare mount point, so each is asked of the root it lands in and
// refused, nothing written there; the same requests once the volume is back
// write where the link leads.
func TestAWriteThroughALinkIntoAnAbsentNestedRootIsRefused(t *testing.T) {
	t.Run("an upload's create", func(t *testing.T) {
		srv, music, usb, vol := nestedConsole(t)
		linkDir(t, usb, filepath.Join(music, "alias"))
		if err := vol.away(); err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
			"root":  filepath.Base(music),
			"files": []map[string]any{{"path": "alias/Band/Record/03.flac", "size": 4}},
		}, &out)
		mustRefuseAsNotMounted(t, code, out, usb, notMountedWords)
		mustBeBare(t, usb)
	})

	t.Run("an upload's commit", func(t *testing.T) {
		srv, music, usb, vol := nestedConsole(t)
		linkDir(t, usb, filepath.Join(music, "alias"))
		h := srv.Handler()
		body := []byte("fresh audio bytes for the volume")
		sess := createSession(t, srv, []map[string]any{{"path": "alias/Band/Record/03.flac", "size": len(body)}},
			map[string]any{"root": filepath.Base(music)})
		sid, _ := sess["id"].(string)
		fid, _ := firstFile(t, sess)["id"].(string)
		if code, out := putChunk(t, h, sid, fid, 0, body, nil); code != http.StatusOK {
			t.Fatalf("chunk = %d %v", code, out)
		}
		if err := vol.away(); err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		code := doJSON(t, h, "POST", "/api/upload/sessions/"+sid+"/commit", nil, &out)
		mustRefuseAsNotMounted(t, code, out, usb, notMountedWords)
		mustBeBare(t, usb)
		if err := vol.back(); err != nil {
			t.Fatal(err)
		}
		var commit map[string]any
		if code := doJSON(t, h, "POST", "/api/upload/sessions/"+sid+"/commit", nil, &commit); code != http.StatusOK || commit["committed"] != float64(1) {
			t.Fatalf("the session's commit once its volume was back = %d %v, want 200 and 1 committed", code, commit)
		}
		srvBgScansWait(t, srv)
		if got, err := os.ReadFile(filepath.Join(usb, "Band", "Record", "03.flac")); err != nil || string(got) != string(body) {
			t.Errorf("the file committed through the link = %q, %v", got, err)
		}
	})

	t.Run("a restore", func(t *testing.T) {
		srv, music, usb, vol := nestedConsole(t)
		linkDir(t, usb, filepath.Join(music, "alias"))
		h := srv.Handler()
		var trashed map[string]any
		if code := doJSON(t, h, "POST", "/api/library/trash", map[string]any{
			"paths": []string{filepath.Base(music) + "/alias/Band/Record/02.flac"},
		}, &trashed); code != http.StatusOK || trashed["ok"] != float64(1) {
			t.Fatalf("trash through the link = %d %v", code, trashed)
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
			t.Errorf("the file restored through the link: %v", err)
		}
	})
}

// TestAWriteThroughALinkIsAskedOfTheRootItLandsIn: the root a write lands in is
// found by comparing where the write lands with each root as it resolves, and
// a root whose path is not there as it is configured, so a root configured
// through a link, and one whose folder went with its volume, are found too.
func TestAWriteThroughALinkIsAskedOfTheRootItLandsIn(t *testing.T) {
	t.Run("a nested root configured through a link", func(t *testing.T) {
		srv, cfg, _ := newTestServer(t)
		enableUploads(t, srv)
		music := cfg.LibraryRoots[0]
		volDir := filepath.Join(t.TempDir(), "usbvol")
		seedLibraryFile(t, music, "Artist/Album/01.flac", "not a real flac")
		seedLibraryFile(t, volDir, "Band/Record/01.flac", "not a real flac")
		usb := filepath.Join(music, "usb")
		linkDir(t, volDir, usb)
		linkDir(t, volDir, filepath.Join(music, "alias"))
		srv.deps.Scanner.SetRoots([]string{music, usb})
		srv.deps.Resolver.SetRoots([]string{music, usb})
		indexLibrary(t, srv)
		if err := newAwayVolume(t, volDir).away(); err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
			"root":  filepath.Base(music),
			"files": []map[string]any{{"path": "alias/Band/Record/03.flac", "size": 4}},
		}, &out)
		mustRefuseAsNotMounted(t, code, out, usb, notMountedWords)
		mustBeBare(t, volDir)
	})

	t.Run("a root whose path is gone, through a link to its parent", func(t *testing.T) {
		srv, cfg, _ := newTestServer(t)
		enableUploads(t, srv)
		music := cfg.LibraryRoots[0]
		mnt := filepath.Join(t.TempDir(), "mnt")
		usb := filepath.Join(mnt, "usb")
		seedLibraryFile(t, music, "Artist/Album/01.flac", "not a real flac")
		seedLibraryFile(t, usb, "Band/Record/01.flac", "not a real flac")
		linkDir(t, mnt, filepath.Join(music, "alias"))
		srv.deps.Scanner.SetRoots([]string{music, usb})
		srv.deps.Resolver.SetRoots([]string{music, usb})
		indexLibrary(t, srv)
		if err := os.Rename(usb, filepath.Join(t.TempDir(), "volume")); err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions", map[string]any{
			"root":  filepath.Base(music),
			"files": []map[string]any{{"path": "alias/usb/Band/Record/03.flac", "size": 4}},
		}, &out)
		mustRefuseAsNotMounted(t, code, out, usb, notMountedWords)
		if _, err := os.Lstat(usb); !os.IsNotExist(err) {
			t.Errorf("the refused upload made the folder of a root whose volume is away: %v", err)
		}
	})
}
