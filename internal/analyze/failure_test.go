package analyze

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// TestMarkUnreadableKeepsTheMessage — these strings are not internal. They go
// to the journal, into `tracks.analysis_fail_reason`, and onto the console's
// unreadable list, and the operator has been reading the truncation one in
// their logs for a week. A `fmt.Errorf("%w: %w", ErrSourceUnreadable, err)`
// would have prefixed every one of them with "source unreadable: ".
func TestMarkUnreadableKeepsTheMessage(t *testing.T) {
	inner := fmt.Errorf("sox: decoded 51.5s of 357.2s probed — source appears truncated")
	got := markUnreadable(inner)
	if got.Error() != inner.Error() {
		t.Errorf("Error() = %q, want the inner message %q unchanged", got, inner)
	}
	if !SourceUnreadable(got) {
		t.Error("marked error does not classify as unreadable")
	}
	if !errors.Is(got, inner) {
		t.Error("marked error lost the original in its chain — errors.Is/As must still reach it")
	}
	if SourceUnreadable(inner) {
		t.Error("an unmarked error classified as unreadable — everything unclassified is transient")
	}
}

// TestOnlyAnExitedDecoderReachesAVerdict is the split that keeps a memory
// kill or a per-job timeout from being recorded as a property of the file.
//
// A decoder that RAN and exited non-zero opened the input and rejected it. One
// killed by a signal reached no conclusion at all — and under load the OOM
// killer picks the biggest decode, which would otherwise sideline the
// operator's longest tracks first.
func TestOnlyAnExitedDecoderReachesAVerdict(t *testing.T) {
	// A real exit status, produced by a real process, so the test cannot
	// disagree with the platform about what ProcessState reports.
	exited := exec.Command("sh", "-c", "exit 3").Run()
	if exited == nil {
		t.Fatal("expected a non-zero exit")
	}
	if !decoderReachedAVerdict(exited) {
		t.Errorf("a clean non-zero exit (%v) was not treated as a verdict", exited)
	}

	signalled := exec.Command("sh", "-c", "kill -9 $$").Run()
	if signalled == nil {
		t.Fatal("expected a signal death")
	}
	if decoderReachedAVerdict(signalled) {
		t.Errorf("a signal death (%v) was treated as a verdict about the file", signalled)
	}

	// Anything that is not an ExitError is not a verdict either: the wait
	// itself failed, so no decoder opinion was ever collected.
	if decoderReachedAVerdict(errors.New("waitid: interrupted")) {
		t.Error("a non-ExitError was treated as a verdict")
	}
	if decoderReachedAVerdict(context.Canceled) {
		t.Error("a cancellation was treated as a verdict")
	}
}

// TestATruncatedDecodeIsClassifiedUnreadable drives the real decoder path,
// not the classifier in isolation.
//
// The truncation check is the ONE signal separating a short file from a short
// read — both sox and ffmpeg exit 0 on a truncated stream — so it is where
// the classification has to be attached, and a test that only exercised
// markUnreadable would prove nothing about that wiring. `cat` stands in for a
// decoder: it emits a few frames of float32 and exits 0, against a probed
// duration far longer, which is precisely the shape the field report's 30
// FLACs produce.
func TestATruncatedDecodeIsClassifiedUnreadable(t *testing.T) {
	// 8 bytes = 2 float32 samples = 2 mono frames, against 60s probed.
	_, err := decodeFramesWith(context.Background(), decoderSox, "sh",
		[]string{"-c", "printf '\\0\\0\\0\\0\\0\\0\\0\\0'"},
		"/library/Artist/Album/06. Jasper Sea.flac", 1, 60, func([]float64) {})
	if err == nil {
		t.Fatal("a decode two frames long against a 60s probe was accepted")
	}
	if !SourceUnreadable(err) {
		t.Errorf("truncation verdict %q was not classified as a property of the file", err)
	}
	if !containsAll(err.Error(), "truncated", "60.0s") {
		t.Errorf("message = %q, want it to still name the probed duration — it is what "+
			"the operator reads in the journal and in the console list", err)
	}
}

// TestADecoderKilledMidStreamIsNotTheFilesFault — the negative half. Same
// harness, but the process dies on a signal, so nothing about the source was
// established and no strike may be recorded.
func TestADecoderKilledMidStreamIsNotTheFilesFault(t *testing.T) {
	_, err := decodeFramesWith(context.Background(), decoderSox, "sh",
		[]string{"-c", "kill -9 $$"},
		"/library/Artist/Album/06. Jasper Sea.flac", 1, 60, func([]float64) {})
	if err == nil {
		t.Fatal("expected an error from a killed decoder")
	}
	if SourceUnreadable(err) {
		t.Errorf("a signal-killed decoder produced a verdict about the file: %q", err)
	}
}

// TestARefusedInputIsClassifiedUnreadable — the other wrapped site. The
// decoder opened nothing it liked and said so with an exit status, which is
// an opinion about the input.
func TestARefusedInputIsClassifiedUnreadable(t *testing.T) {
	_, err := decodeFramesWith(context.Background(), decoderSox, "sh",
		[]string{"-c", "echo 'FAIL formats: bad header' >&2; exit 2"},
		"/library/Artist/Album/06. Jasper Sea.flac", 1, 0, func([]float64) {})
	if err == nil {
		t.Fatal("expected an error from a refusing decoder")
	}
	if !SourceUnreadable(err) {
		t.Errorf("a non-zero decoder exit was not classified as a verdict: %q", err)
	}
	// The stderr rides along redacted — it is persisted and rendered, so the
	// absolute source path must not survive into it.
	if containsAll(err.Error(), "/library/Artist") {
		t.Errorf("the absolute source path reached the stored reason: %q", err)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
