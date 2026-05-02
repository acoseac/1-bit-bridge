package api

import (
	"fmt"
	"net/http"
	"strings"
)

// events handles GET /v1/events?topics=upscale,pairing.
//
// Wire shape (RFC EventSource / "Server-Sent Events"):
//   - Content-Type: text/event-stream
//   - Each event is `event: <topic>\ndata: <json>\n[id: <n>\n]\n`
//   - Heartbeat events (`event: heartbeat`) every 15s for liveness
//   - "dropped" events surface a slow-consumer policy bump so iOS can
//     trigger a manual fetch to reconcile state
//
// Authentication: standard `Authorization: Bearer <token>` (the same
// rule as every other authed `/v1/*` endpoint). Pre-PR clients fall
// back to polling on 404 / 401 transparently.
//
// Topic filtering: `?topics=upscale,pairing` registers an allowlist
// (prefix match — "pairing" matches every "pairing.<id>" event). An
// absent or empty topics param subscribes to all topics.
//
// Last-Event-ID replay: the standard `Last-Event-ID` header lets a
// reconnecting client request events newer than the last ID it saw.
// The broker holds a bounded replay buffer; reconnects within the
// buffer window get the missed events, beyond it get a "dropped"
// hint so iOS knows it needs to refetch state.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if s.eventBroker == nil {
		// Broker not wired (test harness, or feature disabled
		// in a future config). 404 is the back-compat shape iOS
		// is built to recognise — falls back to polling.
		writeError(w, http.StatusNotFound, "events_not_supported",
			"this bridge does not support push events; clients should use the polling endpoints")
		return
	}

	topics := parseTopicsParam(r.URL.Query().Get("topics"))
	lastEventID := r.Header.Get("Last-Event-ID")

	// Disable response encoding negotiation. A future global gzip
	// middleware would buffer the response until close (defeating
	// `Flush()`) — explicit `identity` is a tripwire AND a
	// defence. Tested by `TestEventsResponseIsNotGzipped`.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Content-Encoding", "identity")
	// Defensive against fronting reverse-proxies (nginx,
	// cloudflare) that buffer responses by default.
	w.Header().Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)

	// Subscribe BEFORE flushing headers so the replay set is
	// captured atomically with the live subscription — no missed
	// events between "I have a Last-Event-ID" and "I'm in the
	// fan-out".
	sub, replay := s.eventBroker.subscribe(topics, lastEventID)
	defer s.eventBroker.unsubscribe(sub)

	// Send headers + an initial flush so iOS's URLSession.bytes
	// hits the open-stream path immediately, not when the first
	// event lands.
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		// Connection already gone (rare — would mean the client
		// closed before we wrote anything). Nothing to recover.
		return
	}

	// Replay any events the subscriber missed since lastEventID.
	for _, env := range replay {
		if err := writeEvent(w, env); err != nil {
			return
		}
	}
	if len(replay) > 0 {
		if err := rc.Flush(); err != nil {
			return
		}
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// Client disconnected (background, navigated away,
			// network blip). Unsubscribe runs via defer.
			return
		case env, ok := <-sub.ch:
			if !ok {
				return
			}
			// Synthetic "dropped" notice if the broker had to
			// evict events from this subscriber's queue.
			if dropped := sub.dropped.Swap(0); dropped > 0 {
				notice := eventEnvelope{
					Topic: "dropped",
					Data:  []byte(fmt.Sprintf(`{"missed":%d}`, dropped)),
				}
				if err := writeEvent(w, notice); err != nil {
					return
				}
			}
			if err := writeEvent(w, env); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// parseTopicsParam splits the comma-separated topics= query param
// into an allowlist. Trims whitespace; drops empty entries. Empty
// input means "all topics" (handled by subscriber.matches).
func parseTopicsParam(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// writeEvent serialises a single event envelope to the SSE wire. Each
// event ends with a blank line per the EventSource spec; iOS's line-
// buffered AsyncSequence parser dispatches at the blank line.
//
// Don't use fmt.Fprintf — explicit Write avoids the variadic
// allocation hot-path on every event, and the topic / data are
// already in the right shape (no escaping needed: topics are
// alphanumeric+. and data is pre-encoded JSON).
func writeEvent(w http.ResponseWriter, env eventEnvelope) error {
	if env.ID != "" {
		if _, err := w.Write([]byte("id: " + env.ID + "\n")); err != nil {
			return err
		}
	}
	if env.Topic != "" {
		if _, err := w.Write([]byte("event: " + env.Topic + "\n")); err != nil {
			return err
		}
	}
	// data: <json>\n\n  — the trailing blank line terminates the event.
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(env.Data); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	return nil
}
