package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the case-EXACTNESS of the prefix/subtree predicates.
//
// SQLite's default LIKE folds ASCII case (nothing sets
// case_sensitive_like — see OpenStore's DSN), so every `path LIKE
// 'prefix/%'` silently covered case-twin sibling directories. PR #532
// converted the rollup/count helpers to a byte range for PERFORMANCE and
// recorded the assumption it relied on — "numerically identical … for
// folder-derived prefixes (which never differ only by case)" — but the
// remaining sites don't take folder-derived prefixes: one takes a
// library-root basename, the others take a watcher-supplied directory.
//
// Every test below fails against the LIKE form.

func prefixCaseStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, context.Background()
}

func seedPaths(t *testing.T, s *Store, ctx context.Context, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := s.UpsertTrack(ctx, &Track{Path: p, Size: 100, ModTime: time.Now()}); err != nil {
			t.Fatalf("seed %q: %v", p, err)
		}
	}
}

func trackExists(t *testing.T, s *Store, ctx context.Context, p string) bool {
	t.Helper()
	got, _ := s.GetTrack(ctx, p)
	return got != nil
}

// TestDeleteTracksByPrefixIsCaseExact is the data-loss case.
//
// ValidateRoots compared basenames byte-exactly, so /srv/Music and
// /srv/music were both accepted as library roots. Removing one called
// DeleteTracksByPrefix("Music/"), whose case-folding LIKE matched BOTH —
// deleting the survivor's rows, and (via the two sidecar enumerations in
// the same method) unlinking its variant and waveform files from disk.
//
// The count the operator confirmed against came from
// CountTracksByPrefix, which was already case-sensitive, so the
// pre-confirm number understated the damage. This test asserts the two
// now agree, because that agreement is the actual contract.
func TestDeleteTracksByPrefixIsCaseExact(t *testing.T) {
	s, ctx := prefixCaseStore(t)
	seedPaths(t, s, ctx,
		"Music/a.flac", "Music/b.flac",
		"music/keep1.flac", "music/keep2.flac",
	)

	shown, err := s.CountTracksByPrefix(ctx, "Music/")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.DeleteTracksByPrefix(ctx, "Music/")
	if err != nil {
		t.Fatal(err)
	}
	if int64(shown) != deleted {
		t.Errorf("count shown to the operator = %d but delete took %d rows — "+
			"the pre-confirm count and the delete must select the same set", shown, deleted)
	}
	for _, p := range []string{"music/keep1.flac", "music/keep2.flac"} {
		if !trackExists(t, s, ctx, p) {
			t.Errorf("%q was deleted by removing the case-twin root %q — "+
				"on a case-sensitive filesystem those are different directories", p, "Music")
		}
	}
	for _, p := range []string{"Music/a.flac", "Music/b.flac"} {
		if trackExists(t, s, ctx, p) {
			t.Errorf("%q survived its own root's removal", p)
		}
	}
}

// TestDeleteTracksByPrefixRefusesEmptyPrefix pins the one place where
// the range form's natural reading ("empty base means everything") would
// be catastrophic. The LIKE form matched nothing for a slash-only
// prefix; whole-library removal belongs to WipeAllTracks /
// WipeFilesystemTracks, so this must never become a wipe.
func TestDeleteTracksByPrefixRefusesEmptyPrefix(t *testing.T) {
	s, ctx := prefixCaseStore(t)
	seedPaths(t, s, ctx, "Music/a.flac", "Other/b.flac")

	for _, prefix := range []string{"", "/", "//"} {
		n, err := s.DeleteTracksByPrefix(ctx, prefix)
		if err == nil {
			t.Errorf("DeleteTracksByPrefix(%q) returned nil error (deleted %d) — "+
				"an unscoped prefix must be refused, never treated as a whole-library wipe", prefix, n)
		}
		if n != 0 {
			t.Errorf("DeleteTracksByPrefix(%q) reported %d rows deleted", prefix, n)
		}
	}
	for _, p := range []string{"Music/a.flac", "Other/b.flac"} {
		if !trackExists(t, s, ctx, p) {
			t.Fatalf("%q was deleted by an unscoped prefix — this is the whole-library-wipe failure mode", p)
		}
	}
}

// TestTrackPathsUnderIsCaseExact is the ScanSubtree half.
//
// TrackPathsUnder is the SCOPE SNAPSHOT for the watcher's bounded
// deletion pass. When the LIKE pulled a case-twin sibling's rows into
// that snapshot they were absent from `seen` (the walk never visits that
// directory), and caseOnlyRenames then fold-matched them to a path that
// WAS seen — so the pass reaped them outright, bypassing the
// missing_count debounce. See TestScanSubtreeSparesCaseTwinDirectory for
// the end-to-end consequence.
func TestTrackPathsUnderIsCaseExact(t *testing.T) {
	s, ctx := prefixCaseStore(t)
	seedPaths(t, s, ctx,
		"Artist/Album/01.flac",
		"Artist/album/01.flac",
		"Artist/Album Extra/01.flac", // shares a prefix but is a different dir
	)

	got, err := s.TrackPathsUnder(ctx, "Artist/Album")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Artist/Album/01.flac"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("TrackPathsUnder(%q) = %v, want %v — the scope must not reach a "+
			"case-twin sibling the walk never visits, nor a longer-named sibling",
			"Artist/Album", got, want)
	}
}

// TestFolderPathsUnderKeepsTheSelfRow is the regression guard for the
// conversion itself.
//
// The descendant half became a byte range, but folder rows are unlike
// track rows: the directory's OWN row exists (the scanner upserts it),
// and in multi-root mode so does the "<base>/." whole-root sentinel. The
// `path = ?` term is the only thing that matches either. Drop it while
// rewriting the query — the obvious simplification, since the range
// looks like it should cover everything — and folder rows silently stop
// being reaped on rename.
func TestFolderPathsUnderKeepsTheSelfRow(t *testing.T) {
	s, ctx := prefixCaseStore(t)
	now := time.Now()
	for _, p := range []string{
		"Artist", "Artist/Album", "Artist/Album/Disc 1",
		// The case-twin sibling AND a descendant of it. The descendant is
		// the one that matters: the pre-fix pattern was "Artist/Album/%",
		// which the folded LIKE matches against "Artist/album/Disc 1" but
		// NOT against the bare "Artist/album" (nothing follows the slash).
		// Seeding only the bare row makes this assertion vacuous.
		"Artist/album", "Artist/album/Disc 1",
		"Root/.", "Root/Sub",
	} {
		if err := s.UpsertFolder(ctx, &Folder{Path: p, ModTime: now}); err != nil {
			t.Fatalf("seed folder %q: %v", p, err)
		}
	}

	got, err := s.FolderPathsUnder(ctx, "Artist/Album")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, "Artist/Album") {
		t.Errorf("FolderPathsUnder(%q) = %v — MISSING the directory's own row; "+
			"the `path = ?` term is load-bearing and must survive any rewrite", "Artist/Album", got)
	}
	if !contains(got, "Artist/Album/Disc 1") {
		t.Errorf("FolderPathsUnder(%q) = %v — missing a descendant", "Artist/Album", got)
	}
	for _, twin := range []string{"Artist/album", "Artist/album/Disc 1"} {
		if contains(got, twin) {
			t.Errorf("FolderPathsUnder(%q) = %v — reached the case-twin sibling %q", "Artist/Album", got, twin)
		}
	}

	// Multi-root whole-root sentinel: relPath(root, root, true) stores
	// "<base>/.", so scoping to it must return that row too.
	got, err = s.FolderPathsUnder(ctx, "Root/.")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, "Root/.") {
		t.Errorf("FolderPathsUnder(%q) = %v — MISSING the whole-root sentinel row", "Root/.", got)
	}
	if !contains(got, "Root/Sub") {
		t.Errorf("FolderPathsUnder(%q) = %v — missing a descendant of the root", "Root/.", got)
	}
}

// TestTrackScopeBaseHandlesDirsEndingInDot pins the one trap in the
// shared base derivation: the multi-root sentinel is "<base>/.", but a
// real directory may legitimately END in a dot. Testing for "." alone
// would trim "Artist/Album." to "Artist/Album" and silently widen the
// scope to a DIFFERENT directory's contents.
func TestTrackScopeBaseHandlesDirsEndingInDot(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Artist/Album", "Artist/Album"},
		{"Artist/Album/", "Artist/Album"}, // caller's trailing slash
		{"Artist/Album//", "Artist/Album"},
		{"Root/.", "Root"},                 // multi-root whole-root sentinel
		{"Artist/Album.", "Artist/Album."}, // real dir ending in a dot — NOT a sentinel
		{"Artist/...", "Artist/..."},
	}
	for _, c := range cases {
		if got := trackScopeBase(c.in); got != c.want {
			t.Errorf("trackScopeBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResetEnrichedMissesUnderPrefixIsCaseExact covers the write in the
// library_meta subtree family. It is one of the few sanctioned
// `enriched_at` writers, and its whole justification is that it is
// tightly scoped — a predicate that silently covers a folder the
// operator did not select sends real MusicBrainz / Cover Art / Deezer
// traffic for it.
func TestResetEnrichedMissesUnderPrefixIsCaseExact(t *testing.T) {
	s, ctx := prefixCaseStore(t)
	// Rows that "miss" (no artwork/artist/release MBID) and are already
	// stamped enriched, so the retry predicate selects them.
	seedPaths(t, s, ctx, "Jazz/a.flac", "jazz/b.flac")
	for _, p := range []string{"Jazz/a.flac", "jazz/b.flac"} {
		if err := s.MarkEnriched(ctx, &Track{Path: p, Size: 100, ModTime: time.Now()}); err != nil {
			t.Fatalf("mark enriched %q: %v", p, err)
		}
	}

	n, err := s.ResetEnrichedMissesUnderPrefix(ctx, "Jazz")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("ResetEnrichedMissesUnderPrefix(%q) re-queued %d rows, want 1 — "+
			"the case-twin folder %q must not be re-enriched", "Jazz", n, "jazz")
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func variantPaths(rows []VariantRow) []string {
	out := make([]string, 0, len(rows))
	for _, v := range rows {
		out = append(out, v.SourcePath)
	}
	return out
}

// TestListVariantsByPathPrefixIsCaseExact is the destructive case, and
// the one this whole file's subject had never been applied to.
//
// Both queries behind DELETE /v1/upscale/variants selected with
// unicode_lower, and RunVariantDelete feeds every returned row to
// os.Remove and then DeleteVariant. So `?prefix=Jazz` also reaped the
// renditions of `jazz/` and `JAZZ/` — different real directories on a
// case-sensitive filesystem, which is Linux, so the VPS, the tenants and
// the Docker image.
//
// Runs on any platform: the rows go in directly and nothing touches the
// filesystem, unlike a case-twin DIRECTORY fixture, which would skip on
// the dev Mac.
func TestListVariantsByPathPrefixIsCaseExact(t *testing.T) {
	s, ctx := prefixCaseStore(t)
	seedPaths(t, s, ctx, "Jazz/a.flac", "jazz/b.flac", "JAZZ/c.flac", "Jazzy/d.flac")
	seedVariant(t, s, "Jazz/a.flac", "optimized-v1-44100-16")
	seedVariant(t, s, "jazz/b.flac", "optimized-v1-44100-16")
	seedVariant(t, s, "JAZZ/c.flac", "optimized-v1-44100-16")
	seedVariant(t, s, "Jazzy/d.flac", "optimized-v1-44100-16")

	got, err := s.ListVariantsByPathPrefix(ctx, "Jazz")
	if err != nil {
		t.Fatalf("ListVariantsByPathPrefix: %v", err)
	}
	paths := variantPaths(got)
	if len(paths) != 1 || paths[0] != "Jazz/a.flac" {
		t.Errorf("prefix Jazz selected %v, want exactly [Jazz/a.flac] — a case twin or a name-prefix sibling would be unlinked", paths)
	}
}

// TestListVariantsForPathIsCaseExact is the same defect on the
// exact-path shape, which the admin console drives once per track when
// it expands an artist into req.Paths.
//
// PROTOCOL.md documents `?path=<rel>` as "the variants of one exact
// source track". The spec was right and the code did not match it, which
// is why this needs no spec change and no Mirror-PR.
func TestListVariantsForPathIsCaseExact(t *testing.T) {
	s, ctx := prefixCaseStore(t)
	seedPaths(t, s, ctx, "Album/Track.flac", "album/track.flac")
	seedVariant(t, s, "Album/Track.flac", "optimized-v1-44100-16")
	seedVariant(t, s, "album/track.flac", "optimized-v1-44100-16")

	got, err := s.ListVariantsForPath(ctx, "Album/Track.flac")
	if err != nil {
		t.Fatalf("ListVariantsForPath: %v", err)
	}
	paths := variantPaths(got)
	if len(paths) != 1 || paths[0] != "Album/Track.flac" {
		t.Errorf("exact path selected %v, want exactly [Album/Track.flac] — the case twin's sidecar would be unlinked", paths)
	}
}

// TestListVariantsStillMatchesAcrossUnicodeNormalisation is the positive
// control, and it is the reason the SQL still folds rather than going
// byte-exact.
//
// unicode_lower does TWO jobs — case folding and NFC composition. The
// composition is load-bearing: the scanner stores the on-disk form,
// which is NFD for anything from HFS+ or synced from a Linux/NAS, while
// iOS sends NFC. A byte-exact query would answer deletedCount: 0 for
// every album with an accent in its path, silently. Acceptance adds
// case-sensitivity and nothing else.
func TestListVariantsStillMatchesAcrossUnicodeNormalisation(t *testing.T) {
	const (
		nfd = "Björk/Vespertine.flac" // o + combining diaeresis
		nfc = "Björk/Vespertine.flac"  // precomposed ö
	)
	s, ctx := prefixCaseStore(t)
	seedPaths(t, s, ctx, nfd)
	seedVariant(t, s, nfd, "optimized-v1-44100-16")

	// The client sends NFC; the row is stored NFD.
	got, err := s.ListVariantsForPath(ctx, nfc)
	if err != nil {
		t.Fatalf("ListVariantsForPath: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("NFC request matched %d rows for an NFD-stored path, want 1 — going byte-exact would silently delete nothing for every accented album", len(got))
	}

	prefixed, err := s.ListVariantsByPathPrefix(ctx, "Björk")
	if err != nil {
		t.Fatalf("ListVariantsByPathPrefix: %v", err)
	}
	if len(prefixed) != 1 {
		t.Errorf("NFC prefix matched %d rows for an NFD-stored path, want 1", len(prefixed))
	}
}

// TestListVariantsByPathPrefixStillAnswersEverythingUnscoped pins the
// delete-all shape the handler gates behind ?confirm=true. Narrowing
// must not have turned an unscoped request into an empty one.
func TestListVariantsByPathPrefixStillAnswersEverythingUnscoped(t *testing.T) {
	s, ctx := prefixCaseStore(t)
	seedPaths(t, s, ctx, "Jazz/a.flac", "Rock/b.flac")
	seedVariant(t, s, "Jazz/a.flac", "optimized-v1-44100-16")
	seedVariant(t, s, "Rock/b.flac", "optimized-v1-44100-16")

	got, err := s.ListVariantsByPathPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListVariantsByPathPrefix(\"\"): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("unscoped prefix selected %d rows, want 2 (every row)", len(got))
	}
}
