package transcode

import (
	"context"
	"sync"
	"testing"
	"time"
)

// dedupSpec builds a JobSpec whose dedup key is stable across calls, so
// repeated enqueues of it contend for exactly one inflight slot.
func dedupSpec(rel string) JobSpec {
	return JobSpec{
		SourceLibraryRel: rel,
		SourceAbsPath:    "/nonexistent/" + rel,
		OutputDir:        "/nonexistent/out",
		TargetSampleRate: 192000,
		TargetBits:       24,
		Kind:             JobKindUpscale,
	}
}

// TestDropInflightThenCompletionDoesNotReleaseTheResubmission is the
// regression test for the ownership-checked dedup release. It drives the
// exact interleaving that DropInflight makes reachable:
//
//	A enqueued, worker holds A
//	DropInflight frees the key       (by design — a resubmission must land)
//	B enqueued, worker holds B
//	A completes                      <- must NOT release B's claim
//	C enqueued                       <- must be refused while B runs
//
// If A's completion releases B's claim, C is admitted and two workers
// run the same (source, variant). They share one deterministic
// `SidecarPath() + ".tmp"`, and RunSox opens every job by removing that
// path to clear crash debris — so the later starter unlinks the
// earlier's in-progress output and they race the rename, publishing a
// truncated sidecar as a complete variant.
func TestDropInflightThenCompletionDoesNotReleaseTheResubmission(t *testing.T) {
	const rel = "Artist/Album/01.flac"

	// Gate the runner so a job can be held mid-flight while the test
	// manipulates the dedup map underneath it.
	var (
		mu       sync.Mutex
		release  = map[int]chan struct{}{}
		started  = make(chan int, 8)
		runCount int
	)
	p := NewPool(nil, 2, 8)
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		mu.Lock()
		runCount++
		id := runCount
		ch := make(chan struct{})
		release[id] = ch
		mu.Unlock()
		started <- id
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return 0, nil
	}
	defer p.Stop()

	releaseRun := func(id int) {
		mu.Lock()
		ch := release[id]
		mu.Unlock()
		close(ch)
	}
	awaitStart := func(t *testing.T, what string) int {
		t.Helper()
		select {
		case id := <-started:
			return id
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never started", what)
			return 0
		}
	}

	// A: enqueued and held mid-run.
	if err := p.Enqueue(dedupSpec(rel)); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	idA := awaitStart(t, "A")

	// The variant-delete handler frees the key while A is still
	// running. That is deliberate: a resubmission must not coalesce
	// against a worker about to write a sidecar the caller is deleting.
	if dropped := p.DropInflight(func(src string) bool { return src == rel }); dropped != 1 {
		t.Fatalf("DropInflight dropped %d, want 1", dropped)
	}

	// B: the resubmission. Claims the same key, and is now the owner.
	if err := p.Enqueue(dedupSpec(rel)); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	idB := awaitStart(t, "B")

	// A completes. Its release must be a no-op — the key it was
	// enqueued under is now B's.
	releaseRun(idA)

	// C must be refused for as long as B holds the claim. Poll rather
	// than sleep-once: A's release happens on its worker goroutine, so
	// a single immediate check could pass simply by racing ahead of the
	// bug rather than by the fix working.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := p.Enqueue(dedupSpec(rel)); err != ErrDuplicateInflight {
			t.Fatalf("a third enqueue was admitted (err=%v) while B is still "+
				"running: A's completion released B's dedup claim. Two workers "+
				"now share one SidecarPath()+\".tmp\", which RunSox removes at "+
				"job start — the later starter unlinks the earlier's output", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// And once B really finishes, the key must free up again —
	// otherwise the ownership check has simply wedged the slot.
	releaseRun(idB)
	freed := false
	for deadline = time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if err := p.Enqueue(dedupSpec(rel)); err == nil {
			freed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !freed {
		t.Fatal("after B completed the dedup key stayed occupied — the " +
			"ownership check must still release the claim its own job holds")
	}
}

// The ordinary path must be untouched: a job that still owns its key
// releases it on completion, so the same (source, variant) can be
// enqueued again afterwards.
func TestReleaseDedupFreesTheClaimItOwns(t *testing.T) {
	const rel = "Artist/Album/02.flac"
	done := make(chan struct{}, 4)

	p := NewPool(nil, 1, 4)
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		done <- struct{}{}
		return 0, nil
	}
	defer p.Stop()

	for i := 0; i < 3; i++ {
		var err error
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if err = p.Enqueue(dedupSpec(rel)); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("run %d: the key never freed after the owning job "+
				"completed: %v", i+1, err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d never executed", i+1)
		}
	}
}

// Claims are monotonic and never reused, which is what makes a stale
// release identifiable. Starting at 1 is load-bearing: a missing map
// entry reads as the zero value, and a claim of 0 would make every
// stale release match it.
func TestClaimGenerationsAreMonotonicAndNonZero(t *testing.T) {
	p := NewPool(nil, 1, 16)
	defer p.Stop()

	seen := map[uint64]bool{}
	var prev uint64
	for i := 0; i < 8; i++ {
		p.mu.Lock()
		p.claimSeq++
		got := p.claimSeq
		p.mu.Unlock()

		if got == 0 {
			t.Fatal("claim generation 0 issued — it collides with the zero " +
				"value a missing inflight entry reads as, so every stale " +
				"release would match")
		}
		if got <= prev {
			t.Fatalf("claim %d did not advance past %d", got, prev)
		}
		if seen[got] {
			t.Fatalf("claim %d reused", got)
		}
		seen[got] = true
		prev = got
	}
}
