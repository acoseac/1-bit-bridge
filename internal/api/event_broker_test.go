package api

import (
	"sync"
	"testing"
	"time"
)

// TestEventBroker_PublishedEventReachesSubscriber is the headline
// contract: a single subscriber registered for "upscale" sees an
// event published to that topic.
func TestEventBroker_PublishedEventReachesSubscriber(t *testing.T) {
	b := newEventBroker()
	stop := b.Start()
	defer stop()

	sub, _ := b.subscribe([]string{"upscale"}, "")
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

	sub, _ := b.subscribe([]string{"pairing"}, "")
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

	sub, _ := b.subscribe([]string{"upscale"}, "")
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

	sub, _ := b.subscribe(nil, "")
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

	sub, _ := b.subscribe(nil, "")
	defer b.unsubscribe(sub)

	// Fill subscriber buffer + 5 more (the buffer is 16; we fire
	// 25 to force ≥9 drops).
	for i := 0; i < 25; i++ {
		b.Publish("upscale.stats", i)
	}

	// Give the broker goroutine time to process the publish queue.
	time.Sleep(50 * time.Millisecond)

	if d := sub.dropped.Load(); d == 0 {
		t.Errorf("dropped counter never incremented; expected slow-consumer policy to evict")
	}

	// Drain whatever the subscriber actually has — must not exceed
	// buffer size.
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
	if drained > subscriberChannelBufferLen+1 {
		// +1 because a heartbeat may have landed between fills
		t.Errorf("drained %d events; expected ≤ %d", drained, subscriberChannelBufferLen+1)
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
	time.Sleep(20 * time.Millisecond) // let the broker record it

	// Subscribe with a Last-Event-ID we never issued.
	_, replay := b.subscribe(nil, "9999999")
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
	time.Sleep(50 * time.Millisecond)

	_, replay := b.subscribe(nil, "1")
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

	sub, _ := b.subscribe(nil, "")
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
