package transcode

import (
	"slices"
	"testing"
	"time"
)

// TestOutageStreaksEndInTheOrderTheyStarted pins the streak record both host
// reports keep: end reports the streaks a job proves over in the order they
// started, never the map's, keeps the ones it does not prove over in that
// order, and a streak started after an end joins the order at its back. The
// lines a job's proof logs are then in one order every time.
func TestOutageStreaksEndInTheOrderTheyStarted(t *testing.T) {
	var o outageStreaks[string, int]
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	keys := func(ended []endedOutage[string, int]) []string {
		var out []string
		for _, e := range ended {
			out = append(out, e.key)
		}
		return out
	}
	for i, k := range []string{"e", "a", "d", "b", "c"} {
		o.note(k, i, now)
	}
	o.note("a", 99, now) // a later failure keeps the streak, and its first failure

	odd := func(k string, _ int) bool { return k == "e" || k == "d" || k == "c" }
	if got := keys(o.end(odd)); !slices.Equal(got, []string{"e", "d", "c"}) {
		t.Errorf("ended %v, want e, d, c: the order they started", got)
	}
	o.note("f", 5, now)
	var firsts []int
	all := func(_ string, first int) bool { firsts = append(firsts, first); return true }
	if got := keys(o.end(all)); !slices.Equal(got, []string{"a", "b", "f"}) {
		t.Errorf("ended %v, want a, b, f: the ones kept, in their order, then the one started since", got)
	}
	if !slices.Equal(firsts, []int{1, 3, 5}) {
		t.Errorf("over was given the failures %v, want each streak's first (1, 3, 5)", firsts)
	}
	if got := o.end(all); got != nil {
		t.Errorf("end with no streak open returned %v, want nothing", got)
	}
}

// TestLazilyWorksItsAnswerOutOnce pins the helper end's callers use for what
// a proof needs from the job: worked out on the first call, and only then.
func TestLazilyWorksItsAnswerOutOnce(t *testing.T) {
	calls := 0
	f := lazily(func() []string { calls++; return nil })
	if calls != 0 {
		t.Fatalf("lazily called its function %d time(s) before it was asked", calls)
	}
	f()
	f()
	if calls != 1 {
		t.Errorf("lazily called its function %d times over two asks, want once (a nil answer included)", calls)
	}
}
