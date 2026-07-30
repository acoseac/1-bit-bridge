package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestBuildResultRowsKeepsArtistsAndTitlesAligned guards against a display bug
// that was silent and looked like real data.
//
// Two earlier shapes got this wrong. The first appended to a shared Artists
// slice only when a recording carried one, so a single artist-less recording
// shifted every later entry and printed a real artist name beside the wrong
// title. The fix kept the slices in step by hand; the current shape keeps one
// struct per recording instead, which makes the misalignment unrepresentable
// rather than merely tested-against. The test remains because the property is
// what matters, not the mechanism that currently enforces it.
func TestBuildResultRowsKeepsArtistsAndTitlesAligned(t *testing.T) {
	results := []acoustid.Result{{
		ID:    "acoustid-1",
		Score: 0.99,
		Recordings: []acoustid.Recording{
			{ID: "r1", Title: "First", Sources: 9, Artists: []acoustid.Artist{{ID: "a1", Name: "Artist One"}}},
			{ID: "r2", Title: "Second (no artist)", Sources: 2}, // the shifting case
			{ID: "r3", Title: "Third", Sources: 40, Artists: []acoustid.Artist{{ID: "a3", Name: "Artist Three"}}},
		},
	}}

	rows := buildResultRows(results)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if len(row.Recordings) != 3 {
		t.Fatalf("got %d recordings, want one row per recording", len(row.Recordings))
	}

	// The load-bearing assertion: the third recording's artist must still line
	// up with the third title, despite the artist-less recording before it.
	want := []fingerprintRecordingRow{
		{Artist: "Artist One", Title: "First", Sources: 9},
		{Artist: "?", Title: "Second (no artist)", Sources: 2},
		{Artist: "Artist Three", Title: "Third", Sources: 40},
	}
	for j, w := range want {
		if row.Recordings[j] != w {
			t.Errorf("recording %d = %+v, want %+v", j, row.Recordings[j], w)
		}
	}
	if row.ID != "acoustid-1" || row.Score != 0.99 {
		t.Errorf("scalar fields lost: %+v", row)
	}
}

func TestBuildResultRowsEmpty(t *testing.T) {
	if rows := buildResultRows(nil); len(rows) != 0 {
		t.Fatalf("got %d rows, want none", len(rows))
	}
}

// lateCancelCtx reports cancellation only from the Nth Err() call onward.
//
// The interleaving under test — cancelled DURING the final file — cannot be
// produced by a timer without a race: the work on a rejected file finishes in
// microseconds, so any sleep long enough to be reliable is also long enough to
// miss. Counting Err() calls makes it exact: call 1 is the loop's pre-check
// (must see a live context), call 2 is the post-append check this guards.
type lateCancelCtx struct {
	context.Context
	calls *int
	after int
}

func (c *lateCancelCtx) Err() error {
	*c.calls++
	if *c.calls > c.after {
		return context.Canceled
	}
	return nil
}

// TestRunFingerprintFilesReportsCancellationOnTheLastFile pins the exit-code
// contract at its weakest point.
//
// Cancellation used to be checked only at the TOP of the next iteration, so an
// interrupt during the last — or only — file fell out of the loop normally and
// reported a clean run. A script wrapping this would see exit 0 for a batch
// that was cut short, which is exactly the distinction the 130 exists to make.
func TestRunFingerprintFilesReportsCancellationOnTheLastFile(t *testing.T) {
	dir := t.TempDir()
	// Exists but is not decodable audio, so fingerprintOne returns a report
	// promptly without consulting the context itself — leaving the loop's own
	// two checks as the only Err() callers.
	path := filepath.Join(dir, "not-audio.flac")
	if err := os.WriteFile(path, []byte("definitely not a flac"), 0o600); err != nil {
		t.Fatal(err)
	}

	calls := 0
	ctx := &lateCancelCtx{Context: context.Background(), calls: &calls, after: 1}
	reports, interrupted := runFingerprintFiles(ctx, nil, []string{path}, time.Second, 0)

	if len(reports) != 1 {
		t.Fatalf("got %d reports, want the file's result kept", len(reports))
	}
	if !interrupted {
		t.Fatal("cancellation during the final file must be reported, or a run that was cut short exits 0")
	}
	if calls < 2 {
		t.Fatalf("expected a post-append cancellation check; Err() called %d time(s)", calls)
	}
}

// TestRunFingerprintFilesKeepsPartialResults — on a long run over a network
// mount the measured results are the expensive part. A Ctrl+C must not throw
// them away, or the egress has to be paid again.
func TestRunFingerprintFilesKeepsPartialResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reports, interrupted := runFingerprintFiles(ctx, nil, []string{"a.flac", "b.flac"}, time.Second, 0)
	if !interrupted {
		t.Error("a pre-cancelled context must report interrupted")
	}
	if reports == nil {
		t.Error("reports must be a usable empty slice, not nil")
	}
}

// TestFingerprintLengthDefaultsAgree pins two constants that must stay equal
// across a layer boundary they cannot share.
//
// internal/config must not import a feature package, so it carries its own
// DefaultFingerprintLengthSeconds. acoustid.DefaultLengthSeconds is the source
// of truth (it is fpcalc's own default and the window AcoustID's reference
// fingerprints were built at). cmd/bridge imports both, so this is the one
// place the equality can be asserted — without it the duplication could drift
// silently and quietly change match confidence.
func TestFingerprintLengthDefaultsAgree(t *testing.T) {
	if config.DefaultFingerprintLengthSeconds != acoustid.DefaultLengthSeconds {
		t.Fatalf("config.DefaultFingerprintLengthSeconds = %d but acoustid.DefaultLengthSeconds = %d;\n"+
			"the acoustid value is the source of truth (fpcalc's default and AcoustID's\n"+
			"reference window) — update config to match, not the other way round.",
			config.DefaultFingerprintLengthSeconds, acoustid.DefaultLengthSeconds)
	}
}
