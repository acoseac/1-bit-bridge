package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/admin"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// A match whose re-queue did not land (backlog B617, round 3, point 4). The
// sweep records a match it accepted together with the re-queue that hands
// it to the enricher, so a re-queue a switch-off or a shutdown cancels, or
// one that fails, leaves nothing that holds the file out of the next sweep:
// a row that already has its artist MBID and still lacks its album's,
// matched by fingerprint, is offered again and re-queued then.

// TestAMatchWhoseReQueueDidNotLandIsReQueuedByTheNextSweep: a row with a
// text-derived artist MBID and no album MBID is matched by the first sweep,
// whose re-queue is cancelled inside its transaction by a switch-off, or
// fails on a live context; nothing of it is kept, neither the re-queue nor
// the match's provenance, and once enrichment is on the next sweep matches
// the file again and re-queues it with its provenance, the verdict cached for
// the enricher.
func TestAMatchWhoseReQueueDidNotLandIsReQueuedByTheNextSweep(t *testing.T) {
	for _, c := range []struct {
		name string
		// hold is the first re-queue's, inside its transaction: on is the
		// sweep's switch.
		hold func(on *atomic.Bool) func(ctx context.Context) error
	}{
		{name: "cancelled inside the reset by a switch-off", hold: func(on *atomic.Bool) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				on.Store(false)
				select {
				case <-ctx.Done():
				case <-time.After(10 * time.Second):
				}
				return nil
			}
		}},
		{name: "a reset that fails", hold: func(*atomic.Bool) func(ctx context.Context) error {
			return func(context.Context) error { return errors.New("injected re-queue failure") }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := os.WriteFile(filepath.Join(root, "a.flac"), []byte("audio"), 0o644); err != nil {
				t.Fatal(err)
			}
			dur := switchDuration
			modTime := time.Now()
			// What the text ladder found: the artist, and no album.
			tr := &manifest.Track{Path: "a.flac", Size: 5, ModTime: modTime, Duration: &dur,
				Artist: "Some Artist", ArtistMBID: switchArtistMBID}
			if err := store.UpsertTrack(ctx, tr); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkEnriched(ctx, tr); err != nil {
				t.Fatal(err)
			}

			upstream := newSwitchCounter(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, switchAcoustIDMatch)
			})
			var on atomic.Bool
			on.Store(true)
			var held atomic.Int64
			hold := c.hold(&on)
			store.SetResetEnrichedBeforeCommitForTest(func(ctx context.Context) error {
				if held.Add(1) > 1 {
					return nil // the next sweep's re-queue lands
				}
				return hold(ctx)
			})
			cache := acoustid.NewCache(16)
			s := &fingerprintSweeper{
				store:       store,
				resolver:    bridgefs.New([]string{root}),
				client:      acoustid.NewClient(upstream.srv.URL, "k", "ua", upstream.srv.Client(), onSwitches),
				cache:       cache,
				workers:     1,
				maxPerRun:   10,
				length:      time.Minute,
				switchEvery: 5 * time.Millisecond,
				compute: func(context.Context, string, time.Duration) (acoustid.Fingerprint, error) {
					return acoustid.Fingerprint{Value: "AQABz0mUaEkSRZEG", Duration: switchDuration, DistinctB64: 40}, nil
				},
			}
			nudge := make(chan struct{}, 1)
			status := &sweepStatus[admin.FingerprintSweepCounts]{}
			runSwitchSweeper(t, s, func() bool { return true }, on.Load, nudge, status)

			// The first sweep: matched, and its re-queue held and not landed.
			eventually(t, func() bool { return held.Load() >= 1 }, "the first sweep's re-queue")
			eventually(t, func() bool {
				running, _, lastEnd, _, _ := status.snapshot()
				return !running && !lastEnd.IsZero()
			}, "the first sweep to end")
			if requeued(t, store, "a.flac") {
				t.Fatal("the first sweep's re-queue landed: the case tests nothing")
			}
			if got, err := store.AcoustIDMatch(ctx, "a.flac"); err != nil || got != "" {
				t.Errorf("the match's provenance after a re-queue that did not land = %q, %v; want none", got, err)
			}

			// Then on: the next sweep offers the file again and re-queues it.
			on.Store(true)
			nudge <- struct{}{}
			eventually(t, func() bool { return requeued(t, store, "a.flac") },
				"the next sweep to re-queue a.flac for its match")
			if got, err := store.AcoustIDMatch(ctx, "a.flac"); err != nil || got != switchAcoustID {
				t.Errorf("the match's provenance with its re-queue = %q, %v; want %q", got, err, switchAcoustID)
			}
			out, ok := cache.Get(acoustid.Key{Path: "a.flac", Size: 5, MTimeNS: modTime.UnixNano()})
			if !ok || !out.Matched || out.Decision.AcoustID != switchAcoustID {
				t.Errorf("the verdict cached for the enricher = %+v, %v; want the match", out, ok)
			}
		})
	}
}

// requeued says whether a.flac's row waits for the enricher again.
func requeued(t *testing.T, store *manifest.Store, path string) bool {
	t.Helper()
	rows, err := store.UnenrichedTracks(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Path == path {
			return true
		}
	}
	return false
}
