package manifest

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dhowden/tag"
)

// TestAnID3GenreOfThousandsOfReferencesFinishesAtOnce is the bound on
// dhowden's genre expansion, which the bridge calls through genreFromTag.
// 4,000 in-range "(0)" references took 6.5s through Genre before the
// bound and answer nothing a tagger writes. The bound is generous: a
// second, on an input the unbounded walk spends several seconds on.
func TestAnID3GenreOfThousandsOfReferencesFinishesAtOnce(t *testing.T) {
	body := textFrameBytes(3, "TCON", strings.Repeat("(0)", 4000))
	m, err := tag.ReadFrom(bytes.NewReader(id3v2TagBytes(3, 0, body)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	start := time.Now()
	got := genreFromTag(m)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("genre expansion took %s, want under 1s", elapsed)
	}
	if got != "" {
		t.Fatalf("genre = %q, want empty: the frame is not a genre", got)
	}
}

// TestAnOrdinaryID3GenreIsWhatDhowdenReturns pins the bound to the
// tags it must not change: genreFromTag stores Genre's own answer,
// including a numeric reference a tagger writes.
func TestAnOrdinaryID3GenreIsWhatDhowdenReturns(t *testing.T) {
	for _, frame := range []string{"(17)", "Rock", "(9)(0)", "Live (2024)", "Jazz (17)"} {
		body := textFrameBytes(3, "TCON", frame)
		m, err := tag.ReadFrom(bytes.NewReader(id3v2TagBytes(3, 0, body)))
		if err != nil {
			t.Fatalf("read %q: %v", frame, err)
		}
		want := strings.TrimSpace(m.Genre())
		if got := genreFromTag(m); got != want {
			t.Errorf("genre %q: got %q, Genre() %q", frame, got, want)
		}
		if want == "" {
			t.Errorf("genre %q: Genre() returned empty, the case does not pin a result", frame)
		}
	}
}
