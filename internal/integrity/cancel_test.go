package integrity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// Both watchers tick on runServe's scanCtx, and their store adapters run on
// it too, so a shutdown fails whatever a tick is doing with the cancellation.
// A tick that STOPPED is not one that failed (ctxerr.WithoutCancellation).
// The fakes below do what the store adapters do when a shutdown lands in
// them: cancel, and answer with an error wrapping the cancellation. Each test
// has a twin in which the same call fails on a live context and is still
// reported.

const (
	msgVariantListFailed = "integrity variant sweep: AllVariants failed"
	msgAdoptFailed       = "integrity variant sweep: adopt failed"
	msgDeleteFailed      = "integrity variant sweep: DB delete failed"
	msgOrphanListFailed  = "orphan sidecar sweep: AllVariants failed"
	msgWalkAborted       = "orphan sidecar sweep: walk aborted"
	msgTickComplete      = "orphan sidecar sweep: tick complete"
	msgTickCutShort      = "orphan sidecar sweep: tick cut short"
	cancelVariantID      = "upscaled-v2-176400-24"
)

// TestAVariantSweepStoppedWhileListingReportsNothing: the catalog listing is
// what the shutdown stops.
func TestAVariantSweepStoppedWhileListingReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewVariantWatcher(listerFunc(func() ([]VariantSnapshot, error) {
		cancel()
		return nil, fmt.Errorf("manifest: all variants: %w", ctx.Err())
	}), &fakeDeleter{}, nil, staticDir(t.TempDir()), time.Hour, 20)

	rec := loggingtest.Record(t)
	report := w.tick(ctx)

	mustNotReport(t, rec, msgVariantListFailed)
	if !report.Skipped || !report.Cancelled {
		t.Errorf("report = %+v, want skipped and cancelled", report)
	}
}

// TestAVariantSweepWhoseListingFailsStillReportsIt is the twin.
func TestAVariantSweepWhoseListingFailsStillReportsIt(t *testing.T) {
	w := NewVariantWatcher(&fakeLister{err: errors.New("database is locked")}, &fakeDeleter{}, nil,
		staticDir(t.TempDir()), time.Hour, 20)

	rec := loggingtest.Record(t)
	report := w.tick(context.Background())

	mustReportOnce(t, rec, msgVariantListFailed)
	if !report.Skipped || report.Cancelled {
		t.Errorf("report = %+v, want skipped and not cancelled", report)
	}
}

// TestAVariantSweepStoppedInARowWriteReportsNothing: the shutdown lands in
// one row's write, an adoption in pass one or a deletion in pass two. That
// row is not counted as failed and not reported: the tick ends there, as the
// check at the top of each pass would, with the one summary line saying it
// was cancelled.
func TestAVariantSweepStoppedInARowWriteReportsNothing(t *testing.T) {
	for _, tc := range rowWriteCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{tc.rows(t, dir)}},
				stoppingReconciler{cancel: cancel}, nil, staticDir(dir), time.Hour, 20)

			rec := loggingtest.Record(t)
			report := w.tick(ctx)

			mustNotReport(t, rec, tc.msg)
			if report.Failed != 0 || !report.Cancelled {
				t.Errorf("report = %+v, want no failures and cancelled", report)
			}
		})
	}
}

// TestAVariantSweepWhoseRowWriteFailsStillReportsIt is the twin.
func TestAVariantSweepWhoseRowWriteFailsStillReportsIt(t *testing.T) {
	for _, tc := range rowWriteCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w := NewVariantWatcher(&fakeLister{snapshots: [][]VariantSnapshot{tc.rows(t, dir)}},
				stoppingReconciler{fail: errors.New("database is locked")}, nil, staticDir(dir), time.Hour, 20)

			rec := loggingtest.Record(t)
			report := w.tick(context.Background())

			mustReportOnce(t, rec, tc.msg)
			if report.Failed != 1 || report.Cancelled {
				t.Errorf("report = %+v, want one failure and not cancelled", report)
			}
		})
	}
}

// rowWriteCases are the two row writes a variant sweep makes.
var rowWriteCases = []struct {
	name string
	msg  string
	rows func(t *testing.T, dir string) []VariantSnapshot
}{
	{
		// Recorded under a directory the catalog left, present at its
		// canonical place under this one: pass one adopts it.
		name: "an adoption", msg: msgAdoptFailed,
		rows: func(t *testing.T, dir string) []VariantSnapshot {
			row, _ := relocatedRow(t, filepath.Join(t.TempDir(), "old"), dir, "A/Album/01.flac", cancelVariantID, 64)
			return []VariantSnapshot{row}
		},
	},
	{
		// A row whose sidecar is at neither place, beside a present one so
		// the directory is not empty (an empty one reads as an unmounted
		// volume, and the sweep is skipped): pass two deletes it.
		name: "a deletion", msg: msgDeleteFailed,
		rows: func(t *testing.T, dir string) []VariantSnapshot {
			present, _ := relocatedRow(t, dir, dir, "A/Album/01.flac", cancelVariantID, 64)
			gone := VariantSnapshot{SourcePath: "A/Album/02.flac", VariantID: cancelVariantID,
				SidecarPath: transcode.VariantSidecarPath(filepath.Join(t.TempDir(), "old"), "A/Album/02.flac", cancelVariantID),
				SizeBytes:   64}
			return []VariantSnapshot{present, gone}
		},
	},
}

// TestAnOrphanSweepStoppedWhileListingReportsNothing: the catalog listing is
// what the shutdown stops.
func TestAnOrphanSweepStoppedWhileListingReportsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewOrphanSidecarSweeper(sidecarListerFunc(func(ctx context.Context) ([]VariantSnapshot, error) {
		cancel()
		return nil, fmt.Errorf("manifest: all variants: %w", ctx.Err())
	}), staticDir(t.TempDir()), time.Hour)

	rec := loggingtest.Record(t)
	s.tick(ctx)

	mustNotReport(t, rec, msgOrphanListFailed)
}

// TestAnOrphanSweepWhoseListingFailsStillReportsIt is the twin.
func TestAnOrphanSweepWhoseListingFailsStillReportsIt(t *testing.T) {
	s := NewOrphanSidecarSweeper(sidecarListerFunc(func(context.Context) ([]VariantSnapshot, error) {
		return nil, errors.New("database is locked")
	}), staticDir(t.TempDir()), time.Hour)

	rec := loggingtest.Record(t)
	s.tick(context.Background())

	mustReportOnce(t, rec, msgOrphanListFailed)
}

// TestAnOrphanSweepWhoseWalkIsStoppedReportsNothing: the shutdown lands
// between the listing and the walk, which then stops at its first entry.
// Nothing is reported, and the tick's summary does not call it complete: it
// is cut short, and says the shutdown stopped it.
func TestAnOrphanSweepWhoseWalkIsStoppedReportsNothing(t *testing.T) {
	dir := orphanTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewOrphanSidecarSweeper(sidecarListerFunc(func(context.Context) ([]VariantSnapshot, error) {
		defer cancel()
		return []VariantSnapshot{{SidecarPath: filepath.Join(dir, "live-row.upscaled-v1-96000-24.flac")}}, nil
	}), staticDir(dir), time.Hour)

	rec := loggingtest.Record(t)
	s.tick(ctx)

	mustNotReport(t, rec, msgWalkAborted)
	mustSummarise(t, rec, msgTickCutShort, "cancelled=true")
}

// TestAnOrphanSweepWhoseWalkRunsOutOfTimeStillReportsIt is the twin. The
// walk stops only for its context, so the failure it can have is a
// deadline: a sweep that ran out of time failed, and is reported.
func TestAnOrphanSweepWhoseWalkRunsOutOfTimeStillReportsIt(t *testing.T) {
	dir := orphanTree(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	s := NewOrphanSidecarSweeper(sidecarListerFunc(func(context.Context) ([]VariantSnapshot, error) {
		return []VariantSnapshot{{SidecarPath: filepath.Join(dir, "live-row.upscaled-v1-96000-24.flac")}}, nil
	}), staticDir(dir), time.Hour)

	rec := loggingtest.Record(t)
	s.tick(ctx)

	mustReportOnce(t, rec, msgWalkAborted)
	mustSummarise(t, rec, msgTickCutShort, "cancelled=false")
}

// TestAnOrphanSweepThatFinishesCallsItsTickComplete is the control for the
// two above: a walk that finishes is summarised as complete.
func TestAnOrphanSweepThatFinishesCallsItsTickComplete(t *testing.T) {
	dir := orphanTree(t)
	s := NewOrphanSidecarSweeper(sidecarListerFunc(func(context.Context) ([]VariantSnapshot, error) {
		return []VariantSnapshot{{SidecarPath: filepath.Join(dir, "live-row.upscaled-v1-96000-24.flac")}}, nil
	}), staticDir(dir), time.Hour)

	rec := loggingtest.Record(t)
	s.tick(context.Background())

	mustSummarise(t, rec, msgTickComplete, "cancelled=false")
}

// mustSummarise fails the test unless the tick logged exactly one summary,
// under msg, carrying attr.
func mustSummarise(t *testing.T, rec *loggingtest.Recorder, msg, attr string) {
	t.Helper()
	other := msgTickComplete
	if msg == msgTickComplete {
		other = msgTickCutShort
	}
	if got := rec.Lines(other); len(got) != 0 {
		t.Errorf("the tick was summarised as %q:\n%s", other, strings.Join(got, "\n"))
	}
	got := rec.Lines(msg)
	if len(got) != 1 || !strings.Contains(got[0], " "+attr) {
		t.Errorf("summary lines %q = %q, want one carrying %s", msg, got, attr)
	}
}

// orphanTree is a variants directory with one sidecar in it, so a walk has
// an entry to stop at.
func orphanTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orphan.upscaled-v1-96000-24.flac"), []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// listerFunc adapts a function to VariantLister.
type listerFunc func() ([]VariantSnapshot, error)

func (f listerFunc) AllVariants() ([]VariantSnapshot, error) { return f() }

// sidecarListerFunc adapts a function to SidecarLister.
type sidecarListerFunc func(ctx context.Context) ([]VariantSnapshot, error)

func (f sidecarListerFunc) AllVariants(ctx context.Context) ([]VariantSnapshot, error) { return f(ctx) }

// stoppingReconciler is a reconciler whose every write is where a shutdown
// lands: it cancels, and answers as the store adapter does, with an error
// wrapping the cancellation. With fail set, every write fails with it on a
// live context instead.
type stoppingReconciler struct {
	cancel context.CancelFunc
	fail   error
}

func (s stoppingReconciler) DeleteVariant(string, string) error { return s.stop() }

func (s stoppingReconciler) AdoptVariantSidecar(string, string, string) error { return s.stop() }

// stop answers one write.
func (s stoppingReconciler) stop() error {
	if s.fail != nil {
		return s.fail
	}
	s.cancel()
	return fmt.Errorf("manifest: variant write: %w", context.Canceled)
}

// mustNotReport fails the test for each msg logged at Warn or above.
func mustNotReport(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 0 {
			t.Errorf("a tick that shutdown stopped reported %q:\n%s", m, strings.Join(got, "\n"))
		}
	}
}

// mustReportOnce fails the test for each msg not logged exactly once at
// Warn or above.
func mustReportOnce(t *testing.T, rec *loggingtest.Recorder, msgs ...string) {
	t.Helper()
	for _, m := range msgs {
		if got := rec.Failures(m); len(got) != 1 {
			t.Errorf("a failure on a live context logged %q %d times, want 1:\n%s", m, len(got), strings.Join(got, "\n"))
		}
	}
}
