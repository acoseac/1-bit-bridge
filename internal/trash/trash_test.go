package trash

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

func newTestManager(t *testing.T, opts ...Option) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	on := true
	// The REAL resolver, not a stub: the bug this fixture missed for the
	// life of the package was in how a stored path maps onto a root, and a
	// hand-written splitter here would have agreed with the hand-written
	// mapping that was wrong.
	return New(func() []string { return []string{root} }, bridgefs.New([]string{root}),
		func() bool { return on }, DefaultTTL, opts...), root
}

// newTwoRootManager is the fixture this package never had. Every test above
// uses one root, which is exactly the shape in which the multi-root bug is
// invisible: with a single root a stored path carries no root basename, so
// joining it onto roots[0] is correct by accident.
func newTwoRootManager(t *testing.T, opts ...Option) (m *Manager, first, second string) {
	t.Helper()
	// Named, not t.TempDir() twice: the stored-path form is
	// "<root basename>/…", so the basenames are load-bearing input.
	base := t.TempDir()
	first = filepath.Join(base, "Music")
	second = filepath.Join(base, "NAS")
	for _, d := range []string{first, second} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	roots := []string{first, second}
	return New(func() []string { return roots }, bridgefs.New(roots),
		func() bool { return true }, DefaultTTL, opts...), first, second
}

func seed(t *testing.T, root, rel, body string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTrashMovesRatherThanUnlinks is the invariant this whole package exists to
// preserve in a weaker form: the bridge has never removed a file from a library
// root, and a delete that reaches this code is recoverable rather than final.
func TestTrashMovesRatherThanUnlinks(t *testing.T) {
	m, root := newTestManager(t)
	src := seed(t, root, "Artist/Album/01.flac", "audio")

	res, err := m.Trash("", []string{"Artist/Album/01.flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 1 || res.Failed != 0 {
		t.Fatalf("trash result = %+v", res)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("the original path still exists")
	}
	entries, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("trash holds %d entries, want 1", len(entries))
	}
	if entries[0].OriginalPath != "Artist/Album/01.flac" {
		t.Errorf("OriginalPath = %q", entries[0].OriginalPath)
	}
	if entries[0].Size != 5 {
		t.Errorf("Size = %d, want 5", entries[0].Size)
	}
	// The bytes are still on disk — trash does NOT free space, which is why
	// the space widget reports reclaimable separately.
	if got := m.Reclaimable(root); got != 5 {
		t.Errorf("Reclaimable = %d, want 5", got)
	}
}

// TestTrashTTLReadsStampDirNotFileMtime is the sharpest failure this design
// avoids.
//
// os.Rename PRESERVES mtime — measured, not assumed. A file stamped years ago
// and trashed just now reads as years old the instant it lands, so an
// mtime-driven sweeper purges it on the very next tick — and does so
// oldest-content-first, destroying the recovery window for precisely the
// material most likely to be irreplaceable.
func TestTrashTTLReadsStampDirNotFileMtime(t *testing.T) {
	now := time.Now()
	m, root := newTestManager(t, WithClock(func() time.Time { return now }))

	p := seed(t, root, "Old/ancient.flac", "audio")
	ancient := now.Add(-5 * 365 * 24 * time.Hour)
	if err := os.Chtimes(p, ancient, ancient); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Trash("", []string{"Old/ancient.flac"}); err != nil {
		t.Fatal(err)
	}

	// Confirm the fixture actually reproduces the trap: the trashed file must
	// still carry its ancient mtime, or this test proves nothing.
	entries, _ := m.List()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	staged := filepath.Join(root, DirName)
	var found string
	_ = filepath.WalkDir(staged, func(q string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = q
		}
		return nil
	})
	if found == "" {
		t.Fatal("could not locate the trashed file")
	}
	info, err := os.Stat(found)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().After(now.Add(-365 * 24 * time.Hour)) {
		t.Fatalf("FIXTURE INVALID: the rename did not preserve mtime (%v), so this test cannot detect an mtime-driven sweeper", info.ModTime())
	}

	purged, _, err := m.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Fatalf("a file trashed seconds ago was purged (%d) — the sweeper is reading the file's mtime, not the stamp directory", purged)
	}
	if got, _ := m.List(); len(got) != 1 {
		t.Fatal("the entry vanished from the trash listing")
	}
}

func TestSweepPurgesPastTheTTL(t *testing.T) {
	now := time.Now()
	clock := now
	m, root := newTestManager(t, WithClock(func() time.Time { return clock }))
	seed(t, root, "A/x.flac", "audio")
	if _, err := m.Trash("", []string{"A/x.flac"}); err != nil {
		t.Fatal(err)
	}
	// Nothing expires inside the window.
	clock = now.Add(DefaultTTL - time.Hour)
	if purged, _, _ := m.Sweep(); purged != 0 {
		t.Fatalf("purged %d inside the TTL window", purged)
	}
	// Past it, the whole stamp goes.
	clock = now.Add(DefaultTTL + time.Hour)
	purged, freed, err := m.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if purged != 1 || freed != 5 {
		t.Fatalf("purged=%d freed=%d, want 1 and 5", purged, freed)
	}
	if got, _ := m.List(); len(got) != 0 {
		t.Errorf("trash still holds %d entries", len(got))
	}
}

// TestRestoreRecreatesMissingParentDirs — trashing an album's last track leaves
// the directory empty, and something (the operator, a tidy-up) may remove it. A
// bare rename back then ENOENTs on exactly the case restore exists for.
func TestRestoreRecreatesMissingParentDirs(t *testing.T) {
	m, root := newTestManager(t)
	seed(t, root, "Artist/Album/01.flac", "audio")
	if _, err := m.Trash("", []string{"Artist/Album/01.flac"}); err != nil {
		t.Fatal(err)
	}
	// The album directory is now empty — remove it, as a tidy-up would.
	if err := os.RemoveAll(filepath.Join(root, "Artist", "Album")); err != nil {
		t.Fatal(err)
	}
	entries, _ := m.List()
	res, err := m.Restore([]string{entries[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 1 {
		t.Fatalf("restore failed: %+v", res.Outcomes)
	}
	if _, err := os.Stat(filepath.Join(root, "Artist", "Album", "01.flac")); err != nil {
		t.Errorf("restored file missing: %v", err)
	}
}

func TestRestoreRefusesToClobberAnExistingFile(t *testing.T) {
	m, root := newTestManager(t)
	seed(t, root, "A/x.flac", "original")
	if _, err := m.Trash("", []string{"A/x.flac"}); err != nil {
		t.Fatal(err)
	}
	seed(t, root, "A/x.flac", "something else")
	entries, _ := m.List()
	res, err := m.Restore([]string{entries[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("restore clobbered an existing file: %+v", res.Outcomes)
	}
	got, _ := os.ReadFile(filepath.Join(root, "A", "x.flac"))
	if string(got) != "something else" {
		t.Errorf("existing file was overwritten: %q", got)
	}
}

func TestPurgeReclaimsAndReportsBytes(t *testing.T) {
	m, root := newTestManager(t)
	seed(t, root, "A/x.flac", "1234567890")
	if _, err := m.Trash("", []string{"A/x.flac"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := m.List()
	res, err := m.Purge([]string{entries[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 1 || res.Bytes != 10 {
		t.Fatalf("purge = %+v, want 1 entry / 10 bytes", res)
	}
	if got := m.Reclaimable(root); got != 0 {
		t.Errorf("Reclaimable = %d after a full purge", got)
	}
}

func TestPurgeWithNoIDsEmptiesEverything(t *testing.T) {
	m, root := newTestManager(t)
	for _, p := range []string{"A/1.flac", "A/2.flac", "B/3.flac"} {
		seed(t, root, p, "xx")
	}
	if _, err := m.Trash("", []string{"A/1.flac", "A/2.flac", "B/3.flac"}); err != nil {
		t.Fatal(err)
	}
	res, err := m.Purge(nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 3 {
		t.Fatalf("empty-trash purged %d of 3", res.OK)
	}
	if got, _ := m.List(); len(got) != 0 {
		t.Errorf("%d entries survived an empty-trash", len(got))
	}
}

// TestTrashRejectsTraversalAndDotSegments — dot segments in particular, so the
// staging and trash directories can never themselves be targets: deleting the
// trash through the delete endpoint would be a loop, and deleting staging would
// race a live upload.
func TestTrashRejectsTraversalAndDotSegments(t *testing.T) {
	m, root := newTestManager(t)
	seed(t, root, "A/x.flac", "audio")
	outside := filepath.Join(filepath.Dir(root), "outside.flac")
	if err := os.WriteFile(outside, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The dot-directory cases must EXIST on disk, or os.Stat refuses them
	// first and the dot-segment guard is never exercised — the test would
	// pass for the wrong reason. Verified by mutation: with these absent,
	// deleting the guard left this test green.
	stagedPart := filepath.Join(root, ".bridge-upload", "sid", "x.part")
	trashedFile := filepath.Join(root, DirName, "1", "A", "x.flac")
	for _, q := range []string{stagedPart, trashedFile} {
		if err := os.MkdirAll(filepath.Dir(q), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(q, []byte("in-flight"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, bad := range []string{
		"../outside.flac",
		"A/../../outside.flac",
		"/etc/passwd",
		".bridge-upload/sid/x.part",
		".bridge-trash/1/A/x.flac",
		"A/./x.flac",
		`A\x.flac`,
		"A/\x00x.flac",
		"",
	} {
		res, err := m.Trash("", []string{bad})
		if err != nil {
			continue
		}
		if res.Failed != 1 {
			t.Errorf("Trash(%q) was not refused: %+v", bad, res.Outcomes)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a file OUTSIDE the library root was removed")
	}
	if _, err := os.Stat(filepath.Join(root, "A", "x.flac")); err != nil {
		t.Error("an unrelated in-root file was removed")
	}
	// Deleting the trash through the delete endpoint would be a loop, and
	// deleting staging would race a live upload.
	if _, err := os.Stat(stagedPart); err != nil {
		t.Error("an in-flight upload's staged file was trashed")
	}
	if _, err := os.Stat(trashedFile); err != nil {
		t.Error("an already-trashed file was re-trashed")
	}
}

func TestTrashRefusesDirectories(t *testing.T) {
	m, root := newTestManager(t)
	seed(t, root, "A/x.flac", "audio")
	res, err := m.Trash("", []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("a directory was trashed: %+v", res.Outcomes)
	}
	if _, err := os.Stat(filepath.Join(root, "A", "x.flac")); err != nil {
		t.Error("trashing a directory took its contents with it")
	}
}

// TestTrashRefusedWhenDisabled — the gate is read LIVE and defaults closed. A
// nil predicate means disabled, which is the safe direction for a destructive
// feature.
func TestTrashRefusedWhenDisabled(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "A/x.flac", "audio")
	roots := func() []string { return []string{root} }

	off := New(roots, bridgefs.New([]string{root}), func() bool { return false }, DefaultTTL)
	if _, err := off.Trash("", []string{"A/x.flac"}); !errors.Is(err, ErrDisabled) {
		t.Errorf("Trash with the gate off = %v, want ErrDisabled", err)
	}
	if _, err := off.Purge(nil); !errors.Is(err, ErrDisabled) {
		t.Errorf("Purge with the gate off = %v, want ErrDisabled", err)
	}
	if _, err := off.Restore([]string{"1/A/x.flac"}); !errors.Is(err, ErrDisabled) {
		t.Errorf("Restore with the gate off = %v, want ErrDisabled", err)
	}
	nilGate := New(roots, bridgefs.New([]string{root}), nil, DefaultTTL)
	if _, err := nilGate.Trash("", []string{"A/x.flac"}); !errors.Is(err, ErrDisabled) {
		t.Errorf("Trash with a nil gate = %v, want ErrDisabled — an unwired gate must fail CLOSED", err)
	}
	// The splitter fails closed the same way, and for the same reason: with
	// no mapping there is no safe guess about which root a path belongs to,
	// and guessing is the whole defect.
	nilSplit := New(roots, nil, func() bool { return true }, DefaultTTL)
	if _, err := nilSplit.Trash("", []string{"A/x.flac"}); !errors.Is(err, ErrRootUnavailable) {
		t.Errorf("Trash with a nil splitter = %v, want ErrRootUnavailable", err)
	}
	if _, err := nilSplit.Restore([]string{"1/A/x.flac"}); !errors.Is(err, ErrRootUnavailable) {
		t.Errorf("Restore with a nil splitter = %v, want ErrRootUnavailable", err)
	}
	if _, err := os.Stat(filepath.Join(root, "A", "x.flac")); err != nil {
		t.Error("a file was trashed while the feature was off")
	}
}

// TestSweepLeavesUnrecognisedDirectoriesAlone — a directory whose name is not a
// stamp has no knowable age, and guessing deletes user content.
func TestSweepLeavesUnrecognisedDirectoriesAlone(t *testing.T) {
	now := time.Now()
	m, root := newTestManager(t, WithClock(func() time.Time { return now.Add(10 * 365 * 24 * time.Hour) }))
	odd := filepath.Join(root, DirName, "not-a-stamp")
	if err := os.MkdirAll(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(odd, "mystery.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(odd, "mystery.flac")); err != nil {
		t.Error("an unrecognised trash directory was purged on a guess")
	}
}

func TestSplitIDRejectsMalformedInput(t *testing.T) {
	for _, bad := range []string{
		"", "nostamp", "abc/A/x.flac", "-1/A/x.flac", "123", "123/../x.flac",
		"123/.hidden/x.flac", "123/",
	} {
		if _, _, err := splitID(bad); err == nil {
			t.Errorf("splitID(%q) was accepted", bad)
		}
	}
	stamp, rel, err := splitID("1700000000000000000/Artist/Album/01.flac")
	if err != nil || stamp != "1700000000000000000" || rel != "Artist/Album/01.flac" {
		t.Errorf("well-formed id: stamp=%q rel=%q err=%v", stamp, rel, err)
	}
}

// TestTrashedFileIsNotVisibleToAScanWalk — the dot-directory is what keeps
// trashed content out of the manifest. Asserted here on the NAME rule the
// scanner applies (shouldSkipDir returns true for any "."-prefixed name), so a
// rename of DirName that dropped the dot fails loudly.
func TestTrashedFileIsNotVisibleToAScanWalk(t *testing.T) {
	if !strings.HasPrefix(DirName, ".") {
		t.Fatalf("DirName = %q — without a leading dot the scanner walks the trash "+
			"and re-indexes every deleted file", DirName)
	}
}

// TestReclaimableIsCachedButInvalidatedByEveryMutation — the sidebar asks for
// this on EVERY page load and the walk is a stat per trashed file, so it is
// cached. A stale answer here is worse than a slow one though: it would tell an
// operator there is nothing to reclaim at the moment they most need to know
// there is. Every path that changes the trash drops the cache.
func TestReclaimableIsCachedButInvalidatedByEveryMutation(t *testing.T) {
	now := time.Now()
	m, root := newTestManager(t, WithClock(func() time.Time { return now }))
	seed(t, root, "A/x.flac", "0123456789")
	if _, err := m.Trash("", []string{"A/x.flac"}); err != nil {
		t.Fatal(err)
	}
	if got := m.Reclaimable(root); got != 10 {
		t.Fatalf("Reclaimable after trashing = %d, want 10 — the trash mutation did not invalidate", got)
	}

	// A second trash inside the TTL window must still be reflected.
	seed(t, root, "A/y.flac", "12345")
	if _, err := m.Trash("", []string{"A/y.flac"}); err != nil {
		t.Fatal(err)
	}
	if got := m.Reclaimable(root); got != 15 {
		t.Fatalf("Reclaimable = %d after a second trash inside the TTL, want 15 — a stale cache", got)
	}

	// Restoring reduces it.
	entries, _ := m.List()
	if _, err := m.Restore([]string{entries[0].ID}); err != nil {
		t.Fatal(err)
	}
	if got := m.Reclaimable(root); got == 15 {
		t.Fatal("Reclaimable unchanged after a restore — the cache survived a mutation")
	}

	// And purging takes it to zero.
	if _, err := m.Purge(nil); err != nil {
		t.Fatal(err)
	}
	if got := m.Reclaimable(root); got != 0 {
		t.Fatalf("Reclaimable = %d after emptying the trash, want 0", got)
	}
}

// --- Multi-root ---
//
// A stored path on a multi-root bridge leads with the root's basename
// (manifest's `relPath`), and this package used to join that whole string onto
// roots[0]. Ordinarily the result does not exist and every delete fails; where
// the two roots' names overlap it moves a real, unrelated file — and the caller
// then retires the manifest row of the file still sitting on disk.
//
// Every other test in this file uses one root, which is the one shape in which
// the bug cannot appear.

// TestTrashOnMultiRootUsesThePathsOwnRoot is the regression test. It fails
// against the pre-fix code with "no such file or directory".
func TestTrashOnMultiRootUsesThePathsOwnRoot(t *testing.T) {
	m, first, second := newTwoRootManager(t)
	// The file the operator selected: it lives under the SECOND root, and its
	// stored path says so.
	want := seed(t, second, "Miles Davis/Kind of Blue/01.flac", "second")
	// The decoy: the exact path the pre-fix join addressed, under the FIRST
	// root. Overlapping directory names are why people have two roots.
	decoy := seed(t, first, "NAS/Miles Davis/Kind of Blue/01.flac", "first")

	res, err := m.Trash("", []string{"NAS/Miles Davis/Kind of Blue/01.flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 1 || res.Failed != 0 {
		t.Fatalf("ok=%d failed=%d, want 1/0 — outcomes: %+v", res.OK, res.Failed, res.Outcomes)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Error("the selected file is still on disk; the delete went somewhere else")
	}
	if body, err := os.ReadFile(decoy); err != nil || string(body) != "first" {
		t.Errorf("the file under the OTHER root was touched (err=%v) — this is the "+
			"case where the wrong-root join trashes real content", err)
	}
	// The trash lives inside the root that owned the file, so the move is a
	// same-filesystem rename rather than a cross-device copy.
	if under := filepath.Join(second, DirName); !strings.HasPrefix(mustSoleTrashed(t, m).Root, second) {
		t.Errorf("entry filed under %q, want a root at %q", mustSoleTrashed(t, m).Root, under)
	}
}

// mustSoleTrashed returns the single trashed entry, failing if there is not
// exactly one — a count assertion the callers would otherwise each repeat.
func mustSoleTrashed(t *testing.T, m *Manager) Entry {
	t.Helper()
	entries, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("List() = %d entries, want 1", len(entries))
	}
	return entries[0]
}

// TestRestoreOnMultiRootReturnsThePathToItsOwnRoot pins the other half. The
// pre-fix restore joined the stored path whole onto the root it found the
// trashed file under, writing to <root>/<rootBasename>/… — a directory the
// scanner would then index as a new artist.
func TestRestoreOnMultiRootReturnsThePathToItsOwnRoot(t *testing.T) {
	m, first, second := newTwoRootManager(t)
	rel := "NAS/Miles Davis/Kind of Blue/01.flac"
	orig := seed(t, second, "Miles Davis/Kind of Blue/01.flac", "second")

	if res, err := m.Trash("", []string{rel}); err != nil || res.OK != 1 {
		t.Fatalf("trash: %v (ok=%d)", err, res.OK)
	}
	res, err := m.Restore([]string{mustSoleTrashed(t, m).ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 1 {
		t.Fatalf("restore ok=%d failed=%d: %+v", res.OK, res.Failed, res.Outcomes)
	}
	if body, err := os.ReadFile(orig); err != nil || string(body) != "second" {
		t.Errorf("the file did not come back to where it was (err=%v)", err)
	}
	// The shape the pre-fix code produced: the root basename repeated inside
	// the root. Asserted explicitly because it is silent — the file exists,
	// just in a directory nobody asked for.
	if _, err := os.Stat(filepath.Join(second, "NAS")); !os.IsNotExist(err) {
		t.Error("restore created <root>/NAS/… — the stored path's root segment was not consumed")
	}
	if _, err := os.Stat(filepath.Join(first, "NAS", "Miles Davis")); !os.IsNotExist(err) {
		t.Error("restore wrote into the wrong root")
	}
}

// TestTrashRefusesAPathNamingAnUnconfiguredRoot — a root removed from the
// config, or renamed on disk, between two operations. The answer is a refusal,
// never a fallback: falling back to some other root IS the bug.
func TestTrashRefusesAPathNamingAnUnconfiguredRoot(t *testing.T) {
	m, first, _ := newTwoRootManager(t)
	decoy := seed(t, first, "Gone/Artist/01.flac", "first")

	res, err := m.Trash("", []string{"Gone/Artist/01.flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK != 0 || res.Failed != 1 {
		t.Fatalf("ok=%d failed=%d, want 0/1", res.OK, res.Failed)
	}
	if got := res.Outcomes[0].Reason; !strings.Contains(got, "not configured") {
		t.Errorf("reason = %q, want it to name the missing root", got)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Error("a file was trashed for a path naming a root that does not exist")
	}
}

// TestTrashRootWantConstrainsRatherThanSelects — a caller naming a root is
// saying "refuse if it is not this one". The old resolveRoot answered "which
// root?" and returned roots[0] for the empty request, which is how a second
// root's path came to be addressed under the first.
func TestTrashRootWantConstrainsRatherThanSelects(t *testing.T) {
	m, first, second := newTwoRootManager(t)
	target := seed(t, second, "Album/01.flac", "second")

	res, err := m.Trash(filepath.Base(first), []string{"NAS/Album/01.flac"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("failed=%d, want 1 — a path outside the named root must be refused", res.Failed)
	}
	if _, err := os.Stat(target); err != nil {
		t.Error("the file was trashed despite naming a different root")
	}
	// NEGATIVE CONTROL: the same path with the RIGHT root goes through, so
	// the assertion above is about the constraint and not about the path.
	if res, err := m.Trash(filepath.Base(second), []string{"NAS/Album/01.flac"}); err != nil || res.OK != 1 {
		t.Fatalf("naming the owning root: err=%v ok=%d, want 1", err, res.OK)
	}
}

// TestRestoreRefusesAnOccupiedOriginOnTheSecondRoot — the origin check already
// existed; this pins that it still fires once `dst` is computed from the
// path's own root rather than roots[0], where it would be checking a path in
// the wrong library.
func TestRestoreRefusesAnOccupiedOriginOnTheSecondRoot(t *testing.T) {
	m, _, second := newTwoRootManager(t)
	rel := "NAS/Album/01.flac"
	seed(t, second, "Album/01.flac", "original")

	if res, err := m.Trash("", []string{rel}); err != nil || res.OK != 1 {
		t.Fatalf("trash: %v", err)
	}
	// Re-ripped while the old copy sat in the trash.
	seed(t, second, "Album/01.flac", "re-ripped")

	res, err := m.Restore([]string{mustSoleTrashed(t, m).ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("failed=%d, want 1 — restore must not overwrite an occupied origin", res.Failed)
	}
	if body, _ := os.ReadFile(filepath.Join(second, "Album", "01.flac")); string(body) != "re-ripped" {
		t.Error("restore overwrote the file at the original path")
	}
}
