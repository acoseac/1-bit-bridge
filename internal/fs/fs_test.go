package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// newTwoRootResolver sets up /tmp/<unique>/a/... and /tmp/<unique>/b/... and
// returns a Resolver with both roots plus the tmp dir for helper asserts.
func newTwoRootResolver(t *testing.T) (*Resolver, string, string) {
	t.Helper()
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	os.MkdirAll(filepath.Join(a, "Album"), 0o755)
	os.MkdirAll(filepath.Join(b, "Album"), 0o755)
	os.WriteFile(filepath.Join(a, "Album", "track.flac"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(b, "Album", "other.flac"), []byte("y"), 0o644)
	return New([]string{a, b}), a, b
}

func newSingleRootResolver(t *testing.T) (*Resolver, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	os.MkdirAll(filepath.Join(root, "Artist", "Album"), 0o755)
	os.WriteFile(filepath.Join(root, "Artist", "Album", "01.flac"), []byte("hi"), 0o644)
	return New([]string{root}), root
}

// --- single-root ---

func TestResolveSingleRootHappy(t *testing.T) {
	r, root := newSingleRootResolver(t)
	got, err := r.Resolve("Artist/Album/01.flac")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(root, "Artist", "Album", "01.flac")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveEmptyPathIsRoot(t *testing.T) {
	r, root := newSingleRootResolver(t)
	got, err := r.Resolve("")
	if err != nil {
		t.Fatalf("Resolve(''): %v", err)
	}
	if got != root {
		t.Errorf("got %q, want %q", got, root)
	}
}

func TestResolveSlashOnlyIsRoot(t *testing.T) {
	r, root := newSingleRootResolver(t)
	got, err := r.Resolve("/")
	if err != nil {
		t.Fatalf("Resolve('/'): %v", err)
	}
	if got != root {
		t.Errorf("got %q, want %q", got, root)
	}
}

// --- traversal rejection ---

func TestResolveRejectsDotDot(t *testing.T) {
	r, _ := newSingleRootResolver(t)
	for _, bad := range []string{
		"..",
		"../secret",
		"Artist/../..",
		"Artist/../../etc/passwd",
		"./../Artist",
	} {
		_, err := r.Resolve(bad)
		if !errors.Is(err, ErrBadPath) {
			t.Errorf("Resolve(%q) err = %v, want ErrBadPath", bad, err)
		}
	}
}

func TestResolveRejectsAbsolutePaths(t *testing.T) {
	// "/etc/passwd" after cleaning + trimming leading "/" becomes
	// "etc/passwd" which would resolve *inside* the root — an attacker
	// leveraging os-native path. The trim-prefix-"/" in Clean handles this.
	// But paths that start with "/" should still be fine because they're
	// interpreted as library-root-relative, just with a redundant leading
	// slash. That's NOT a security hole. What IS a hole is "../.." style
	// escapes, tested above. This test documents the intended behavior.
	r, root := newSingleRootResolver(t)
	got, err := r.Resolve("/Artist/Album/01.flac")
	if err != nil {
		t.Fatalf("leading-/ should be accepted (root-relative): %v", err)
	}
	want := filepath.Join(root, "Artist", "Album", "01.flac")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveRejectsNullByte(t *testing.T) {
	r, _ := newSingleRootResolver(t)
	_, err := r.Resolve("Artist\x00/hax")
	if !errors.Is(err, ErrBadPath) {
		t.Errorf("null byte err = %v, want ErrBadPath", err)
	}
}

func TestResolveNormalizesRedundantSeparators(t *testing.T) {
	r, root := newSingleRootResolver(t)
	got, err := r.Resolve("Artist//Album///01.flac")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "Artist", "Album", "01.flac")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- multi-root ---

func TestResolveMultiRootPicksBasenameMatch(t *testing.T) {
	r, a, b := newTwoRootResolver(t)
	gotA, err := r.Resolve("a/Album/track.flac")
	if err != nil {
		t.Fatalf("Resolve(a): %v", err)
	}
	if gotA != filepath.Join(a, "Album", "track.flac") {
		t.Errorf("a: got %q, want %q", gotA, filepath.Join(a, "Album", "track.flac"))
	}
	gotB, err := r.Resolve("b/Album/other.flac")
	if err != nil {
		t.Fatalf("Resolve(b): %v", err)
	}
	if gotB != filepath.Join(b, "Album", "other.flac") {
		t.Errorf("b: got %q, want %q", gotB, filepath.Join(b, "Album", "other.flac"))
	}
}

func TestResolveMultiRootRejectsUnknown(t *testing.T) {
	r, _, _ := newTwoRootResolver(t)
	_, err := r.Resolve("c/Album/track.flac")
	if !errors.Is(err, ErrUnknownRoot) {
		t.Errorf("err = %v, want ErrUnknownRoot", err)
	}
}

func TestResolveMultiRootEmptyPathIsAmbiguous(t *testing.T) {
	r, _, _ := newTwoRootResolver(t)
	_, err := r.Resolve("")
	if !errors.Is(err, ErrUnknownRoot) {
		t.Errorf("empty path in multi-root: err = %v, want ErrUnknownRoot", err)
	}
}

// --- symlink escape ---

func TestResolveStillStopsSymlinkEscape(t *testing.T) {
	// Create a root containing a symlink that points outside. Resolve
	// itself uses filepath.Abs, not EvalSymlinks, so the string check
	// passes — but ResolveChecked's os.Stat follows the link and would
	// expose outside content. This test verifies the check after Resolve is
	// enough for our threat model (we're preventing *lexical* escape). If
	// the user deliberately plants a symlink they own inside the library
	// root, they're showing that file — that's expected behavior.
	//
	// The important bit: "../" in the CLIENT path can never punch out,
	// regardless of what symlinks the server has set up.
	if runtime.GOOS == "windows" {
		t.Skip("symlink test requires unix-style symlinks")
	}
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "leak.txt"), []byte("secret"), 0o644)

	root := filepath.Join(tmp, "root")
	os.MkdirAll(root, 0o755)
	if err := os.Symlink(outside, filepath.Join(root, "jump")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	r := New([]string{root})
	// Lexically this resolves to <root>/jump/leak.txt — which is INSIDE
	// the root by string comparison. The threat we're preventing is a
	// client path that escapes without the server's help.
	got, err := r.Resolve("jump/leak.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Errorf("resolved path %q escaped root %q", got, root)
	}
}

// --- ResolveChecked ---

func TestResolveCheckedNotFound(t *testing.T) {
	r, _ := newSingleRootResolver(t)
	_, _, err := r.ResolveChecked("Artist/Album/nonexistent.flac")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveCheckedReturnsInfo(t *testing.T) {
	r, _ := newSingleRootResolver(t)
	abs, info, err := r.ResolveChecked("Artist/Album/01.flac")
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Error("expected file, got dir")
	}
	if info.Name() != "01.flac" {
		t.Errorf("name = %q", info.Name())
	}
	if !strings.HasSuffix(abs, filepath.Join("Artist", "Album", "01.flac")) {
		t.Errorf("abs = %q", abs)
	}
}

func TestRootsIsAcopy(t *testing.T) {
	r, _ := newSingleRootResolver(t)
	got := r.Roots()
	got[0] = "poison"
	got2 := r.Roots()
	if got2[0] == "poison" {
		t.Error("Roots mutated internal state")
	}
}

func TestValidateRootsRejectsDuplicateBasename(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a", "Music")
	b := filepath.Join(tmp, "b", "Music")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateRoots([]string{a, b}); err == nil {
		t.Fatal("expected collision error, got nil")
	} else if !strings.Contains(err.Error(), "Music") {
		t.Errorf("err doesn't name the colliding basename: %v", err)
	}
}

// TestValidateRootsFoldsCase pins the collision check as case-INSENSITIVE.
//
// Byte-exact comparison accepted /srv/Music and /srv/music as two roots.
// That is wrong on its own terms — the filesystem is case-insensitive on
// macOS and Windows, so they are the same directory there and one root
// becomes unreachable — but the sharp consequence was downstream: track
// paths are keyed by basename, and the removal path matched them
// case-insensitively, so removing one root deleted the other's rows and
// unlinked its variant and waveform sidecars from disk.
//
// Those predicates are case-exact now. Refusing the configuration as well
// is deliberate defence in depth: it keeps the collision from being
// expressible at all, rather than relying on every present and future
// consumer of a basename to keep getting it right.
func TestValidateRootsFoldsCase(t *testing.T) {
	for _, c := range [][2]string{
		{"/srv/Music", "/srv/music"},
		{"/a/MUSIC", "/b/music"},
		{"/a/MÚSICA", "/b/música"}, // Unicode-aware, not just ASCII
	} {
		err := ValidateRoots([]string{c[0], c[1]})
		if err == nil {
			t.Errorf("ValidateRoots(%q, %q) = nil — basenames differing only by case must collide", c[0], c[1])
			continue
		}
		if !strings.Contains(err.Error(), c[0]) || !strings.Contains(err.Error(), c[1]) {
			t.Errorf("error should name both roots so the operator can act on it: %v", err)
		}
	}
	// Genuinely distinct basenames still pass.
	if err := ValidateRoots([]string{"/a/Music", "/b/Musique"}); err != nil {
		t.Errorf("distinct basenames should not collide: %v", err)
	}
}

// TestFoldRootBasenameIsSharedAcrossGuards pins the helper ValidateRoots
// and the admin/CLI remove-root guards all key off. Those three agreeing
// is the invariant: ValidateRoots decides which configurations are
// expressible, the remove guards decide which removals are unambiguous,
// and a divergence yields a config that validates but whose removal can't
// tell the two roots apart.
func TestFoldRootBasenameIsSharedAcrossGuards(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/srv/Music", "music"},
		{"/srv/music", "music"},
		{"/srv/MUSIC", "music"},
		{"/a/b/Audiobooks", "audiobooks"},
	}
	for _, c := range cases {
		if got := FoldRootBasename(c.in); got != c.want {
			t.Errorf("FoldRootBasename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if FoldRootBasename("/srv/Music") != FoldRootBasename("/other/music") {
		t.Error("case-twin basenames must fold to the same key")
	}
}

// TestFoldRootBasenameIsConcurrencySafe guards the ONE edit that would make
// the package-level caser unsafe.
//
// basenameFolder is shared, which is fine only because cases.Lower(und) with
// no options resolves to x/text's stateless `undLower` singleton. Adding an
// option or naming a locale silently switches makeLower to a `&lowerCaser{}`
// that embeds a context Transform overwrites on entry — a one-word change,
// invisible in review, that would turn ValidateRoots and both remove-root
// guards into a data race. Those run concurrently from admin request
// handling and gate a DESTRUCTIVE root removal, so a corrupted fold is a
// wrong collision verdict on a delete.
//
// The assertions are what give this teeth beyond -race: a shared stateful
// caser interleaves two calls' contexts and returns garbage, so the
// per-input equality check fails even in a non-race run. Distinct non-ASCII
// inputs per goroutine make that interleaving observable — identical ASCII
// inputs could corrupt into each other unnoticed.
func TestFoldRootBasenameIsConcurrencySafe(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/srv/MÚSICA", "música"},
		{"/srv/Ärchiv", "ärchiv"},
		{"/srv/ØRESUND", "øresund"},
		{"/srv/ÅKERFELDT", "åkerfeldt"},
		{"/srv/Ægir", "ægir"},
		{"/srv/ΣΊΣΥΦΟΣ", "σίσυφος"},
		{"/srv/Żółw", "żółw"},
		{"/srv/Music", "music"},
	}

	const goroutines = 32
	const iterations = 3000
	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for g := 0; g < goroutines; g++ {
		c := cases[g%len(cases)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if got := FoldRootBasename(c.in); got != c.want {
					select {
					case errs <- fmt.Sprintf("FoldRootBasename(%q) = %q, want %q — the shared caser is not stateless", c.in, got, c.want):
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

func TestValidateRootsSingleRootIsOK(t *testing.T) {
	if err := ValidateRoots([]string{"/a/Music"}); err != nil {
		t.Errorf("single root should not error: %v", err)
	}
}

func TestValidateRootsUniqueBasenames(t *testing.T) {
	if err := ValidateRoots([]string{"/a/Music", "/b/Audiobooks"}); err != nil {
		t.Errorf("unique basenames should not error: %v", err)
	}
}

// --- hot-reload ---

func TestSetRootsSwapsBasenameIndex(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	c := filepath.Join(tmp, "c")
	for _, d := range []string{a, b, c} {
		os.MkdirAll(filepath.Join(d, "Album"), 0o755)
	}
	os.WriteFile(filepath.Join(c, "Album", "only.flac"), []byte("z"), 0o644)

	r := New([]string{a, b})

	// Before swap: "c/..." is unknown.
	if _, err := r.Resolve("c/Album/only.flac"); !errors.Is(err, ErrUnknownRoot) {
		t.Errorf("pre-swap: want ErrUnknownRoot, got %v", err)
	}

	// After swap: "a/..." is unknown (removed) and "c/..." resolves.
	r.SetRoots([]string{b, c})

	if _, err := r.Resolve("a/Album/track.flac"); !errors.Is(err, ErrUnknownRoot) {
		t.Errorf("post-swap: want ErrUnknownRoot for removed root, got %v", err)
	}
	got, err := r.Resolve("c/Album/only.flac")
	if err != nil {
		t.Fatalf("post-swap resolve new root: %v", err)
	}
	want := filepath.Join(c, "Album", "only.flac")
	if got != want {
		t.Errorf("post-swap resolve = %q, want %q", got, want)
	}

	roots := r.Roots()
	if len(roots) != 2 || roots[0] != b || roots[1] != c {
		t.Errorf("Roots() = %v, want [%q, %q]", roots, b, c)
	}
}

func TestSetRootsSingleToMultiTransition(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "Music")
	b := filepath.Join(tmp, "Audiobooks")
	for _, d := range []string{a, b} {
		os.MkdirAll(filepath.Join(d, "Album"), 0o755)
	}
	os.WriteFile(filepath.Join(a, "Album", "x.flac"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(b, "Album", "y.flac"), []byte("y"), 0o644)

	r := New([]string{a})
	// Single-root form: client path has no basename prefix.
	if _, err := r.Resolve("Album/x.flac"); err != nil {
		t.Fatalf("single-root resolve: %v", err)
	}

	r.SetRoots([]string{a, b})
	// Multi-root form: client path now routes by basename.
	got, err := r.Resolve("Music/Album/x.flac")
	if err != nil {
		t.Fatalf("multi-root resolve a: %v", err)
	}
	if got != filepath.Join(a, "Album", "x.flac") {
		t.Errorf("multi-root resolve = %q", got)
	}
	if _, err := r.Resolve("Audiobooks/Album/y.flac"); err != nil {
		t.Fatalf("multi-root resolve b: %v", err)
	}
}

// --- filesystem-root configuration ---

func TestResolveAcceptsFilesystemRootUnix(t *testing.T) {
	// Operators using Docker may mount the library directly at "/", or run
	// the bridge on a dedicated VM where the entire FS is the library. The
	// prefix-check used to build "//" and reject every path; this pins the
	// trim-suffix fix.
	if runtime.GOOS == "windows" {
		t.Skip("filesystem-root test for unix-style paths")
	}
	r := New([]string{"/"})
	got, err := r.Resolve("etc/hostname")
	if err != nil {
		t.Fatalf("Resolve under root /: %v", err)
	}
	want := filepath.Join("/", "etc", "hostname")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveAcceptsFilesystemRootWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows drive-root semantics")
	}
	// On Windows, filepath.Abs("C:\\") returns "C:\\". The prefix-check used
	// to build "C:\\\\" and reject every path under the drive root.
	r := New([]string{`C:\`})
	got, err := r.Resolve("Windows/System32/drivers/etc/hosts")
	if err != nil {
		t.Fatalf("Resolve under root C:\\: %v", err)
	}
	if !strings.HasPrefix(got, `C:\`) {
		t.Errorf("got %q, expected prefix C:\\", got)
	}
}

func TestResolveFilesystemRootStillRejectsTraversal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("filesystem-root test for unix-style paths")
	}
	r := New([]string{"/"})
	for _, bad := range []string{"..", "../etc", "etc/../.."} {
		if _, err := r.Resolve(bad); !errors.Is(err, ErrBadPath) {
			t.Errorf("Resolve(%q) under root /: err = %v, want ErrBadPath", bad, err)
		}
	}
}

func TestSetRootsConcurrentResolve(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	for _, d := range []string{a, b} {
		os.MkdirAll(filepath.Join(d, "Album"), 0o755)
	}
	os.WriteFile(filepath.Join(a, "Album", "x.flac"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(b, "Album", "y.flac"), []byte("y"), 0o644)

	r := New([]string{a})
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			// The resolve call may or may not succeed depending on whether
			// the swap has landed — we only care that it doesn't race.
			_, _ = r.Resolve("Album/x.flac")
			_, _ = r.Resolve("a/Album/x.flac")
		}
	}()
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			r.SetRoots([]string{a})
		} else {
			r.SetRoots([]string{a, b})
		}
	}
	<-done
}
