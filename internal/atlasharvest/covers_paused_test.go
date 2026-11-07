package atlasharvest

import (
	"context"
	"maps"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// What the cover leg does with a refetcher that answers ErrCoversPaused,
// serve's answer while enrichment is switched off (backlog B617).

var pausedReleases = []string{
	"11111111-1111-4111-8111-111111111111",
	"22222222-2222-4222-8222-222222222222",
	"33333333-3333-4333-8333-333333333333",
}

// pausedClient is a client with every pausedReleases cover pending and its
// refetcher, which counts what it is asked.
func pausedClient(t *testing.T, refetch func(mbid string) (bool, error)) (*Client, *atomic.Int64) {
	t.Helper()
	state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
	if err := state.AddPendingCovers(pausedReleases); err != nil {
		t.Fatal(err)
	}
	asked := &atomic.Int64{}
	return &Client{State: state, Refetcher: refetcherFunc(func(_ context.Context, mbid string) (bool, error) {
		asked.Add(1)
		return refetch(mbid)
	})}, asked
}

// TestAPausedCoverLegLeavesThePendingCoversAsTheyAre: a pause ends the cover
// leg at its first refetch, every tick, and settles nothing, so through more
// ticks than the attempt cap every pending cover keeps its attempt count of
// zero, where as many misses would have dropped each one; nothing is logged.
// Switched on, the next tick drains them.
func TestAPausedCoverLegLeavesThePendingCoversAsTheyAre(t *testing.T) {
	var on atomic.Bool
	c, asked := pausedClient(t, func(string) (bool, error) {
		if !on.Load() {
			return false, ErrCoversPaused
		}
		return true, nil
	})
	rec := loggingtest.Record(t)
	want := map[string]int{pausedReleases[0]: 0, pausedReleases[1]: 0, pausedReleases[2]: 0}
	for tick := 1; tick <= maxCoverRefetchAttempts+2; tick++ {
		c.refreshCovers(context.Background())
		if got := asked.Load(); got != int64(tick) {
			t.Fatalf("after %d paused tick(s) the refetcher was asked %d time(s), want once a tick", tick, got)
		}
	}
	if got := c.State.PendingCoversSnapshot(); !maps.Equal(got, want) {
		t.Errorf("after %d paused ticks the pending covers are %v, want %v", maxCoverRefetchAttempts+2, got, want)
	}
	mustNotReport(t, rec, "atlasharvest.cover_refetch_failed", "atlasharvest.pending_covers.settle_failed")

	on.Store(true)
	c.refreshCovers(context.Background())
	if got := c.State.PendingCoversSnapshot(); len(got) != 0 {
		t.Errorf("switched on, the next tick left %v pending", got)
	}
}

// TestAPauseSettlesWhatTheLegLearntBeforeIt: the switch going off part-way
// through a tick's cover leg settles the answer taken before the pause and
// leaves the covers after it as they were.
func TestAPauseSettlesWhatTheLegLearntBeforeIt(t *testing.T) {
	var first atomic.Value
	c, asked := pausedClient(t, func(mbid string) (bool, error) {
		if first.CompareAndSwap(nil, mbid) {
			return true, nil
		}
		return false, ErrCoversPaused
	})
	c.refreshCovers(context.Background())
	if got := asked.Load(); got != 2 {
		t.Errorf("the refetcher was asked %d time(s), want the cover written and the paused one", got)
	}
	got := c.State.PendingCoversSnapshot()
	resolved, _ := first.Load().(string)
	if _, ok := got[resolved]; ok || len(got) != 2 {
		t.Fatalf("pending after a cover written and a pause = %v, want the two not asked about or paused, %s settled", got, resolved)
	}
	for mbid, attempts := range got {
		if attempts != 0 {
			t.Errorf("%s counted %d attempt(s) through a pause", mbid, attempts)
		}
	}
}

// TestTheLyricsLegRunsWhileTheCoversArePaused: a paused cover leg ends only
// itself: the tick's lyrics leg, its own opt-in, still asks Atlas and keeps
// its document, and the pending cover stays as it was (backlog B617).
func TestTheLyricsLegRunsWhileTheCoversArePaused(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{"rec-1": {{Status: "available", Plain: "words"}}},
		hits:       map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	c.MBIDs = emptyMBIDs{}
	c.Sink = discardMeta{}
	store, err := OpenStateStore(filepath.Join(t.TempDir(), "atlas-harvest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCredential(st.Token, st.AtlasBaseURL, st.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if err := store.AddPendingCovers(pausedReleases[:1]); err != nil {
		t.Fatal(err)
	}
	c.State = store
	var paused atomic.Int64
	c.Refetcher = refetcherFunc(func(context.Context, string) (bool, error) {
		paused.Add(1)
		return false, ErrCoversPaused
	})

	c.tick(context.Background())

	if paused.Load() != 1 {
		t.Errorf("the cover leg was asked %d time(s), want once", paused.Load())
	}
	stub.mu.Lock()
	hits := stub.hits["rec-1"]
	stub.mu.Unlock()
	if _, ok := sink.docs["a/x.flac"]; !ok || hits == 0 {
		t.Errorf("the lyrics leg after a paused cover leg asked Atlas %d time(s) and kept a document %v", hits, ok)
	}
	if got := store.PendingCoversSnapshot(); !maps.Equal(got, map[string]int{pausedReleases[0]: 0}) {
		t.Errorf("the paused cover is pending as %v", got)
	}
}
