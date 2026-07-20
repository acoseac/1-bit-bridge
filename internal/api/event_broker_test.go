package api

import (
	"sync"
	"testing"
	"time"
)

// awaitTrue polls `cond` every 1ms until it returns true or the
// deadline elapses. Replaces fixed `time.Sleep(...)` calls in tests
// that were previously waiting "long enough" for the broker
// goroutine to process work — flaky in slow CI environments where
// the guess is too short, wasteful where it's too long. CodeRabbit
// on PR #135 second-pass.
func awaitTrue(t *testing.T, deadline time.Duration, msg string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition never satisfied within %s: %s", deadline, msg)
}

// TestEventBroker_PublishedEventReachesSubscriber is the headline
// contract: a single subscriber registered for "upscale" sees an
// event published to that topic.
func TestEventBroker_PublishedEventReachesSubscriber(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	sub, _, _ := b.subscribe([]string{"upscale"}, "", 0)
	defer b.unsubscribe(sub)

	b.Publish("upscale.stats", map[string]int{"queued": 3})

	select {
	case env := <-sub.ch:
		if env.Topic != "upscale.stats" {
			t.Errorf("topic = %q, want upscale.stats", env.Topic)
		}
		if string(env.Data) != `{"queued":3}` {
			t.Errorf("data = %s, want {\"queued\":3}", env.Data)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("event was not delivered within 100ms")
	}
}

// TestEventBroker_PrefixMatchAllowsPairingWildcard: a subscriber
// registered for "pairing" must receive every "pairing.<id>" event
// without enumerating in-flight requestIDs. This is the load-bearing
// shape iOS will use.
func TestEventBroker_PrefixMatchAllowsPairingWildcard(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	sub, _, _ := b.subscribe([]string{"pairing"}, "", 0)
	defer b.unsubscribe(sub)

	b.Publish("pairing.abc123", map[string]string{"state": "approved"})

	select {
	case env := <-sub.ch:
		if env.Topic != "pairing.abc123" {
			t.Errorf("topic = %q, want pairing.abc123", env.Topic)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("prefix-matched event was not delivered")
	}
}

// TestEventBroker_NonMatchingTopicFiltered: a subscriber registered
// for "upscale" must NOT receive a "pairing.<id>" event.
func TestEventBroker_NonMatchingTopicFiltered(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	sub, _, _ := b.subscribe([]string{"upscale"}, "", 0)
	defer b.unsubscribe(sub)

	b.Publish("pairing.abc123", map[string]string{"state": "approved"})

	select {
	case env := <-sub.ch:
		t.Errorf("non-matching topic was delivered: %q", env.Topic)
	case <-time.After(50 * time.Millisecond):
		// expected — filter dropped the event
	}
}

// TestEventBroker_EmptyTopicsListMatchesAll: an empty allowlist means
// "subscribe to everything" (server-side default for clients that
// omit ?topics=).
func TestEventBroker_EmptyTopicsListMatchesAll(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	sub, _, _ := b.subscribe(nil, "", 0)
	defer b.unsubscribe(sub)

	b.Publish("anything.goes", map[string]int{})
	b.Publish("upscale.stats", map[string]int{})

	count := 0
	deadline := time.After(100 * time.Millisecond)
	for count < 2 {
		select {
		case <-sub.ch:
			count++
		case <-deadline:
			t.Errorf("expected 2 events, got %d", count)
			return
		}
	}
}

// TestEventBroker_SlowConsumerDropsOldest: when a subscriber's channel
// fills (because the handler isn't reading fast enough), the broker
// drops the OLDEST event and increments the dropped counter so the
// handler can emit a synthetic "dropped" notice on the next delivery.
func TestEventBroker_SlowConsumerDropsOldest(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	sub, _, _ := b.subscribe(nil, "", 0)
	defer b.unsubscribe(sub)

	// Fill subscriber buffer + 5 more (the buffer is 16; we fire
	// 25 to force ≥9 drops).
	for i := 0; i < 25; i++ {
		b.Publish("upscale.stats", i)
	}

	// Wait for the broker goroutine to record at least one drop, proving
	// the slow-consumer eviction fired. Bounded poll instead of a fixed
	// sleep so slow CI doesn't flake.
	awaitTrue(t, 500*time.Millisecond,
		"dropped counter never incremented (slow-consumer policy didn't evict)",
		func() bool { return sub.dropped.Load() > 0 })

	// Quiesce the broker BEFORE draining. Otherwise its fan-out goroutine
	// keeps delivering the still-queued publishes into sub.ch while the
	// non-blocking drain below frees a slot on every receive that the
	// broker immediately refills — under CPU saturation (e.g. the full
	// `make` gate's parallel packages + cross-compile) the broker outruns
	// the drain and `drained` overshoots the buffer cap, flaking the test.
	// Stop() waits for the goroutine to exit and does NOT close sub.ch, so
	// the drain reads a stable buffered snapshot; the deferred stop() is
	// then an idempotent no-op.
	stop()

	// Once the broker is stopped the channel content is stable. The buffer
	// filled to capacity before the first drop (awaitTrue confirmed a drop
	// happened, which can't occur until the buffer is full), and drop-oldest
	// keeps it full thereafter — so the drained count is a hard, race-free
	// EXACT buffer capacity. Asserting equality (not just an upper bound)
	// also catches a regression that under-delivers / loses events. (Gemini
	// on PR #430.)
	drained := 0
loop:
	for {
		select {
		case <-sub.ch:
			drained++
		default:
			break loop
		}
	}
	if drained != subscriberChannelBufferLen {
		t.Errorf("drained %d events; expected exactly %d (full buffer)", drained, subscriberChannelBufferLen)
	}
}

// TestEventBroker_StopDrainsCleanly: Stop() returns only after the
// goroutine has exited. Idempotent — calling Stop twice doesn't panic.
func TestEventBroker_StopDrainsCleanly(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	stop() // first call drains
	stop() // second call must be a no-op (Stop is idempotent)
}

// TestEventBroker_ReplaySinceReturnsNothingForUnknownID: a Last-
// Event-ID we don't recognise (older than the buffer) returns no
// replay events. iOS interprets the empty replay + a "dropped" hint
// as "I missed too much; refetch state".
func TestEventBroker_ReplaySinceReturnsNothingForUnknownID(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	b.Publish("upscale.stats", 1)
	// Bounded poll until the broker records the event in its
	// replay buffer; replaces a fixed 20ms sleep that was flaky
	// under CI load.
	awaitTrue(t, 500*time.Millisecond,
		"broker did not record published event in replay buffer",
		func() bool {
			b.mu.Lock()
			defer b.mu.Unlock()
			return len(b.replayBuffer) >= 1
		})

	// Subscribe with a Last-Event-ID we never issued.
	_, replay, _ := b.subscribe(nil, "9999999", 0)
	if len(replay) != 0 {
		t.Errorf("expected empty replay for unknown ID, got %d events", len(replay))
	}
}

// TestEventBroker_ReplaySinceReturnsEventsAfterID: a known Last-
// Event-ID returns events strictly after it.
func TestEventBroker_ReplaySinceReturnsEventsAfterID(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	b.Publish("upscale.stats", 1) // id = "1"
	b.Publish("upscale.stats", 2) // id = "2"
	b.Publish("upscale.stats", 3) // id = "3"
	// Wait for all three to land in the replay buffer before the
	// reconnect read.
	awaitTrue(t, 500*time.Millisecond,
		"broker did not record all three events",
		func() bool {
			b.mu.Lock()
			defer b.mu.Unlock()
			return len(b.replayBuffer) >= 3
		})

	_, replay, _ := b.subscribe(nil, "1", 0)
	if len(replay) != 2 {
		t.Errorf("expected 2 events after id=1, got %d", len(replay))
	}
}

// TestEventBroker_ConcurrentPublishersAreSafe: hammering Publish from
// many goroutines must not race or panic.
func TestEventBroker_ConcurrentPublishersAreSafe(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	sub, _, _ := b.subscribe(nil, "", 0)
	defer b.unsubscribe(sub)

	const goroutines = 10
	const perGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				b.Publish("upscale.stats", i)
			}
		}()
	}
	wg.Wait()

	// Drain whatever the subscriber received — assertion is
	// "no panic, race-detector clean", not exact count
	// (slow-consumer policy is allowed to drop).
	drained := 0
	deadline := time.After(200 * time.Millisecond)
loop:
	for {
		select {
		case <-sub.ch:
			drained++
		case <-deadline:
			break loop
		}
	}
	t.Logf("drained %d events out of %d published", drained, goroutines*perGoroutine)
}

// TestEventBroker_NopPublisherDoesNotPanic: the back-compat publisher
// returned when no broker is wired silently drops everything.
func TestEventBroker_NopPublisherDoesNotPanic(t *testing.T) {
	var p EventPublisher = nopEventPublisher{}
	p.Publish("upscale.stats", "anything") // must not panic
	p.Publish("pairing.abc", nil)          // nil payload also fine
}

// TestEventBroker_HeartbeatAndDroppedEventsOmitID: PROTOCOL.md
// promises that synthetic transport-layer signals (heartbeat,
// dropped) are NOT part of the Last-Event-ID stream — including
// them would let a heartbeat-only window mask a missed publish on
// reconnect. The broker enforces this by leaving `ID` empty on
// those envelopes; writeEvent's "if env.ID != \"\" { write id }"
// branch then drops the line from the wire. CodeRabbit on PR #135
// caught the docs claiming "every event carries id" — this test is
// the tripwire for the contract.
func TestEventBroker_HeartbeatAndDroppedEventsOmitID(t *testing.T) {
	heartbeat := eventEnvelope{Topic: "heartbeat", Data: []byte("{}"), ID: ""}
	dropped := eventEnvelope{Topic: "dropped", Data: []byte(`{"missed":3}`), ID: ""}
	if heartbeat.ID != "" {
		t.Errorf("heartbeat envelope must have empty ID, got %q", heartbeat.ID)
	}
	if dropped.ID != "" {
		t.Errorf("dropped envelope must have empty ID, got %q", dropped.ID)
	}
}

// TestEventBroker_StartIsIdempotent: calling Start() multiple times
// must NOT spawn multiple run goroutines. Without the sync.Once
// guard, a second Start would defer close(b.doneCh) on the same
// channel — Stop's <-b.doneCh would unblock for the first goroutine,
// then the second goroutine's deferred close would panic on
// already-closed-channel. CodeRabbit + Qodo on PR #135 caught this.
func TestEventBroker_StartIsIdempotent(t *testing.T) {
	b := newEventBroker()
	stop1 := b.Start()
	stop2 := b.Start() // must NOT spawn a second goroutine
	stop3 := b.Start() // ditto

	// All three stop fns delegate to the same broker.Stop; one
	// call drains the goroutine, the rest are no-ops. No panic on
	// already-closed-channel.
	stop1()
	stop2()
	stop3()
}

// TestEventBroker_SubscribeIsAtomicWithFanout drives a small
// burst of publishes during a concurrent subscribe() call. Without
// the consolidated locking, an event published in the gap between
// "capture replay" and "join fan-out" would either be delivered
// twice (broker started recording before subscribe ran, ALSO
// delivered after) or vanish entirely (recorded BEFORE replay
// snapshot, fan-out reached subscriber AFTER it joined). With
// proper locking, every published event is delivered exactly once.
func TestEventBroker_SubscribeIsAtomicWithFanout(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	// Establish a known last-event-ID by subscribing FIRST, then
	// publishing the seed event so the probe captures its ID
	// reliably (publishing before subscribe means the probe joins
	// after fan-out and would miss the seed).
	probe, _, _ := b.subscribe(nil, "", 0)
	defer b.unsubscribe(probe)
	b.Publish("upscale.stats", 0)
	var firstID string
	select {
	case env := <-probe.ch:
		firstID = env.ID
	case <-time.After(100 * time.Millisecond):
		t.Fatal("did not receive seed event")
	}

	// Now drive a publish burst while reconnecting (subscribe
	// with the firstID). The reconnecting subscriber MUST receive
	// each event exactly once — either via replay or via live
	// fan-out — never both.
	const burstSize = 20
	go func() {
		for i := 0; i < burstSize; i++ {
			b.Publish("upscale.stats", i)
		}
	}()

	// Race the subscribe() against the publish burst.
	sub, replay, _ := b.subscribe(nil, firstID, 0)
	defer b.unsubscribe(sub)

	seenIDs := map[string]int{}
	for _, env := range replay {
		seenIDs[env.ID]++
	}

	// Drain whatever the live fan-out delivered.
	deadline := time.After(300 * time.Millisecond)
loop:
	for {
		select {
		case env := <-sub.ch:
			seenIDs[env.ID]++
		case <-deadline:
			break loop
		}
	}

	for id, count := range seenIDs {
		if count > 1 {
			t.Errorf("event id=%s delivered %d times (replay+live duplicate); subscribe is not atomic with fan-out",
				id, count)
		}
	}
}
