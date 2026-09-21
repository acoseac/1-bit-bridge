package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedAnalysisFile writes a source file and indexes it with the stat the
// scanner would have recorded, so the manifest row and the disk agree — the
// state every gate here is defined against.
func seedAnalysisFile(t *testing.T, store *manifest.Store, root, rel string, body []byte) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, body, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: rel, Size: info.Size(), ModTime: info.ModTime(),
	}); err != nil {
		t.Fatalf("UpsertTrack %q: %v", rel, err)
	}
}

func isCandidate(res analysisScanResult, rel string) bool {
	for _, c := range res.candidates {
		if c.SourceLibraryRel == rel {
			return true
		}
	}
	return false
}

// TestARefusedSourceStopsBeingOfferedAndComesBackWhenReplaced is the defect
// this feature exists for, end to end at the walk.
//
// 30 truncated FLACs in the operator's library were re-selected on every
// sweep and re-failed identically — 1,385 WARN lines in 7 days on one host,
// and the same 30 failing again at every start on the next. The zero-byte
// skip beside this one could not cover them: its own comment says it "stays
// mtime/size-driven so it can't suppress a real file that's only TRANSIENTLY
// failing (those keep a non-zero size)", and a truncated file keeps a
// non-zero size.
//
// The second half is what makes the first half safe. Replacing the file must
// re-open it with no flag, no button and no operator memory — that is the
// remedy the list exists to prompt, and a debounce that outlived the repair
// would make the feature worse than the bug.
func TestARefusedSourceStopsBeingOfferedAndComesBackWhenReplaced(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolver := bridgefs.New([]string{root})
	out := t.TempDir()

	seedAnalysisFile(t, store, root, "Unknown Artist/Qobuz/06. Jasper Sea.flac", []byte("fLaC-truncated"))
	seedAnalysisFile(t, store, root, "Unknown Artist/Qobuz/07. Fine.flac", []byte("fLaC-complete"))

	res, err := collectAnalysisCandidates(ctx, store, resolver, out, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !isCandidate(res, "Unknown Artist/Qobuz/06. Jasper Sea.flac") {
		t.Fatal("the broken file is not a candidate before any verdict — the rest proves nothing")
	}

	// Three sweeps, three refusals — what the pool records.
	for i := 0; i < manifest.AnalysisFailureThreshold(); i++ {
		if _, err := store.RecordAnalysisFailure(ctx, "Unknown Artist/Qobuz/06. Jasper Sea.flac",
			"sox: decoded 51.5s of 357.2s probed — source appears truncated"); err != nil {
			t.Fatal(err)
		}
	}

	res, err = collectAnalysisCandidates(ctx, store, resolver, out, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if isCandidate(res, "Unknown Artist/Qobuz/06. Jasper Sea.flac") {
		t.Error("a source refused three times running is still being offered — the retry loop is open")
	}
	if res.unreadable != 1 {
		t.Errorf("unreadable = %d, want 1 — the skip must be COUNTED, not silent", res.unreadable)
	}
	if !isCandidate(res, "Unknown Artist/Qobuz/07. Fine.flac") {
		t.Error("the healthy sibling stopped being offered too")
	}

	// --force bypasses the FRESHNESS gate, not unanalyzability — the same
	// posture the zero-byte skip takes, and the reason --retry-failed exists.
	res, err = collectAnalysisCandidates(ctx, store, resolver, out, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if isCandidate(res, "Unknown Artist/Qobuz/06. Jasper Sea.flac") {
		t.Error("--force re-offered a suppressed source; it must not")
	}

	// The operator replaces the file and the next scan records its geometry.
	seedAnalysisFile(t, store, root, "Unknown Artist/Qobuz/06. Jasper Sea.flac",
		[]byte("fLaC-complete-this-time-and-longer"))
	res, err = collectAnalysisCandidates(ctx, store, resolver, out, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !isCandidate(res, "Unknown Artist/Qobuz/06. Jasper Sea.flac") {
		t.Error("a replaced file is still suppressed — the version gate did not re-open it")
	}
	if res.unreadable != 0 {
		t.Errorf("unreadable = %d after the repair, want 0", res.unreadable)
	}
}

// TestALiveStatOverrulesAStaleSuppression — the walk knows something the
// suppression query cannot: what is on disk right now.
//
// Between an operator replacing a broken file and the next scan, the manifest
// row still describes the old one, so the recorded verdicts still "match" in
// SQL. Taking that on trust would hold a repaired file out of analysis until
// a scan happened to run. The walk compares against its live stat and, on any
// disagreement, analyses — resolved toward doing the work, the same direction
// the scanner's "we could not see this path" rule takes.
func TestALiveStatOverrulesAStaleSuppression(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolver := bridgefs.New([]string{root})
	out := t.TempDir()

	rel := "Artist/Album/01 Broken.flac"
	seedAnalysisFile(t, store, root, rel, []byte("fLaC-truncated"))
	for i := 0; i < manifest.AnalysisFailureThreshold(); i++ {
		if _, err := store.RecordAnalysisFailure(ctx, rel, "sox: source appears truncated"); err != nil {
			t.Fatal(err)
		}
	}
	res, err := collectAnalysisCandidates(ctx, store, resolver, out, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if isCandidate(res, rel) {
		t.Fatal("not suppressed to begin with — the rest of this test proves nothing")
	}

	// Replace the file on disk WITHOUT re-indexing: the manifest row is now
	// stale, exactly as it is between a repair and the next scan.
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)),
		[]byte("fLaC-complete-and-noticeably-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = collectAnalysisCandidates(ctx, store, resolver, out, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !isCandidate(res, rel) {
		t.Error("the walk kept suppressing a file that no longer matches the version " +
			"the verdicts were recorded against")
	}
}

// TestRetryFailedClearsTheRecordAndHonoursTheFilter — the explicit way back,
// and the reason it goes through the path form rather than a prefix range:
// --filter is a case-sensitive SUBSTRING match, which no byte range
// expresses.
func TestRetryFailedClearsTheRecordAndHonoursTheFilter(t *testing.T) {
	ctx := context.Background()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, rel := range []string{"Miles Davis/Kind of Blue/01.flac", "Bill Evans/Waltz/01.flac"} {
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: rel, Size: 10}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RecordAnalysisFailure(ctx, rel, "sox: source appears truncated"); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := runAnalyzeRetryFailed(ctx, &stdout, &stderr, store, "Kind of Blue", false); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	rows, err := store.ListUnreadableTracksForAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "Bill Evans/Waltz/01.flac" {
		t.Fatalf("after a filtered retry the record holds %v, want only the unmatched album", rows)
	}
	if !strings.Contains(stdout.String(), "cleared 1") {
		t.Errorf("stdout = %q, want it to report what it cleared", stdout.String())
	}

	// No filter clears the library.
	stdout.Reset()
	if code := runAnalyzeRetryFailed(ctx, &stdout, &stderr, store, "", false); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	rows, err = store.ListUnreadableTracksForAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("record still holds %d row(s) after an unfiltered retry", len(rows))
	}
}

// TestRetryFailedUnderDryRunClearsNothing — a dry run that quietly re-opened
// 30 suppressed sources would be the one thing a dry run must not do, and
// refusing the combination outright would make the operator run the
// destructive form to find out how much it would touch.
func TestRetryFailedUnderDryRunClearsNothing(t *testing.T) {
	ctx := context.Background()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertTrack(ctx, &manifest.Track{Path: "A/01.flac", Size: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordAnalysisFailure(ctx, "A/01.flac", "sox: source appears truncated"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runAnalyzeRetryFailed(ctx, &stdout, &stderr, store, "", true); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	rows, err := store.ListUnreadableTracksForAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("a dry run cleared the record: %d row(s) left, want 1", len(rows))
	}
	if !strings.Contains(stdout.String(), "would clear 1") ||
		!strings.Contains(stdout.String(), "A/01.flac") {
		t.Errorf("stdout = %q, want it to name what it WOULD clear", stdout.String())
	}
}

// TestUnreadableSkipIsNotFoldedIntoMissing — one is a path the bridge could
// not address, the other a file it addressed and could not read. They have
// different remedies, and the run summary used to print `missing` under the
// word "unreadable", so a reader who saw a number there learned the wrong
// thing about their library.
func TestUnreadableSkipIsNotFoldedIntoMissing(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	seedAnalysisFile(t, store, root, "present/but-broken.flac", []byte("fLaC-truncated"))
	// Indexed, never on disk: unresolvable, not unreadable.
	if err := store.UpsertTrack(ctx, &manifest.Track{Path: "absent/never-there.flac", Size: 99}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < manifest.AnalysisFailureThreshold(); i++ {
		if _, err := store.RecordAnalysisFailure(ctx, "present/but-broken.flac",
			"sox: source appears truncated"); err != nil {
			t.Fatal(err)
		}
	}
	res, err := collectAnalysisCandidates(ctx, store, bridgefs.New([]string{root}), t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.unreadable != 1 || res.missing != 1 {
		t.Errorf("unreadable = %d, missing = %d; want 1 and 1 counted separately",
			res.unreadable, res.missing)
	}
}

// TestAFreshWaveformOutranksASuppression pins the ORDER of the two gates, and
// with it the agreement between the walk and Store.AnalysisCoverage.
//
// Both answer "suppressed AND NOT analysed-fresh": the SQL says so in its
// CASE, the walk says so by checking suppression after the freshness gate. The
// state is unreachable in production — a successful analysis clears the
// strikes — but two surfaces that agree only because nothing can reach the
// disagreement agree by luck, and the next change to either gate's position
// would not notice.
func TestAFreshWaveformOutranksASuppression(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rel := "Artist/Album/01 Contradictory.flac"
	seedAnalysisFile(t, store, root, rel, []byte("fLaC-body"))
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAnalysis(ctx, manifest.AnalysisRow{
		SourcePath:    rel,
		WaveformPath:  filepath.Join(t.TempDir(), "w.bin"),
		WaveformTag:   "deadbeef",
		WaveformSize:  42,
		SourceMTimeNS: info.ModTime().UnixNano(),
		SourceSize:    info.Size(),
		SchemaVersion: analyze.WaveformSchemaVersion,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < manifest.AnalysisFailureThreshold(); i++ {
		if _, err := store.RecordAnalysisFailure(ctx, rel, "sox: source appears truncated"); err != nil {
			t.Fatal(err)
		}
	}

	res, err := collectAnalysisCandidates(ctx, store, bridgefs.New([]string{root}), t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.skipped != 1 || res.unreadable != 0 {
		t.Errorf("skipped = %d, unreadable = %d; want 1 and 0 — a track with a fresh "+
			"waveform is up to date, and the coverage query counts it the same way",
			res.skipped, res.unreadable)
	}

	// The coverage query must reach the same verdict on the same row.
	cov, err := store.AnalysisCoverage(ctx, analyze.WaveformSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	if cov.UnreadableExcluded != 0 {
		t.Errorf("coverage excluded %d as unreadable, want 0 — it is analysed",
			cov.UnreadableExcluded)
	}
}
