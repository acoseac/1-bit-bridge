package transcode

import (
	"sync"
	"time"
)

// outageRewarnAfter bounds the silence while an outage lasts. An outage ends
// when a job proves it over, and one can be missed: the tool comes back and
// goes again before any job that needs it succeeds, or the directory that
// refused is not written again, and a second outage would otherwise be
// reported by the first one's Warn alone.
const outageRewarnAfter = 24 * time.Hour

// outageStreaks is the per-key streak record behind the pool's two reports of
// jobs that failed for a fact about this HOST rather than their source: the
// tools it lacks (toolOutages) and the output directories it cannot write
// (outputOutages). It is the M-SEARCH rule (discovery.SendFailureLog) applied
// per key: a key's streak starts with the first job that fails for it, counts
// every job after, and ends when a job proves the key over. Its owner reports
// the start at Warn (and again after outageRewarnAfter of silence), the jobs
// between at Debug, and the end at Info, so a queue of 5,000 jobs behind one
// fault logs one Warn instead of 5,000.
//
// One record for both reports, so the two cannot drift on when a streak
// warns or ends; each report keeps its own keys, its own proof and its own
// words.
type outageStreaks[K comparable, F any] struct {
	mu   sync.Mutex
	open map[K]*outageStreak[F]
}

// outageStreak is one key's streak: when it started, when it was last
// reported at Warn, how many jobs it has cost, and the failure that started
// it (which is what an output fault's proof is judged against).
type outageStreak[F any] struct {
	since, warnedAt time.Time
	failed          int
	first           F
}

// endedOutage is a streak a job proved over, with its key.
type endedOutage[K comparable, F any] struct {
	key K
	outageStreak[F]
}

// note records one job that failed for key at now (f is its failure, kept
// when it starts the streak), and returns the streak as it now stands and
// whether to report it at Warn: when it starts, or when outageRewarnAfter has
// passed since it was last reported.
func (o *outageStreaks[K, F]) note(key K, f F, now time.Time) (s outageStreak[F], warn bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.open == nil {
		o.open = map[K]*outageStreak[F]{}
	}
	cur := o.open[key]
	if cur == nil {
		cur = &outageStreak[F]{since: now, first: f}
		o.open[key] = cur
	}
	cur.failed++
	warn = cur.warnedAt.IsZero() || now.Sub(cur.warnedAt) >= outageRewarnAfter
	if warn {
		cur.warnedAt = now
	}
	return *cur, warn
}

// end closes the open streaks, among the keys candidates names and in its
// order, that over reports proven by the job, and returns them. candidates is
// called only while a streak is open, so a success on a healthy host costs a
// lock and a length check.
func (o *outageStreaks[K, F]) end(candidates func() []K, over func(K, F) bool) []endedOutage[K, F] {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.open) == 0 {
		return nil
	}
	var ended []endedOutage[K, F]
	for _, key := range candidates() {
		s, ok := o.open[key]
		if !ok || !over(key, s.first) {
			continue
		}
		ended = append(ended, endedOutage[K, F]{key: key, outageStreak: *s})
		delete(o.open, key)
	}
	return ended
}

// clockNow is now, or time.Now when now is nil: the reports' test seam.
func clockNow(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}
