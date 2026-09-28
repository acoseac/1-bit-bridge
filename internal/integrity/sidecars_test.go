package integrity

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// sweepPercent is the mass-orphan threshold the sweeper tests run under:
// the shipped default, taken from config rather than retyped, so a change
// to it re-asks these tests at the value bridges actually run (the #940
// property test's rule).
const sweepPercent = config.DefaultVariantSweepMaxDeletePercent

// fakeSidecarLister is the test-side SidecarLister implementation.
// Returns a snapshot of the configured `known` set; thread-safe
// via the mutex so a test that mutates the snapshot mid-tick (to
// model concurrent UpsertVariant writes) doesn't trip the race
// detector.
type fakeSidecarLister struct {
	mu    sync.Mutex
	known map[string]struct{}
	rows  []VariantSnapshot
}

// withLiveRow returns a known-set carrying one real sidecar path.
//
// An EMPTY known-set is now a refusal, because it is the state in which every
// file on disk reads as an orphan — so a fixture that used one was describing
// the very bridge the sweeper must not act on, while asserting that it acts.
// One live row is also simply more honest: a library with orphans has variants.
// The path names no file on disk, deliberately: a `track_variants` row whose
// sidecar is gone is a real state (it is what the REVERSE sweeper exists for),
// and seeding an actual file would add an entry to the walk and perturb the
// counts the caller is measuring.
func withLiveRow(outputDir string) map[string]struct{} {
	return map[string]struct{}{
		filepath.Join(outputDir, "live-row.upscaled-v1-96000-24.flac"): {},
	}
}

// AllVariants projects the fixture's path set as rows with no source
// identity, so the sweep's known set is exactly these paths — the
// pre-relocation contract every test here was written against. A test
// that needs the CANONICAL spelling in the known set seeds `rows`
// instead (TestOrphanSweeperKnowsARelocatedCatalogsCanonicalPaths).
func (f *fakeSidecarLister) AllVariants(ctx context.Context) ([]VariantSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]VariantSnapshot, 0, len(f.known)+len(f.rows))
	for k := range f.known {
		out = append(out, VariantSnapshot{SidecarPath: k})
	}
	out = append(out, f.rows...)
	return out, nil
}

// pathSet is a known set naming each of paths, for fakeSidecarLister.
func pathSet(paths ...[]string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, group := range paths {
		for _, p := range group {
			out[p] = struct{}{}
		}
	}
	return out
}

// seedTestSidecarTree writes `n` `.flac` files into outputDir, named
// `<prefix><i>.flac` with `i` left-padded to 4 digits for lexical
// ordering predictability. Returns the absolute paths in lexical
// order. Test helper, not a sweeper concern.
func seedTestSidecarTree(t *testing.T, outputDir, prefix string, n int) []string {
	t.Helper()
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		// Pad to 4 digits so up to 10,000 entries sort lexically.
		name := prefix
		istr := strconv.Itoa(i)
		for len(istr) < 4 {
			istr = "0" + istr
		}
		name += istr + ".flac"
		full := filepath.Join(outputDir, name)
		if err := os.WriteFile(full, []byte{0}, 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
		paths[i] = full
	}
	sort.Strings(paths)
	return paths
}

// ageFixtures back-dates every file under dir so the sweeper's
// grace-period check sees them as settled.
//
// The tests used `gracePeriodForTest = 1ns` to mean "nothing is too
// fresh to sweep", which relies on `tickStart.Sub(info.ModTime())` being
// positive for a file written moments earlier. That does not hold on
// Windows: NTFS stamps come from the system clock, whose granularity is
// ~15ms, while Go's time.Now() reads a high-resolution source — so a
// just-written file can carry a stamp AHEAD of a tickStart sampled
// afterwards. The difference goes negative, every candidate looks
// too-fresh, and the sweeper correctly skips all of them.
//
// Back-dating states the precondition directly instead of inferring it
// from a clock race, which is both portable and a better description of
// what these tests mean: a sidecar older than the grace period is
// sweepable. Production grace is minutes, so the skew never mattered
// there.
func ageFixtures(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-1 * time.Hour)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(path, old, old)
	})
	if err != nil {
		t.Fatalf("age fixtures under %q: %v", dir, err)
	}
}

// strandedTree is #940's shape at a test's scale: 20 rows, each with its
// file, over 1,000 files no row names. Returns the directory and the live
// rows' paths.
func strandedTree(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	live := seedTestSidecarTree(t, dir, "live-", 20)
	seedTestSidecarTree(t, dir, "stranded-", 1000)
	ageFixtures(t, dir)
	return dir, live
}

// countFiles is how many regular files dir holds, recursively.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatalf("count files under %q: %v", dir, err)
	}
	return n
}

// switchableLister answers AllVariants with its fields as they are at the
// call, so a test can fail or empty the listing for one tick. The ticks it
// serves run on the test goroutine, so it needs no lock.
type switchableLister struct {
	rows []VariantSnapshot
	err  error
}

func (l *switchableLister) AllVariants(context.Context) ([]VariantSnapshot, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.rows, nil
}

// rowsNaming is one row with no source identity per path.
func rowsNaming(paths ...[]string) []VariantSnapshot {
	var out []VariantSnapshot
	for _, group := range paths {
		for _, p := range group {
			out = append(out, VariantSnapshot{SidecarPath: p})
		}
	}
	return out
}

// TestShouldConsiderSidecarFile pins the pure file-type predicate.
// `.flac` is the only extension considered today; mismatched
// extensions (incl. case variation) are skipped. TakeSidecarInventory
// hands it BASENAMES, so the bare names must answer as their paths do.
func TestShouldConsiderSidecarFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/transcoded/Album/01.flac", true},
		{"/tmp/transcoded/Album/01.FLAC", false}, // case-sensitive ext match
		{"/tmp/transcoded/Album/01.flac.tmp", false},
		{"/tmp/transcoded/Album/01.mp3", false},
		{"/tmp/transcoded/Album/01.wav", false},
		{"/tmp/transcoded/Album/01", false},
		{"", false},
		{"01.flac", true},
		{"01 Track.flac.upscaled-v2-176400-24.flac", true},
		{"01 Track.flac.upscaled-v2-176400-24.flac.tmp", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := shouldConsiderSidecarFile(c.path); got != c.want {
				t.Errorf("shouldConsiderSidecarFile(%q) = %v, want %v",
					c.path, got, c.want)
			}
		})
	}
}

// TestOrphanSidecarSweeperWalksTheLiveVariantsDir — the tree to walk is
// a hot setting, and the sweeper walked the one captured at construction
// for the rest of the process (its own docblock said a restart was needed,
// which was true and was the defect): new sidecars landed where it never
// looked, and orphans there were never reclaimed.
//
// Asked per tick in both directions: after the move the new root's orphan
// goes, and an orphan that appears in the OLD root afterwards stays, since
// that tree is no longer the one configured. (This test also pinned the
// chunk-resume cursor's reset on a root change until 2026-09-28; there is
// no cursor now, because every tick walks the whole tree.)
func TestOrphanSidecarSweeperWalksTheLiveVariantsDir(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	orphanA := seedTestSidecarTree(t, a, "a", 1)[0]
	orphanB := seedTestSidecarTree(t, b, "b", 1)[0]
	ageFixtures(t, a)
	ageFixtures(t, b)

	current := a
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: withLiveRow(a)}, func() string { return current }, time.Hour, sweepPercent)
	s.gracePeriodForTest = 1 * time.Nanosecond

	if n := s.tick(context.Background()); n != 1 {
		t.Fatalf("first tick unlinked %d, want 1 (%s)", n, orphanA)
	}
	current = b
	lateA := seedTestSidecarTree(t, a, "late", 1)[0]
	ageFixtures(t, a)
	if n := s.tick(context.Background()); n != 1 {
		t.Fatalf("tick after the variants dir moved unlinked %d, want 1 — the sweeper walked the "+
			"tree captured at construction", n)
	}
	if _, err := os.Stat(orphanB); !os.IsNotExist(err) {
		t.Errorf("orphan under the new root %q survived: %v", orphanB, err)
	}
	if _, err := os.Stat(lateA); err != nil {
		t.Errorf("a file under the OLD root was unlinked after the move: %v", err)
	}
}

// TestOrphanSidecarSweeperRefusesAnEmptyRoot — a live provider can answer
// "" (cmd/bridge's returns it on a nil config snapshot), a nil one answers
// nothing, and neither may sweep the working directory. The reason this
// used to give, that WalkDir("") walks the working directory, is false: it
// visits "" with an lstat ENOENT (measured with go1.26.6 on macOS, Linux
// and Windows). So the test passed with the refusal deleted, from a working
// directory with no sidecar in it. The hazard is a root RESOLVED before the
// walk, as #959 made the CLI sweeps do (resolveSidecarRoot), since
// filepath.EvalSymlinks("") is "." — and since 2026-09-28 this sweep walks
// through TakeSidecarInventory, which resolves. So the working directory
// here holds a sidecar-shaped orphan past its grace, which such a sweep
// would unlink. Two refusals stand in front of it now, the tick's and
// TakeSidecarInventory's own, and this pins the pair: either alone keeps
// the file.
func TestOrphanSidecarSweeperRefusesAnEmptyRoot(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	orphan := seedTestSidecarTree(t, cwd, "t", 1)[0]
	ageFixtures(t, cwd)
	for name, provider := range map[string]func() string{"an empty answer": staticDir(""), "a nil provider": nil} {
		s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: withLiveRow("/nowhere")}, provider, time.Hour, sweepPercent)
		s.gracePeriodForTest = 1 * time.Nanosecond
		if n := s.tick(context.Background()); n != 0 {
			t.Errorf("%s: tick unlinked %d, want 0", name, n)
		}
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("a sidecar-shaped file in the working directory was unlinked: %v", err)
	}
}

// TestOrphanSidecarSweeperTickUnlinksOrphans is the headline contract:
// files on disk that have no matching `track_variants.sidecar_path`
// entry get unlinked; files that ARE in the snapshot stay put. Three
// orphans are below the mass-orphan floor of ten, so the refusal stays
// out of it.
func TestOrphanSidecarSweeperTickUnlinksOrphans(t *testing.T) {
	outputDir := t.TempDir()
	paths := seedTestSidecarTree(t, outputDir, "t", 6)

	// Half are "known" to the DB; the other half are orphans.
	known := map[string]struct{}{
		paths[0]: {},
		paths[1]: {},
		paths[2]: {},
	}
	lister := &fakeSidecarLister{known: known}

	s := NewOrphanSidecarSweeper(lister, staticDir(outputDir), 1*time.Hour, sweepPercent)
	// Bypass the 10-minute production grace floor — the race-protection
	// regression has its own dedicated test below.
	s.gracePeriodForTest = 1 * time.Nanosecond
	ageFixtures(t, outputDir)
	unlinked := s.tick(context.Background())
	if unlinked != 3 {
		t.Errorf("unlinked = %d, want 3 (paths[3..5] are orphans)", unlinked)
	}
	// Verify the known files survived and the orphans are gone.
	for _, p := range paths[:3] {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("known sidecar %q was unlinked (should have survived): %v", p, err)
		}
	}
	for _, p := range paths[3:] {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan %q was NOT unlinked: stat err=%v", p, err)
		}
	}
}

// A live sidecar whose DB SidecarPath differs from its on-disk (WalkDir)
// path only in case must NOT be unlinked on a case-insensitive FS. The
// known-set is case-folded, so the mixed-case on-disk file still matches;
// on the pre-fix raw lookup the file was treated as orphan and deleted.
func TestOrphanSidecarSweeperCaseInsensitive(t *testing.T) {
	outputDir := t.TempDir()
	dir := filepath.Join(outputDir, "Artist", "Album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "Track.flac")
	if err := os.WriteFile(sidecar, []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}
	// DB records the SAME file under a different-cased path (as if written
	// on a case-insensitive FS with mixed casing).
	dbPath := filepath.Join(outputDir, "artist", "album", "track.flac")
	lister := &fakeSidecarLister{known: map[string]struct{}{dbPath: {}}}

	s := NewOrphanSidecarSweeper(lister, staticDir(outputDir), 1*time.Hour, sweepPercent)
	s.gracePeriodForTest = 1 * time.Nanosecond
	ageFixtures(t, outputDir)
	if unlinked := s.tick(context.Background()); unlinked != 0 {
		t.Errorf("unlinked = %d, want 0 (live sidecar with a case-only DB delta)", unlinked)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("live sidecar %q was unlinked over a casing delta: %v", sidecar, err)
	}
}

// TestOrphanSidecarSweeperRespectsChunkCap pins what the chunk bounds
// now: UNLINKS per tick, not the walk. 150 orphans against a catalog of
// 150 rows (whose files are gone, so every file on disk is an orphan) are
// not a MASS — `orphans > rows` fails, the refusal's lost-index term — so
// they drain as 100 on the first tick and 50 on the second, and a third
// tick finds nothing.
func TestOrphanSidecarSweeperRespectsChunkCap(t *testing.T) {
	outputDir := t.TempDir()
	const testChunk = 100
	totalEntries := testChunk + 50
	orphans := seedTestSidecarTree(t, outputDir, "x", totalEntries)
	gone := make([]string, totalEntries)
	for i := range gone {
		gone[i] = filepath.Join(outputDir, "rows", "row-"+strconv.Itoa(i)+".upscaled-v1-96000-24.flac")
	}
	lister := &fakeSidecarLister{known: pathSet(gone)}

	s := NewOrphanSidecarSweeper(lister, staticDir(outputDir), 1*time.Hour, sweepPercent)
	s.chunkSizeForTest = testChunk
	s.gracePeriodForTest = 1 * time.Nanosecond
	ageFixtures(t, outputDir)

	if n := s.tick(context.Background()); n != testChunk {
		t.Errorf("tick1 unlinked = %d, want %d (the chunk)", n, testChunk)
	}
	if n := s.tick(context.Background()); n != totalEntries-testChunk {
		t.Errorf("tick2 unlinked = %d, want %d (the remainder)", n, totalEntries-testChunk)
	}
	if n := s.tick(context.Background()); n != 0 {
		t.Errorf("tick3 unlinked = %d, want 0 (nothing left)", n)
	}
	for _, p := range orphans {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan %q survived two ticks: %v", p, err)
		}
	}
}

// TestOrphanSidecarSweeperRefusesAStrandedTree is #940's shape, the one
// that change left the background sweep open to: after a lost index the
// catalog holds a handful of rows (the auto-optimize sweeper's fresh
// renders, each with its file) over a stranded tree many times its size.
// The only guard this sweep had asked whether the catalog was EMPTY, which
// one row answers "no", and its chunked walk then unlinked the tree a
// chunk a tick — measured on the old code with this fixture, 480, 500 and
// the last 20 — while `bridge upscale --gc` refused the same tree. Every
// tick refuses it now, nothing is unlinked on any of them, and the refusal
// is logged ONCE, not once a tick.
func TestOrphanSidecarSweeperRefusesAStrandedTree(t *testing.T) {
	dir, live := strandedTree(t)
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	s.chunkSizeForTest = 500

	rec := loggingtest.Record(t)
	for i := 1; i <= 3; i++ {
		if n := s.tick(context.Background()); n != 0 {
			t.Fatalf("tick %d unlinked %d file(s) of a stranded tree, want 0", i, n)
		}
	}
	if got := countFiles(t, dir); got != 1020 {
		t.Errorf("%d of 1,020 files survive the ticks", got)
	}
	warns := rec.Failures()
	if len(warns) != 1 || !strings.Contains(warns[0], msgOrphanRefusal) {
		t.Fatalf("want exactly one WARN, the refusal; got %d:\n%s", len(warns), strings.Join(warns, "\n"))
	}
	if !strings.Contains(warns[0], "1000 of 1020 file(s)") || !strings.Contains(warns[0], "20 row(s)") {
		t.Errorf("the refusal should name the numbers: %s", warns[0])
	}
	if strings.Contains(warns[0], "variants move") {
		t.Errorf("the refusal names `bridge variants move`, which needs the rows a lost index lacks: %s", warns[0])
	}
	summaries := rec.Lines(msgOrphanTickComplete)
	if len(summaries) != 3 {
		t.Fatalf("want one summary per tick, got %d:\n%s", len(summaries), strings.Join(summaries, "\n"))
	}
	for _, line := range summaries {
		if !strings.Contains(line, " refused=true") || !strings.Contains(line, " orphans=1000") {
			t.Errorf("a refused tick's summary should say so, with the count: %s", line)
		}
	}
}

// TestOrphanSidecarSweeperRefusesOnTheFullOrphanCount — the inventory keeps
// only a chunk's worth of orphan PATHS (MaxOrphanPaths) and counts all of
// them. The refusal must read the COUNT: 1,000 orphans against a catalog of
// 150 rows is a lost index, while the 100 paths a chunk of 100 retains are
// fewer than the rows, and a refusal fed that number would proceed and
// unlink them.
func TestOrphanSidecarSweeperRefusesOnTheFullOrphanCount(t *testing.T) {
	dir := t.TempDir()
	live := seedTestSidecarTree(t, dir, "live-", 150)
	seedTestSidecarTree(t, dir, "orphan-", 1000)
	ageFixtures(t, dir)
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	s.chunkSizeForTest = 100

	rec := loggingtest.Record(t)
	if n := s.tick(context.Background()); n != 0 {
		t.Errorf("unlinked %d, want 0 — the refusal read the retained paths, not the count", n)
	}
	if got := countFiles(t, dir); got != 1150 {
		t.Errorf("%d of 1,150 files survive", got)
	}
	if lines := rec.Failures(msgOrphanRefusal); len(lines) != 1 || !strings.Contains(lines[0], "1000 of 1150 file(s)") {
		t.Errorf("want one refusal naming the full count, got %q", lines)
	}
}

// TestOrphanSidecarSweeperKeepsItsRefusalStreakThroughATickThatDecidedNothing
// — the refusal is logged once per streak, and a tick that never reached a
// verdict is evidence of nothing: a listing that failed, a walk that failed
// or was stopped, a catalog that read empty. None of them may end the
// streak, which would have the next refused tick WARN again, and none may
// claim the check passed.
func TestOrphanSidecarSweeperKeepsItsRefusalStreakThroughATickThatDecidedNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// middle runs the second of three ticks, the one that decides
		// nothing, and puts the lister back.
		middle func(s *OrphanSidecarSweeper, l *switchableLister)
	}{
		{"the listing fails", func(s *OrphanSidecarSweeper, l *switchableLister) {
			l.err = errors.New("database is locked")
			s.tick(context.Background())
			l.err = nil
		}},
		{"the walk fails", func(s *OrphanSidecarSweeper, _ *switchableLister) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			s.tick(ctx)
		}},
		{"the walk is stopped", func(s *OrphanSidecarSweeper, _ *switchableLister) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.tick(ctx)
		}},
		{"the catalog reads empty", func(s *OrphanSidecarSweeper, l *switchableLister) {
			rows := l.rows
			l.rows = nil
			s.tick(context.Background())
			l.rows = rows
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, live := strandedTree(t)
			l := &switchableLister{rows: rowsNaming(live)}
			s := NewOrphanSidecarSweeper(l, staticDir(dir), time.Hour, sweepPercent)
			s.gracePeriodForTest = time.Nanosecond

			rec := loggingtest.Record(t)
			s.tick(context.Background())
			tc.middle(s, l)
			s.tick(context.Background())

			if got := rec.Lines(msgOrphanRefusal); len(got) != 1 {
				t.Errorf("refusal logged %d times over refuse / %s / refuse, want once:\n%s",
					len(got), tc.name, strings.Join(got, "\n"))
			}
			if got := rec.Lines(msgOrphanRefusalLifted); len(got) != 0 {
				t.Errorf("a tick that decided nothing ended the streak:\n%s", strings.Join(got, "\n"))
			}
			if got := countFiles(t, dir); got != 1020 {
				t.Errorf("%d of 1,020 files survive", got)
			}
		})
	}
}

// TestOrphanSidecarSweeperSaysOnceWhenItStopsRefusing — the other end of
// the latch. A tick that proceeds outside a streak says nothing beyond its
// summary; the refusal WARNs once however many ticks refuse; the first tick
// that proceeds after it logs one Info line, and the ticks after that
// nothing. The catalog is what moves here, as it would when an operator
// restores the rows `bridge doctor` said were lost.
func TestOrphanSidecarSweeperSaysOnceWhenItStopsRefusing(t *testing.T) {
	dir := t.TempDir()
	live := seedTestSidecarTree(t, dir, "live-", 20)
	stranded := seedTestSidecarTree(t, dir, "stranded-", 1000)
	ageFixtures(t, dir)
	restored := rowsNaming(live, stranded)
	l := &switchableLister{rows: restored}
	s := NewOrphanSidecarSweeper(l, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	rec := loggingtest.Record(t)
	counts := func() (refused, lifted int) {
		return len(rec.Lines(msgOrphanRefusal)), len(rec.Lines(msgOrphanRefusalLifted))
	}

	s.tick(context.Background())
	if r, lf := counts(); r != 0 || lf != 0 {
		t.Fatalf("a healthy tick outside a streak logged %d refusal(s) and %d lifted line(s), want neither", r, lf)
	}

	l.rows = rowsNaming(live)
	s.tick(context.Background())
	s.tick(context.Background())
	if r, lf := counts(); r != 1 || lf != 0 {
		t.Fatalf("two refused ticks logged %d refusal(s) and %d lifted line(s), want 1 and 0", r, lf)
	}
	if lines := rec.Failures(msgOrphanRefusal); len(lines) != 1 {
		t.Errorf("the refusal is not at WARN: %q", rec.Lines(msgOrphanRefusal))
	}

	l.rows = restored
	s.tick(context.Background())
	s.tick(context.Background())
	if r, lf := counts(); r != 1 || lf != 1 {
		t.Fatalf("after the rows came back: %d refusal(s), %d lifted line(s), want 1 and 1", r, lf)
	}
	lifted := rec.Lines(msgOrphanRefusalLifted)[0]
	if !strings.HasPrefix(lifted, "INFO ") || !strings.Contains(lifted, " rows=1020") {
		t.Errorf("the lifted line should be Info and carry the counts it passed on: %s", lifted)
	}
	if got := countFiles(t, dir); got != 1020 {
		t.Errorf("%d of 1,020 files survive; nothing here was an orphan once the rows came back", got)
	}
}

// TestOrphanSidecarSweeperRefusesAWalkThatCouldNotReadPartOfTheTree is
// CodeRabbit's finding on #1063: a stranded tree whose larger part sits
// behind a directory the bridge's user cannot list. The walk sees 20 live
// files and 15 stranded ones, 15 orphans against 20 rows, which the
// mass-orphan check lets through; on the head it was found on, the tick
// unlinked those 15, while the 1,000 behind the locked directory make the
// whole tree refuse. Each tick refuses the partial walk now, under its own
// WARN, once for the streak. Once the directory is readable the whole tree
// refuses as a lost index, and that WARN comes at once, because a refusal
// of the other kind starts a new streak. Nothing is unlinked on any tick.
func TestOrphanSidecarSweeperRefusesAWalkThatCouldNotReadPartOfTheTree(t *testing.T) {
	skipWhereModesDenyNothing(t)
	dir := t.TempDir()
	live := seedTestSidecarTree(t, dir, "live-", 20)
	seedTestSidecarTree(t, dir, "stranded-", 15)
	locked := filepath.Join(dir, "locked")
	seedTestSidecarTree(t, locked, "stranded-", 1000)
	ageFixtures(t, dir)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	rec := loggingtest.Record(t)

	requireTicksUnlinkNothing(t, s, 2, "the walk could not read part of the tree")
	requireLinesSay(t, rec.Failures(msgOrphanPartialWalk), 1,
		"the partial-walk WARN, once for two ticks, naming what the walk could not read and what it counted",
		"could not list 1 director(y/ies)", "15 orphan(s) of 35 file(s) against 20 row(s)")
	requireLinesSay(t, rec.Lines(msgOrphanRefusal), 0,
		"the part the walk saw passes the mass-orphan check, so no lost-index WARN")
	requireLinesSay(t, rec.Lines(msgOrphanTickComplete), 2,
		"each refused tick's summary, with the unreadable count", " refused=true", " unreadable=1")

	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	requireTicksUnlinkNothing(t, s, 1, "the whole tree is readable")
	requireLinesSay(t, rec.Failures(msgOrphanRefusal), 1,
		"the whole tree refuses as a lost index at once, naming its count", "1015 of 1035 file(s)")
	requireLinesSay(t, rec.Lines(msgOrphanRefusalLifted), 0,
		"a refusal of the other kind is not the check passing")
	if got := countFiles(t, dir); got != 1035 {
		t.Errorf("%d of 1,035 files survive", got)
	}
}

// skipWhereModesDenyNothing stops a test that locks a directory by its
// mode where no mode can deny this user: chmod 0 denies nothing on
// Windows, and root reads through any mode.
func skipWhereModesDenyNothing(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("directory modes deny nothing on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
}

// requireTicksUnlinkNothing runs n ticks of s and fails the test at the
// first that unlinks anything; what says which state the ticks ran in.
func requireTicksUnlinkNothing(t *testing.T, s *OrphanSidecarSweeper, n int, what string) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if got := s.tick(context.Background()); got != 0 {
			t.Fatalf("%s: tick %d unlinked %d file(s), want 0", what, i, got)
		}
	}
}

// requireLinesSay checks that lines holds exactly count lines, each
// carrying every one of wants, and fails the test for each that does not;
// what names the expectation in the failure.
func requireLinesSay(t *testing.T, lines []string, count int, what string, wants ...string) {
	t.Helper()
	if len(lines) != count {
		t.Errorf("%s: want %d line(s), got %d:\n%s", what, count, len(lines), strings.Join(lines, "\n"))
		return
	}
	for _, line := range lines {
		for _, w := range wants {
			if !strings.Contains(line, w) {
				t.Errorf("%s: %q is missing from: %s", what, w, line)
			}
		}
	}
}

// TestReclaimOrphansRefusesAnUnpairedInventory — the unlink step refuses an
// inventory whose listed and walked paths do not pair up, unlinking
// nothing (Gemini on #1063), rather than index past the shorter list or cut
// it to the chunk, either of which panics in `bridge serve`'s sweep
// goroutine. The walked lists here are slices of their own, as a list built
// apart would be. Driven directly: TakeSidecarInventory cannot return such
// an inventory, which is the premise the check guards.
func TestReclaimOrphansRefusesAnUnpairedInventory(t *testing.T) {
	dir := t.TempDir()
	files := seedTestSidecarTree(t, dir, "orphan-", 3)
	ageFixtures(t, dir)
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{}, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	walkedCopy := func(n int) []string { return append([]string(nil), files[:n]...) }

	for _, tc := range []struct {
		name  string
		inv   SidecarInventory
		chunk int
	}{
		{"a walked path missing", SidecarInventory{OrphanPaths: files, OrphanWalkedPaths: walkedCopy(2)}, 10},
		{"a walked path missing, cut to a chunk", SidecarInventory{OrphanPaths: files, OrphanWalkedPaths: walkedCopy(1)}, 2},
		{"a walked path with no orphan", SidecarInventory{OrphanPaths: files[:2], OrphanWalkedPaths: walkedCopy(3)}, 10},
		{"the scratch lists unpaired", SidecarInventory{OrphanPaths: files, OrphanWalkedPaths: walkedCopy(3), ScratchPaths: files[:1]}, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tally, err := s.reclaimOrphans(context.Background(), tc.inv, tc.chunk, time.Now())
			if !errors.Is(err, ErrUnpairedInventory) {
				t.Errorf("reclaimOrphans() error = %v, want ErrUnpairedInventory", err)
			}
			if tally != (orphanTally{}) {
				t.Errorf("tally = %+v, want nothing done", tally)
			}
			if got := countFiles(t, dir); got != 3 {
				t.Errorf("%d of 3 files survive an inventory that was refused", got)
			}
		})
	}
}

// TestOrphanSidecarSweeperRepeatsARefusalOnceADay — a streak that goes on
// is logged again once a day, so a journal read a week later still shows
// the sweep refusing, and never more often than that.
func TestOrphanSidecarSweeperRepeatsARefusalOnceADay(t *testing.T) {
	dir, live := strandedTree(t)
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	rec := loggingtest.Record(t)

	s.tick(context.Background())
	s.lastRefusalLog = s.lastRefusalLog.Add(-(orphanRefusalRepeat - time.Minute))
	s.tick(context.Background())
	if got := len(rec.Lines(msgOrphanRefusal)); got != 1 {
		t.Fatalf("a refusal a minute short of a day old was repeated: %d lines", got)
	}
	s.lastRefusalLog = s.lastRefusalLog.Add(-2 * time.Minute)
	s.tick(context.Background())
	if got := len(rec.Lines(msgOrphanRefusal)); got != 2 {
		t.Fatalf("a refusal over a day old was not repeated: %d lines, want 2", got)
	}
}

// fakeLstat is the fs.FileInfo reclaimOrphan reads from an Lstat: a mode
// and a modification time, and nothing else.
type fakeLstat struct {
	fs.FileInfo
	mode  fs.FileMode
	mtime time.Time
}

func (f fakeLstat) Mode() fs.FileMode  { return f.mode }
func (f fakeLstat) ModTime() time.Time { return f.mtime }

// TestReclaimOrphanRechecksThePathBeforeUnlinkingIt pins the re-check the
// unlink step makes. The inventory classified the path during the walk;
// by the unlink the tree may have moved on, and the sweep needs the mtime
// anyway, so the path is asked again — through the inventory's own
// classifyWalkEntry, so the two cannot disagree about what a candidate is.
//
// Driven through the (lstat, stat) seam, because the Windows junction
// shape (ModeIrregular without ModeDir, since Go 1.23) cannot be built on
// any other platform, and a guard that only runs on one CI leg looks
// exactly like one that passed. The path is a REAL regular file in every
// row that has one, so whenever the helper reaches os.Remove it removes
// something the assertion can see.
func TestReclaimOrphanRechecksThePathBeforeUnlinkingIt(t *testing.T) {
	tickStart := time.Now()
	const grace = time.Minute
	old := tickStart.Add(-time.Hour)
	lstatOf := func(mode fs.FileMode, mtime time.Time) func(string) (fs.FileInfo, error) {
		return func(string) (fs.FileInfo, error) { return fakeLstat{mode: mode, mtime: mtime}, nil }
	}
	failing := func(err error) func(string) (fs.FileInfo, error) {
		return func(string) (fs.FileInfo, error) { return nil, err }
	}
	toDir := func(string) (fs.FileInfo, error) { return fakeFileInfo{dir: true}, nil }
	toFile := func(string) (fs.FileInfo, error) { return fakeFileInfo{}, nil }

	for _, tc := range []struct {
		name        string
		lstat, stat func(string) (fs.FileInfo, error)
		noFile      bool // the path holds nothing on disk
		want        orphanOutcome
		wantErr     bool
		wantRemoved bool
	}{
		{name: "an old regular file is unlinked", lstat: lstatOf(0, old), stat: toFile,
			want: orphanUnlinked, wantRemoved: true},
		{name: "a Windows junction to a directory is left", lstat: lstatOf(fs.ModeIrregular, old), stat: toDir,
			want: orphanNotAFile},
		{name: "a symlink to a directory is left", lstat: lstatOf(fs.ModeSymlink, old), stat: toDir,
			want: orphanNotAFile},
		{name: "a link it cannot stat is left", lstat: lstatOf(fs.ModeSymlink, old), stat: failing(fs.ErrPermission),
			want: orphanUnreadable, wantErr: true},
		{name: "an lstat that fails leaves the path", lstat: failing(fs.ErrPermission), stat: toFile,
			want: orphanUnreadable, wantErr: true},
		{name: "a dangling link is junk, and unlinked", lstat: lstatOf(fs.ModeSymlink, old), stat: failing(fs.ErrNotExist),
			want: orphanUnlinked, wantRemoved: true},
		{name: "a file inside the grace is left", lstat: lstatOf(0, tickStart.Add(-time.Second)), stat: toFile,
			want: orphanInGrace},
		{name: "a path that vanished before the re-check counts as done", lstat: failing(fs.ErrNotExist), stat: toFile,
			noFile: true, want: orphanGone},
		{name: "a path that vanished before the unlink counts as done", lstat: lstatOf(0, old), stat: toFile,
			noFile: true, want: orphanGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.upscaled-v2-176400-24.flac")
			if !tc.noFile {
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := reclaimOrphan(path, tickStart, grace, tc.lstat, tc.stat)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("reclaimOrphan = (%d, %v), want (%d, error %v)", got, err, tc.want, tc.wantErr)
			}
			if tc.noFile {
				return
			}
			_, statErr := os.Lstat(path)
			if removed := errors.Is(statErr, fs.ErrNotExist); removed != tc.wantRemoved {
				t.Errorf("file removed = %v, want %v", removed, tc.wantRemoved)
			}
		})
	}
}

// TestReclaimOrphanLeavesALinkToADirectory is the same re-check on a real
// filesystem: a symlink to a directory where the orphan stood is never
// unlinked, since it may be the only reference to an album parked on
// another volume. The tick starts an hour ahead with a nanosecond's grace,
// so the grace cannot be what keeps it.
func TestReclaimOrphanLeavesALinkToADirectory(t *testing.T) {
	base := t.TempDir()
	parked := filepath.Join(base, "volume", "Album")
	if err := os.MkdirAll(parked, 0o755); err != nil {
		t.Fatal(err)
	}
	track := filepath.Join(parked, "01.flac")
	if err := os.WriteFile(track, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "variants", "Album.flac")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parked, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	got, err := reclaimOrphan(link, time.Now().Add(time.Hour), time.Nanosecond, os.Lstat, os.Stat)
	if got != orphanNotAFile || err != nil {
		t.Fatalf("reclaimOrphan = (%d, %v), want (%d, nil)", got, err, orphanNotAFile)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the link was removed or replaced: %v", err)
	}
	if _, err := os.Stat(track); err != nil {
		t.Errorf("the parked album's file is gone: %v", err)
	}

	gone, err := reclaimOrphan(filepath.Join(base, "variants", "never-there.flac"), time.Now(), time.Nanosecond, os.Lstat, os.Stat)
	if gone != orphanGone || err != nil {
		t.Errorf("a path with nothing at it = (%d, %v), want (%d, nil)", gone, err, orphanGone)
	}
}

// TestOrphanSidecarSweeperWalksASymlinkedVariantsDir — a behaviour change,
// on purpose. This sweep's own WalkDir Lstat'd its root, so a variants
// directory that is itself a symlink (`/srv/variants -> /mnt/vol/…`, the
// ordinary mountpoint alias) was one non-directory entry and the sweep
// found nothing there, ever. TakeSidecarInventory resolves the root and
// reports under the configured spelling (#959), so the orphan behind the
// link is reclaimed like any other, while the known file beside it and the
// link itself — never a candidate — stay.
func TestOrphanSidecarSweeperWalksASymlinkedVariantsDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "volume", "variants")
	paths := seedTree(t, target, "Artist/Album/01.flac.upscaled-v2-176400-24.flac", "Artist/Album/orphan.flac")
	link := filepath.Join(base, "variants")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	ageFixtures(t, target)
	known := map[string]struct{}{filepath.Join(link, "Artist", "Album", filepath.Base(paths[0])): {}}
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: known}, staticDir(link), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond

	if n := s.tick(context.Background()); n != 1 {
		t.Fatalf("unlinked %d through a symlinked variants dir, want 1 (the orphan)", n)
	}
	if _, err := os.Stat(paths[1]); !os.IsNotExist(err) {
		t.Errorf("the orphan survived: %v", err)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Errorf("the known sidecar was unlinked: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the variants directory's link was removed or replaced: %v", err)
	}
}

// TestAnOrphanSweepUnlinksInTheTreeItWalked pins that a tick unlinks the
// files its walk counted, by the paths the walk visited, never through
// the configured spelling. The variants directory here is a symlink, and
// the test repoints it at a second tree between the walk and the first
// unlink (beforeUnlinksForTest), as an operator moving the alias to
// another volume while the bridge runs would. Through the link, the
// unlink found the same name in the second tree and removed it: a file
// the walk never counted, past the mass-orphan refusal, whose verdict
// was taken over the first tree (CodeRabbit on #1063).
func TestAnOrphanSweepUnlinksInTheTreeItWalked(t *testing.T) {
	base := t.TempDir()
	rels := []string{"Artist/Album/01.flac.upscaled-v2-176400-24.flac", "Artist/Album/orphan.flac"}
	first := filepath.Join(base, "first", "variants")
	second := filepath.Join(base, "second", "variants")
	firstPaths := seedTree(t, first, rels...)
	secondPaths := seedTree(t, second, rels...)
	ageFixtures(t, first)
	ageFixtures(t, second)
	link := filepath.Join(base, "variants")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	known := map[string]struct{}{filepath.Join(link, filepath.FromSlash(rels[0])): {}}
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: known}, staticDir(link), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	s.beforeUnlinksForTest = func() {
		if err := os.Remove(link); err != nil {
			t.Fatalf("repoint the variants link: %v", err)
		}
		if err := os.Symlink(second, link); err != nil {
			t.Fatalf("repoint the variants link: %v", err)
		}
	}

	if n := s.tick(context.Background()); n != 1 {
		t.Fatalf("unlinked %d, want 1 (the orphan the walk counted)", n)
	}
	if _, err := os.Stat(firstPaths[1]); !os.IsNotExist(err) {
		t.Errorf("the orphan the walk counted survived: %v", err)
	}
	if _, err := os.Stat(secondPaths[1]); err != nil {
		t.Errorf("the file of that name in the tree the link now points at was unlinked: %v", err)
	}
	for _, p := range []string{firstPaths[0], secondPaths[0]} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a known sidecar was unlinked: %s: %v", p, err)
		}
	}
}

// TestOrphanSidecarSweeperHonoursCancellation pins the ctx-cancel
// contract: a context cancelled before the walk returns promptly and
// unlinks nothing — the walk stops at its first entry, and the unlinks
// come only after a walk that finished. (Until the walk and the unlinks
// were split, a few entries could go before the check.)
func TestOrphanSidecarSweeperHonoursCancellation(t *testing.T) {
	outputDir := t.TempDir()
	seedTestSidecarTree(t, outputDir, "c", 200)
	lister := &fakeSidecarLister{known: withLiveRow(outputDir)}
	s := NewOrphanSidecarSweeper(lister, staticDir(outputDir), 1*time.Hour, sweepPercent)
	s.gracePeriodForTest = 1 * time.Nanosecond
	ageFixtures(t, outputDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the tick starts

	done := make(chan int, 1)
	go func() {
		done <- s.tick(ctx)
	}()
	select {
	case n := <-done:
		if n != 0 {
			t.Errorf("a stopped tick unlinked %d, want 0", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not return within 5s after ctx cancel")
	}
	if got := countFiles(t, outputDir); got != 200 {
		t.Errorf("%d of 200 files survive a stopped tick", got)
	}
}

// TestOrphanSidecarSweeperStartIntervalZeroIsNoOp pins the disable
// path: interval ≤ 0 → no goroutine spawned. The stopFn is still
// safe to call (no-op).
func TestOrphanSidecarSweeperStartIntervalZeroIsNoOp(t *testing.T) {
	dir := t.TempDir()
	lister := &fakeSidecarLister{known: withLiveRow(dir)}
	s := NewOrphanSidecarSweeper(lister, staticDir(dir), 0, sweepPercent)
	stop := s.Start(context.Background())
	// stopFn must be idempotent and safe.
	stop()
	stop()
}

// TestOrphanSidecarSweeperStartTickFires drives the watcher through
// one full Start → tick → stop cycle with the test seam. Confirms
// the goroutine wakes, fires the initial-boot tick, and stops on
// the returned stopFn.
func TestOrphanSidecarSweeperStartTickFires(t *testing.T) {
	outputDir := t.TempDir()
	seedTestSidecarTree(t, outputDir, "s", 3) // all orphan
	lister := &fakeSidecarLister{known: withLiveRow(outputDir)}
	s := NewOrphanSidecarSweeper(lister, staticDir(outputDir), 1*time.Hour, sweepPercent)
	s.gracePeriodForTest = 1 * time.Nanosecond
	ageFixtures(t, outputDir)

	tickFired := make(chan int, 1)
	s.SetOnTickComplete(func(unlinked int) {
		select {
		case tickFired <- unlinked:
		default:
			// drop subsequent ticks (we only care about the boot tick)
		}
	})

	stop := s.Start(context.Background())
	defer stop()

	select {
	case n := <-tickFired:
		if n != 3 {
			t.Errorf("boot-tick unlinked = %d, want 3", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("boot tick did not fire within 2s")
	}
}

// TestOrphanSidecarSweeperPreservesNonFlacFiles confirms the
// file-type filter: the sweeper does not touch `.tmp`, `.txt`,
// `.flac.partial`, or any non-`.flac` files in the variants
// directory. Defensive guard for operators who use the variants
// directory for related artifacts (less common, but the file-type
// filter shape makes the sweeper safe by construction).
func TestOrphanSidecarSweeperPreservesNonFlacFiles(t *testing.T) {
	outputDir := t.TempDir()
	// Mix .flac (orphan, will be unlinked) with various other
	// extensions (must survive).
	nonFlacFiles := []string{
		"readme.txt",
		"partial.flac.tmp",
		"backup.bak",
		"manifest.json",
	}
	for _, name := range nonFlacFiles {
		full := filepath.Join(outputDir, name)
		if err := os.WriteFile(full, []byte("test"), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	flacOrphan := filepath.Join(outputDir, "track.flac")
	if err := os.WriteFile(flacOrphan, []byte{0}, 0o644); err != nil {
		t.Fatalf("write flac: %v", err)
	}

	lister := &fakeSidecarLister{known: withLiveRow(outputDir)}
	s := NewOrphanSidecarSweeper(lister, staticDir(outputDir), 1*time.Hour, sweepPercent)
	s.gracePeriodForTest = 1 * time.Nanosecond
	ageFixtures(t, outputDir)
	unlinked := s.tick(context.Background())

	if unlinked != 1 {
		t.Errorf("unlinked = %d, want 1 (only the .flac orphan)", unlinked)
	}
	for _, name := range nonFlacFiles {
		full := filepath.Join(outputDir, name)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("non-flac file %q was unlinked (should have survived): %v",
				full, err)
		}
	}
	if _, err := os.Stat(flacOrphan); !os.IsNotExist(err) {
		t.Errorf("orphan .flac was NOT unlinked: stat err=%v", err)
	}
}

// TestOrphanSidecarSweeperGracePeriodProtectsConcurrentWrites is the
// race-condition regression test Gemini HIGH on PR #282 asked for.
// A concurrent `UpsertVariant` writer lands the sidecar on disk
// BEFORE its row commits to `track_variants`. If the sweeper takes
// its snapshot DURING that window, the file looks orphan; without
// the grace-period gate, the sweeper would unlink it behind the
// writer's in-flight transaction.
//
// Contract: a fresh file (modtime < grace) is skipped REGARDLESS of
// whether the snapshot contains its path. Both branches must respect
// the grace:
//   - Sidecar present in known-set: protected by the known-set check.
//   - Sidecar absent from known-set + modtime < grace: protected by
//     the grace gate, at the re-check before the unlink (this test).
//   - Sidecar absent + modtime >= grace: ordinary orphan, unlinked.
//
// Test shape: seed one fresh file (modtime ≈ now), set
// gracePeriodForTest to a value longer than test wall-clock (5s is
// safe), assert the file SURVIVES the sweep. Then backdate the file
// past the grace, sweep again, assert it's gone.
func TestOrphanSidecarSweeperGracePeriodProtectsConcurrentWrites(t *testing.T) {
	outputDir := t.TempDir()
	flacFile := filepath.Join(outputDir, "in-flight.flac")
	if err := os.WriteFile(flacFile, []byte{0}, 0o644); err != nil {
		t.Fatalf("write fresh file: %v", err)
	}

	// The known set does not hold the file: the writer's row hasn't
	// committed yet. Without the grace gate, the sweeper would unlink
	// immediately.
	lister := &fakeSidecarLister{known: withLiveRow(outputDir)}
	s := NewOrphanSidecarSweeper(lister, staticDir(outputDir), 1*time.Hour, sweepPercent)
	// Grace period longer than test wall-clock — the fresh file's
	// modtime (just now) is firmly inside the grace window.
	s.gracePeriodForTest = 5 * time.Second

	unlinked := s.tick(context.Background())
	if unlinked != 0 {
		t.Errorf("tick with fresh in-flight file unlinked %d files; want 0 "+
			"(grace period should protect concurrent UpsertVariant writes)",
			unlinked)
	}
	if _, err := os.Stat(flacFile); err != nil {
		t.Errorf("fresh in-flight file %q was unlinked (grace period "+
			"should have protected it): %v", flacFile, err)
	}

	// Now backdate the file past the grace window. Real-world this
	// is the "transaction committed long ago but the row was later
	// deleted, leaving the sidecar truly orphaned" case.
	pastModTime := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(flacFile, pastModTime, pastModTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	unlinked = s.tick(context.Background())
	if unlinked != 1 {
		t.Errorf("tick with backdated orphan unlinked %d files; want 1",
			unlinked)
	}
	if _, err := os.Stat(flacFile); !os.IsNotExist(err) {
		t.Errorf("backdated orphan %q was NOT unlinked: stat err=%v",
			flacFile, err)
	}
}

// TestOrphanSidecarSweeperEffectiveOverrides locks the test-seam
// helpers' contract — production constants when override is zero or
// negative, override value when positive. Defensive against an
// accidental shape change to `effectiveGracePeriod` /
// `effectiveChunkSize` that would silently bypass the production
// floor.
func TestOrphanSidecarSweeperEffectiveOverrides(t *testing.T) {
	cases := []struct {
		name          string
		override      time.Duration
		wantGrace     time.Duration
		chunkOverride int
		wantChunkSize int
	}{
		{"zero-uses-production", 0, gcGracePeriod, 0, gcChunkSize},
		{"negative-uses-production", -1 * time.Second, gcGracePeriod, -5, gcChunkSize},
		{"positive-override-wins", 250 * time.Millisecond, 250 * time.Millisecond, 250, 250},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &OrphanSidecarSweeper{
				gracePeriodForTest: c.override,
				chunkSizeForTest:   c.chunkOverride,
			}
			if got := s.effectiveGracePeriod(); got != c.wantGrace {
				t.Errorf("effectiveGracePeriod = %v, want %v", got, c.wantGrace)
			}
			if got := s.effectiveChunkSize(); got != c.wantChunkSize {
				t.Errorf("effectiveChunkSize = %d, want %d", got, c.wantChunkSize)
			}
		})
	}
}

// TestOrphanSidecarSweepRefusesAnEmptyKnownSet is the guard the FORWARD sweep
// did not have while its reverse twin had two.
//
// `AllVariants` returning zero rows with a nil error is an ordinary state,
// not a fault: `rm -f bridge.db*` + restart is the reset procedure this repo's
// own CLAUDE.md documents, and `run` takes a tick at boot; a single<->multi
// root flip runs WipeFilesystemTracks and `track_variants` CASCADEs on
// `tracks`. In that window every file under the variants directory misses an
// empty `known` and the walk would unlink the whole rendition tree.
func TestOrphanSidecarSweepRefusesAnEmptyKnownSet(t *testing.T) {
	outputDir := t.TempDir()
	seedTestSidecarTree(t, outputDir, "orphan", 3)
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: map[string]struct{}{}}, staticDir(outputDir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	ageFixtures(t, outputDir)

	if n := s.tick(context.Background()); n != 0 {
		t.Errorf("unlinked = %d, want 0 — an empty catalog must refuse, not reap everything", n)
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("%d files survive, want 3 — the rendition tree was reaped on an empty catalog", len(entries))
	}

	// NEGATIVE CONTROL, and what stops this passing against a sweeper that
	// simply never unlinks anything: with ONE row in the catalog the same
	// three orphans go (three is below the mass-orphan floor of ten).
	s2 := NewOrphanSidecarSweeper(&fakeSidecarLister{known: withLiveRow(outputDir)}, staticDir(outputDir), time.Hour, sweepPercent)
	s2.gracePeriodForTest = time.Nanosecond
	if n := s2.tick(context.Background()); n != 3 {
		t.Errorf("unlinked = %d with a populated catalog, want 3 — the refusal is now unconditional", n)
	}
}

// TestOrphanSidecarSweepIsQuietOnAnEmptyCatalogAndAnEmptyDir — an empty set
// over an empty directory is not the hazard, it is a bridge that has never
// transcoded anything. It must stay a silent no-op rather than a refusal an
// operator has to interpret.
func TestOrphanSidecarSweepIsQuietOnAnEmptyCatalogAndAnEmptyDir(t *testing.T) {
	outputDir := t.TempDir()
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: map[string]struct{}{}}, staticDir(outputDir), time.Hour, sweepPercent)
	rec := loggingtest.Record(t)
	if n := s.tick(context.Background()); n != 0 {
		t.Errorf("unlinked = %d, want 0", n)
	}
	if got := rec.Failures(); len(got) != 0 {
		t.Errorf("an empty catalog over an empty directory warned:\n%s", strings.Join(got, "\n"))
	}
}

// TestOrphanSidecarSweepSkipsDotDirectories — `bridge upscale --gc` has pruned
// these since it was written; this sweeper, the same walk unattended on a
// timer, did not. With `variantsDir` on a dedicated volume, `.Trashes/<uid>/`
// and `.Trash-1000/` sit under the walk root, so any `.flac` inside one is
// missing from the catalog and older than the grace: files an operator put in
// the Trash specifically so they could get them back. Pruned now by the walk
// the sweep shares with `--gc` (TakeSidecarInventory).
//
// Asserted with a POPULATED catalog, so it pins the walk prune rather than
// riding on the empty-set refusal above.
func TestOrphanSidecarSweepSkipsDotDirectories(t *testing.T) {
	outputDir := t.TempDir()
	trash := filepath.Join(outputDir, ".Trashes", "501")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		t.Fatal(err)
	}
	rescued := filepath.Join(trash, "someones-album.flac")
	if err := os.WriteFile(rescued, []byte("recoverable"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(outputDir, "real.upscaled-v1-96000-24.flac")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: withLiveRow(outputDir)}, staticDir(outputDir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	ageFixtures(t, outputDir)

	// The real orphan still goes — this is not a sweeper that stopped working.
	if n := s.tick(context.Background()); n != 1 {
		t.Errorf("unlinked = %d, want 1 (the orphan beside the dot-dir)", n)
	}
	if _, err := os.Stat(rescued); err != nil {
		t.Errorf("a file inside %s was unlinked: %v", trash, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the real orphan survived; the prune is too wide")
	}
}
