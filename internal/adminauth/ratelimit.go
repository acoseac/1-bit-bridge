package adminauth

import (
	"sync"
	"time"
)

// Rate-limit policy. 5 failed attempts per (clientIP, username)
// per 15 minutes; on threshold, the handler should slow the
// response (5s wait) instead of returning 429 — don't tell the
// attacker they're being throttled, and don't fast-fail (which
// would otherwise let them parallelise attempts cheaply).
//
// The 5s delay is the load-bearing part: it stretches a 100-attempt
// dictionary attack from microseconds to 500 seconds while bcrypt's
// per-attempt cost (~250ms at cost 12) does the heavy lifting on
// the verification side. Hostile networks see a hard ceiling on
// throughput.
const (
	RateLimitMaxAttempts = 5
	RateLimitWindow      = 15 * time.Minute
	RateLimitDelay       = 5 * time.Second

	// janitorInterval is how often we sweep expired buckets out
	// of the map. Cheap (one Lock + iterate); doesn't need to be
	// frequent because buckets are small and short-lived.
	janitorInterval = 1 * time.Hour
)

// bucket tracks failed attempts for a single (IP, username) key.
// attempts is the count within the current window; firstAttempt
// marks when the window started (rolling-window reset semantics).
type bucket struct {
	attempts      int
	firstAttempt  time.Time
	lastAttemptAt time.Time
}

// RateLimiter is a process-local, in-memory failed-login tracker.
// No persistence — restart wipes the state. That's fine; the
// bcrypt cost per attempt prevents online brute force from being
// meaningful in any time window the limiter would care about, and
// a determined attacker willing to wait through restarts can be
// blocked at the firewall layer.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	stopCh  chan struct{}
	done    chan struct{}
}

// NewRateLimiter creates a limiter and starts its janitor
// goroutine. The janitor exits when Stop is called.
func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{
		buckets: make(map[string]*bucket),
		now:     time.Now,
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go rl.runJanitor()
	return rl
}

// Stop terminates the janitor goroutine. Safe to call multiple
// times; subsequent calls are no-ops.
func (rl *RateLimiter) Stop() {
	select {
	case <-rl.stopCh:
		return
	default:
	}
	close(rl.stopCh)
	<-rl.done
}

// Allow reports whether the (clientIP, username) bucket is below
// the failure threshold. Does NOT increment — call RecordFailure
// after a verification attempt fails. Allow / RecordFailure are
// split so a successful Verify doesn't artificially bump the
// counter on the success path.
func (rl *RateLimiter) Allow(clientIP, username string) bool {
	key := clientIP + "|" + username
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		return true
	}
	if rl.now().Sub(b.firstAttempt) > RateLimitWindow {
		// Window expired; the limiter no longer holds this
		// bucket against the caller. RecordFailure will reset
		// the window if a fresh failure lands.
		return true
	}
	return b.attempts < RateLimitMaxAttempts
}

// RecordFailure increments the failed-attempt counter for the
// given key. Resets the window when the previous window expired.
// Caller hands the resulting bucket state back via Allow on the
// next attempt.
func (rl *RateLimiter) RecordFailure(clientIP, username string) {
	key := clientIP + "|" + username
	now := rl.now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok || now.Sub(b.firstAttempt) > RateLimitWindow {
		rl.buckets[key] = &bucket{
			attempts:      1,
			firstAttempt:  now,
			lastAttemptAt: now,
		}
		return
	}
	b.attempts++
	b.lastAttemptAt = now
}

// RecordSuccess clears the failure history for a key — a
// successful login should give the operator a clean slate so a
// past typo storm doesn't carry forward.
func (rl *RateLimiter) RecordSuccess(clientIP, username string) {
	key := clientIP + "|" + username
	rl.mu.Lock()
	delete(rl.buckets, key)
	rl.mu.Unlock()
}

// runJanitor sweeps expired buckets. Idle for janitorInterval
// then takes the lock briefly. The work is bounded by len(buckets)
// which is bounded by the number of recently-active (IP, user)
// pairs — small in any realistic deployment.
func (rl *RateLimiter) runJanitor() {
	defer close(rl.done)
	tick := time.NewTicker(janitorInterval)
	defer tick.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-tick.C:
			rl.sweep()
		}
	}
}

func (rl *RateLimiter) sweep() {
	cutoff := rl.now().Add(-RateLimitWindow)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for k, b := range rl.buckets {
		if b.lastAttemptAt.Before(cutoff) {
			delete(rl.buckets, k)
		}
	}
}
