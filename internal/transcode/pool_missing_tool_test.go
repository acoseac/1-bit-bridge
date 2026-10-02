package transcode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// missingToolSourceSize and missingToolSourceMTime are the file version every
// job in these tests converts: the track row's size and mtime_ns, which the
// suppression predicate compares a strike against.
const (
	missingToolSourceSize  = int64(4096)
	missingToolSourceMTime = int64(1_700_000_000_000_000_000)
)

// announcingPool is a one-worker pool over a real store holding a track row
// for each source a test converts, with a recorder of every log line and a
// channel carrying each job's announcement: "failed: <reason>" or "done".
type announcingPool struct {
	pool      *Pool
	store     *manifest.Store
	log       *loggingtest.Recorder
	announced chan string
}

func newAnnouncingPool(t *testing.T, rels ...string) *announcingPool {
	t.Helper()
	log := loggingtest.Record(t)
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })
	for _, rel := range rels {
		if err := store.UpsertTrack(context.Background(), &manifest.Track{
			Path:    rel,
			Size:    missingToolSourceSize,
			ModTime: time.Unix(0, missingToolSourceMTime).UTC(),
		}); err != nil {
			t.Fatalf("seed track %q: %v", rel, err)
		}
	}
	p := NewPool(store, 1, 4)
	// Registered after the store's Close, so it runs first.
	t.Cleanup(p.Stop)
	announced := make(chan string, 16)
	p.SetOnJobFailed(func(_, _, errMsg string, _ float64, _ uuid.UUID, _ time.Time) {
		announced <- "failed: " + errMsg
	})
	p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) {
		announced <- "done"
	})
	return &announcingPool{pool: p, store: store, log: log, announced: announced}
}

// newToolFreePool is an announcingPool running the REAL runner, Run, with
// PATH set to pathDir.
func newToolFreePool(t *testing.T, pathDir string, rels ...string) *announcingPool {
	t.Helper()
	t.Setenv("PATH", pathDir)
	resetFFmpegSnapshotForTest()
	t.Cleanup(resetFFmpegSnapshotForTest)
	return newAnnouncingPool(t, rels...)
}

// sourceFile writes a stand-in source (no tool here reads its bytes) at the
// version the track row records, as a scan leaves it, and returns a spec for
// it at that version: Run renders a file only while it is still the version
// its row records.
func sourceFile(t *testing.T, rel string) JobSpec {
	t.Helper()
	abs := filepath.Join(t.TempDir(), filepath.Base(rel))
	if err := os.WriteFile(abs, make([]byte, missingToolSourceSize), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	at := time.Unix(0, missingToolSourceMTime)
	if err := os.Chtimes(abs, at, at); err != nil {
		t.Fatalf("stamp source: %v", err)
	}
	return JobSpec{
		SourceAbsPath:    abs,
		SourceLibraryRel: rel,
		SourceSize:       missingToolSourceSize,
		SourceMTimeNS:    missingToolSourceMTime,
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
		TempDir:          t.TempDir(),
	}
}

// dsdSource is sourceFile for a DSD64 stereo .dsf, rendered to the faithful
// PCM tier.
func dsdSource(t *testing.T, rel string) JobSpec {
	t.Helper()
	s := sourceFile(t, rel)
	s.SourceIsDSD = true
	s.Kind = JobKindPCMRender
	s.SourceSampleRate = 2822400
	s.SourceChannels = 2
	return s
}

// run sends spec n times, one after another, and returns each job's
// announcement. Each retry is sent once the job before it has been announced,
// which is when its path is free again (#988).
func (a *announcingPool) run(t *testing.T, spec JobSpec, n int) []string {
	t.Helper()
	var got []string
	for i := 0; i < n; i++ {
		if err := a.pool.Enqueue(spec); err != nil {
			t.Fatalf("Enqueue #%d: %v", i+1, err)
		}
		select {
		case msg := <-a.announced:
			got = append(got, msg)
		case <-time.After(30 * time.Second):
			t.Fatalf("job #%d announced nothing within 30s (stats %+v)", i+1, a.pool.Stats())
		}
	}
	return got
}

// strikes reports whether the source is suppressed from the candidate
// queries, and how many strike records the operator's retry action clears
// under its folder (which it does).
func (a *announcingPool) strikes(t *testing.T, rel string) (suppressed int, records int64) {
	t.Helper()
	ctx := context.Background()
	suppressed, err := a.store.SuppressedVariantFailureCount(ctx)
	if err != nil {
		t.Fatalf("SuppressedVariantFailureCount: %v", err)
	}
	records, err = a.store.ClearVariantFailuresUnderPrefix(ctx, filepath.ToSlash(filepath.Dir(rel)))
	if err != nil {
		t.Fatalf("ClearVariantFailuresUnderPrefix: %v", err)
	}
	return suppressed, records
}

// lines returns the recorded lines carrying msg at level ("WARN", "INFO" or
// "DEBUG").
func (a *announcingPool) lines(level, msg string) []string {
	var out []string
	for _, l := range a.log.Lines(msg) {
		if strings.HasPrefix(l, level+" ") {
			out = append(out, l)
		}
	}
	return out
}

// TestAJobThatCannotRunItsToolStrikesNoSource drives the real pool and the
// real runner on a PATH that holds none of sox, ffmpeg or ffprobe: the state
// of a job queued moments before the tool went away, which the live upscale
// gate cannot stop (it re-probes every 30 s and refuses only new work).
//
// Each job fails, is counted and is announced, as before. What must not
// happen is a strike: a strike is a statement about the SOURCE, and three of
// them take it out of every candidate query for 30 days, which installing
// the tool does not undo (the strike is keyed on the file's size and mtime).
// The pool struck on any runner error that was not a timeout or a shutdown.
//
// The three cases are the three shapes a missing tool takes on the way out
// of the runner: exec's own lookup failure on the sox-direct route (a FLAC),
// the same failure after the ALAC route's decoder probe failed open (sox
// could not be probed, so it is tried), and the DSD route the ffmpeg probe
// refuses before anything runs (ErrDSDDecodeUnavailable).
func TestAJobThatCannotRunItsToolStrikesNoSource(t *testing.T) {
	for _, tc := range []struct {
		name, rel, tool string
		spec            func(*testing.T, string) JobSpec
	}{
		{name: "sox missing, FLAC", rel: "Music/Album/01.flac", tool: toolSox, spec: sourceFile},
		{name: "sox missing, ALAC", rel: "Music/Album/01.m4a", tool: toolSox, spec: sourceFile},
		{name: "ffmpeg missing, DSD", rel: "Music/Album/01.dsf", tool: toolFFmpeg, spec: dsdSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newToolFreePool(t, t.TempDir(), tc.rel)
			got := a.run(t, tc.spec(t, tc.rel), 3)

			if st := a.pool.Stats(); st.Failed != 3 || st.Done != 0 || st.Inflight != 0 {
				t.Errorf("Stats() = %+v, want 3 failed and nothing in flight: the jobs still fail", st)
			}
			for i, g := range got {
				if !strings.HasPrefix(g, "failed: ") {
					t.Errorf("job #%d announced %q, want a failure", i+1, g)
				}
			}
			if suppressed, records := a.strikes(t, tc.rel); suppressed != 0 || records != 0 {
				t.Errorf("after three jobs that could not run %s: %d source(s) suppressed and %d strike "+
					"record(s), want none: a job that could not run a tool reached no verdict on its file",
					tc.tool, suppressed, records)
			}
			// One outage, one warning: the M-SEARCH rule. Each job's failure
			// still reached its jobFailed event above.
			if warns := a.log.Failures(); len(warns) != 1 || !strings.Contains(warns[0], "tool="+tc.tool) {
				t.Errorf("three jobs that failed for want of %s logged these warnings:\n%s\nwant exactly one, naming the tool",
					tc.tool, strings.Join(warns, "\n"))
			}
		})
	}
}

// scripted is a runner that answers each job with the next of its outcomes,
// for the tests that drive the outage report through the pool. Every job here
// is sent after the one before it is announced, so the order is the test's.
type scripted struct {
	mu       sync.Mutex
	outcomes []func() (RunResult, error)
}

func (s *scripted) then(outcomes ...func() (RunResult, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes = append(s.outcomes, outcomes...)
}

func (s *scripted) run(context.Context, JobSpec) (RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.outcomes[0]
	s.outcomes = s.outcomes[1:]
	return next()
}

// A success recording the route it took, and the two failures a missing tool
// produces: exec's lookup failure for sox, and the route mark for ffmpeg.
func succeededVia(r decodeRoute) func() (RunResult, error) {
	return func() (RunResult, error) {
		_, settings, _, _ := JobSpec{TargetSampleRate: 176400, TargetBits: 24}.soxArgsFrom([]string{"in"}, r.String())
		return RunResult{SizeBytes: 4, Settings: settings}, nil
	}
}

func soxMissing() (RunResult, error) { return RunResult{}, soxLookupFailure() }

func ffmpegMissing() (RunResult, error) {
	return RunResult{}, markToolUnavailable(toolFFmpeg, "ffmpeg and ffprobe not found on PATH",
		ErrDSDDecodeUnavailable)
}

// TestAToolOutageIsReportedWhenItStartsAndWhenAJobProvesItBack pins the
// outage report: one Warn when a tool's outage starts, the jobs after it at
// Debug, and one Info when a job proves the tool back, with how many jobs the
// outage cost. Keyed PER TOOL: with ffmpeg missing, a FLAC job's success
// (which ran sox alone) must not report ffmpeg back, or a queue mixing FLAC
// and DSD jobs would log a Warn and an Info per DSD job, the flood the report
// exists to stop.
func TestAToolOutageIsReportedWhenItStartsAndWhenAJobProvesItBack(t *testing.T) {
	const flacRel, dsdRel = "Music/Album/01.flac", "Music/Album/02.dsf"
	a := newAnnouncingPool(t, flacRel, dsdRel)
	s := &scripted{}
	a.pool.runner = s.run
	a.pool.fsyncFn = noopFsync // the scripted successes write no sidecar
	flac, dsd := sourceFile(t, flacRel), dsdSource(t, dsdRel)

	s.then(soxMissing, soxMissing, soxMissing, succeededVia(routeSoxDirect))
	a.run(t, flac, 4)
	s.then(ffmpegMissing, ffmpegMissing, succeededVia(routeSoxDirect), ffmpegMissing, succeededVia(routeFFmpegPipe))
	a.run(t, dsd, 2)
	a.run(t, flac, 1)
	a.run(t, dsd, 1)
	a.run(t, flac, 1)

	warns := a.lines("WARN", logToolUnavailable)
	if len(warns) != 2 || !strings.Contains(warns[0], "tool=sox") || !strings.Contains(warns[1], "tool=ffmpeg") {
		t.Errorf("Warn lines:\n%s\nwant two: sox's outage, then ffmpeg's", strings.Join(warns, "\n"))
	}
	if debug := a.lines("DEBUG", logToolUnavailable); len(debug) != 4 {
		t.Errorf("Debug lines:\n%s\nwant four: the second and third job of each outage",
			strings.Join(debug, "\n"))
	}
	back := a.lines("INFO", logToolBack)
	if len(back) != 2 || !strings.Contains(back[0], "tool=sox") || !strings.Contains(back[0], "failedJobs=3") ||
		!strings.Contains(back[1], "tool=ffmpeg") || !strings.Contains(back[1], "failedJobs=3") {
		t.Errorf("recovery lines:\n%s\nwant sox back after 3 failed jobs, then ffmpeg back after 3 "+
			"(the FLAC success between ffmpeg's failures ran sox alone, and proves nothing about ffmpeg)",
			strings.Join(back, "\n"))
	}
}

// TestAToolOutageThatOutlastsADayIsReportedAgain pins the re-warn. An outage
// ends only when a job proves the tool back, and that can be missed (the tool
// comes back and goes again before any job that needs it succeeds), so a day
// of silence is broken by one more Warn carrying the count so far.
func TestAToolOutageThatOutlastsADayIsReportedAgain(t *testing.T) {
	const rel = "Music/Album/01.flac"
	a := newAnnouncingPool(t, rel)
	s := &scripted{}
	a.pool.runner = s.run
	clock := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	a.pool.outages.now = func() time.Time { return clock }
	spec := sourceFile(t, rel)

	s.then(soxMissing, soxMissing, soxMissing)
	a.run(t, spec, 1)
	clock = clock.Add(outageRewarnAfter - time.Minute)
	a.run(t, spec, 1)
	clock = clock.Add(time.Minute)
	a.run(t, spec, 1)

	warns := a.lines("WARN", logToolUnavailable)
	if len(warns) != 2 || !strings.Contains(warns[1], "failedJobs=3") {
		t.Errorf("Warn lines:\n%s\nwant the outage's first job, then its third once a day had passed, "+
			"carrying the count so far", strings.Join(warns, "\n"))
	}
}
