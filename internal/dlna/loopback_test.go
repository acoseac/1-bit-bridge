package dlna

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// loopbackLib is the track lookup the loopback route reads: a track id to
// its file. The production adapter is the DLNA listener's manifest adapter.
type loopbackLib map[string]TrackInfo

func (l loopbackLib) GetTrackInfo(id string) (TrackInfo, bool) {
	ti, ok := l[id]
	return ti, ok
}

// loopbackFixture is one library root with a track inside it, a directory
// beside the root holding a file the route must never serve, and the
// handler under test.
type loopbackFixture struct {
	root    string
	outside string
	track   []byte
	secret  []byte
	lib     loopbackLib
	h       http.Handler
}

func newLoopbackFixture(t *testing.T) *loopbackFixture {
	t.Helper()
	base := t.TempDir()
	f := &loopbackFixture{
		root:    filepath.Join(base, "Music"),
		outside: filepath.Join(base, "outside"),
		track:   randomBytes(t, 64<<10),
		secret:  []byte("SECRET-OUTSIDE-THE-LIBRARY"),
		lib:     loopbackLib{},
	}
	mustMkdir(t, filepath.Join(f.root, "Artist", "Album"))
	mustMkdir(t, f.outside)
	mustWrite(t, filepath.Join(f.root, "Artist", "Album", "01.flac"), f.track)
	mustWrite(t, filepath.Join(f.outside, "secret.flac"), f.secret)
	f.add("inside", filepath.Join(f.root, "Artist", "Album", "01.flac"), ".flac")
	f.h = LoopbackFileHandler(f.lib, func() []string { return []string{f.root} })
	return f
}

func (f *loopbackFixture) add(id, abs, ext string) {
	f.lib[id] = TrackInfo{TrackID: id, AbsolutePath: abs, FileExtension: ext}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	// Every byte value appears, so a transform of any of them shows.
	for i := 0; i < 256 && i < n; i++ {
		b[i] = byte(i)
	}
	return b
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
}

// loopbackGet sends one request through the handler from remote.
func loopbackGet(h http.Handler, method, path, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:7791"+path, nil)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// assertRefusal requires a typed refusal: the status, a JSON body whose
// error is code, a message, and none of the bytes the route must not serve.
func assertRefusal(t *testing.T, rec *httptest.ResponseRecorder, status int, code string, never ...[]byte) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d (body %q)", rec.Code, status, short(rec.Body.String()))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q, want application/json", ct)
	}
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("refusal is not JSON: %v (%q)", err, short(rec.Body.String()))
	}
	if body.Error != code || body.Message == "" {
		t.Fatalf("refusal %+v, want error %q with a message", body, code)
	}
	for _, b := range never {
		if strings.Contains(rec.Body.String(), string(b)) {
			t.Fatalf("refusal carried bytes it must not serve")
		}
	}
}

// TestLoopbackRouteRefusesARemoteThatIsNotLoopback is the defence for a
// listener that is ever bound beyond loopback: the handler itself refuses
// a request whose remote address is not this host's loopback.
func TestLoopbackRouteRefusesARemoteThatIsNotLoopback(t *testing.T) {
	f := newLoopbackFixture(t)
	for _, remote := range []string{
		"192.168.1.20:50000",
		"10.0.0.8:1234",
		"100.101.102.103:41641",
		"[fd7a:115c:a1e0::1]:41641",
		"[fe80::1%en0]:5000",
		"[2001:db8::7]:443",
		"0.0.0.0:1",
		"[::]:1",
		"",
		"not-an-address",
		"localhost:1",
	} {
		t.Run(remote, func(t *testing.T) {
			rec := loopbackGet(f.h, http.MethodGet, "/dlna/file/inside", remote, nil)
			assertRefusal(t, rec, http.StatusForbidden, "loopback_only", f.track[:256])
		})
	}
	for _, remote := range []string{"127.0.0.1:50000", "127.0.0.2:9", "[::1]:50000", "[::ffff:127.0.0.1]:1"} {
		t.Run(remote, func(t *testing.T) {
			rec := loopbackGet(f.h, http.MethodGet, "/dlna/file/inside", remote, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d from %s, want 200 (%q)", rec.Code, remote, short(rec.Body.String()))
			}
		})
	}
}

// TestLoopbackRouteServesTheFileUnaltered: the bytes are the file's, whole
// or by range, and HEAD sends none. No transcoding, ever.
func TestLoopbackRouteServesTheFileUnaltered(t *testing.T) {
	f := newLoopbackFixture(t)
	mpd := map[string]string{"User-Agent": "Music Player Daemon 0.24.0"}

	rec := loopbackGet(f.h, http.MethodGet, "/dlna/file/inside", "127.0.0.1:1", mpd)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (%q)", rec.Code, short(rec.Body.String()))
	}
	if got := rec.Body.Bytes(); string(got) != string(f.track) {
		t.Fatalf("body is %d bytes and differs from the %d-byte file", len(got), len(f.track))
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(f.track)) {
		t.Fatalf("content length %q", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("accept-ranges %q", got)
	}
	// The serving half is the DLNA file route's: MPD gets the same type it
	// gets from the LAN listener.
	dlna := FileHandler(newTestLib(f.lib["inside"]), nil, nil, nil)
	ref := httptest.NewRecorder()
	refReq := httptest.NewRequest(http.MethodGet, "/dlna/file/inside", nil)
	refReq.Header.Set("User-Agent", mpd["User-Agent"])
	dlna(ref, refReq)
	if got, want := rec.Header().Get("Content-Type"), ref.Header().Get("Content-Type"); got != want || got == "" {
		t.Fatalf("content type %q, the DLNA route sends %q", got, want)
	}

	for _, tc := range []struct {
		rng  string
		want []byte
	}{
		{"bytes=10-19", f.track[10:20]},
		{"bytes=0-0", f.track[0:1]},
		{"bytes=-5", f.track[len(f.track)-5:]},
		{"bytes=65000-", f.track[65000:]},
	} {
		rec := loopbackGet(f.h, http.MethodGet, "/dlna/file/inside", "[::1]:1", map[string]string{"Range": tc.rng})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("%s: status %d", tc.rng, rec.Code)
		}
		if string(rec.Body.Bytes()) != string(tc.want) {
			t.Fatalf("%s: wrong bytes", tc.rng)
		}
		if rec.Header().Get("Content-Range") == "" {
			t.Fatalf("%s: no content range", tc.rng)
		}
	}

	head := loopbackGet(f.h, http.MethodHead, "/dlna/file/inside", "127.0.0.1:1", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD status %d body %d bytes", head.Code, head.Body.Len())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(f.track)) {
		t.Fatalf("HEAD content length %q", got)
	}
}

// TestLoopbackRouteConfinesToTheLibraryRoots: a file outside every root, a
// symlink out of a root, and a path with `..` are refused, by the id the
// client names and by the path the index holds. A link that stays inside
// a root, and a root that is itself a link, are served.
func TestLoopbackRouteConfinesToTheLibraryRoots(t *testing.T) {
	f := newLoopbackFixture(t)
	secret := filepath.Join(f.outside, "secret.flac")

	// The index holds a path outside every root: a row the roots no longer
	// cover, or one that never should have been there.
	f.add("outside", secret, ".flac")
	// The index holds a path whose `..` leaves the root. Joined by hand:
	// filepath.Join would clean the `..` away.
	sep := string(filepath.Separator)
	f.add("dotdot", strings.Join([]string{f.root, "Artist", "..", "..", "outside", "secret.flac"}, sep), ".flac")

	links := runtime.GOOS != "windows"
	if links {
		// A file in the root that links out of it.
		mustSymlink(t, secret, filepath.Join(f.root, "Artist", "Album", "02.flac"))
		f.add("link-out", filepath.Join(f.root, "Artist", "Album", "02.flac"), ".flac")
		// A directory in the root that links out of it.
		mustSymlink(t, f.outside, filepath.Join(f.root, "jump"))
		f.add("dir-link-out", filepath.Join(f.root, "jump", "secret.flac"), ".flac")
		// A relative link out.
		mustSymlink(t, filepath.Join("..", "..", "..", "outside", "secret.flac"),
			filepath.Join(f.root, "Artist", "Album", "03.flac"))
		f.add("relative-link-out", filepath.Join(f.root, "Artist", "Album", "03.flac"), ".flac")
		// A link that stays inside the root is served.
		mustSymlink(t, filepath.Join(f.root, "Artist", "Album", "01.flac"), filepath.Join(f.root, "alias.flac"))
		f.add("link-in", filepath.Join(f.root, "alias.flac"), ".flac")
	}

	for _, id := range []string{"outside", "dotdot", "link-out", "dir-link-out", "relative-link-out"} {
		if _, ok := f.lib[id]; !ok {
			continue
		}
		t.Run("index "+id, func(t *testing.T) {
			rec := loopbackGet(f.h, http.MethodGet, "/dlna/file/"+id, "127.0.0.1:1", nil)
			assertRefusal(t, rec, http.StatusForbidden, "outside_library", f.secret)
		})
	}
	if links {
		rec := loopbackGet(f.h, http.MethodGet, "/dlna/file/link-in", "127.0.0.1:1", nil)
		if rec.Code != http.StatusOK || string(rec.Body.Bytes()) != string(f.track) {
			t.Fatalf("a link inside the root: status %d", rec.Code)
		}
	}

	// What the client names is a track id, one path segment, never a path.
	for _, path := range []string{
		"/dlna/file/..",
		"/dlna/file/../../outside/secret.flac",
		"/dlna/file/%2e%2e/%2e%2e/outside/secret.flac",
		"/dlna/file/..%2f..%2foutside%2fsecret.flac",
		"/dlna/file/" + strings.TrimPrefix(filepath.ToSlash(secret), "/"),
		"/dlna/file//" + strings.TrimPrefix(filepath.ToSlash(secret), "/"),
		"/dlna/file/inside/../outside",
		"/dlna/file/inside/..",
		"/dlna/file/inside/extra",
		"/dlna/file/inside%2f..%2f..",
	} {
		t.Run("url "+path, func(t *testing.T) {
			// The raw request target, as a client sends it: nothing on the
			// way to the handler removes a dot segment (the server mounts no
			// ServeMux, which would clean and redirect).
			rec := loopbackGet(f.h, http.MethodGet, path, "127.0.0.1:1", nil)
			assertRefusal(t, rec, http.StatusNotFound, "not_found", f.secret)
		})
	}

	if links {
		t.Run("a root that is itself a link", func(t *testing.T) {
			linkedRoot := filepath.Join(filepath.Dir(f.root), "MusicLink")
			mustSymlink(t, f.root, linkedRoot)
			lib := loopbackLib{"via-link": {
				TrackID: "via-link", FileExtension: ".flac",
				AbsolutePath: filepath.Join(linkedRoot, "Artist", "Album", "01.flac"),
			}}
			h := LoopbackFileHandler(lib, func() []string { return []string{linkedRoot} })
			rec := loopbackGet(h, http.MethodGet, "/dlna/file/via-link", "127.0.0.1:1", nil)
			if rec.Code != http.StatusOK || string(rec.Body.Bytes()) != string(f.track) {
				t.Fatalf("status %d (%q)", rec.Code, short(rec.Body.String()))
			}
		})
	}

	t.Run("no roots serve nothing", func(t *testing.T) {
		h := LoopbackFileHandler(f.lib, func() []string { return nil })
		rec := loopbackGet(h, http.MethodGet, "/dlna/file/inside", "127.0.0.1:1", nil)
		assertRefusal(t, rec, http.StatusForbidden, "outside_library", f.track[:256])
	})
}

// TestLoopbackRouteRefusesALinkSwappedAfterTheCheck: a link that points
// inside a root when the route judges it, and out of the roots when the
// route opens it, is refused. The request is parked between the two.
func TestLoopbackRouteRefusesALinkSwappedAfterTheCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("swaps a symlink in place")
	}
	f := newLoopbackFixture(t)
	link := filepath.Join(f.root, "Artist", "Album", "swap.flac")
	mustSymlink(t, filepath.Join(f.root, "Artist", "Album", "01.flac"), link)
	f.add("swap", link, ".flac")
	swapped := false
	h := &loopbackHandler{
		lib:   f.lib,
		roots: func() []string { return []string{f.root} },
		beforeOpen: func() {
			if err := os.Remove(link); err != nil {
				t.Errorf("remove the link: %v", err)
			}
			if err := os.Symlink(filepath.Join(f.outside, "secret.flac"), link); err != nil {
				t.Errorf("swap the link: %v", err)
			}
			swapped = true
		},
	}
	rec := loopbackGet(h, http.MethodGet, "/dlna/file/swap", "127.0.0.1:1", nil)
	if !swapped {
		t.Fatal("the request never reached the open")
	}
	assertRefusal(t, rec, http.StatusForbidden, "outside_library", f.secret)
}

// TestLoopbackRouteListsNothing: the file route is the only route, and it
// serves one file by id. No directory, no description, no control.
func TestLoopbackRouteListsNothing(t *testing.T) {
	f := newLoopbackFixture(t)
	for _, path := range []string{
		"/",
		"/dlna",
		"/dlna/",
		"/dlna/file",
		"/dlna/file/",
		"/dlna/description.xml",
		"/dlna/cds/control",
		"/dlna/silence.wav",
		"/dlna/artwork/abc",
		"/v1/list",
		"/v1/download?path=Artist/Album/01.flac",
		"/Artist/Album/01.flac",
	} {
		t.Run(path, func(t *testing.T) {
			rec := loopbackGet(f.h, http.MethodGet, path, "127.0.0.1:1", nil)
			assertRefusal(t, rec, http.StatusNotFound, "not_found", f.track[:256])
		})
	}
}

// TestLoopbackRouteServesNoVariantAndNoRoutedTrack: the route serves the
// library's own files. A rendition is not one, and neither is a track the
// bridge proxies from an upstream server.
func TestLoopbackRouteServesNoVariantAndNoRoutedTrack(t *testing.T) {
	f := newLoopbackFixture(t)
	sidecar := filepath.Join(t.TempDir(), "x.flac")
	mustWrite(t, sidecar, []byte("RENDITION"))
	ti := f.lib["inside"]
	ti.Variants = []VariantInfo{{VariantID: "v1", AbsolutePath: sidecar, FileExtension: ".flac"}}
	f.lib["inside"] = ti
	f.lib["routed"] = TrackInfo{TrackID: "routed", RelativePath: "2go/a.flac", FileExtension: ".flac"}

	rec := loopbackGet(f.h, http.MethodGet, "/dlna/file/inside/variant-v1.flac", "127.0.0.1:1", nil)
	assertRefusal(t, rec, http.StatusNotFound, "not_found", []byte("RENDITION"))
	rec = loopbackGet(f.h, http.MethodGet, "/dlna/file/routed", "127.0.0.1:1", nil)
	assertRefusal(t, rec, http.StatusNotFound, "not_found")
}

// TestLoopbackRouteRefusesOtherMethods: GET and HEAD only.
func TestLoopbackRouteRefusesOtherMethods(t *testing.T) {
	f := newLoopbackFixture(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, "SUBSCRIBE"} {
		t.Run(m, func(t *testing.T) {
			rec := loopbackGet(f.h, m, "/dlna/file/inside", "127.0.0.1:1", nil)
			assertRefusal(t, rec, http.StatusMethodNotAllowed, "method_not_allowed", f.track[:256])
			if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
				t.Fatalf("Allow %q", got)
			}
		})
	}
}

// TestLoopbackRouteRefusesWhatIsNotAFile: a directory at a track's path,
// and a file that is gone, are refused by type.
func TestLoopbackRouteRefusesWhatIsNotAFile(t *testing.T) {
	f := newLoopbackFixture(t)
	f.add("dir", filepath.Join(f.root, "Artist"), ".flac")
	f.add("gone", filepath.Join(f.root, "Artist", "Album", "99.flac"), ".flac")
	rec := loopbackGet(f.h, http.MethodGet, "/dlna/file/dir", "127.0.0.1:1", nil)
	assertRefusal(t, rec, http.StatusNotFound, "not_a_file")
	rec = loopbackGet(f.h, http.MethodGet, "/dlna/file/gone", "127.0.0.1:1", nil)
	assertRefusal(t, rec, http.StatusNotFound, "not_found")
}

// TestLoopbackServerBindsOnlyLoopback: every address the server listens on
// is a loopback address, the base URL names 127.0.0.1, and a real request
// through it is served.
func TestLoopbackServerBindsOnlyLoopback(t *testing.T) {
	f := newLoopbackFixture(t)
	srv := NewLoopbackServer(LoopbackConfig{Library: f.lib, Roots: func() []string { return []string{f.root} }, Port: 0})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	addrs := srv.Addrs()
	if len(addrs) == 0 {
		t.Fatal("no listener")
	}
	port := ""
	for _, a := range addrs {
		tcp, ok := a.(*net.TCPAddr)
		if !ok || !tcp.IP.IsLoopback() {
			t.Fatalf("listens on %v, which is not loopback", a)
		}
		p := strconv.Itoa(tcp.Port)
		if port != "" && p != port {
			t.Fatalf("two ports: %s and %s", port, p)
		}
		port = p
	}
	if got, want := srv.BaseURL(), "http://127.0.0.1:"+port; got != want {
		t.Fatalf("base URL %q, want %q", got, want)
	}
	for _, a := range addrs {
		url := "http://" + a.String() + "/dlna/file/inside"
		resp, err := loopbackClient().Get(url)
		if err != nil {
			t.Fatalf("get %s: %v", url, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || string(body) != string(f.track) {
			t.Fatalf("%s: status %d, %d bytes, err %v", url, resp.StatusCode, len(body), err)
		}
	}
}

// TestLoopbackServerStopsWithTheServer: Stop ends a body still streaming
// and closes the port.
func TestLoopbackServerStopsWithTheServer(t *testing.T) {
	f := newLoopbackFixture(t)
	big := filepath.Join(f.root, "big.dsf")
	file, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	file.Close()
	f.add("big", big, ".dsf")

	srv := NewLoopbackServer(LoopbackConfig{Library: f.lib, Roots: func() []string { return []string{f.root} }, Port: 0})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = srv.Stop(context.Background())
		}
	})
	base := srv.BaseURL()
	resp, err := loopbackClient().Get(base + "/dlna/file/big")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	one := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, one); err != nil {
		t.Fatalf("first byte: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no grace: Stop must close the stream, not wait for it
	_ = srv.Stop(ctx)
	stopped = true

	n, rerr := io.Copy(io.Discard, resp.Body)
	if n+1 >= size && rerr == nil {
		t.Fatalf("the stream ran to its end after Stop (%d bytes)", n+1)
	}
	host := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", host)
	if err == nil {
		conn.Close()
		t.Fatalf("%s still accepts after Stop", host)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !strings.Contains(err.Error(), "refused") {
		t.Fatalf("dial after Stop: %v", err)
	}
}

// short keeps a failure message readable when the body is a track.
func short(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// loopbackClient keeps its own transport so a closed connection from one
// test is never handed to another (backlog B456).
func loopbackClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}
