package library

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// openWatchdog is how long an open may take before a test calls it held: a
// plain open of a named pipe waits for a writer, and the resolver's own
// returns in milliseconds.
const openWatchdog = 10 * time.Second

// libraryFixture is a library root, an index of it in a store of its own,
// and a resolver over both, the way serve wires one: the store, and the
// path resolver whose roots are the live ones.
type libraryFixture struct {
	t     *testing.T
	root  string
	store *manifest.Store
	paths *bridgefs.Resolver
	r     *Resolver
}

func newLibraryFixture(t *testing.T) *libraryFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &libraryFixture{t: t, root: root, store: store, paths: bridgefs.New([]string{root})}
	f.r, err = New(Config{Store: store, Paths: f.paths})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// add writes data to rel below the root and indexes it, and returns its
// reference.
func (f *libraryFixture) add(rel string, data []byte) engine.Reference {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return f.index(rel)
}

// index gives the index a row for rel, whatever is at it, and returns its
// reference.
func (f *libraryFixture) index(rel string) engine.Reference {
	f.t.Helper()
	if err := f.store.UpsertTrack(context.Background(), &manifest.Track{Path: rel, Size: 1}); err != nil {
		f.t.Fatal(err)
	}
	return Reference(rel)
}

// open opens ref under the watchdog: a resolver that opened a named pipe
// with a plain open would wait for a writer, and the test would hang.
func (f *libraryFixture) open(ref engine.Reference) (playback.Content, error) {
	return f.openWith(context.Background(), ref)
}

func (f *libraryFixture) openWith(ctx context.Context, ref engine.Reference) (playback.Content, error) {
	f.t.Helper()
	type result struct {
		c   playback.Content
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := f.r.Open(ctx, ref)
		done <- result{c, err}
	}()
	select {
	case res := <-done:
		return res.c, res.err
	case <-time.After(openWatchdog):
		f.t.Fatalf("the open of %s did not return within %v", ref, openWatchdog)
		return playback.Content{}, nil
	}
}

// read reads c whole, as the backend does, through a section of its Size,
// and closes it.
func read(t *testing.T, c playback.Content) []byte {
	t.Helper()
	b, err := io.ReadAll(io.NewSectionReader(c.Data, 0, c.Size))
	if err != nil {
		t.Fatal(err)
	}
	if c.Closer == nil {
		t.Fatal("the content has no Closer: the file it opened would stay open")
	}
	if err := c.Closer.Close(); err != nil {
		t.Fatal(err)
	}
	return b
}

// refusedWith requires err to be an engine.Unplayable with reason, its
// detail naming no path of the library's or its root.
func (f *libraryFixture) refusedWith(what string, c playback.Content, err error, reason engine.Reason) {
	f.t.Helper()
	if err == nil {
		read(f.t, c)
		f.t.Fatalf("%s: opened, want refused %s", what, reason)
	}
	var u *engine.Unplayable
	if !errors.As(err, &u) {
		f.t.Fatalf("%s: %v (%T), want an *engine.Unplayable, which the backend passes on as it is", what, err, err)
	}
	if u.Reason != reason {
		f.t.Fatalf("%s: refused %s (%s), want %s", what, u.Reason, u.Detail, reason)
	}
	if c.Data != nil || c.Closer != nil {
		f.t.Errorf("%s: a refusal handed content over", what)
	}
	for _, p := range []string{f.root, filepath.ToSlash(f.root), filepath.Base(f.root) + string(filepath.Separator)} {
		if strings.Contains(u.Detail, p) || strings.Contains(err.Error(), p) {
			f.t.Errorf("%s: the refusal names a path (%q): %v", what, p, err)
		}
	}
}

// TestTheReferenceIsTheDLNATrackID pins what a reference is: the id
// /dlna/file/{id} names for an original, dlna.TrackID("", path), 16 hex
// digits, and so a printable reference the engine takes.
func TestTheReferenceIsTheDLNATrackID(t *testing.T) {
	for _, p := range []string{"Artist/Album/01 Song.flac", "a.wav", "Music/Ünïcode/x.aiff"} {
		ref := Reference(p)
		if ref.String() != dlna.TrackID("", p) {
			t.Errorf("Reference(%q) = %s, want dlna.TrackID(\"\", path) = %s", p, ref, dlna.TrackID("", p))
		}
		if len(ref.String()) != 16 || strings.Trim(ref.String(), "0123456789abcdef") != "" {
			t.Errorf("Reference(%q) = %q, not 16 lowercase hex digits", p, ref)
		}
	}
}

// TestTheResolverOpensAnIndexedFileInsideTheRoots: an indexed file inside
// a root opens as content of its own bytes and size, which its Closer
// releases.
func TestTheResolverOpensAnIndexedFileInsideTheRoots(t *testing.T) {
	f := newLibraryFixture(t)
	data := []byte("the samples of a track, as the file holds them")
	ref := f.add("Artist/Album/01 a.flac", data)
	c, err := f.open(ref)
	if err != nil {
		t.Fatal(err)
	}
	if c.Size != int64(len(data)) {
		t.Errorf("size %d, want %d", c.Size, len(data))
	}
	if got := read(t, c); string(got) != string(data) {
		t.Errorf("read %q, want %q", got, data)
	}
}

// TestTheResolverRefusesAReferenceNoIndexedTrackHas: a reference no row of
// the index hashes to, and one that is not a track's shape at all, are
// not_indexed.
func TestTheResolverRefusesAReferenceNoIndexedTrackHas(t *testing.T) {
	f := newLibraryFixture(t)
	f.add("a.flac", []byte("a"))
	// A file the index has no row for is not the library's, whatever is on
	// disk.
	if err := os.WriteFile(filepath.Join(f.root, "unindexed.flac"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := f.open(Reference("unindexed.flac"))
	f.refusedWith("a file the index has no row for", c, err, ReasonNotIndexed)
	for _, s := range []string{"file:1", "0123456789ABCDEF", "0123456789abcde", "/dlna/file/0123456789abcdef"} {
		ref, err := engine.NewReference(s)
		if err != nil {
			t.Fatal(err)
		}
		c, err := f.open(ref)
		f.refusedWith("the reference "+s, c, err, ReasonNotIndexed)
	}
}

// TestTheResolverChecksAfreshAtEveryOpen: what one open established is not
// taken for the next. A row removed after an open is not_indexed at the
// next, a file removed is missing_file, a root no longer configured is
// outside_library, and each comes back once what it needs is back.
func TestTheResolverChecksAfreshAtEveryOpen(t *testing.T) {
	f := newLibraryFixture(t)
	data := []byte("a track")
	ref := f.add("A/a.flac", data)
	opens := func(what string) {
		t.Helper()
		c, err := f.open(ref)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := read(t, c); string(got) != string(data) {
			t.Fatalf("%s: read %q", what, got)
		}
	}
	opens("first")

	if err := f.store.DeleteTrack(context.Background(), "A/a.flac"); err != nil {
		t.Fatal(err)
	}
	c, err := f.open(ref)
	f.refusedWith("the row removed after an open", c, err, ReasonNotIndexed)
	f.index("A/a.flac")
	opens("the row back")

	file := filepath.Join(f.root, "A", "a.flac")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	c, err = f.open(ref)
	f.refusedWith("the file removed", c, err, engine.ReasonMissing)
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	opens("the file back")

	// With two roots a stored path begins with its root's name, and a root
	// taken out of the configuration leaves its paths naming none.
	second := filepath.Join(t.TempDir(), "Second")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	f.paths.SetRoots([]string{f.root, second})
	ref = f.index("Music/A/a.flac")
	opens("two roots")
	f.paths.SetRoots([]string{second, filepath.Join(t.TempDir(), "Third")})
	c, err = f.open(ref)
	f.refusedWith("the root no longer configured", c, err, ReasonOutsideLibrary)
	f.paths.SetRoots([]string{f.root, second})
	opens("the root configured again")
}

// TestTheResolverRefusesAFileOutsideTheRoots: a row whose file, its links
// resolved, lies outside every root is outside_library, and so is a row
// whose path climbs out of the root; neither is opened, a directory outside
// the root included, which an open would refuse as no file.
func TestTheResolverRefusesAFileOutsideTheRoots(t *testing.T) {
	f := newLibraryFixture(t)
	outside := filepath.Join(t.TempDir(), "secret.flac")
	if err := os.WriteFile(outside, []byte("not the library's"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := f.open(f.index("../" + filepath.Base(filepath.Dir(outside)) + "/secret.flac"))
	f.refusedWith("a row whose path climbs out of the root", c, err, ReasonOutsideLibrary)
	if runtime.GOOS == "windows" {
		t.Skip("the link half needs a symbolic link, which this host may refuse")
	}
	if err := os.Symlink(outside, filepath.Join(f.root, "out.flac")); err != nil {
		t.Fatal(err)
	}
	c, err = f.open(f.index("out.flac"))
	f.refusedWith("a link out of the root", c, err, ReasonOutsideLibrary)
	// Refused before it is opened: a directory outside the root, opened,
	// would be refused as no file instead.
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(f.root, "outdir.flac")); err != nil {
		t.Fatal(err)
	}
	c, err = f.open(f.index("outdir.flac"))
	f.refusedWith("a link out of the root to a directory", c, err, ReasonOutsideLibrary)
	// A link that stays inside is the library's.
	if err := os.WriteFile(filepath.Join(f.root, "real.flac"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.root, "real.flac"), filepath.Join(f.root, "in.flac")); err != nil {
		t.Fatal(err)
	}
	c, err = f.open(f.index("in.flac"))
	if err != nil {
		t.Fatalf("a link that stays inside the root: %v", err)
	}
	if got := read(t, c); string(got) != "inside" {
		t.Errorf("a link that stays inside the root read %q", got)
	}
}

// TestTheResolverRefusesALinkSwappedInAfterItsCheck: the file is replaced
// by a link out of the root between the containment check and the open
// (the resolver's BeforeOpen seam); the open follows the link, and the
// second resolution, after the open, refuses what it reached.
func TestTheResolverRefusesALinkSwappedInAfterItsCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a symbolic link, which this host may refuse")
	}
	f := newLibraryFixture(t)
	ref := f.add("a.flac", []byte("the library's"))
	outside := filepath.Join(t.TempDir(), "secret.flac")
	if err := os.WriteFile(outside, []byte("not the library's"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.root, "a.flac")
	f.r.cfg.Seams = &Seams{BeforeOpen: func() {
		if err := os.Remove(file); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, file); err != nil {
			t.Error(err)
		}
	}}
	c, err := f.open(ref)
	f.refusedWith("a link swapped in after the check", c, err, ReasonOutsideLibrary)
}

// TestTheResolverRefusesWhatIsNotAFile: a directory at a row's path is
// not_a_file, refused at once.
func TestTheResolverRefusesWhatIsNotAFile(t *testing.T) {
	f := newLibraryFixture(t)
	if err := os.MkdirAll(filepath.Join(f.root, "dir.flac"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := f.open(f.index("dir.flac"))
	f.refusedWith("a directory", c, err, ReasonNotAFile)
}

// TestTheResolverRefusesARoutedTrack: a row an upstream server routes is
// not the bridge's own file, and is refused as routed, a file at its path
// below the root or not.
func TestTheResolverRefusesARoutedTrack(t *testing.T) {
	f := newLibraryFixture(t)
	ref := f.add("2go/Album/x.flac", []byte("a file that happens to be there"))
	if err := f.store.UpsertUPnPRouting(context.Background(), &manifest.UPnPRouting{
		SourcePath: "2go/Album/x.flac", ServerUDN: "uuid:2go", ObjectID: "1", ResURL: "http://192.0.2.1/x.flac", LastSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	c, err := f.open(ref)
	f.refusedWith("a routed track", c, err, ReasonRouted)
}

// TestTheResolverRefusesAnAmbiguousReference: two rows whose paths hash to
// one reference name no one file, and the reference is refused rather than
// one of them played (a collision of 64 bits, made here by the id seam).
func TestTheResolverRefusesAnAmbiguousReference(t *testing.T) {
	f := newLibraryFixture(t)
	f.add("a.flac", []byte("a"))
	f.add("b.flac", []byte("b"))
	f.r.idOf = func(string) string { return "00000000000000aa" }
	ref, err := engine.NewReference("00000000000000aa")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.open(ref)
	f.refusedWith("two rows with one reference", c, err, ReasonAmbiguous)
}

// TestTheResolverRecoversAPanic: a panic in the resolver, a callback the
// backend calls on a goroutine of its own, is recovered as the entry's
// refusal and logged with its stack, never its value, and the next open
// works.
func TestTheResolverRecoversAPanic(t *testing.T) {
	f := newLibraryFixture(t)
	ref := f.add("a.flac", []byte("a"))
	rec := loggingtest.Record(t)
	secret := filepath.Join(f.root, "a value a panic may carry")
	panics := true
	f.r.cfg.Seams = &Seams{Opens: func(engine.Reference) {
		if panics {
			panic(secret)
		}
	}}
	c, err := f.open(ref)
	f.refusedWith("a panic in the resolver", c, err, ReasonResolverFailed)
	lines := rec.Lines(panicMessage)
	if len(lines) != 1 || !strings.Contains(lines[0], "stack=") {
		t.Fatalf("the panic logged %d times, want once with its stack: %q", len(lines), rec.All())
	}
	if all := strings.Join(rec.All(), "\n"); strings.Contains(all, secret) {
		t.Errorf("the log carries the panic's value: %s", all)
	}
	panics = false
	c, err = f.open(ref)
	if err != nil {
		t.Fatalf("the open after the panic: %v", err)
	}
	read(t, c)
}

// TestTheResolverStopsAtItsContext: an open whose context has ended, the
// occurrence released or replaced, opens nothing and logs no failure of
// the index.
func TestTheResolverStopsAtItsContext(t *testing.T) {
	f := newLibraryFixture(t)
	ref := f.add("a.flac", []byte("a"))
	rec := loggingtest.Record(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := f.openWith(ctx, ref)
	f.refusedWith("a cancelled open", c, err, engine.ReasonMissing)
	if failures := rec.Failures(); len(failures) > 0 {
		t.Errorf("a cancelled open logged a failure: %q", failures)
	}
}
