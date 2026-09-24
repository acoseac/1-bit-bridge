package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// heldAnalysisPool returns a one-worker pool that finishes nothing while the
// test runs: the worker holds its first job until Stop cancels it, and the
// rest wait in the queue. So every path the test enqueues stays queued or
// running across sweeps, the state a long first analysis leaves the pool in.
//
// Stop is registered after the caller's store cleanup, so it runs first
// (cleanups are LIFO), and it counts nothing: the job it cancels and the jobs
// it drains are all uncounted, and none of them writes to the store.
func heldAnalysisPool(t *testing.T, store *manifest.Store) *analyze.Pool {
	t.Helper()
	pool := analyze.NewPool(store, 1, 16,
		analyze.WithFsync(func(string) error { return nil }),
		analyze.WithRunner(func(ctx context.Context, _ analyze.AnalyzeSpec) (analyze.Result, error) {
			<-ctx.Done()
			return analyze.Result{}, ctx.Err()
		}))
	t.Cleanup(pool.Stop)
	return pool
}

// TestASecondSweepCountsTheQueuedBacklogAsAlreadyQueued pins what a sweep
// reports while an earlier sweep's jobs are still waiting.
//
// A track has no analysis row until its job finishes, so every sweep during a
// long first analysis re-offers the whole backlog. The pool answered a path it
// already held with nil, and enqueueAll counted every nil as enqueued: each
// sweep reported a full queue of old work (DefaultAnalysisQueueCap is 5000,
// plus the jobs running) as new, in the `auto-analysis sweep enqueued tracks`
// line and in the Jobs card's `enqueued`, a part of a line that has to add up
// to the track total.
//
// The second sweep re-offers three held paths and one new one. The honest
// answer (1 enqueued, 3 already queued) and the old one (4 enqueued) differ in
// exactly the field that was wrong, and the new job is checked against the
// pool's own counter rather than against a number this test chose.
func TestASecondSweepCountsTheQueuedBacklogAsAlreadyQueued(t *testing.T) {
	root := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	addTrack := func(name string) {
		t.Helper()
		body := []byte("fLaC-nonzero-bytes")
		if err := os.WriteFile(filepath.Join(root, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: name, Size: int64(len(body)), ModTime: time.Now()}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", name, err)
		}
	}
	for _, name := range []string{"a.flac", "b.flac", "c.flac"} {
		addTrack(name)
	}
	pool := heldAnalysisPool(t, store)
	s := &analysisSweeper{
		store: store, resolver: bridgefs.New([]string{root}),
		outputDir: t.TempDir(), pool: pool, enabled: alwaysAnalysisEnabled,
	}

	first := s.sweep(ctx)
	if first == nil || first.Enqueued != 3 || first.AlreadyQueued != 0 {
		t.Fatalf("first sweep = %+v, want 3 enqueued and 0 already queued", first)
	}
	if st := pool.Stats(); st.Inflight != 3 || st.Done+st.Failed != 0 {
		t.Fatalf("after the first sweep the pool reads %+v, want all three paths still held "+
			"and none finished: the fixture is not the state this test is about", st)
	}

	addTrack("d.flac")
	second := s.sweep(ctx)
	if second == nil {
		t.Fatal("the second sweep recorded no counts")
	}
	accepted := int(pool.Stats().Enqueued) - first.Enqueued
	if accepted != 1 {
		t.Fatalf("the pool accepted %d new job(s) on the second sweep, want 1 (d.flac)", accepted)
	}
	if second.Enqueued != accepted {
		t.Errorf("the second sweep reported %d enqueued while the pool accepted %d: "+
			"paths still queued from the first sweep were reported as new work", second.Enqueued, accepted)
	}
	if second.AlreadyQueued != 3 {
		t.Errorf("the second sweep reported %d already queued, want 3 (a, b and c are still held)", second.AlreadyQueued)
	}
	if second.Total != 4 || second.QueueSaturated {
		t.Errorf("the second sweep = %+v, want 4 tracks and a queue with room", second)
	}
}

// TestBridgeAnalyzeDispatchesPastAPathAlreadyQueued pins how `bridge
// analyze`'s producer loop reads the pool's answers.
//
// The loop stops on any error it does not recognise, so once Enqueue answers a
// held path with ErrDuplicateInflight rather than nil, the loop has to accept
// it in the same change: otherwise a run stops dispatching at the first
// duplicate and leaves the rest of the library unanalysed. A duplicate is
// accepted work, since the job already queued produces the waveform.
//
// Today's candidates cannot produce one. Their paths are the tracks table's
// primary key, offered once each to a pool the run itself created. The arm is
// there for the day that changes, which is why the test drives the loop with
// an enqueue of its own rather than through a candidate list.
//
// The closed-pool case is the control: that answer still stops the dispatch,
// so a loop that ignored every error could not pass.
func TestBridgeAnalyzeDispatchesPastAPathAlreadyQueued(t *testing.T) {
	candidates := []analyze.AnalyzeSpec{
		{SourceLibraryRel: "a.flac"}, {SourceLibraryRel: "b.flac"}, {SourceLibraryRel: "c.flac"},
	}
	for _, tc := range []struct {
		name string
		atB  error
		want []string
	}{
		{"a path already queued is accepted", analyze.ErrDuplicateInflight, []string{"a.flac", "b.flac", "c.flac"}},
		{"a closed pool stops the dispatch", analyze.ErrPoolClosed, []string{"a.flac", "b.flac"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var offered []string
			enqueue := func(spec analyze.AnalyzeSpec) error {
				offered = append(offered, spec.SourceLibraryRel)
				if spec.SourceLibraryRel == "b.flac" {
					return tc.atB
				}
				return nil
			}
			if dispatchAnalysisCandidates(context.Background(), enqueue, candidates) {
				t.Fatal("the dispatch reported an interrupt, with a context nothing cancels")
			}
			if !slices.Equal(offered, tc.want) {
				t.Errorf("offered %v, want %v", offered, tc.want)
			}
		})
	}
}
