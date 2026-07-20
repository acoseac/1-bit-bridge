package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- F22: subscriber caps -------------------------------------------------

// TestSubscribeEnforcesGlobalCap pins the broker-wide subscription bound.
// Without it, `GET /v1/pairing/{id}/events` — which authenticates with the
// pollSecret from an UNAUTHENTICATED POST — lets a remote caller hold
// unlimited goroutines, channels and fds open.
func TestSubscribeEnforcesGlobalCap(t *testing.T) {
	b := newEventBroker()
	subs := make([]*subscriber, 0, maxBrokerSubscribers)
	for i := 0; i < maxBrokerSubscribers; i++ {
		sub, _, err := b.subscribe(nil, "", 0)
		if err != nil {
			t.Fatalf("subscribe %d: unexpected error %v", i, err)
		}
		subs = append(subs, sub)
	}
	if _, _, err := b.subscribe(nil, "", 0); err == nil {
		t.Fatal("subscribe past maxBrokerSubscribers: want error, got nil")
	}
	// A slot must free on disconnect — the cap is transient, not terminal.
	b.unsubscribe(subs[0])
	if _, _, err := b.subscribe(nil, "", 0); err != nil {
		t.Fatalf("subscribe after unsubscribe: want success, got %v", err)
	}
}

// TestSubscribeEnforcesPerTopicCap pins the per-pairing-request bound: one
// pollSecret must not authorise unlimited streams for its own request id.
func TestSubscribeEnforcesPerTopicCap(t *testing.T) {
	b := newEventBroker()
	topic := "pairing.abc123"
	for i := 0; i < maxPairingSubscribersPerRequest; i++ {
		if _, _, err := b.subscribe([]string{topic}, "", maxPairingSubscribersPerRequest); err != nil {
			t.Fatalf("subscribe %d: unexpected error %v", i, err)
		}
	}
	if _, _, err := b.subscribe([]string{topic}, "", maxPairingSubscribersPerRequest); err == nil {
		t.Fatal("subscribe past per-topic cap: want error, got nil")
	}
	// A DIFFERENT request id has its own budget — one caller flooding its own
	// request must not lock out an unrelated pairing attempt.
	if _, _, err := b.subscribe([]string{"pairing.other"}, "", maxPairingSubscribersPerRequest); err != nil {
		t.Fatalf("subscribe for a different topic: want success, got %v", err)
	}
	// And the bearer-authed /v1/events shape (maxPerTopic 0) is unaffected:
	// several of the operator's devices legitimately share topic filters.
	if _, _, err := b.subscribe([]string{topic}, "", 0); err != nil {
		t.Fatalf("subscribe with maxPerTopic=0: want success, got %v", err)
	}
}

// TestSubscribeCapIsRaceFree pins that the cap holds under concurrent
// subscribes. A check-then-act shape outside the lock would let N
// simultaneous callers all observe "under the limit" before any inserted.
func TestSubscribeCapIsRaceFree(t *testing.T) {
	b := newEventBroker()
	const goroutines = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, _, err := b.subscribe([]string{"pairing.same"}, "", maxPairingSubscribersPerRequest); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted > maxPairingSubscribersPerRequest {
		t.Fatalf("cap exceeded under concurrency: accepted %d, cap %d", accepted, maxPairingSubscribersPerRequest)
	}
}

// --- F27: topic-list cap --------------------------------------------------

// TestParseTopicsParamRejectsOversizedList pins the bound on the topics=
// allowlist. subscriber.matches is a linear scan that short-circuits on a
// hit, so non-matching topics force a full walk — for every event and every
// heartbeat, inside the broker's global mutex.
func TestParseTopicsParamRejectsOversizedList(t *testing.T) {
	atCap := strings.Repeat("t,", maxTopicsPerSubscription-1) + "t"
	if _, err := parseTopicsParam(atCap); err != nil {
		t.Fatalf("exactly maxTopicsPerSubscription topics: want success, got %v", err)
	}
	over := strings.Repeat("t,", maxTopicsPerSubscription) + "t"
	if _, err := parseTopicsParam(over); err == nil {
		t.Fatal("past maxTopicsPerSubscription: want error, got nil")
	}
}

// --- F25: history batch decode cap ---------------------------------------

// TestHistoryBatchDecodeCapsStructExpansion pins that a crafted body of
// minimal events can't drive struct allocation past the cap. `{}` is ~3 bytes
// with its separator, so a 4 MiB body decodes to ~1.4M structs pre-fix — and
// the handler then sized its output slice on that same uncapped count.
func TestHistoryBatchDecodeCapsStructExpansion(t *testing.T) {
	const over = historyMaxBatchEvents + 500
	var sb strings.Builder
	sb.WriteString(`{"events":[`)
	for i := 0; i < over; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{}`)
	}
	sb.WriteString(`]}`)

	var body historyBatchRequest
	if err := json.Unmarshal([]byte(sb.String()), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(body.Events.items); got != historyMaxBatchEvents {
		t.Fatalf("materialised %d structs, want the cap %d", got, historyMaxBatchEvents)
	}
	// The dropped count stays EXACT — the decoder parses-and-discards past
	// the cap rather than bailing, so the 202's {accepted,dropped} contract
	// is preserved.
	if got, want := body.Events.overflow, over-historyMaxBatchEvents; got != want {
		t.Fatalf("overflow = %d, want %d", got, want)
	}
}

// TestHistoryBatchDecodeUnderCapRoundTrips guards against the cap breaking
// the normal path — the realistic batch is in the low hundreds.
func TestHistoryBatchDecodeUnderCapRoundTrips(t *testing.T) {
	body := historyBatchRequest{}
	raw := `{"events":[
	  {"path":"A/one.flac","startedAt":1700000000000000000,"durationUsed":12.5,"codec":"FLAC"},
	  {"path":"A/two.flac","startedAt":1700000001000000000,"durationUsed":3}
	]}`
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(body.Events.items); got != 2 {
		t.Fatalf("len(items) = %d, want 2", got)
	}
	if body.Events.overflow != 0 {
		t.Fatalf("overflow = %d, want 0", body.Events.overflow)
	}
	if body.Events.items[0].Path != "A/one.flac" || body.Events.items[0].DurationUsed != 12.5 {
		t.Fatalf("first event decoded wrong: %+v", body.Events.items[0])
	}
	if body.Events.items[1].Codec != "" {
		t.Fatalf("omitted codec should stay zero, got %q", body.Events.items[1].Codec)
	}
}

// TestHistoryBatchDecodeRejectsNonArray pins that a non-array "events" is a
// clean 400-shaped error rather than a silent empty batch.
func TestHistoryBatchDecodeRejectsNonArray(t *testing.T) {
	var body historyBatchRequest
	if err := json.Unmarshal([]byte(`{"events":{"nope":1}}`), &body); err == nil {
		t.Fatal("object for events: want error, got nil")
	}
	// null → empty, matching stdlib slice-decode.
	body = historyBatchRequest{}
	if err := json.Unmarshal([]byte(`{"events":null}`), &body); err != nil {
		t.Fatalf("null events: want success, got %v", err)
	}
	if len(body.Events.items) != 0 || body.Events.overflow != 0 {
		t.Fatalf("null events should decode empty, got %+v", body.Events)
	}
}

// --- F17: endpoint-walk TTL cache ----------------------------------------

// TestEndpointsCacheCollapsesRepeatedWalks pins that /v1/health's interface
// enumeration runs at most once per TTL window. Uncached it was 1 + N kernel
// dumps per request on an unauthenticated, unrate-limited route.
func TestEndpointsCacheCollapsesRepeatedWalks(t *testing.T) {
	c := newEndpointsCache()
	var calls int
	var mu sync.Mutex
	compute := func() []string {
		mu.Lock()
		calls++
		mu.Unlock()
		return []string{"https://example:7788"}
	}
	for i := 0; i < 50; i++ {
		if got := c.endpoints(compute); len(got) != 1 {
			t.Fatalf("endpoints() = %v, want one entry", got)
		}
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("compute called %d times across the TTL window, want 1", got)
	}
}

// TestEndpointsCacheRecomputesAfterTTL pins that the cache actually expires —
// a permanently-frozen endpoint list would hide a real interface change.
func TestEndpointsCacheRecomputesAfterTTL(t *testing.T) {
	c := &endpointsCache{ttl: 10 * time.Millisecond}
	var calls int
	compute := func() []string {
		calls++
		return []string{fmt.Sprintf("https://example:%d", calls)}
	}
	first := c.endpoints(compute)
	time.Sleep(25 * time.Millisecond)
	second := c.endpoints(compute)
	if first[0] == second[0] {
		t.Fatalf("cache did not expire: both reads returned %q", first[0])
	}
	if calls != 2 {
		t.Fatalf("compute called %d times, want 2", calls)
	}
}

// TestEndpointsCacheSingleflightsConcurrentMisses pins that a flood arriving
// at the expiry boundary collapses onto ONE recompute rather than each
// caller running its own interface walk.
func TestEndpointsCacheSingleflightsConcurrentMisses(t *testing.T) {
	c := newEndpointsCache()
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	compute := func() []string {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release // hold the flight open so the others pile up behind it
		return []string{"https://example:7788"}
	}
	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = c.endpoints(compute)
		}()
	}
	// Let the in-flight compute finish once the others have had a chance to
	// join it.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("compute called %d times under a concurrent miss flood, want 1", got)
	}
}
