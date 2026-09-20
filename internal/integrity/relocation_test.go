package integrity

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// The 2026-09-20 field report, as tests.
//
// bridge.db was copied from a host whose variants dir was
// /mnt/bridge-variants to one where it is /srv/bridge-variants, and all
// 10,248 sidecars (259.7 GiB) were copied byte-identical to the new dir.
// The boot sweep stat'd every row's recorded path, got ENOENT for all of
// them, and deleted every row without a log line; the mount-loss guard
// did not fire because the configured dir was healthy and full. Three
// minutes later the auto-optimize sweeper began re-rendering over the
// files. Every test in this file describes a state that sweep should
// have recognised.

// captureLogs redirects the default slog handler into a buffer. The
// package logger resolves slog.Default() at log time, so swapping the
// default is enough. Tests that capture logs must not run in parallel
// with anything else that logs — none in this package do.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// logLines returns the captured lines containing needle.
func logLines(buf *bytes.Buffer, needle string) []string {
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}

// relocatedCatalog seeds n rows in the incident's shape: recorded under
// oldDir (never created), files byte-identical at their canonical place
// under newDir. Returns the rows and their canonical paths.
func relocatedCatalog(t *testing.T, oldDir, newDir string, n int) ([]VariantSnapshot, []string) {
	t.Helper()
	rows := make([]VariantSnapshot, n)
	canonical := make([]string, n)
	for i := range rows {
		source := fmt.Sprintf("Artist %d/Album/%02d - Track.flac", i%7, i)
		rows[i], canonical[i] = relocatedRow(t, oldDir, newDir, source, "upscaled-v2-176400-24", 100+i)
	}
	return rows, canonical
}

// TestVariantWatcher_adoptsARelocatedCatalog is the incident with the
// fix in place: every row is adopted at its canonical path, nothing is
// deleted, nothing reaches the wire, and the tick says so in one line.
func TestVariantWatcher_adoptsARelocatedCatalog(t *testing.T) {
	buf := captureLogs(t)
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	newDir := filepath.Join(t.TempDir(), "srv", "bridge-variants")
	rows, canonical := relocatedCatalog(t, oldDir, newDir, 40)

	lister := &fakeLister{snapshots: [][]VariantSnapshot{rows}}
	store := &fakeDeleter{}
	publisher := &fakePublisher{}
	w := NewVariantWatcher(lister, store, publisher.publish, staticDir(newDir), time.Hour, 20)

	report := awaitBootSweep(t, w, 0)
	if report.Adopted != 40 || report.Present != 0 || report.Refused != 0 {
		t.Fatalf("report = %+v, want 40 adopted and nothing else", report)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Fatalf("DeleteVariant called for %v — a relocated row must never be reaped", got)
	}
	adopted := store.adopted()
	if len(adopted) != 40 {
		t.Fatalf("adopted %d rows, want 40", len(adopted))
	}
	for i, r := range rows {
		want := r.SourcePath + "|" + r.VariantID + "|" + canonical[i]
		if adopted[i] != want {
			t.Errorf("adoption[%d] = %q, want %q", i, adopted[i], want)
		}
	}
	if publisher.eventCount() != 0 {
		t.Errorf("published %d upscale.deleted events for a relocation; a path-only change is nothing to a client", publisher.eventCount())
	}

	// The summary line: one, at Info, carrying the counts.
	summaries := logLines(buf, "integrity variant sweep: summary")
	if len(summaries) != 1 {
		t.Fatalf("want exactly one summary line, got %d:\n%s", len(summaries), buf.String())
	}
	for _, want := range []string{"level=INFO", "rows=40", "adopted=40", "deleted=0", "refused=0"} {
		if !strings.Contains(summaries[0], want) {
			t.Errorf("summary %q lacks %q", summaries[0], want)
		}
	}
	// Per-row adoption lines: the first logSampleCap at Info, the rest at
	// Debug — a 10k-row relocation must not write 10k Info lines.
	perRow := logLines(buf, "adopted relocated sidecar")
	if len(perRow) != 40 {
		t.Fatalf("want a per-row line for each adoption (40), got %d", len(perRow))
	}
	var info, debug int
	for _, l := range perRow {
		switch {
		case strings.Contains(l, "level=INFO"):
			info++
		case strings.Contains(l, "level=DEBUG"):
			debug++
		}
	}
	if info != logSampleCap || debug != 40-logSampleCap {
		t.Errorf("per-row adoption lines: %d at Info and %d at Debug, want %d and %d", info, debug, logSampleCap, 40-logSampleCap)
	}
}

// TestVariantWatcher_doesNotAdoptAPartialCopy — a copy still running:
// the canonical file exists but is shorter than the row says. Neither
// adopted nor deleted; the next tick asks again. And a row whose file
// is at neither place, in the same tick, is still reaped (below the
// floor, so the guard stays out of it).
func TestVariantWatcher_doesNotAdoptAPartialCopy(t *testing.T) {
	buf := captureLogs(t)
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	newDir := t.TempDir()
	partial, canonical := relocatedRow(t, oldDir, newDir, "A/Album/01.flac", "upscaled-v2-176400-24", 500)
	if err := os.Truncate(canonical, 250); err != nil { // the copy is half done
		t.Fatal(err)
	}
	gone := VariantSnapshot{SourcePath: "A/Album/02.flac", VariantID: "upscaled-v2-176400-24",
		SidecarPath: transcode.VariantSidecarPath(oldDir, "A/Album/02.flac", "upscaled-v2-176400-24"), SizeBytes: 7}

	lister := &fakeLister{snapshots: [][]VariantSnapshot{{partial, gone}}}
	store := &fakeDeleter{}
	publisher := &fakePublisher{}
	w := NewVariantWatcher(lister, store, publisher.publish, staticDir(newDir), time.Hour, 20)

	report := awaitBootSweep(t, w, 1)
	if report.Mismatched != 1 || report.Adopted != 0 {
		t.Fatalf("report = %+v, want 1 mismatched, 0 adopted", report)
	}
	if got := store.adopted(); len(got) != 0 {
		t.Errorf("adopted a partial copy: %v", got)
	}
	if got := store.deleted(); len(got) != 1 || got[0] != "A/Album/02.flac|upscaled-v2-176400-24" {
		t.Errorf("deleted = %v, want only the row missing at both locations", got)
	}
	if lines := logLines(buf, "different size"); len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") {
		t.Errorf("want one WARN about the size mismatch, got %v", lines)
	}
	if lines := logLines(buf, "integrity variant sweep: summary"); len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") {
		t.Errorf("a tick that deleted a row must summarise at WARN, got %v", lines)
	}
}

// TestVariantWatcher_refusesAMassDeleteWhileTheTreeHoldsSidecars is
// the guard for the case adoption cannot rescue: the rows point at the
// old host, the files are in the tree but not where the layout says
// (or the copy is not there yet), and one tick would reap most of the
// catalog. It refuses, warns, and deletes nothing — and the same
// catalog with the guard disabled, or over a tree that holds no
// sidecars, is reaped as it always was.
func TestVariantWatcher_refusesAMassDeleteWhileTheTreeHoldsSidecars(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	const n = 30
	catalog := func() []VariantSnapshot {
		rows := make([]VariantSnapshot, n)
		for i := range rows {
			source := fmt.Sprintf("Artist/Album/%02d.flac", i)
			rows[i] = VariantSnapshot{SourcePath: source, VariantID: "upscaled-v2-176400-24",
				SidecarPath: transcode.VariantSidecarPath(oldDir, source, "upscaled-v2-176400-24"), SizeBytes: 10}
		}
		return rows
	}
	// A tree that holds real sidecars, but under a layout the probe does
	// not know (a flat dump of the old tree's files).
	treeWithSidecars := func(t *testing.T) string {
		dir := t.TempDir()
		for i := 0; i < 5; i++ {
			name := fmt.Sprintf("%02d.flac.upscaled-v2-176400-24.flac", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	treeWithJunk := func(t *testing.T) string {
		dir := t.TempDir()
		writeDecoySidecar(t, dir)
		return dir
	}

	cases := []struct {
		name        string
		dir         func(*testing.T) string
		percent     int
		wantDeleted int
		wantRefused int
	}{
		{"refused: every row missing, sidecars in the tree", treeWithSidecars, 20, 0, n},
		{"proceeds: the tree holds no sidecars", treeWithJunk, 20, n, 0},
		{"proceeds: guard disabled at 100", treeWithSidecars, 100, n, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			dir := tc.dir(t)
			lister := &fakeLister{snapshots: [][]VariantSnapshot{catalog()}}
			store := &fakeDeleter{}
			publisher := &fakePublisher{}
			w := NewVariantWatcher(lister, store, publisher.publish, staticDir(dir), time.Hour, tc.percent)

			report := awaitBootSweep(t, w, tc.wantDeleted)
			if report.Refused != tc.wantRefused {
				t.Fatalf("report = %+v, want refused=%d", report, tc.wantRefused)
			}
			if got := len(store.deleted()); got != tc.wantDeleted {
				t.Errorf("DeleteVariant called %d times, want %d", got, tc.wantDeleted)
			}
			refusals := logLines(buf, "refusing to delete rows")
			if tc.wantRefused > 0 {
				if len(refusals) != 1 || !strings.Contains(refusals[0], "level=WARN") {
					t.Fatalf("want one WARN refusal, got %v", refusals)
				}
				if !strings.Contains(refusals[0], "30 of 30 rows (100%)") {
					t.Errorf("the refusal should name the numbers: %s", refusals[0])
				}
				if publisher.eventCount() != 0 {
					t.Errorf("a refused tick published %d events", publisher.eventCount())
				}
			} else if len(refusals) != 0 {
				t.Errorf("unexpected refusal: %v", refusals)
			}
			summaries := logLines(buf, "integrity variant sweep: summary")
			if len(summaries) != 1 || !strings.Contains(summaries[0], "level=WARN") {
				t.Errorf("a tick that deleted or refused must summarise at WARN, got %v", summaries)
			}
		})
	}
}

// TestVariantWatcher_belowTheFloorTheGuardStaysOut — one deleted
// sidecar in a small catalog is 25%, and it is not a relocation.
func TestVariantWatcher_belowTheFloorTheGuardStaysOut(t *testing.T) {
	dir := t.TempDir()
	// A real sidecar in the tree, so the guard's third condition holds and
	// only the floor keeps it out.
	if err := os.WriteFile(filepath.Join(dir, "x.flac.upscaled-v2-176400-24.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := make([]VariantSnapshot, 4)
	for i := range rows {
		rows[i] = VariantSnapshot{SourcePath: fmt.Sprintf("A/%d.flac", i), VariantID: "upscaled-v2-176400-24",
			SidecarPath: filepath.Join(dir, fmt.Sprintf("%d.flac", i)), SizeBytes: 1}
		if i > 0 {
			if err := os.WriteFile(rows[i].SidecarPath, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	lister := &fakeLister{snapshots: [][]VariantSnapshot{rows}}
	store := &fakeDeleter{}
	w := NewVariantWatcher(lister, store, nil, staticDir(dir), time.Hour, 20)
	report := awaitBootSweep(t, w, 1)
	if report.Refused != 0 || report.Present != 3 {
		t.Fatalf("report = %+v, want the one missing row reaped and the rest present", report)
	}
}

// TestVariantWatcher_healthyTickLogsOneInfoLine — silence was the
// report's first finding. A tick over a healthy catalog writes exactly
// one line, at Info; a tick over an empty catalog writes nothing.
func TestVariantWatcher_healthyTickLogsOneInfoLine(t *testing.T) {
	buf := captureLogs(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "ok.flac")
	if err := os.WriteFile(p, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	lister := &fakeLister{snapshots: [][]VariantSnapshot{{{SourcePath: "A/1.flac", VariantID: "upscaled-v2-176400-24", SidecarPath: p, SizeBytes: 2}}}}
	w := NewVariantWatcher(lister, &fakeDeleter{}, nil, staticDir(dir), time.Hour, 20)
	if r := w.tick(context.Background()); r.Present != 1 || r.Rows != 1 {
		t.Fatalf("report = %+v", r)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[0], "present=1") {
		t.Fatalf("want exactly one Info summary line, got:\n%s", buf.String())
	}

	buf.Reset()
	empty := NewVariantWatcher(&fakeLister{}, &fakeDeleter{}, nil, staticDir(dir), time.Hour, 20)
	if r := empty.tick(context.Background()); r.Rows != 0 {
		t.Fatalf("report = %+v", r)
	}
	if buf.Len() != 0 {
		t.Errorf("an empty catalog has nothing to summarise, got:\n%s", buf.String())
	}
}

// TestVariantWatcher_adoptFailureKeepsTheRow — the UPDATE failed; the
// file is there and the row still points at the old path. Not a
// deletion under any reading; the next tick asks again.
func TestVariantWatcher_adoptFailureKeepsTheRow(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	newDir := t.TempDir()
	rows, _ := relocatedCatalog(t, oldDir, newDir, 3)
	lister := &fakeLister{snapshots: [][]VariantSnapshot{rows}}
	store := &fakeDeleter{adoptErr: fmt.Errorf("database is locked")}
	w := NewVariantWatcher(lister, store, nil, staticDir(newDir), time.Hour, 20)
	report := awaitBootSweep(t, w, 0)
	if report.Failed != 3 || report.Adopted != 0 {
		t.Fatalf("report = %+v, want 3 failed adoptions and nothing deleted", report)
	}
	if got := store.deleted(); len(got) != 0 {
		t.Errorf("deleted %v after a failed adoption", got)
	}
}

// TestOrphanSweeperKnowsARelocatedCatalogsCanonicalPaths is the forward
// sweep's half of the report: a database copied to a host where the
// variants dir has a new path knew NOTHING under that dir, so the
// background orphan sweep — had it been enabled — would have unlinked
// the byte-identical tree chunk by chunk. With the canonical spelling in
// the known set the relocated files are kept, and a true orphan beside
// them is still reaped (the positive control).
func TestOrphanSweeperKnowsARelocatedCatalogsCanonicalPaths(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	newDir := t.TempDir()
	rows, canonical := relocatedCatalog(t, oldDir, newDir, 12)
	orphan := filepath.Join(newDir, "Artist 0", "Album", "nobody-owns-me.flac")
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ageFixtures(t, newDir)

	s := NewOrphanSidecarSweeper(&fakeSidecarLister{rows: rows}, staticDir(newDir), time.Hour)
	s.gracePeriodForTest = time.Nanosecond
	if n := s.tick(context.Background()); n != 1 {
		t.Fatalf("tick unlinked %d files, want exactly the one orphan", n)
	}
	for _, p := range canonical {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("relocated sidecar %s was unlinked by the forward sweep: %v", p, err)
		}
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("the true orphan survived: %v", err)
	}
}
