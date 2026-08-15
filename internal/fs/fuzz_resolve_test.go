// Fuzz coverage for the path resolver's containment guarantee.
//
// This is the single highest-value invariant in the bridge: `Resolve` is what
// stands between a bearer-authenticated client and the host filesystem, and
// every served track goes through it. The package docblock states the
// guarantee as "a client path can never escape a root without the server's
// help" — this asserts exactly that, as a property, over arbitrary input
// rather than over a table of the traversal shapes we happened to think of.
//
// A table test can only refuse the escapes its author imagined. The escapes
// that matter are the ones nobody imagined: separator confusion, a `..` that
// only becomes one after cleaning, a multi-root first segment that resolves
// somewhere other than the root it names.
//
// Both modes are checked on every input because they take DIFFERENT code
// paths: single-root treats the whole path as root-relative, multi-root
// routes on the first segment through basenameIndex. A containment bug could
// exist in either alone.
//
// Note the deliberate asymmetry in what counts as failure: any ERROR is a
// pass (refusing input is always safe), and only a SUCCESS that lands outside
// every configured root fails. The test therefore cannot be satisfied by
// making the resolver stricter in a way that breaks real paths — it only
// catches genuine escapes.
package fs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzResolveContainment(f *testing.F) {
	for _, s := range []string{
		"Artist/Album/x.flac", "../../etc/passwd", "/etc/passwd",
		"a/../../b", "./x", "..", "", "a\x00b", `..\..\windows`,
		"Music/../../x", "Music/./../../x", "Music//x",
		"Music", "More/x.flac", "Solo/x.flac",
	} {
		f.Add(s)
	}

	base := f.TempDir()
	single := filepath.Join(base, "Solo")
	m1 := filepath.Join(base, "Music")
	m2 := filepath.Join(base, "More")
	for _, d := range []string{single, m1, m2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			f.Fatal(err)
		}
	}
	rSingle := New([]string{single})
	rMulti := New([]string{m1, m2})

	// check asserts the containment property for one resolver. Errors are
	// expected outcomes — only their KIND is constrained, so a future
	// refactor can't start returning something a caller would mishandle.
	check := func(t *testing.T, r *Resolver, roots []string, in string) {
		abs, err := r.Resolve(in)
		if err != nil {
			if !errors.Is(err, ErrBadPath) && !errors.Is(err, ErrUnknownRoot) && !errors.Is(err, ErrNotFound) {
				t.Fatalf("Resolve(%q): unexpected error kind %v", in, err)
			}
			return
		}
		if !filepath.IsAbs(abs) {
			t.Fatalf("Resolve(%q) returned a non-absolute path %q", in, abs)
		}
		for _, root := range roots {
			ra, aerr := filepath.Abs(root)
			if aerr != nil {
				continue
			}
			sep := string(filepath.Separator)
			if abs == ra || strings.HasPrefix(abs, strings.TrimSuffix(ra, sep)+sep) {
				return
			}
		}
		t.Fatalf("ESCAPE: Resolve(%q) = %q, which is outside every root %v", in, abs, roots)
	}

	f.Fuzz(func(t *testing.T, in string) {
		check(t, rSingle, []string{single}, in)
		check(t, rMulti, []string{m1, m2}, in)
	})
}
