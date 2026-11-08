package integrity

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestALinkIsNeverACurve: the waveform guard counts curve files, and a link
// is never one, whatever it leads to. A waveform directory whose tree holds
// only a link to an empty folder, or only a link to a curve-named file, holds
// no curve while the rows record curves, and is refused as a volume away; the
// variant probe, which takes a link to a directory as a tree that may hold
// renditions, is the variants' own (backlog B652, round 2).
func TestALinkIsNeverACurve(t *testing.T) {
	ctx := context.Background()
	opts := WaveformJudgeOptions{MaxMissingPercent: WaveformRegenMaxMissingPercent}
	t.Run("a link to an empty folder", func(t *testing.T) {
		c := newWaveformCatalog(t, 3)
		if err := os.RemoveAll(c.dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(c.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(c.dir, "Artist")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		requireRefused(t, JudgeWaveformCatalog(ctx, c.dir, c.rows, opts), WaveformRefusalDirAway, c.rows[0].SourcePath)
	})
	t.Run("a link to a curve-named file", func(t *testing.T) {
		c := newWaveformCatalog(t, 3)
		c.remove(t, 3)
		target := filepath.Join(t.TempDir(), "elsewhere.waveform.bin")
		writeCurveFixture(t, target, 24)
		if err := os.Symlink(target, c.rows[2].WaveformPath); err != nil {
			t.Skipf("symlink: %v", err)
		}
		requireRefused(t, JudgeWaveformCatalog(ctx, c.dir, c.rows, opts), WaveformRefusalDirAway, c.rows[0].SourcePath)
	})
}

// TestTheTreeIsLookedAtAgainBeforeAnyAdmission: a judgment probes the tree for
// a curve once more before it admits, whatever the floor and the ratio say. Two
// rows, below the floor of ten: one curve missing at the probe that begins the
// judgment, and the other deleted after it, the directory the same one
// throughout, so neither identity check sees it, and the ratio, below its
// floor, looks at nothing. Refused at the last look, as a tree holding no
// curve while the rows record one; a judgment its budget stops is told so too.
func TestTheTreeIsLookedAtAgainBeforeAnyAdmission(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		maxRows int
	}{
		{"a whole judgment", 0},
		{"a bounded judgment", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newWaveformCatalog(t, 2)
			c.remove(t, 1)
			opts := WaveformJudgeOptions{MaxMissingPercent: WaveformRegenMaxMissingPercent, MaxRows: tc.maxRows,
				afterProbe: func() {
					if err := os.Remove(c.rows[1].WaveformPath); err != nil {
						t.Error(err)
					}
				}}
			j := JudgeWaveformCatalog(ctx, c.dir, c.rows, opts)
			requireRefused(t, j, WaveformRefusalDirAway, c.rows[0].SourcePath)
			if j.Undecided {
				t.Errorf("judgment %+v: a refusal the last look proves is no undecided judgment", j)
			}
		})
	}
	t.Run("a curve still there admits", func(t *testing.T) {
		c := newWaveformCatalog(t, 3)
		c.remove(t, 1)
		opts := WaveformJudgeOptions{MaxMissingPercent: WaveformRegenMaxMissingPercent,
			afterProbe: func() {
				if err := os.Remove(c.rows[1].WaveformPath); err != nil {
					t.Error(err)
				}
			}}
		j := JudgeWaveformCatalog(ctx, c.dir, c.rows, opts)
		if !j.Admits() || !j.Repairable(c.rows[0].SourcePath) || !j.Repairable(c.rows[1].SourcePath) {
			t.Fatalf("judgment %+v: want both gone curves admitted while a third is there", j)
		}
	})
}

// blockingSink is a log sink that has stopped answering: every record it is
// handed waits until release, the first one telling entered, and is then
// recorded as the inner handler records it.
type blockingSink struct {
	entered, released    chan struct{}
	enterOnce, leaveOnce sync.Once
}

func newBlockingSink() *blockingSink {
	return &blockingSink{entered: make(chan struct{}), released: make(chan struct{})}
}

func (s *blockingSink) release() { s.leaveOnce.Do(func() { close(s.released) }) }

func (s *blockingSink) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(time.Minute):
		t.Fatal("nothing reached the log sink")
	}
}

// blockingHandler is blockingSink's handler over inner, keeping what a
// derived logger binds as inner keeps it.
type blockingHandler struct {
	sink  *blockingSink
	inner slog.Handler
}

func (h blockingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h blockingHandler) Handle(ctx context.Context, r slog.Record) error {
	h.sink.enterOnce.Do(func() { close(h.sink.entered) })
	<-h.sink.released
	return h.inner.Handle(ctx, r)
}

func (h blockingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return blockingHandler{sink: h.sink, inner: h.inner.WithAttrs(as)}
}

func (h blockingHandler) WithGroup(name string) slog.Handler {
	return blockingHandler{sink: h.sink, inner: h.inner.WithGroup(name)}
}

// TestAGuardWhoseLogIsBlockedStillAnswersAndCloses: the guard logs a refusal
// after it lets its lock go, so a log sink that blocks holds no Ask, and Close
// returns within its grace while the sink still blocks, its own warning
// included (said from a goroutine of its own), and the judgment it left is
// joined once the sink answers.
func TestAGuardWhoseLogIsBlockedStillAnswersAndCloses(t *testing.T) {
	old := stopGrace
	stopGrace = 100 * time.Millisecond
	t.Cleanup(func() { stopGrace = old })
	start := func(t *testing.T) (*WaveformRegenGuard, *waveformCatalog, *blockingSink, *loggingtest.Recorder) {
		t.Helper()
		rec := loggingtest.NewRecorder()
		sink := newBlockingSink()
		loggingtest.SetDefault(t, slog.New(blockingHandler{sink: sink, inner: rec.Logger().Handler()}))
		t.Cleanup(sink.release)
		c := newWaveformCatalog(t, 1)
		c.remove(t, 1) // the last curve gone: the judgment refuses, and says so
		g := NewWaveformRegenGuard(context.Background(), WaveformRegenConfig{
			List:              func(context.Context) ([]WaveformSnapshot, error) { return c.rows, nil },
			Dir:               func() string { return c.dir },
			Admit:             func(string, string, os.FileInfo) {},
			Interval:          time.Hour,
			MaxMissingPercent: WaveformRegenMaxMissingPercent,
		})
		if a := g.Ask(c.rows[0].SourcePath); a.Kind != WaveformJudging {
			t.Fatalf("first ask: %v, want a judgment started", a.Kind)
		}
		sink.waitEntered(t)
		return g, c, sink, rec
	}
	t.Run("an ask", func(t *testing.T) {
		g, c, sink, _ := start(t)
		answered := make(chan WaveformAnswer, 1)
		go func() { answered <- g.Ask(c.rows[0].SourcePath) }()
		select {
		case a := <-answered:
			if a.Kind != WaveformRefused {
				t.Errorf("an ask while the refusal's line is in a blocked sink: %v, want refused", a.Kind)
			}
		case <-time.After(5 * time.Second):
			t.Error("Ask waited on a blocked log sink: the guard logs under its lock")
		}
		sink.release()
		g.Close()
	})
	t.Run("the close", func(t *testing.T) {
		g, _, sink, rec := start(t)
		closed := make(chan struct{})
		go func() { g.Close(); close(closed) }()
		early := false
		select {
		case <-closed:
			early = true
		case <-time.After(stopGrace + 5*time.Second):
			t.Error("Close did not return within its grace while the log sink was blocked")
		}
		sink.release()
		<-closed
		g.Wait()
		// Once the sink answers, nothing it was handed is lost: the refusal,
		// and the grace's warning of a Close that returned before it.
		deadline := time.Now().Add(time.Minute)
		for len(rec.Lines(msgWaveformRegenRefused)) == 0 || early && !anyCarrying(rec.All(), "did not stop within the grace") {
			if time.Now().After(deadline) {
				t.Fatalf("after the sink answered: %q, want the refusal and the grace's warning said", rec.All())
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

func anyCarrying(lines []string, s string) bool {
	for _, l := range lines {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}
