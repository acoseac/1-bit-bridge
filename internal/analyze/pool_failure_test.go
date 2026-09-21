package analyze

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
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
			if tc.wantStrike {
				if len(rows) != 1 {
					t.Fatalf("recorded %d verdict(s), want 1 — a source the decoder refused must "+
						"stop being offered", len(rows))
				}
				if rows[0].Reason != tc.err.Error() {
					t.Errorf("reason = %q, want the decoder's own message %q", rows[0].Reason, tc.err)
				}
			} else if len(rows) != 0 {
				t.Fatalf("recorded %d verdict(s) for a %s, want 0 — a fact about the host must "+
					"never sideline a file", len(rows), tc.name)
			}
		})
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

// enqueueAndSettle submits one job and waits for the pool to go fully idle.
//
// `Failed` ALONE is the wrong signal and made these tests flaky (1 run in 5).
// processJob increments failedCnt BEFORE it logs and before releaseDedup, so
// a waiter that stops at `Failed == n` can observe the count while the log
// line has not been written and — worse — while the dedup slot is still held,
// which makes the NEXT Enqueue a silent no-op (a duplicate returns nil) and
// hangs the following wait on a count that will never arrive.
//
// Waiting for the dedup to drain fixes both, because releaseDedup runs after
// noteFailure: idle implies the line has landed and the path is free.
func enqueueAndSettle(t *testing.T, p *Pool, rel string, wantFailed uint64) {
	t.Helper()
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: rel, SourceAbsPath: "/lib/" + rel}); err != nil {
		t.Fatalf("enqueue %s: %v", rel, err)
	}
	waitFor(t, func() bool {
		st := p.Stats()
		return st.Failed == wantFailed && st.Inflight == 0 && st.QueueLen == 0
	})
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
