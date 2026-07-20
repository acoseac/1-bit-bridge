package api

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// eventBroker is the in-process pub-sub backing GET /v1/events. One
// shared goroutine fans out published events to all live subscribers;
// each subscriber owns a small per-subscriber channel buffered for
// burst absorption. The cardinality is "one operator, one or two
// devices" so the simple shared-goroutine fan-out beats per-subscriber
// goroutines on both code complexity and footprint.
//
// Topics are flat strings (e.g. "upscale.stats", "pairing.<id>"). A
// subscriber registers an allowlist; the broker drops events whose
// topic isn't in the subscriber's set. Allowlist match is a prefix
// check on "<area>." so "pairing" matches "pairing.<id>" without the
// caller having to enumerate all in-flight requestIDs.
//
// Slow consumer policy: per-subscriber channel is buffered. When full,
// the broker drops the OLDEST event (LRU) and emits a synthetic
// "dropped" event with the count missed since the last delivery. iOS
// consumes "dropped" as a signal to do a manual fetch and reconcile
// state. This matches the CodeRabbit "slow consumer" concern shape
// flagged on the rate-limiter PR — fail-detect rather than fail-silent.
//
// Lifecycle: created at api.Server construction, started via
// `apiSrv.StartEventBroker()` returning a stopFn — same pattern as the
// pairing rate-limit GC. cmd/bridge/main.go defers the stopFn so the
// broker drains cleanly on graceful shutdown.

// eventEnvelope is a single publishable event.
type eventEnvelope struct {
	Topic string          `json:"-"` // routing key, NOT serialised in `data:`
	Data  json.RawMessage `json:"-"` // pre-encoded JSON payload
	ID    string          `json:"-"` // monotonic per-broker, used for SSE Last-Event-ID
}

// subscriber holds a single connected client's state. Owned by the
// SSE handler; lifecycle is tied to the request context (handler exit
// → unsubscribe → channel close).
type subscriber struct {
	ch          chan eventEnvelope // buffered; broker writes, handler reads
	topics      []string           // allowlist (prefix match — "pairing" → "pairing.*")
	lastEventID string             // SSE Last-Event-ID for replay on reconnect
	dropped     atomic.Int64       // count of events dropped since last delivery
}

// matches reports whether the topic should be delivered to this
// subscriber. Prefix match on "topic" or "topic." so callers can
// register "pairing" and receive every "pairing.<id>" event without
// enumerating in-flight requests. Empty topics list = all events.
func (sub *subscriber) matches(topic string) bool {
	if len(sub.topics) == 0 {
		return true
	}
	for _, allowed := range sub.topics {
		if topic == allowed {
			return true
		}
		// Prefix match: "pairing" matches "pairing.x" but not "pairingX".
		// Direct slice/byte compares avoid the `allowed+"."` string this
		// fanout-loop hot path otherwise allocated on every event.
		if len(topic) > len(allowed) && topic[len(allowed)] == '.' && topic[:len(allowed)] == allowed {
			return true
		}
	}
	return false
}

// eventBroker fan-out cardinality. With "one operator, one or two
// devices" cardinality, a small fixed buffer is sufficient. The
// publish channel size sets a soft cap on how many events the broker
// can have in flight before the publisher blocks; we keep this small
// because publishers (transcode pool, pairing.Store) are all in-
// process and a brief blocking pulse beats unbounded growth.
const (
	eventPublishBufferSize     = 64
	subscriberChannelBufferLen = 16
	heartbeatInterval          = 15 * time.Second

	// maxBrokerSubscribers bounds concurrent SSE subscriptions across the
	// whole broker. Each subscription costs a goroutine, a 16-slot channel,
	// and a held TLS conn + fd — and `fanoutLocked` walks EVERY subscriber
	// under `b.mu`, so an unbounded map degrades the 15s heartbeat for all
	// clients well before memory runs out.
	//
	// Load-bearing because `GET /v1/pairing/{id}/events` is reachable
	// WITHOUT a bearer token (it authenticates with the pollSecret from an
	// unauthenticated POST /v1/pairing/requests), so nothing else caps how
	// many streams a remote caller can hold open. The bound is generous
	// against the real shape — "one operator, a handful of devices", each
	// holding one /v1/events stream plus at most one pairing stream.
	maxBrokerSubscribers = 256

	// maxPairingSubscribersPerRequest bounds streams sharing ONE pairing
	// request id. A single pollSecret must not authorise unlimited
	// subscriptions; 3 covers a legitimate retry racing a stale connection
	// that hasn't been reaped yet. Applied only to the pairing route —
	// `/v1/events` is bearer-authed and passes 0 (no per-topic cap), since
	// several devices legitimately share the same topic filters there.
	maxPairingSubscribersPerRequest = 3
)

// errTooManySubscribers is returned by subscribe when either the global or
// the per-topic subscription cap would be exceeded. Handlers map it to 503
// with a Retry-After — the condition is transient (a cap frees as soon as
// any stream disconnects), so it is deliberately not a 4xx.
var errTooManySubscribers = errors.New("too many active event subscribers")

type eventBroker struct {
	publish chan eventEnvelope

	// Single mu covers BOTH subscribers and replayBuffer. Combined
	// access is required so subscribe() can capture replay AND
	// insert the new subscriber atomically with respect to the
	// broker goroutine's record+fanout cycle (which also takes
	// this lock). Without the combined coverage, events published
	// in the gap between replay-capture and subscriber-insertion
	// would either duplicate or vanish — see subscribe() doc for
	// the trade-off math. Contention is low (subscribe is rare,
	// fan-out is hot but short-held).
	mu             sync.Mutex
	subscribers    map[*subscriber]struct{}
	replayBuffer   []eventEnvelope
	replayCapacity int

	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
	startOnce sync.Once // guards Start() against duplicate goroutine spawn

	nextID atomic.Int64
}

func newEventBroker() *eventBroker {
	return &eventBroker{
		publish:        make(chan eventEnvelope, eventPublishBufferSize),
		subscribers:    make(map[*subscriber]struct{}),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
		replayCapacity: 100,
	}
}

// Publish accepts a typed payload, encodes it once, and queues it for
// the broker goroutine to fan out. Non-blocking: the publish channel
// is buffered, but if full, the OLDEST queued event is dropped to
// make room for the new one (LRU eviction at the broker level,
// matching the slow-consumer policy at the subscriber level).
//
// Why drop-oldest at the publisher rather than the previous
// goroutine-per-overflow approach: under burst load the
// goroutine-spawn path could pile up an unbounded queue of blocked
// goroutines AND deliver events to subscribers in scheduler-
// dependent order, breaking the monotonic event-ID contract iOS
// relies on for replay. Drop-oldest preserves order (the queue
// stays a FIFO) and bounds memory at `eventPublishBufferSize`.
// CodeRabbit on PR #135 caught the goroutine-spawn antipattern.
//
// The dropped event would have lived for milliseconds at most before
// fan-out anyway — discarding it under sustained back-pressure is
// preferable to either blocking the publisher (would stall the
// transcode pool / pairing.Store) or unbounded queueing (memory DoS
// surface). iOS subscribers see a `dropped` notice via the existing
// per-subscriber bookkeeping when their fan-out queue is the
// bottleneck.
func (b *eventBroker) Publish(topic string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		// Caller passed something non-encodable. Log + drop —
		// shipping a typed event with broken JSON would just
		// confuse iOS more than dropping it.
		logger.Error("event broker: marshal failed",
			"topic", topic, "err", err)
		return
	}
	id := b.nextID.Add(1)
	env := eventEnvelope{
		Topic: topic,
		Data:  data,
		ID:    formatEventID(id),
	}
	for {
		select {
		case b.publish <- env:
			return
		default:
			// Channel full. Drop the oldest queued event to
			// make room. Loop because a concurrent producer
			// may have just refilled the slot we tried to
			// claim.
			select {
			case <-b.publish:
				// Slot freed; loop sends the new event.
			default:
				// Channel concurrently drained — race-fine.
			}
		}
	}
}

// formatEventID renders a monotonic int64 as a stable string for the
// SSE `id:` field. iOS sends it back via `Last-Event-ID` on reconnect.
func formatEventID(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

// Start launches the broker goroutine. Returns a stop func that
// signals shutdown and waits for the goroutine to exit. Caller
// (cmd/bridge/main.go) defers the stop func.
//
// Idempotent: calling Start a second time is a no-op (returns a
// stop fn that delegates to the same underlying Stop). Without the
// `startOnce` guard, a second Start would spawn another goroutine
// that defers `close(b.doneCh)` on the same channel — Stop's
// `<-b.doneCh` would unblock for the first goroutine, then the
// second goroutine's deferred close would panic on
// already-closed-channel. CodeRabbit + Qodo on PR #135 caught this.
func (b *eventBroker) Start() func() {
	b.startOnce.Do(func() {
		go b.run()
	})
	return func() {
		b.Stop()
	}
}

// Stop signals the broker goroutine to exit and waits for it. Idempotent.
func (b *eventBroker) Stop() {
	b.stopOnce.Do(func() {
		close(b.stopCh)
	})
	<-b.doneCh
}

func (b *eventBroker) run() {
	defer close(b.doneCh)
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-b.stopCh:
			return
		case env := <-b.publish:
			// One critical section covers BOTH replay-buffer
			// append AND subscriber fan-out. subscribe() holds
			// the same lock for replay-capture +
			// subscriber-insertion. Result: a publish either
			// fully completes (reaching the replay buffer +
			// every then-live subscriber) before subscribe()
			// snapshots, OR fully completes after — never
			// half-and-half. Closes the duplicate-vs-missed
			// race the bots flagged.
			b.mu.Lock()
			b.recordReplayLocked(env)
			b.fanoutLocked(env)
			b.mu.Unlock()
		case <-heartbeat.C:
			b.mu.Lock()
			b.fanoutHeartbeatLocked()
			b.mu.Unlock()
		}
	}
}

// fanout writes the event to every matching subscriber, applying the
// drop-oldest policy on full channels. Caller MUST hold b.mu — used
// from `run()` so the record+fanout cycle is atomic with respect to
// `subscribe()`.
func (b *eventBroker) fanoutLocked(env eventEnvelope) {
	for sub := range b.subscribers {
		if !sub.matches(env.Topic) {
			continue
		}
		select {
		case sub.ch <- env:
		default:
			// Channel full. Drop the oldest in-flight event by
			// reading one off, then send the new one. Bumps the
			// dropped counter so the handler can emit a synthetic
			// "dropped" notice to the wire.
			select {
			case <-sub.ch:
				sub.dropped.Add(1)
			default:
				// Channel concurrently drained — race-fine.
			}
			select {
			case sub.ch <- env:
			default:
				// Still full (rare). Drop this event entirely.
				sub.dropped.Add(1)
			}
		}
	}
}

// fanoutHeartbeatLocked sends an empty heartbeat envelope to every
// subscriber. Caller MUST hold b.mu (called from run()).
func (b *eventBroker) fanoutHeartbeatLocked() {
	env := eventEnvelope{
		Topic: "heartbeat",
		Data:  []byte("{}"),
		ID:    "",
	}
	for sub := range b.subscribers {
		select {
		case sub.ch <- env:
		default:
			// Heartbeat full = subscriber is wedged. Drop
			// silently (better than blocking the broker on a
			// non-load-bearing keepalive).
		}
	}
}

// subscribe registers a subscriber atomically with respect to the
// broker's record+fanout cycle. Returns the subscriber handle and
// any events the broker has cached newer than `lastEventID` (for SSE
// reconnect replay). The replay slice is empty for first-time
// subscribers (lastEventID == "").
//
// Atomicity is load-bearing: replay-capture and subscriber-insertion
// MUST happen in a single critical section that the broker
// goroutine's record+fanout cycle also serializes against. Otherwise
// an event published in the gap is either delivered twice (via
// replay AND fan-out — Qodo + CodeRabbit on PR #135's first commit)
// OR missed entirely (the naive "do replay first" fix). The single
// `mu` covering BOTH subscribers map AND replay buffer plus the
// broker goroutine taking the same lock for record+fanout closes
// both windows: from any handler's perspective, the broker's
// processing of an event is either fully before or fully after the
// subscribe() call.
//
// Capacity: refuses with errTooManySubscribers when the global
// maxBrokerSubscribers cap is reached, or when `maxPerTopic` > 0 and any
// requested topic already has that many subscribers. Both checks run inside
// the same critical section as the insert, so concurrent subscribes cannot
// race past the cap (the check-then-act shape would let N simultaneous
// requests all observe "under the limit" before any inserted).
//
// Caller MUST call unsubscribe when the request ends — and MUST NOT call it
// when subscribe returned an error (there is nothing registered to remove).
func (b *eventBroker) subscribe(topics []string, lastEventID string, maxPerTopic int) (*subscriber, []eventEnvelope, error) {
	sub := &subscriber{
		ch:          make(chan eventEnvelope, subscriberChannelBufferLen),
		topics:      topics,
		lastEventID: lastEventID,
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) >= maxBrokerSubscribers {
		return nil, nil, errTooManySubscribers
	}
	if maxPerTopic > 0 {
		for _, t := range topics {
			if b.countExactTopicLocked(t) >= maxPerTopic {
				return nil, nil, errTooManySubscribers
			}
		}
	}
	replay := b.replaySinceLocked(lastEventID, topics)
	b.subscribers[sub] = struct{}{}
	return sub, replay, nil
}

// countExactTopicLocked counts subscribers registered for exactly `topic`.
// Exact (not prefix) match: the per-topic cap exists to bound streams sharing
// one pairing request id, and a prefix match would let an unrelated broad
// subscription ("pairing") consume another request's budget. O(subscribers),
// but bounded by maxBrokerSubscribers and only paid on subscribe, which is
// rare relative to fan-out. Caller MUST hold b.mu.
func (b *eventBroker) countExactTopicLocked(topic string) int {
	n := 0
	for sub := range b.subscribers {
		for _, t := range sub.topics {
			if t == topic {
				n++
				break
			}
		}
	}
	return n
}

// unsubscribe removes a subscriber. Idempotent — calling twice is a
// no-op.
func (b *eventBroker) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	delete(b.subscribers, sub)
	b.mu.Unlock()
}

// recordReplayLocked appends the event to the bounded replay buffer.
// Caller MUST hold b.mu.
func (b *eventBroker) recordReplayLocked(env eventEnvelope) {
	b.replayBuffer = append(b.replayBuffer, env)
	if len(b.replayBuffer) > b.replayCapacity {
		b.replayBuffer = b.replayBuffer[len(b.replayBuffer)-b.replayCapacity:]
	}
}

// replaySinceLocked returns events newer than lastEventID that match
// the subscriber's topics. Empty lastEventID returns nothing (first-
// connect subscribers don't get history). An ID we don't recognise
// (older than the buffer's oldest) also returns nothing — iOS
// interprets that as "I missed too much; refetch state". Caller
// MUST hold b.mu.
func (b *eventBroker) replaySinceLocked(lastEventID string, topics []string) []eventEnvelope {
	if lastEventID == "" {
		return nil
	}
	startIdx := -1
	for i, env := range b.replayBuffer {
		if env.ID == lastEventID {
			startIdx = i + 1 // events AFTER this id
			break
		}
	}
	if startIdx == -1 {
		return nil
	}
	dummy := &subscriber{topics: topics}
	out := make([]eventEnvelope, 0, len(b.replayBuffer)-startIdx)
	for _, env := range b.replayBuffer[startIdx:] {
		if dummy.matches(env.Topic) {
			out = append(out, env)
		}
	}
	return out
}

// EventPublisher is the interface upstream services (transcode pool,
// pairing store) consume to publish events without taking a hard
// dependency on the api package. Wired at cmd/bridge boundary.
type EventPublisher interface {
	Publish(topic string, payload interface{})
}

// nopEventPublisher is the default when no broker is wired (test
// harnesses, pre-this-PR bridges). All Publish calls drop silently.
type nopEventPublisher struct{}

func (nopEventPublisher) Publish(string, interface{}) {
	// Intentional no-op — see the nopEventPublisher type docstring.
}
