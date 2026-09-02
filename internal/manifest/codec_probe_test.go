package manifest

import (
	"context"
	"path/filepath"
	"testing"
)

// HasTracksWithCodec backs the `bridge doctor` ALAC/ffmpeg warning. It matches
// case-insensitively because the codec column carries whatever the extractor
// stamped, and it deliberately counts suppressed and routed rows: the question
// is "does this operator have files of this kind", not "what does /v1 serve".
func TestHasTracksWithCodec(t *testing.T) {
	s := mustOpenCodecStore(t)
	ctx := context.Background()

	if has, err := s.HasTracksWithCodec(ctx, "ALAC"); err != nil || has {
		t.Fatalf("empty library: has=%v err=%v, want false/nil", has, err)
	}

	rate, bits := 44100.0, 16
	mustUpsert := func(path, codec string, suppressed bool) {
		t.Helper()
		tr := &Track{Path: path, Title: "t", Artist: "a", Album: "b",
			Codec: codec, SampleRate: &rate, BitsPerSample: &bits}
		if err := s.UpsertTrack(ctx, tr); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
		if suppressed {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE tracks SET dupe_suppressed = 1 WHERE path = ?`, path); err != nil {
				t.Fatalf("suppress %s: %v", path, err)
			}
		}
	}

	mustUpsert("A/flac.flac", "FLAC", false)
	if has, err := s.HasTracksWithCodec(ctx, "ALAC"); err != nil || has {
		t.Fatalf("FLAC-only library: has=%v err=%v, want false/nil", has, err)
	}

	// Lower-cased on the row, upper-cased in the query: the extractor's
	// stamp and the caller's spelling must not have to agree.
	mustUpsert("A/alac.m4a", "alac", false)
	if has, err := s.HasTracksWithCodec(ctx, "ALAC"); err != nil || !has {
		t.Fatalf("case-insensitive match: has=%v err=%v, want true/nil", has, err)
	}

	// A suppressed duplicate still counts — the operator still has the file,
	// and would still want to know ffmpeg is missing.
	s2 := mustOpenCodecStore(t)
	if err := s2.UpsertTrack(ctx, &Track{Path: "B/x.m4a", Title: "t", Artist: "a", Album: "b",
		Codec: "ALAC", SampleRate: &rate, BitsPerSample: &bits}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s2.db.ExecContext(ctx, `UPDATE tracks SET dupe_suppressed = 1`); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if has, err := s2.HasTracksWithCodec(ctx, "ALAC"); err != nil || !has {
		t.Errorf("a suppressed ALAC row must still count: has=%v err=%v", has, err)
	}
}

func mustOpenCodecStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
