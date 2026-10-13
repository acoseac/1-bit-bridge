package albumgain

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

func albumWithShortMate(t *testing.T) (*harness, transcode.JobSpec, string) {
	t.Helper()
	refs := album(3)
	h := newHarness(t, refs...)
	h.meas.errs[refs[1].Path] = transcode.ErrFFmpegDecodeIncomplete
	h.meas.peaks[refs[2].Path] = fp(-5.0)
	return h, compactSpec(refs[0].Path, dsd64), refs[1].Path
}

func gainLines(logs string) int {
	return strings.Count(logs, "could not be measured")
}

// TestAShortMateIsMeasuredOnceAcrossTwoRenders: two renders of one album
// share a mate whose decode is shorter than its container. The second
// render uses the remembered failure.
func TestAShortMateIsMeasuredOnceAcrossTwoRenders(t *testing.T) {
	var logs bytes.Buffer
	loggingtest.SetDefault(t, slog.New(slog.NewTextHandler(&logs, nil)))
	h, j, mate := albumWithShortMate(t)
	for n := 0; n < 2; n++ {
		g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0))
		if err != nil || !ok || g != 4.0 {
			t.Fatalf("render %d: gain=%.1f ok=%v err=%v, want 4.0 from the measurable tracks", n, g, ok, err)
		}
	}
	if n := h.meas.callCount(mate); n != 1 {
		t.Errorf("the short mate was decoded %d times, want 1", n)
	}
	if n := gainLines(logs.String()); n != 1 {
		t.Errorf("the short mate was logged %d times, want 1:\n%s", n, logs.String())
	}
}

// TestAChangedShortMateIsMeasuredAgain: a new mtime is a new file, so the
// remembered failure does not cover it.
func TestAChangedShortMateIsMeasuredAgain(t *testing.T) {
	var logs bytes.Buffer
	loggingtest.SetDefault(t, slog.New(slog.NewTextHandler(&logs, nil)))
	h, j, mate := albumWithShortMate(t)
	if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); err != nil {
		t.Fatal(err)
	}
	for i := range h.cat.refs {
		if h.cat.refs[i].Path == mate {
			h.cat.refs[i].MTimeNS++
		}
	}
	if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); err != nil {
		t.Fatal(err)
	}
	if n := h.meas.callCount(mate); n != 2 {
		t.Errorf("the changed mate was decoded %d times, want 2", n)
	}
	if n := gainLines(logs.String()); n != 2 {
		t.Errorf("the changed mate was logged %d times, want one line per version:\n%s", n, logs.String())
	}
}

// TestATransientMateFailureIsMeasuredAgain: a decoder error that is not a
// length disagreement, and a length disagreement on a cancelled call, are
// not remembered.
func TestATransientMateFailureIsMeasuredAgain(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	mate := refs[1].Path
	h.meas.errs[mate] = errors.New("read: input/output error")
	h.meas.peaks[refs[2].Path] = fp(-5.0)
	j := compactSpec(refs[0].Path, dsd64)
	for n := 0; n < 2; n++ {
		if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.meas.callCount(mate); n != 2 {
		t.Errorf("an I/O error was decoded %d times, want 2", n)
	}

	h.meas.errs[mate] = transcode.ErrFFmpegDecodeIncomplete
	ctx, cancel := context.WithCancel(context.Background())
	h.meas.hook = func(path string) {
		if path == mate {
			cancel()
		}
	}
	if _, _, err := h.r.AlbumGainDB(ctx, j, fp(-8.0)); err == nil {
		t.Fatal("a cancelled survey returned nil")
	}
	h.meas.hook = nil
	before := h.meas.callCount(mate)
	if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); err != nil {
		t.Fatal(err)
	}
	if n := h.meas.callCount(mate); n != before+1 {
		t.Errorf("the cancelled decode was remembered: calls %d then %d", before, n)
	}
}
