package manifest

import (
	"context"
	"testing"
)

// The verdict strings are internal/analyze's (AudioMD5Verified /
// AudioMD5Mismatch). They are spelled out here rather than imported
// because analyze imports manifest, not the other way round — and the
// store's only coupling to them is empty vs non-empty, so it is the
// emptiness these tests actually exercise, not the spelling.
const (
	md5Verified = "verified"
	md5Mismatch = "mismatch"
)

func md5Row(path, state string, retryable bool) AnalysisRow {
	return AnalysisRow{
		SourcePath:        path,
		WaveformPath:      "/tmp/" + path + ".wf",
		WaveformTag:       "deadbeef",
		WaveformSize:      1024,
		SourceMTimeNS:     1,
		SourceSize:        2,
		SchemaVersion:     "wf4",
		CreatedAt:         1,
		AudioMD5State:     state,
		AudioMD5Retryable: retryable,
	}
}

// The counter is the whole mechanism, so pin its transition table
// directly rather than only through the store.
func TestNextAudioMD5Attempts(t *testing.T) {
	at := func(n int) *AnalysisRow { return &AnalysisRow{AudioMD5Attempts: n} }

	for _, tc := range []struct {
		name     string
		existing *AnalysisRow
		fresh    AnalysisRow
		want     int
		why      string
	}{
		{"first transient failure", nil, md5Row("a.flac", "", true), 1,
			"a first attempt starts the count, not the cap"},
		{"transient again", at(1), md5Row("a.flac", "", true), 2, ""},
		{"transient saturates at the cap", at(AudioMD5MaxAttempts),
			md5Row("a.flac", "", true), AudioMD5MaxAttempts,
			"a long outage must not run the number away past the cap"},
		{"permanent jumps straight to the cap", nil, md5Row("a.flac", "", false),
			AudioMD5MaxAttempts,
			"no checksum / odd depth / truncated: counting up would spend two " +
				"more full decodes to reach an answer already in hand"},
		{"verified clears", at(2), md5Row("a.flac", md5Verified, false), 0,
			"a later source edit that re-opens the question deserves a full budget"},
		{"mismatch clears", at(2), md5Row("a.flac", md5Mismatch, false), 0, ""},
		{"verified clears even from the cap", at(AudioMD5MaxAttempts),
			md5Row("a.flac", md5Verified, false), 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextAudioMD5Attempts(tc.existing, &tc.fresh)
			if got != tc.want {
				msg := "nextAudioMD5Attempts = %d, want %d"
				if tc.why != "" {
					msg += " — " + tc.why
				}
				t.Errorf(msg, got, tc.want)
			}
		})
	}
}

// WantsAudioMD5Retry is what the analysis scan-skip gate consults.
// mtime / size / schema are all unchanged for these rows, so it is the
// only thing that can re-open one.
func TestWantsAudioMD5Retry(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  *AnalysisRow
		want bool
	}{
		{"nil row", nil, false},
		{"empty verdict, no attempts", &AnalysisRow{}, true},
		{"empty verdict, under cap", &AnalysisRow{AudioMD5Attempts: AudioMD5MaxAttempts - 1}, true},
		{"empty verdict, at cap", &AnalysisRow{AudioMD5Attempts: AudioMD5MaxAttempts}, false},
		{"verified", &AnalysisRow{AudioMD5State: md5Verified}, false},
		{"mismatch", &AnalysisRow{AudioMD5State: md5Mismatch}, false},
		{"verified at cap", &AnalysisRow{
			AudioMD5State: md5Verified, AudioMD5Attempts: AudioMD5MaxAttempts}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.WantsAudioMD5Retry(); got != tc.want {
				t.Errorf("WantsAudioMD5Retry = %v, want %v", got, tc.want)
			}
		})
	}
}

// End-to-end through the store: a repeatedly-transient row must advance
// and then STOP, and the row must still be written on the tick where the
// counter is the only thing that changed. That last part is the subtle
// one — an exhausted retry is identical to its predecessor in every
// other column, so a no-op-write optimisation that ignored the counter
// would leave the row asking forever.
func TestUpsertAnalysisTransientRetriesAreBoundedAndPersisted(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	const p = "Music/Artist/Album/01.flac"
	if err := s.UpsertTrack(ctx, &Track{Path: p, Size: 2}); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= AudioMD5MaxAttempts+2; i++ {
		if err := s.UpsertAnalysis(ctx, md5Row(p, "", true)); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		got, err := s.GetAnalysis(ctx, p)
		if err != nil || got == nil {
			t.Fatalf("attempt %d: GetAnalysis: %v", i, err)
		}
		want := i
		if want > AudioMD5MaxAttempts {
			want = AudioMD5MaxAttempts
		}
		if got.AudioMD5Attempts != want {
			t.Fatalf("after %d transient failures: attempts = %d, want %d "+
				"(the counter must persist across runs — it is the only thing "+
				"that stops the retry, and the only column that changed)",
				i, got.AudioMD5Attempts, want)
		}
		if wantRetry := i < AudioMD5MaxAttempts; got.WantsAudioMD5Retry() != wantRetry {
			t.Fatalf("after %d failures: WantsAudioMD5Retry = %v, want %v",
				i, got.WantsAudioMD5Retry(), wantRetry)
		}
	}
}

// A permanent verdict must not spend the budget discovering what it
// already knows.
func TestUpsertAnalysisPermanentFailureStopsImmediately(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	const p = "Music/Artist/Album/02.flac"
	if err := s.UpsertTrack(ctx, &Track{Path: p, Size: 2}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpsertAnalysis(ctx, md5Row(p, "", false)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAnalysis(ctx, p)
	if err != nil || got == nil {
		t.Fatalf("GetAnalysis: %v", err)
	}
	if got.WantsAudioMD5Retry() {
		t.Errorf("a file that cannot be verified (no stored checksum, odd bit "+
			"depth, truncated decode) must not be re-analysed: attempts = %d, "+
			"cap = %d", got.AudioMD5Attempts, AudioMD5MaxAttempts)
	}
}

// A real verdict clears the counter, so a later source edit that
// re-opens the question starts from a full budget rather than an
// exhausted one.
func TestUpsertAnalysisVerdictClearsTheCounter(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	const p = "Music/Artist/Album/03.flac"
	if err := s.UpsertTrack(ctx, &Track{Path: p, Size: 2}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpsertAnalysis(ctx, md5Row(p, "", true)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAnalysis(ctx, md5Row(p, md5Verified, false)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAnalysis(ctx, p)
	if err != nil || got == nil {
		t.Fatalf("GetAnalysis: %v", err)
	}
	if got.AudioMD5State != md5Verified {
		t.Errorf("AudioMD5State = %q, want %q", got.AudioMD5State, md5Verified)
	}
	if got.AudioMD5Attempts != 0 {
		t.Errorf("attempts = %d, want 0 — a resolved verdict returns the budget",
			got.AudioMD5Attempts)
	}
}

// The v30 backfill must NOT make every pre-existing row eligible. The
// verification pass is FLAC-only, so an MP3 / M4A / WAV row also carries
// an empty verdict — at 0 they all read as retry-worthy, and since each
// retry is a full re-analysis that would quietly re-decode most of the
// library on upgrade.
//
// Simulated by writing rows the pre-v30 way (attempts left at the
// column default) and running the same UPDATE the migration runs.
func TestAudioMD5AttemptsBackfillSparesFLACAndCapsTheRest(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	rows := []string{
		"Music/A/x.flac",
		"Music/A/y.FLAC", // case must not decide this
		"Music/A/z.mp3",
		"Music/A/w.m4a",
	}
	for _, p := range rows {
		if err := s.UpsertTrack(ctx, &Track{Path: p, Size: 2}); err != nil {
			t.Fatal(err)
		}
		// Straight to the DB so the row looks pre-v30: an empty verdict
		// with the column at its default rather than at the cap
		// UpsertAnalysis would now write.
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO track_analysis
				(source_path, waveform_path, waveform_tag, waveform_size,
				 source_mtime_ns, source_size, schema_version, created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			p, "/tmp/wf", "deadbeef", 10, 1, 2, "wf4", 1); err != nil {
			t.Fatalf("seed %q: %v", p, err)
		}
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE track_analysis SET audio_md5_attempts = ?
		  WHERE lower(source_path) NOT LIKE '%.flac'`,
		AudioMD5MaxAttempts); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	for _, tc := range []struct {
		path      string
		wantRetry bool
	}{
		{"Music/A/x.flac", true},
		{"Music/A/y.FLAC", true},
		{"Music/A/z.mp3", false},
		{"Music/A/w.m4a", false},
	} {
		got, err := s.GetAnalysis(ctx, tc.path)
		if err != nil || got == nil {
			t.Fatalf("GetAnalysis %q: %v", tc.path, err)
		}
		if got.WantsAudioMD5Retry() != tc.wantRetry {
			t.Errorf("%s: WantsAudioMD5Retry = %v, want %v — non-FLAC rows must "+
				"be capped by the backfill (the pass never runs on them, so a "+
				"retry is a full re-decode that can change nothing)",
				tc.path, got.WantsAudioMD5Retry(), tc.wantRetry)
		}
	}
}
