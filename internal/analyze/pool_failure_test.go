package analyze

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestPoolRecordsAStrikeOnlyForAFileVerdict is the whole classification rule
// at the layer that applies it.
//
// A decoder verdict about the source is recorded, so the candidate stops
// being offered after the threshold. Everything else — a missing sox, a
// faulted read, a full output volume — is a fact about the host and must
// leave no trace, because a 30-second outage would otherwise sideline every
// track that happened to be in flight.
func TestPoolRecordsAStrikeOnlyForAFileVerdict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStrike bool
	}{
		{"decoder verdict", markUnreadable(errors.New("sox: decoded 1.0s of 300.0s probed — source appears truncated")), true},
		{"toolchain missing", errors.New(`start sox: exec: "sox": executable file not found in $PATH`), false},
		{"faulted read", fmt.Errorf("read pcm: %w", errors.New("input/output error")), false},
		{"output volume full", errors.New("write waveform tmp: no space left on device"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			putTrack(t, s, "A/B/01.flac")
			p := NewPool(s, 1, 4,
				WithFsync(noFsync),
				WithRunner(func(context.Context, AnalyzeSpec) (Result, error) {
					return Result{}, tc.err
				}))
			if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "A/B/01.flac", SourceAbsPath: "/lib/A/B/01.flac"}); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return p.Stats().Failed == 1 })
			p.Stop()

			rows, err := s.ListUnreadableTracksForAdmin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			assertVerdictRows(t, rows, tc.wantStrike, tc.err, tc.name)
		})
	}
}

// assertVerdictRows checks the one thing each case above is about: whether
// the failure left a row behind, and — when it should have — that the row
// carries the decoder's own words rather than a rewrite of them.
//
// Extracted so the table body stays a table body. Inline it scored 16 on
// SonarCloud's go:S3776 against a ceiling of 15: the branch nesting inside a
// subtest inside a range is what the rule measures, and a table-driven test
// accumulates that without getting harder to read. Pulling the assertion out
// is the fix the rule is asking for either way.
func assertVerdictRows(t *testing.T, rows []manifest.AdminUnreadableTrack, wantStrike bool, cause error, kind string) {
	t.Helper()
	if !wantStrike {
		if len(rows) != 0 {
			t.Fatalf("recorded %d verdict(s) for a %s, want 0 — a fact about the host must "+
				"never sideline a file", len(rows), kind)
		}
		return
	}
	if len(rows) != 1 {
		t.Fatalf("recorded %d verdict(s), want 1 — a source the decoder refused must "+
			"stop being offered", len(rows))
	}
	if rows[0].Reason != cause.Error() {
		t.Errorf("reason = %q, want the decoder's own message %q", rows[0].Reason, cause)
	}
}

// TestPoolReachesTheThresholdAndStopsBeingOffered — three refusals of the
// same file version is what the debounce is for. The count is what the
// candidate walk reads, so this pins the pool's end of it.
func TestPoolReachesTheThresholdAndStopsBeingOffered(t *testing.T) {
	s := newStore(t)
	putTrack(t, s, "A/B/01.flac")
	p := NewPool(s, 1, 4,
		WithFsync(noFsync),
		WithRunner(func(context.Context, AnalyzeSpec) (Result, error) {
			return Result{}, markUnreadable(errors.New("sox: source appears truncated"))
		}))
	defer p.Stop()

	for i := 1; i <= 3; i++ {
		enqueueAndSettle(t, p, "A/B/01.flac", uint64(i))
	}
	sup, err := s.SuppressedAnalysisPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sup["A/B/01.flac"]; !ok {
		t.Fatalf("suppressed set = %v, want A/B/01.flac after three verdicts", sup)
	}
}

// TestASuccessfulAnalysisClearsTheStrikes — the counter must measure
// CONSECUTIVE failures, and the pool is the only place a success is observed.
// Without this a file that fails twice a year suppresses itself eventually,
// with successful analyses on either side of the strikes.
func TestASuccessfulAnalysisClearsTheStrikes(t *testing.T) {
	s := newStore(t)
	putTrack(t, s, "A/B/01.flac")
	fail := true
	p := NewPool(s, 1, 4,
		WithFsync(noFsync),
		WithRunner(func(context.Context, AnalyzeSpec) (Result, error) {
			if fail {
				return Result{}, markUnreadable(errors.New("sox: source appears truncated"))
			}
			return Result{
				WaveformPath: "/w/x.waveform.bin", WaveformTag: "deadbeef",
				WaveformSize: 42, SchemaVersion: WaveformSchemaVersion,
			}, nil
		}))
	defer p.Stop()

	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "A/B/01.flac", SourceAbsPath: "/lib/A/B/01.flac"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.Stats().Failed == 1 })
	rows, err := s.ListUnreadableTracksForAdmin(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = (%d rows, %v), want 1 before the success", len(rows), err)
	}

	fail = false
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "A/B/01.flac", SourceAbsPath: "/lib/A/B/01.flac"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p.Stats().Done == 1 })
	rows, err = s.ListUnreadableTracksForAdmin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("list still has %d row(s) after a successful analysis, want 0", len(rows))
	}
}

// TestACountedFailureHasAlreadyReleasedItsPath pins what a failure count
// means: the job is FINISHED. Its own bookkeeping has landed and its path is
// free, so a retry sent the moment the count moves is accepted. Along the way,
// every snapshot shows each accepted job in exactly one place: in flight, done
// or failed.
//
// The CI failure behind it (#986's `test -race (rest)` leg) was
// TestASuccessfulAnalysisClearsTheStrikes waiting for Failed == 1 and then
// re-enqueueing the same path. processJob counted the failure first, then
// wrote the strike and its WARN, and released the path last. Enqueue then
// answered a path that was still held with nil and queued nothing, so a retry
// landing in that window was dropped without a trace and the wait for Done
// ran out. It answers ErrDuplicateInflight now, which a retry would at least
// see; the fix is still that the window is gone.
// The same test passed 900 runs in a row on a laptop: a window between two
// statements shows up on a loaded runner and nowhere else.
//
// So this test does not race the window. It parks the worker inside it,
// in the failure's own WARN. That is the one step between the old count and
// the old release a test can hold without a hook in production code: the
// package logger resolves slog.Default at log time (loggingtest.ParkOn).
func TestACountedFailureHasAlreadyReleasedItsPath(t *testing.T) {
	park := loggingtest.ParkOn(t, analyzeFailedMsg)
	s := newStore(t)
	putTrack(t, s, "A/B/01.flac")
	var runs atomic.Int32
	p := NewPool(s, 1, 4,
		WithFsync(noFsync),
		WithRunner(func(context.Context, AnalyzeSpec) (Result, error) {
			if runs.Add(1) == 1 {
				return Result{}, markUnreadable(errors.New("sox: source appears truncated"))
			}
			return Result{WaveformPath: "/w/x.waveform.bin", WaveformTag: "t", SchemaVersion: WaveformSchemaVersion}, nil
		}))
	// Deferred in this order so the worker is let go BEFORE Stop waits for
	// it, on every way out of the test, a failed assertion included.
	defer p.Stop()
	defer park.Release()

	// settle waits for cond and checks, on every poll rather than only at the
	// end, that each accepted job is exactly one of in flight, done or
	// failed: never both, never neither.
	settle := func(cond func(PoolStats) bool) {
		t.Helper()
		waitFor(t, func() bool {
			st := p.Stats()
			if st.Enqueued != uint64(st.Inflight)+st.Done+st.Failed {
				t.Fatalf("Stats() = %+v: a job is counted twice, or missing, across in flight / done / failed", st)
			}
			return cond(st)
		})
	}

	spec := AnalyzeSpec{SourceLibraryRel: "A/B/01.flac", SourceAbsPath: "/lib/A/B/01.flac"}
	if err := p.Enqueue(spec); err != nil {
		t.Fatal(err)
	}
	park.Wait(t)

	// The job is still writing its own failure, so it is still running: in
	// flight, and not counted.
	if st := p.Stats(); st.Inflight != 1 || st.Done+st.Failed != 0 {
		// Show what a retry gets at this point before failing. With the count
		// published early, it is the CI failure: dropped as a duplicate.
		retryErr := p.Enqueue(spec)
		t.Fatalf("parked in its own failure WARN, the job reads Inflight=%d Done=%d Failed=%d, want 1/0/0; "+
			"a retry sent now returned %v and Enqueued went %d -> %d",
			st.Inflight, st.Done, st.Failed, retryErr, st.Enqueued, p.Stats().Enqueued)
	}
	park.Release()

	settle(func(st PoolStats) bool { return st.Failed == 1 })
	rows, err := s.ListUnreadableTracksForAdmin(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = (%d rows, %v) once the failure is counted, want the strike already recorded", len(rows), err)
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("a retry sent on Failed == 1 returned %v: the job had been counted before "+
			"it released its path", err)
	}
	if got := p.Stats().Enqueued; got != 2 {
		t.Fatalf("Enqueued = %d after a retry sent on Failed == 1 was accepted, want 2", got)
	}
	settle(func(st PoolStats) bool { return st.Done == 1 })
}

// syncBuffer is a bytes.Buffer whose writes and reads are serialised.
//
// slog's handler locks around its own writes, so the WRITERS are already
// serialised — but the test goroutine's read is not ordered against them by
// anything, and "the sequencing happens to make it safe" is not a property a
// test should rest on. The mutex costs nothing and removes the question.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs points slog.Default at a buffer for the duration of a test.
// logging.Component resolves slog.Default() at LOG time (never at
// construction — see its docblock), so redirecting the default handler
// reaches this package's package-level logger without a seam.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// enqueueAndSettle submits one job and waits for its failure to be counted.
//
// The count alone is enough, and that is the pool's guarantee, not this
// helper's: finishJob counts a job in the same critical section that releases
// its path, after noteFailure has written the strike and the log line. So
// `Failed == n` means the line has landed and the next Enqueue of the path is
// accepted.
//
// It was not always enough. While processJob counted first and released last,
// this helper also waited for the pool to go idle. That fixed the tests that
// use it, which were flaky 1 run in 5, and left the pool as it was.
// TestASuccessfulAnalysisClearsTheStrikes never used the helper and flaked the
// same way in CI (#986). TestACountedFailureHasAlreadyReleasedItsPath pins the
// order now.
func enqueueAndSettle(t *testing.T, p *Pool, rel string, wantFailed uint64) {
	t.Helper()
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: rel, SourceAbsPath: "/lib/" + rel}); err != nil {
		t.Fatalf("enqueue %s: %v", rel, err)
	}
	waitFor(t, func() bool { return p.Stats().Failed == wantFailed })
}

// TestTheFailureWarnFiresOncePerFileVersion is the other half of the field
// report, and the half a debounce alone does not fix.
//
// Three sweeps produce three refusals before the threshold suppresses, and
// the operator's host was logging one WARN per refusal per sweep forever —
// 1,385 lines in 7 days for 30 files. The cost is not disk; it is that every
// other line in the journal becomes unfindable, which is the same lesson the
// M-SEARCH send-failure streak suppression records.
//
// The strike count is the gate: the FIRST verdict against a file version
// warns, later ones drop to Debug. A new file version warns again, because
// that is genuinely new information.
func TestTheFailureWarnFiresOncePerFileVersion(t *testing.T) {
	buf := captureLogs(t)
	s := newStore(t)
	putTrack(t, s, "A/B/01.flac")
	p := NewPool(s, 1, 4,
		WithFsync(noFsync),
		WithRunner(func(context.Context, AnalyzeSpec) (Result, error) {
			return Result{}, markUnreadable(errors.New(
				"sox: decoded 51.5s of 357.2s probed — source appears truncated"))
		}))
	defer p.Stop()

	for i := 1; i <= 3; i++ {
		enqueueAndSettle(t, p, "A/B/01.flac", uint64(i))
	}

	if n := strings.Count(buf.String(), `level=WARN msg="analyze: failed"`); n != 1 {
		t.Errorf("%d WARN lines for three refusals of one file version, want 1\n%s", n, buf.String())
	}
	// Kept, not silenced: the first line still carries the decoder's message
	// with both durations, which is what the operator acts on.
	if !strings.Contains(buf.String(), "decoded 51.5s of 357.2s probed") {
		t.Errorf("the surviving WARN lost the decoder's message:\n%s", buf.String())
	}
	if n := strings.Count(buf.String(), "level=DEBUG"); n != 2 {
		t.Errorf("%d DEBUG lines, want 2 — the repeats must still be observable at debug", n)
	}
}

// TestATransientFailureKeepsWarningEveryTime is the deliberate asymmetry.
//
// A transient failure has no marker to deduplicate against, and one that
// repeats forever is an alarm the operator needs: a missing sox or a failing
// disk must not be silenced by the fix for truncated files.
func TestATransientFailureKeepsWarningEveryTime(t *testing.T) {
	buf := captureLogs(t)
	s := newStore(t)
	putTrack(t, s, "A/B/01.flac")
	p := NewPool(s, 1, 4,
		WithFsync(noFsync),
		WithRunner(func(context.Context, AnalyzeSpec) (Result, error) {
			return Result{}, errors.New(`start sox: exec: "sox": executable file not found in $PATH`)
		}))
	defer p.Stop()

	for i := 1; i <= 3; i++ {
		enqueueAndSettle(t, p, "A/B/01.flac", uint64(i))
	}
	if n := strings.Count(buf.String(), `level=WARN msg="analyze: failed"`); n != 3 {
		t.Errorf("%d WARN lines for three toolchain failures, want 3 — a fact about the "+
			"host is not deduplicated away\n%s", n, buf.String())
	}
}

// TestATimedOutJobNeverReachesTheClassifier is the platform-independent half
// of "we killed it, so it says nothing about the file", and the half that
// matters most on Windows.
//
// decoderReachedAVerdict excludes a signal-killed decoder, but Windows has no
// signals: a process we terminate exits with a status there, `Exited()` is
// true, and the classifier alone would read the bridge's own kill as the
// file's fault. The per-job timeout is the case the bridge causes, and
// processJob excludes it on `DeadlineExceeded` before asking the classifier
// at all — which works the same everywhere.
//
// The runner returns a decoder-shaped VERDICT, so the exclusion is doing the
// work rather than the classification: unguarded, this is recorded as a
// strike. (Windows CI on #947. An earlier version of this test named the
// cancellation arm instead, which a control showed it never exercised —
// shutdown is caught by p.closed, so that arm was unreachable and has been
// removed.)
func TestATimedOutJobNeverReachesTheClassifier(t *testing.T) {
	s := newStore(t)
	putTrack(t, s, "A/B/01.flac")
	p := NewPool(s, 1, 4,
		WithJobTimeout(40*time.Millisecond),
		WithFsync(noFsync),
		WithRunner(func(ctx context.Context, _ AnalyzeSpec) (Result, error) {
			<-ctx.Done() // the job context expires under us, as a timeout does
			// Exactly what a decoder killed on Windows yields: a verdict-shaped
			// non-zero exit. Unguarded, this is recorded as the file's fault.
			return Result{}, markUnreadable(errors.New("sox: exit status 1 (stderr: )"))
		}))
	defer p.Stop()

	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "A/B/01.flac", SourceAbsPath: "/lib/A/B/01.flac"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		st := p.Stats()
		return st.Failed == 1 && st.Inflight == 0
	})

	rows, err := s.ListUnreadableTracksForAdmin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("recorded %d verdict(s) against a job the bridge itself timed out, want 0 "+
			"— on Windows the classifier cannot tell that kill from a refusal, so this "+
			"guard is the one that has to hold", len(rows))
	}
}
