package integrity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The VariantWatcher's mass-delete refusal went through no latch (backlog
// B65): every refused tick logged the refusal at WARN, and its summary line
// at WARN too. Measured with a real `bridge serve` at a 2 s interval over
// twelve rows whose sidecars were gone while the tree still held one
// sidecar: seven ticks in thirteen seconds, fourteen WARN lines. It shares
// the orphan sweep's latch now: one WARN when a streak of refused ticks
// starts, again at most once a day while it lasts, and one Info line when a
// tick's relocation check proceeds again. Each refused tick still logs its
// summary, at Info.

// relocationShape seeds n rows recorded under a directory that never
// existed, with no file at their canonical place under the returned
// variants directory either, and one sidecar in that directory no row
// names: the shape MassDeleteRefusal refuses (the tree still holds sidecar
// files) at the default threshold. It returns the directory, the rows, and
// the one sidecar, so a test can remove it to turn the shape into a
// deletion the check lets through.
func relocationShape(t *testing.T, n int) (dir string, rows []VariantSnapshot, sidecar string) {
	t.Helper()
	dir = t.TempDir()
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	rows = make([]VariantSnapshot, n)
	for i := range rows {
		source := fmt.Sprintf("Artist/Album/%02d.flac", i)
		rows[i] = VariantSnapshot{SourcePath: source, VariantID: "upscaled-v2-176400-24",
			SidecarPath: transcode.VariantSidecarPath(oldDir, source, "upscaled-v2-176400-24"), SizeBytes: 10}
	}
	sidecar = transcode.VariantSidecarPath(dir, "Other/Album/01.flac", "upscaled-v2-176400-24")
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, rows, sidecar
}

// requireRefusedTicks runs n ticks of w and fails the test at the first
// that does not refuse all of want rows and delete none; what says which
// state the ticks ran in.
func requireRefusedTicks(t *testing.T, w *VariantWatcher, n, want int, what string) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if r := w.tick(context.Background()); r.Refused != want || r.Deleted != 0 {
			t.Fatalf("%s: tick %d report %+v, want %d refused and none deleted", what, i, r, want)
		}
	}
}

// TestVariantWatcherLatchesItsMassDeleteRefusal — three refused ticks log
// one WARN between them, naming the numbers and what to do, and three
// summary lines at Info carrying the refused count. On main: six WARN
// lines, the refusal's and the summary's on every tick.
func TestVariantWatcherLatchesItsMassDeleteRefusal(t *testing.T) {
	dir, rows, _ := relocationShape(t, 30)
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, &fakeDeleter{}, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	requireRefusedTicks(t, w, 3, 30, "every row is gone while the tree holds a sidecar")
	requireLinesSay(t, rec.Failures(), 1, "one WARN for three refused ticks, the refusal's",
		msgVariantRefusal, "30 of 30 rows (100%)", " variants_dir="+dir, "--allow-mass-delete", "once a day")
	requireLinesSay(t, rec.Lines(msgVariantSweepSummary), 3, "a summary line per tick, at Info", "INFO ", " refused=30")
}

// TestVariantWatcherSaysOnceWhenItStopsRefusing — the first tick whose
// relocation check proceeds after a streak says so once, at Info, with the
// counts it proceeded on; the ticks after it say nothing more. Here the
// sidecar that made the tree read as a relocation goes, so the missing rows
// are a library whose files really went, and the tick reaps them.
func TestVariantWatcherSaysOnceWhenItStopsRefusing(t *testing.T) {
	dir, rows, sidecar := relocationShape(t, 30)
	store := &fakeDeleter{}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	requireRefusedTicks(t, w, 1, 30, "every row is gone while the tree holds a sidecar")
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if r := w.tick(context.Background()); r.Refused != 0 || r.Deleted != 30 {
			t.Fatalf("tick %d after the sidecar went: report %+v, want the 30 rows deleted", i+1, r)
		}
	}
	requireLinesSay(t, rec.Lines(msgVariantRefusalLifted), 1, "the lifted line, once, with the counts it passed on",
		"INFO ", " rows=30", " missing=30", " ended=relocation", " variants_dir="+dir)
	requireLinesSay(t, rec.Failures(msgVariantRefusal), 1, "the refusal's one WARN")
}

// TestVariantWatcherEndsItsStreakOnAnEmptyCatalog — a catalog with no rows
// has nothing to refuse: the streak ends with the lifted line, and a
// relocation after it is a new streak whose WARN is logged at once, not a
// day later.
func TestVariantWatcherEndsItsStreakOnAnEmptyCatalog(t *testing.T) {
	dir, rows, _ := relocationShape(t, 30)
	lister := &fakeLister{snapshots: [][]VariantSnapshot{rows, nil, rows}}
	w := NewVariantWatcher(lister, &fakeDeleter{}, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	requireRefusedTicks(t, w, 1, 30, "the relocation")
	if r := w.tick(context.Background()); r.Rows != 0 {
		t.Fatalf("the empty catalog's tick: report %+v", r)
	}
	requireLinesSay(t, rec.Lines(msgVariantRefusalLifted), 1, "the lifted line, for the empty catalog", " rows=0")
	requireRefusedTicks(t, w, 1, 30, "the relocation, again")
	requireLinesSay(t, rec.Failures(msgVariantRefusal), 2, "a WARN for each streak")
}

// TestVariantWatcherKeepsItsStreakThroughATickThatDecidedNothing — a tick
// that never asked either guard's question is evidence of nothing, so it
// neither ends a streak nor starts one: a failed listing, and a tick the
// shutdown stopped before its first row. The streak goes on through both,
// with one WARN and no lifted line. (A variants directory the mount-loss
// guard reads as unmounted was the third such tick until 2026-09-30: it is
// a refusal of its own kind now, TestVariantWatcherSaysEachKindOfRefusalWhenItStarts.)
func TestVariantWatcherKeepsItsStreakThroughATickThatDecidedNothing(t *testing.T) {
	dir, rows, _ := relocationShape(t, 30)
	lister := &fakeLister{snapshots: [][]VariantSnapshot{rows}}
	w := NewVariantWatcher(lister, &fakeDeleter{}, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	requireRefusedTicks(t, w, 1, 30, "the relocation")

	lister.mu.Lock()
	lister.err = errors.New("database is locked")
	lister.mu.Unlock()
	if r := w.tick(context.Background()); !r.Skipped {
		t.Fatalf("the failed listing's tick: report %+v, want skipped", r)
	}
	lister.mu.Lock()
	lister.err = nil
	lister.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := w.tick(ctx); !r.Cancelled {
		t.Fatalf("the stopped tick: report %+v, want cancelled", r)
	}

	requireRefusedTicks(t, w, 1, 30, "the relocation, after two ticks that decided nothing")
	requireLinesSay(t, rec.Failures(msgVariantRefusal), 1, "one WARN for the whole streak")
	requireLinesSay(t, rec.Lines(msgVariantRefusalLifted), 0, "a streak that never lifted")
}

// unmountedShape seeds n rows whose sidecars are at their canonical places
// under a variants directory, and returns that directory, the rows, and an
// empty directory beside it: the mountpoint an unmount leaves, which the
// mount-loss guard reads as unmounted while the rows exist. A test points
// the watcher at one or the other to unmount and remount the volume.
func unmountedShape(t *testing.T, n int) (mounted, unmounted string, rows []VariantSnapshot) {
	t.Helper()
	mounted, unmounted = t.TempDir(), t.TempDir()
	rows = make([]VariantSnapshot, n)
	for i := range rows {
		source := fmt.Sprintf("Artist/Album/%02d.flac", i)
		p := transcode.VariantSidecarPath(mounted, source, "upscaled-v2-176400-24")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		rows[i] = VariantSnapshot{SourcePath: source, VariantID: "upscaled-v2-176400-24", SidecarPath: p, SizeBytes: 10}
	}
	return mounted, unmounted, rows
}

// TestVariantWatcherLatchesItsVariantsDirRefusal — the mount-loss guard's
// skip is a refusal like the relocation one (backlog B131): three ticks
// over a variants directory that reads as unmounted log one WARN between
// them, naming why, how many rows it kept and what to do, and three
// summary lines at Info that say the tick was skipped. On main: three
// WARN lines, one per tick, and no summary.
func TestVariantWatcherLatchesItsVariantsDirRefusal(t *testing.T) {
	_, unmounted, rows := unmountedShape(t, 12)
	store := &fakeDeleter{}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil, staticDir(unmounted), time.Hour, 20)
	rec := loggingtest.Record(t)

	for i := 1; i <= 3; i++ {
		if r := w.tick(context.Background()); !r.Skipped || r.Rows != 12 || r.Deleted != 0 {
			t.Fatalf("tick %d over an unmounted variants directory: report %+v, want 12 rows skipped", i, r)
		}
	}
	requireLinesSay(t, rec.Failures(), 1, "one WARN for three skipped ticks, the refusal's",
		msgVariantsDirUnavailable, " reason=variants directory is empty ", " rows=12",
		" variants_dir="+unmounted, "Mount the volume", "once a day")
	requireLinesSay(t, rec.Lines(msgVariantSweepSummary), 3, "a summary line per tick, at Info",
		"INFO ", " rows=12", " skipped=true", " deleted=0")
	if got := store.deleted(); len(got) != 0 {
		t.Errorf("a skipped tick deleted rows: %v", got)
	}
}

// TestVariantWatcherSaysEachKindOfRefusalWhenItStarts — the two guards
// are two kinds of one latch: a streak that turns from one into the other
// WARNs at once, since the advice differs, and a tick of the running kind
// does not. A relocation, then an unmount, then the volume back with the
// relocation still there: three WARNs, one per streak, and no lifted line
// between them, since no tick passed both guards.
func TestVariantWatcherSaysEachKindOfRefusalWhenItStarts(t *testing.T) {
	dir, rows, _ := relocationShape(t, 30)
	current := dir
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, &fakeDeleter{}, nil,
		func() string { return current }, time.Hour, 20)
	rec := loggingtest.Record(t)

	requireRefusedTicks(t, w, 2, 30, "the relocation")
	current = t.TempDir()
	for i := 0; i < 2; i++ {
		if r := w.tick(context.Background()); !r.Skipped {
			t.Fatalf("an unmounted variants directory's tick: report %+v, want skipped", r)
		}
	}
	current = dir
	requireRefusedTicks(t, w, 2, 30, "the relocation, once the directory is back")

	requireLinesSay(t, rec.Failures(msgVariantRefusal), 2, "a relocation WARN for each of its two streaks")
	requireLinesSay(t, rec.Failures(msgVariantsDirUnavailable), 1, "one mount-loss WARN for its streak")
	requireLinesSay(t, rec.Lines(msgVariantRefusalLifted), 0, "no tick passed both guards")
}

// TestVariantWatcherSaysOnceWhenTheVariantsDirectoryComesBack — the first
// tick that finds the directory again, every rendition in it, passes both
// guards: it says so once, at Info, naming the refusal that ended, and the
// ticks after it say nothing more. Nothing was deleted on either side.
func TestVariantWatcherSaysOnceWhenTheVariantsDirectoryComesBack(t *testing.T) {
	mounted, unmounted, rows := unmountedShape(t, 12)
	current := unmounted
	store := &fakeDeleter{}
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, store, nil,
		func() string { return current }, time.Hour, 20)
	rec := loggingtest.Record(t)

	for i := 0; i < 2; i++ {
		if r := w.tick(context.Background()); !r.Skipped {
			t.Fatalf("the unmounted tick: report %+v, want skipped", r)
		}
	}
	current = mounted
	for i := 0; i < 2; i++ {
		if r := w.tick(context.Background()); r.Skipped || r.Present != 12 || r.Deleted != 0 {
			t.Fatalf("tick %d after the volume came back: report %+v, want 12 present", i+1, r)
		}
	}
	requireLinesSay(t, rec.Lines(msgVariantRefusalLifted), 1, "the lifted line, once",
		"INFO ", " rows=12", " missing=0", " ended=variantsDirUnavailable", " variants_dir="+mounted)
	requireLinesSay(t, rec.Failures(), 1, "the mount-loss WARN, and nothing else", msgVariantsDirUnavailable)
	if got := store.deleted(); len(got) != 0 {
		t.Errorf("rows were deleted: %v", got)
	}
}

// TestVariantWatcherRepeatsItsRefusalOnceADay — a streak that goes on is
// logged again once a day, so a journal read a week later still shows the
// watcher refusing, and never more often than that.
func TestVariantWatcherRepeatsItsRefusalOnceADay(t *testing.T) {
	dir, rows, _ := relocationShape(t, 30)
	w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{rows}}, &fakeDeleter{}, nil, staticDir(dir), time.Hour, 20)
	rec := loggingtest.Record(t)

	requireRefusedTicks(t, w, 1, 30, "the relocation")
	w.refusal.lastLog = w.refusal.lastLog.Add(-(sweepRefusalRepeat - time.Minute))
	requireRefusedTicks(t, w, 1, 30, "the relocation, a minute short of a day on")
	requireLinesSay(t, rec.Failures(msgVariantRefusal), 1, "no repeat a minute short of a day")
	w.refusal.lastLog = w.refusal.lastLog.Add(-2 * time.Minute)
	requireRefusedTicks(t, w, 2, 30, "the relocation, a minute past a day on")
	requireLinesSay(t, rec.Failures(msgVariantRefusal), 2, "one repeat a minute past a day, and no more")
}

// TestRefusalLatch pins the latch both background sweeps share, on its own:
// a streak WARNs when it starts, when it turns into another kind, and once a
// repeat after its last WARN; lift reports the streak it ended exactly once;
// a new streak after a lift logs at once, whenever the last one logged; and
// what status publishes is the streak's kind and start, moved when a streak
// starts or ends and at no other step.
func TestRefusalLatch(t *testing.T) {
	var l refusalLatch[OrphanRefusalKind]
	t0 := time.Now()
	start := func(at time.Duration, k OrphanRefusalKind) OrphanSweepStatus {
		return OrphanSweepStatus{Refusing: k, Since: t0.Add(at)}
	}
	if got := l.status(); got != (OrphanSweepStatus{}) {
		t.Fatalf("a latch no tick has moved publishes %+v, want the zero status", got)
	}
	day := sweepRefusalRepeat
	for _, step := range []struct {
		name       string
		at         time.Duration
		kind       OrphanRefusalKind
		lift       bool
		wantLog    bool
		wantEnded  OrphanRefusalKind
		wantStatus OrphanSweepStatus
	}{
		{name: "a streak starts", at: 0, kind: OrphanRefusalMassOrphans, wantLog: true,
			wantStatus: start(0, OrphanRefusalMassOrphans)},
		{name: "the same kind, an hour on", at: time.Hour, kind: OrphanRefusalMassOrphans,
			wantStatus: start(0, OrphanRefusalMassOrphans)},
		{name: "another kind", at: 2 * time.Hour, kind: OrphanRefusalPartialWalk, wantLog: true,
			wantStatus: start(2*time.Hour, OrphanRefusalPartialWalk)},
		{name: "that kind, a minute short of a day on", at: 2*time.Hour + day - time.Minute, kind: OrphanRefusalPartialWalk,
			wantStatus: start(2*time.Hour, OrphanRefusalPartialWalk)},
		{name: "that kind, a day on", at: 2*time.Hour + day, kind: OrphanRefusalPartialWalk, wantLog: true,
			wantStatus: start(2*time.Hour, OrphanRefusalPartialWalk)},
		{name: "a lift ends the streak", at: 2*time.Hour + day + time.Minute, lift: true, wantEnded: OrphanRefusalPartialWalk},
		{name: "a second lift finds none", at: 2*time.Hour + day + 2*time.Minute, lift: true},
		{name: "a new streak of the old kind logs at once", at: 2*time.Hour + day + 3*time.Minute, kind: OrphanRefusalPartialWalk,
			wantLog: true, wantStatus: start(2*time.Hour+day+3*time.Minute, OrphanRefusalPartialWalk)},
	} {
		now := t0.Add(step.at)
		if step.lift {
			if got := l.lift(); got != step.wantEnded {
				t.Errorf("%s: lift = %q, want %q", step.name, got, step.wantEnded)
			}
		} else if logIt := l.refuse(now, step.kind); logIt != step.wantLog {
			t.Errorf("%s: refuse = %v, want %v", step.name, logIt, step.wantLog)
		}
		if got := l.status(); !got.Since.Equal(step.wantStatus.Since) || got.Refusing != step.wantStatus.Refusing {
			t.Errorf("%s: status %+v, want %+v", step.name, got, step.wantStatus)
		}
	}
}
