package integrity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// waveformCatalog is a waveform directory and rows recording a curve for
// each of n sources, every curve written at its canonical place.
type waveformCatalog struct {
	dir  string
	rows []WaveformSnapshot
}

func newWaveformCatalog(t *testing.T, n int) *waveformCatalog {
	t.Helper()
	c := &waveformCatalog{dir: filepath.Join(t.TempDir(), "waveforms")}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		row := WaveformSnapshot{SourcePath: "Artist/Album " + string(rune('A'+i%26)) + "/" + strings.Repeat("x", i/26+1) + ".flac", SizeBytes: 24}
		row.WaveformPath = CanonicalWaveformPath(c.dir, row)
		writeCurveFixture(t, row.WaveformPath, int(row.SizeBytes))
		c.rows = append(c.rows, row)
	}
	return c
}

// writeCurveFixture writes size bytes at path, making its directories.
func writeCurveFixture(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

// remove deletes the curves of the first n rows.
func (c *waveformCatalog) remove(t *testing.T, n int) {
	t.Helper()
	for _, r := range c.rows[:n] {
		if err := os.Remove(r.WaveformPath); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheWaveformProbeRecognisesCurves: the shared probe, over the waveform
// family, finds a tree of curves healthy, where the variant probe (which looks
// for renditions) reads it as holding nothing; and it reads what an unmounted
// volume looks like as it does for variants.
func TestTheWaveformProbeRecognisesCurves(t *testing.T) {
	c := newWaveformCatalog(t, 2)
	if b := WaveformDirSweepBlock(c.dir); b.Reason != "" || b.Info == nil {
		t.Errorf("a waveform directory holding curves reads %+v, want healthy", b)
	}
	if b := VariantsDirSweepBlock(c.dir); !b.Empty {
		t.Errorf("fixture: the variant probe reads a tree of curves as %+v, want it holding no rendition", b)
	}
	only := t.TempDir()
	writeCurveFixture(t, filepath.Join(only, "Artist", ".DS_Store"), 8)
	writeCurveFixture(t, filepath.Join(only, "Artist", "01.flac.waveform.bin.tmp"), 8)
	if b := WaveformDirSweepBlock(only); !b.Empty || b.Reason != "waveform directory holds no waveform" {
		t.Errorf("a waveform directory holding no curve reads %+v, want it empty of curves", b)
	}
	if b := WaveformDirSweepBlock(filepath.Join(only, "gone")); !b.Missing || b.Reason != "waveform directory is missing" {
		t.Errorf("a waveform directory that is not there reads %+v, want missing", b)
	}
}

// TestEveryWaveformVerdictHasItsAction: DecideWaveform gives each verdict
// LocateWaveform reaches its action, and a curve rendered again its canonical
// place, whether or not the row recorded it there.
func TestEveryWaveformVerdictHasItsAction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "waveforms")
	row := func(src, recorded string, size int64) WaveformSnapshot {
		return WaveformSnapshot{SourcePath: src, WaveformPath: recorded, SizeBytes: size}
	}
	canon := func(src string) string { return CanonicalWaveformPath(dir, WaveformSnapshot{SourcePath: src}) }
	elsewhere := filepath.Join(t.TempDir(), "old-host", "waveforms")

	writeCurveFixture(t, canon("present.flac"), 10)
	writeCurveFixture(t, canon("moved.flac"), 10)
	writeCurveFixture(t, canon("partial.flac"), 4)

	cases := []struct {
		name   string
		row    WaveformSnapshot
		action WaveformAction
		target string
	}{
		{"present", row("present.flac", canon("present.flac"), 10), WaveformServe, ""},
		{"relocated", row("moved.flac", filepath.Join(elsewhere, "moved.flac.waveform.bin"), 10), WaveformAdopt, ""},
		{"missing where it was written", row("gone.flac", canon("gone.flac"), 10), WaveformRegenerate, canon("gone.flac")},
		{"missing at both places", row("gone2.flac", filepath.Join(elsewhere, "gone2.flac.waveform.bin"), 10), WaveformRegenerate, canon("gone2.flac")},
		{"a partial copy at the canonical place", row("partial.flac", filepath.Join(elsewhere, "partial.flac.waveform.bin"), 10), WaveformReplace, canon("partial.flac")},
	}
	for _, tc := range cases {
		d := DecideWaveform(dir, tc.row)
		if d.Action != tc.action || d.Target != tc.target {
			t.Errorf("%s: %v %q (verdict %v), want %v %q", tc.name, d.Action, d.Target, d.Verdict, tc.action, tc.target)
		}
	}
	if d := DecideWaveform("", row("gone.flac", canon("gone.flac"), 10)); d.Action != WaveformLeave {
		t.Errorf("a missing curve with no waveform directory known: %v, want leave (nowhere to render it)", d.Action)
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		locked := filepath.Join(t.TempDir(), "locked")
		writeCurveFixture(t, filepath.Join(locked, "x.flac.waveform.bin"), 10)
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		d := DecideWaveform(dir, row("x.flac", filepath.Join(locked, "x.flac.waveform.bin"), 10))
		if d.Verdict != SidecarUnknown || d.Action != WaveformLeave {
			t.Errorf("a recorded path a stat cannot read: %v %v, want unknown and leave (it proves nothing)", d.Verdict, d.Action)
		}
	}
}

// TestTheJudgmentRefusesWhatLooksLikeAVolumeAwayOrATreeEmptied: the catalog
// judged whole, never the one row. Each refusal admits no row.
func TestTheJudgmentRefusesWhatLooksLikeAVolumeAwayOrATreeEmptied(t *testing.T) {
	ctx := context.Background()
	opts := WaveformJudgeOptions{MaxMissingPercent: WaveformRegenMaxMissingPercent}
	t.Run("one curve gone, another still there", func(t *testing.T) {
		c := newWaveformCatalog(t, 2)
		c.remove(t, 1)
		j := JudgeWaveformCatalog(ctx, c.dir, c.rows, opts)
		if !j.Admits() || !j.Repairable(c.rows[0].SourcePath) || j.Repairable(c.rows[1].SourcePath) || j.Identity() == nil {
			t.Fatalf("judgment %+v: want the one gone curve admitted, the other not", j)
		}
	})
	t.Run("the last curve gone", func(t *testing.T) {
		c := newWaveformCatalog(t, 1)
		c.remove(t, 1)
		requireRefused(t, JudgeWaveformCatalog(ctx, c.dir, c.rows, opts), WaveformRefusalDirAway, c.rows[0].SourcePath)
	})
	t.Run("the directory gone", func(t *testing.T) {
		c := newWaveformCatalog(t, 3)
		if err := os.RemoveAll(c.dir); err != nil {
			t.Fatal(err)
		}
		requireRefused(t, JudgeWaveformCatalog(ctx, c.dir, c.rows, opts), WaveformRefusalDirMissing, c.rows[0].SourcePath)
	})
	t.Run("a volume away: the mountpoint's own directory holds no curve", func(t *testing.T) {
		c := newWaveformCatalog(t, 3)
		if err := os.Rename(c.dir, c.dir+".volume"); err != nil {
			t.Fatal(err)
		}
		writeCurveFixture(t, filepath.Join(c.dir, ".DS_Store"), 8)
		requireRefused(t, JudgeWaveformCatalog(ctx, c.dir, c.rows, opts), WaveformRefusalDirAway, c.rows[0].SourcePath)
	})
	t.Run("emptied past the threshold", func(t *testing.T) {
		c := newWaveformCatalog(t, 40)
		c.remove(t, 12) // 30%, past the floor of ten, while 28 curves remain
		requireRefused(t, JudgeWaveformCatalog(ctx, c.dir, c.rows, opts), WaveformRefusalEmptied, c.rows[0].SourcePath)
	})
	t.Run("below the floor", func(t *testing.T) {
		c := newWaveformCatalog(t, 12)
		c.remove(t, 9)
		if j := JudgeWaveformCatalog(ctx, c.dir, c.rows, opts); !j.Admits() || j.ToRegenerate() != 9 {
			t.Fatalf("nine of twelve gone: %+v, want admitted (under the floor of ten, a ratio means nothing)", j)
		}
	})
	t.Run("partial copies count with the missing", func(t *testing.T) {
		c := newWaveformCatalog(t, 40)
		for _, r := range c.rows[:12] {
			if err := os.Truncate(r.WaveformPath, 3); err != nil {
				t.Fatal(err)
			}
			r.WaveformPath = filepath.Join(t.TempDir(), "elsewhere.waveform.bin")
		}
		rows := append([]WaveformSnapshot(nil), c.rows...)
		for i := range rows[:12] {
			rows[i].WaveformPath = filepath.Join(c.dir+".old", rows[i].SourcePath)
		}
		j := JudgeWaveformCatalog(ctx, c.dir, rows, opts)
		if j.Mismatched != 12 {
			t.Fatalf("fixture: %d partial copies counted, want 12 (%+v)", j.Mismatched, j)
		}
		requireRefused(t, j, WaveformRefusalEmptied, rows[0].SourcePath)
	})
	t.Run("a budget that cannot tell", func(t *testing.T) {
		c := newWaveformCatalog(t, 40)
		c.remove(t, 1)
		j := JudgeWaveformCatalog(ctx, c.dir, c.rows, WaveformJudgeOptions{MaxMissingPercent: 20, MaxRows: 5})
		if !j.Undecided || j.Admits() || j.Looked != 5 {
			t.Fatalf("a judgment over 5 of 40 rows: %+v, want undecided, admitting nothing", j)
		}
	})
	t.Run("a budget that already proves the refusal", func(t *testing.T) {
		c := newWaveformCatalog(t, 40)
		c.remove(t, 12)
		j := JudgeWaveformCatalog(ctx, c.dir, c.rows, WaveformJudgeOptions{MaxMissingPercent: 20, MaxRows: 12})
		requireRefused(t, j, WaveformRefusalEmptied, c.rows[0].SourcePath)
	})
}

func requireRefused(t *testing.T, j WaveformJudgment, kind WaveformRefusalKind, row string) {
	t.Helper()
	if j.Refusal != kind || j.Admits() || j.Repairable(row) || j.Identity() != nil || j.Reason == "" {
		t.Fatalf("judgment %+v: want refused as %s, with its reason, admitting nothing", j, kind)
	}
}

// fakeClock is a clock a test moves.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// guardHarness runs a WaveformRegenGuard over a catalog, recording what it
// admits and each judgment it makes.
type guardHarness struct {
	g      *WaveformRegenGuard
	clock  *fakeClock
	judged chan WaveformJudgment
	mu     sync.Mutex
	admits []string
}

func newGuardHarness(t *testing.T, c *waveformCatalog) *guardHarness {
	t.Helper()
	h := &guardHarness{clock: &fakeClock{now: time.Unix(1_800_000_000, 0)}, judged: make(chan WaveformJudgment, 16)}
	h.g = NewWaveformRegenGuard(context.Background(), WaveformRegenConfig{
		List: func(context.Context) ([]WaveformSnapshot, error) { return c.rows, nil },
		Dir:  func() string { return c.dir },
		Admit: func(p, _ string, id os.FileInfo) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if id != nil {
				h.admits = append(h.admits, p)
			}
		},
		Interval:          time.Hour,
		MaxMissingPercent: WaveformRegenMaxMissingPercent,
		Now:               h.clock.Now,
		OnJudged:          func(j WaveformJudgment) { h.judged <- j },
	})
	t.Cleanup(h.g.Close)
	return h
}

func (h *guardHarness) waitJudged(t *testing.T) WaveformJudgment {
	t.Helper()
	select {
	case j := <-h.judged:
		return j
	case <-time.After(time.Minute):
		t.Fatal("no judgment came")
	}
	return WaveformJudgment{}
}

// waitAdmitted waits, until the test's deadline, for n admissions in all.
func (h *guardHarness) waitAdmitted(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		h.mu.Lock()
		got := append([]string(nil), h.admits...)
		h.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// TestTheGuardJudgesBeforeTheFirstRegenerationAndThenOnceAnInterval: the
// first ask starts a judgment and is answered when it admits; asks while one
// stands are answered from it, never judged again; a curve gone after the
// judgment waits for the next one, which comes only once the interval has
// passed.
func TestTheGuardJudgesBeforeTheFirstRegenerationAndThenOnceAnInterval(t *testing.T) {
	c := newWaveformCatalog(t, 4)
	c.remove(t, 1)
	h := newGuardHarness(t, c)

	if a := h.g.Ask(c.rows[0].SourcePath); a.Kind != WaveformJudging {
		t.Fatalf("first ask: %v, want a judgment started", a.Kind)
	}
	if a := h.g.Ask(c.rows[0].SourcePath); a.Kind != WaveformJudging {
		t.Fatalf("an ask while judging: %v, want it to wait for the judgment", a.Kind)
	}
	if j := h.waitJudged(t); !j.Admits() {
		t.Fatalf("judgment %+v, want admitted", j)
	}
	if got := h.waitAdmitted(t, 1); len(got) != 1 || got[0] != c.rows[0].SourcePath {
		t.Fatalf("admitted %v, want the one row asked for, once", got)
	}
	if a := h.g.Ask(c.rows[0].SourcePath); a.Kind != WaveformAdmitted || a.Identity == nil || a.Dir != c.dir {
		t.Fatalf("an ask while the judgment stands: %+v, want admitted with the directory it saw", a)
	}
	if err := os.Remove(c.rows[1].WaveformPath); err != nil { // rows[1] goes after the judgment
		t.Fatal(err)
	}
	if a := h.g.Ask(c.rows[1].SourcePath); a.Kind != WaveformNotJudged {
		t.Fatalf("a curve gone after the judgment: %v, want it left to the next", a.Kind)
	}
	select {
	case j := <-h.judged:
		t.Fatalf("a second judgment inside the interval: %+v", j)
	default:
	}
	h.clock.Advance(time.Hour)
	if a := h.g.Ask(c.rows[1].SourcePath); a.Kind != WaveformJudging {
		t.Fatalf("an ask past the interval: %v, want a new judgment", a.Kind)
	}
	if j := h.waitJudged(t); !j.Admits() || !j.Repairable(c.rows[1].SourcePath) {
		t.Fatalf("second judgment %+v, want rows[1] admitted", j)
	}
}

// TestTheGuardsRefusalStandsForItsInterval: a judgment that refuses is the
// answer to every ask until the interval has passed, and admits nothing.
func TestTheGuardsRefusalStandsForItsInterval(t *testing.T) {
	c := newWaveformCatalog(t, 1)
	c.remove(t, 1)
	h := newGuardHarness(t, c)
	h.g.Ask(c.rows[0].SourcePath)
	if j := h.waitJudged(t); j.Refusal != WaveformRefusalDirAway {
		t.Fatalf("judgment %+v, want the last curve's deletion refused", j)
	}
	if a := h.g.Ask(c.rows[0].SourcePath); a.Kind != WaveformRefused {
		t.Fatalf("ask after a refusal: %v, want refused", a.Kind)
	}
	if got := h.waitAdmitted(t, 0); len(got) != 0 {
		t.Fatalf("a refusing judgment admitted %v", got)
	}
}

// TestACatalogThatCannotBeReadIsRefused: a listing that fails decides a
// refusal of its own kind, never an admission.
func TestACatalogThatCannotBeReadIsRefused(t *testing.T) {
	c := newWaveformCatalog(t, 2)
	judged := make(chan WaveformJudgment, 1)
	g := NewWaveformRegenGuard(context.Background(), WaveformRegenConfig{
		List:     func(context.Context) ([]WaveformSnapshot, error) { return nil, errors.New("database is locked") },
		Dir:      func() string { return c.dir },
		Admit:    func(string, string, os.FileInfo) { t.Error("admitted over a catalog that could not be read") },
		OnJudged: func(j WaveformJudgment) { judged <- j },
	})
	t.Cleanup(g.Close)
	g.Ask(c.rows[0].SourcePath)
	if j := <-judged; j.Refusal != WaveformRefusalCatalogUnread || !strings.Contains(j.Reason, "database is locked") {
		t.Fatalf("judgment %+v, want refused as the catalog unread", j)
	}
}

// TestAClosedGuardAdmitsNothing: once Close has begun, no ask is answered but
// closed and no admission is told.
func TestAClosedGuardAdmitsNothing(t *testing.T) {
	c := newWaveformCatalog(t, 3)
	c.remove(t, 1)
	release := make(chan struct{})
	listed := make(chan struct{})
	g := NewWaveformRegenGuard(context.Background(), WaveformRegenConfig{
		List: func(ctx context.Context) ([]WaveformSnapshot, error) {
			close(listed)
			<-release
			return c.rows, ctx.Err()
		},
		Dir:   func() string { return c.dir },
		Admit: func(string, string, os.FileInfo) { t.Error("admitted after Close") },
	})
	g.Ask(c.rows[0].SourcePath)
	<-listed
	closed := make(chan struct{})
	go func() { g.Close(); close(closed) }()
	for {
		if a := g.Ask(c.rows[0].SourcePath); a.Kind == WaveformClosed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	<-closed
}

// TestWaveformDirChangedIsTheVariantChecksWords: analyze --gc's check reads
// as it did, in the waveform directory's words, through the shared body.
func TestWaveformDirChangedIsTheVariantChecksWords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "waveforms")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	start := WaveformDirSweepBlock(dir).Info
	if start == nil {
		writeCurveFixture(t, filepath.Join(dir, "a.waveform.bin"), 4)
		start = WaveformDirSweepBlock(dir).Info
	}
	if got := WaveformDirChanged(dir, start); got != "" {
		t.Fatalf("an unchanged directory reads %q", got)
	}
	if err := os.Rename(dir, dir+".away"); err != nil {
		t.Fatal(err)
	}
	if got := WaveformDirChanged(dir, start); got != "the waveform directory went missing during the sweep" {
		t.Errorf("gone: %q", got)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := WaveformDirChanged(dir, start); got != "the waveform directory is no longer the directory this sweep began on" {
		t.Errorf("replaced: %q", got)
	}
	if got := VariantsDirChanged(dir, start); got != VariantsDirReplaced {
		t.Errorf("the variant check's own words moved: %q", got)
	}
}
