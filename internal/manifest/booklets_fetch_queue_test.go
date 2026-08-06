package manifest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// steppingClock installs a monotonically advancing clock on the store so
// checked_at values are deterministically ordered without time.Sleep.
func steppingClock(t *testing.T, s *Store) {
	t.Helper()
	var n atomic.Int64
	base := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return base.Add(time.Duration(n.Add(1)) * time.Second) }
}

// seedPendingBooklet records an available-but-unfetched row (the state
// BookletsToFetch selects from).
func seedPendingBooklet(t *testing.T, s *Store, mbid string) {
	t.Helper()
	if err := s.UpsertBookletAvailability(context.Background(), mbid, true, "etag-"+mbid, 1024); err != nil {
		t.Fatalf("seed %s: %v", mbid, err)
	}
}

func fetchQueue(t *testing.T, s *Store, limit, maxAttempts int) []string {
	t.Helper()
	rows, err := s.BookletsToFetch(context.Background(), limit, maxAttempts)
	if err != nil {
		t.Fatalf("BookletsToFetch: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ReleaseMBID)
	}
	return out
}

// TestBookletFetchFailureRotatesTheQueue is the head-of-line regression gate.
//
// BookletsToFetch orders by checked_at ASC, and NOTHING advanced checked_at on
// a failed download: UpsertBookletAvailability is only reached for MBIDs
// BookletsToCheck returned (and that filter excludes available = 1 rows), and
// MarkBookletUnavailable is the 404 path. So a row whose download kept failing
// held its original checked_at forever and stayed pinned at the head of the
// ordering — with bookletFetchPerTick = 3, three such rows consumed the entire
// per-tick budget on every tick and the background pre-cache sweep stopped
// making progress across the whole library.
//
// Negative control: drop the `checked_at = ?` assignment from
// MarkBookletFetchFailed and this test fails — the failing row is still
// selected ahead of the fresh one.
func TestBookletFetchFailureRotatesTheQueue(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	steppingClock(t, s)

	// bkRel1 is checked first, so it heads the queue.
	seedPendingBooklet(t, s, bkRel1)
	seedPendingBooklet(t, s, bkRel2)

	if got := fetchQueue(t, s, 1, 8); len(got) != 1 || got[0] != bkRel1 {
		t.Fatalf("initial queue head = %v, want [%s] (oldest checked_at first)", got, bkRel1)
	}

	// The download fails (not a 404 — a size-cap refusal, a 5xx, a write
	// error). The row must rotate behind the release that has not been tried.
	if err := s.MarkBookletFetchFailed(ctx, bkRel1); err != nil {
		t.Fatalf("MarkBookletFetchFailed: %v", err)
	}

	if got := fetchQueue(t, s, 1, 8); len(got) != 1 || got[0] != bkRel2 {
		t.Fatalf("queue head after a failed fetch = %v, want [%s] — a failing row "+
			"must not keep consuming the per-tick budget ahead of untried releases", got, bkRel2)
	}
	// Both are still pending; only the ORDER changed.
	if got := fetchQueue(t, s, 10, 8); len(got) != 2 || got[0] != bkRel2 || got[1] != bkRel1 {
		t.Fatalf("full queue = %v, want [%s %s]", got, bkRel2, bkRel1)
	}
}

// TestBookletFetchAttemptCapDropsPersistentFailure pins the other half: a row
// that keeps failing eventually leaves the sweep entirely, so the bridge stops
// re-downloading the same failing bytes every tick forever. Below the cap it is
// still a candidate, so a transient failure that clears still lands.
//
// Negative control: drop `AND check_attempts < ?` from BookletsToFetch (or the
// `check_attempts + 1` from MarkBookletFetchFailed) and the capped-out
// assertion fails.
func TestBookletFetchAttemptCapDropsPersistentFailure(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	steppingClock(t, s)

	const attemptCap = 3
	seedPendingBooklet(t, s, bkRel1)

	for i := 1; i < attemptCap; i++ {
		if err := s.MarkBookletFetchFailed(ctx, bkRel1); err != nil {
			t.Fatal(err)
		}
		if got := fetchQueue(t, s, 10, attemptCap); len(got) != 1 {
			t.Fatalf("after %d failed attempts queue = %v, want the row still "+
				"a candidate — a transient failure must be retried", i, got)
		}
	}

	// The attempt that reaches the cap takes it out of contention.
	if err := s.MarkBookletFetchFailed(ctx, bkRel1); err != nil {
		t.Fatal(err)
	}
	if got := fetchQueue(t, s, 10, attemptCap); len(got) != 0 {
		t.Fatalf("queue after %d failed attempts = %v, want empty (cap reached)", attemptCap, got)
	}
	// A non-positive cap means unbounded — fail OPEN, never "fetch nothing".
	if got := fetchQueue(t, s, 10, 0); len(got) != 1 {
		t.Fatalf("queue under an unset cap = %v, want the row (fail-open)", got)
	}

	// A fresh availability verdict clears the tally, so a re-published booklet
	// re-enters the sweep.
	seedPendingBooklet(t, s, bkRel1)
	if got := fetchQueue(t, s, 10, attemptCap); len(got) != 1 {
		t.Fatalf("queue after a fresh availability check = %v, want the row re-armed", got)
	}
}

// TestBookletFetchFailureNeverBurnsACheckAttempt pins the interaction with the
// check-side cap, which shares the check_attempts column.
//
// The two budgets are only disjoint because the state transitions zero the
// counter. MarkBookletUnavailable (the 404 path) moves a row from available = 1
// — where a fetch tally can now have accumulated — to available = 0, which is
// exactly the state BookletsToCheck reads. Without the reset, a release that
// failed to download and then vanished upstream would arrive in the check
// rotation with its budget already spent and never be re-probed.
//
// Negative control: drop `check_attempts = 0` from MarkBookletUnavailable and
// the re-probe assertion fails.
func TestBookletFetchFailureNeverBurnsACheckAttempt(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	steppingClock(t, s)

	const attemptCap = 3
	seedPendingBooklet(t, s, bkRel1)
	for i := 0; i < attemptCap+2; i++ {
		if err := s.MarkBookletFetchFailed(ctx, bkRel1); err != nil {
			t.Fatal(err)
		}
	}

	// While available, the fetch tally is invisible to the check filter
	// (which skips available = 1 rows for its own reasons).
	if got, err := s.BookletsToCheck(ctx, []string{bkRel1}, attemptCap); err != nil || len(got) != 0 {
		t.Fatalf("BookletsToCheck while available = (%v, %v), want none", got, err)
	}

	// Atlas 404s the download: the row flips unavailable and must re-enter the
	// check rotation with a full budget.
	if err := s.MarkBookletUnavailable(ctx, bkRel1); err != nil {
		t.Fatal(err)
	}
	if row := mustGetBooklet(t, s, bkRel1); row.CheckAttempts != 0 {
		t.Fatalf("check_attempts after MarkBookletUnavailable = %d, want 0 — a fetch "+
			"tally must not be carried into the check budget", row.CheckAttempts)
	}
	got, err := s.BookletsToCheck(ctx, []string{bkRel1}, attemptCap)
	if err != nil || len(got) != 1 || got[0] != bkRel1 {
		t.Fatalf("BookletsToCheck after the 404 flip = (%v, %v), want the release re-probed", got, err)
	}

	// And the guard holds the other way: a stale fetch failure landing after
	// the flip must not consume a check attempt.
	if err := s.MarkBookletFetchFailed(ctx, bkRel1); err != nil {
		t.Fatal(err)
	}
	if row := mustGetBooklet(t, s, bkRel1); row.CheckAttempts != 0 {
		t.Fatalf("check_attempts after a fetch failure on an unavailable row = %d, want 0",
			row.CheckAttempts)
	}
}
