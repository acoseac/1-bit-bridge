package manifest

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureDefaultLogger installs a buffer-backed slog default for the test
// and restores the previous one. Safe because `logging.Component` resolves
// `slog.Default()` at LOG time (dynamicHandler), not at package-init time.
func captureDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A cancelled context must SUPPRESS the probe's error log — a shutdown or
// a client hang-up otherwise emits a burst of Errors describing normal
// teardown. The gate is on the LOG ONLY: the error must still be RETURNED,
// because that return is what drives the /v1/artwork handler's fail-open
// to a bounded 202 rather than a terminal 404. A "fix" that returned early
// instead of just skipping the log would silently flip that classification.
func TestMBIDProbesSuppressLogOnCancelledContextButStillReturnError(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	s.UpsertTrack(context.Background(), &Track{
		Path: "a/b.flac", Size: 1, ModTime: time.Now(),
		Artist: "A", Album: "B",
		ArtworkMBID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		ArtistMBID:  "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		call func(context.Context) (bool, error)
	}{
		{"HasTrackWithArtworkMBID", func(c context.Context) (bool, error) {
			return s.HasTrackWithArtworkMBID(c, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		}},
		{"HasTrackWithArtistMBID", func(c context.Context) (bool, error) {
			return s.HasTrackWithArtistMBID(c, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
		}},
		{"ArtworkMBIDEnrichmentPending", func(c context.Context) (bool, error) {
			return s.ArtworkMBIDEnrichmentPending(c, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		}},
		{"ArtistMBIDEnrichmentPending", func(c context.Context) (bool, error) {
			return s.ArtistMBIDEnrichmentPending(c, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureDefaultLogger(t)
			_, err := tc.call(ctx)
			if err == nil {
				t.Fatal("cancelled context returned a nil error — the caller's " +
					"fail-open depends on this error propagating; the ctx guard " +
					"must gate the LOG, never the return")
			}
			if got := buf.String(); strings.Contains(got, "level=ERROR") {
				t.Errorf("cancelled context emitted an error log:\n%s", got)
			}
		})
	}
}

// The counter-test: with a LIVE context, a genuine database fault must
// still be logged. Without this, "suppress the noise" could be
// implemented as "never log", and a real broken store would go silent.
func TestMBIDProbesStillLogGenuineFaultOnLiveContext(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	s.Close() // every subsequent query is a real fault, ctx is fine

	buf := captureDefaultLogger(t)
	_, err := s.HasTrackWithArtworkMBID(context.Background(),
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err == nil {
		t.Fatal("closed store returned a nil error")
	}
	if got := buf.String(); !strings.Contains(got, "hasTrackWithJSONField") {
		t.Errorf("genuine fault on a live context was NOT logged:\n%s", got)
	}
}
