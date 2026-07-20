package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/pairing"
)

// minPairingStreamLifetime floors the pairing SSE stream's bounded lifetime.
// An already-approved request reports a remaining TTL of <= 0, and the stream
// must still stay open long enough for the token to reach iOS and for its
// DELETE acknowledgment to land.
const minPairingStreamLifetime = 90 * time.Second

// Shared pairing-error messages surfaced by every /v1/pairing*
// handler. Four+ duplicates each across the create / poll / SSE /
// acknowledge paths.
const (
	errMsgPairingNotSupported = "this bridge does not support tap-to-pair"
	errMsgPairingBadSecret    = "missing or invalid poll secret"
)

// pairingCreateRequest is the POST /v1/pairing/requests body shape iOS sends.
type pairingCreateRequest struct {
	DeviceName     string `json:"deviceName"`
	ClientVersion  string `json:"clientVersion,omitempty"`
	PollSecretHash string `json:"pollSecretHash"` // hex SHA-256 of the iOS-generated 32-byte secret
	// DeviceToken is the iOS client's durable recovery token (Keychain,
	// device-local). Optional / additive — pre-feature clients omit it.
	// Lets the admin bind the device_registrations row (with a real
	// device name) at approval time. Capped by pairingMaxBodyBytes along
	// with the rest of the body.
	DeviceToken string `json:"deviceToken,omitempty"`
}

// pairingCreateResponse is the 201 body returned to iOS.
//
// `bridgeStartedAt` is Unix milliseconds — iOS observes this on the
// initial POST and on every poll; a value change between calls means
// the bridge restarted mid-pairing and iOS surfaces a terminal "bridge
// restarted, please request again" rather than blindly retrying. Same
// timestamp the api Server captures on construction.
type pairingCreateResponse struct {
	RequestID        string `json:"requestId"`
	VerificationCode string `json:"verificationCode"`
	TTLSeconds       int    `json:"ttlSeconds"`
	BridgeStartedAt  int64  `json:"bridgeStartedAt"`
}

// pairingPollResponse is the 200 body for GET /v1/pairing/{requestID}.
//
// Token / TokenID are only set when status == "approved". They are
// returned on every authorized poll while the request is in that state
// (NOT read-once) — see the pairing package doc for the read-many
// delivery contract.
//
// Aliased to the exported `PairingStateEvent` (below) so the SSE
// publisher in cmd/bridge can build the same wire shape directly,
// keeping the polling endpoint and the SSE event JSON-identical from
// iOS's decoder perspective.
type pairingPollResponse = PairingStateEvent

// PairingStateEvent is the wire shape the bridge publishes on the SSE
// `pairing.<requestID>` topic AND returns from `GET /v1/pairing/{id}`.
// Single source of truth — iOS reuses one decoder across both transports.
//
// JSON tags match the polling contract; do NOT rename without bumping
// `ProtocolVersion` and a Mirror-PR pair on the iOS side.
type PairingStateEvent struct {
	Status              string `json:"status"`
	TTLSecondsRemaining int    `json:"ttlSecondsRemaining"`
	BridgeStartedAt     int64  `json:"bridgeStartedAt"`
	VerificationCode    string `json:"verificationCode,omitempty"`
	Token               string `json:"token,omitempty"`
	TokenID             string `json:"tokenId,omitempty"`
}

// pairingMaxBodyBytes caps the POST body size — pollSecretHash is 64
// chars, deviceName / clientVersion are short strings; even
// mega-padded JSON is well under 4 KiB. Defence-in-depth against a
// malicious client trying to OOM the JSON decoder.
const pairingMaxBodyBytes = 4 * 1024

// pairingRequest handles POST /v1/pairing/requests. Unauthenticated by
// design — the pollSecret hash IS the authentication for subsequent
// polls. The TLS pin captured at first contact is the only trust anchor
// at this point.
func (s *Server) pairingRequest(w http.ResponseWriter, r *http.Request) {
	if s.pairing == nil {
		writeError(w, http.StatusNotFound, "pairing_not_supported", errMsgPairingNotSupported)
		return
	}
	// Per-IP rate limit (burst=5, ~1 req / 5 s steady). The pairing
	// flow is unauthenticated, so an unauthenticated rate limit at the
	// entry point is the right shape — a noisy LAN attacker can't
	// burn through the operator's 16-pending admin queue alone. A
	// portless RemoteAddr keys on the raw host (see clientIP); only a
	// genuinely empty RemoteAddr yields "" and falls open (see allow()).
	ip := clientIP(r)
	if !s.pairingRateLimiter.allow(ip) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"too many pairing requests; try again in a few seconds")
		return
	}
	var req pairingCreateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, pairingMaxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request",
			"request body must be JSON", err)
		return
	}
	if req.DeviceName == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "deviceName is required")
		return
	}

	// Validate the optional device token at the boundary, reusing the same
	// lowercase-hex/length rule the authed X-Device-Token path enforces. A
	// malformed value can't match a later authed request, so it would only
	// store an orphan registration on approve — drop it to "" (the binding
	// then forms cleanly on the device's first authed request) rather than
	// failing the whole pairing over an optional field. (Gemini on PR #334.)
	deviceToken := req.DeviceToken
	if deviceToken != "" && !validDeviceToken(deviceToken) {
		deviceToken = ""
	}
	out, err := s.pairing.CreateRequest(
		req.DeviceName,
		req.ClientVersion,
		req.PollSecretHash,
		ip,
		s.fingerprint,
		deviceToken,
	)
	switch {
	case errors.Is(err, pairing.ErrBadHash):
		writeError(w, http.StatusBadRequest, "bad_request",
			"pollSecretHash must be 64 hex characters (SHA-256 of the poll secret)")
		return
	case errors.Is(err, pairing.ErrQueueFull):
		writeError(w, http.StatusServiceUnavailable, "queue_full",
			"too many pending pairing requests; try again later")
		return
	case err != nil:
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't create this pairing request", err)
		return
	}

	writeJSON(w, http.StatusCreated, pairingCreateResponse{
		RequestID:        out.ID,
		VerificationCode: out.VerificationCode,
		TTLSeconds:       s.pairing.TTLSeconds(),
		BridgeStartedAt:  s.startedAt.UnixMilli(),
	})
}

// pairingPoll handles GET /v1/pairing/{requestID}.
//
// Authentication: Authorization: Bearer <pollSecret> — the raw secret
// whose SHA-256 was submitted at request creation. The Store
// constant-time-compares the hash internally.
//
// Status mapping:
//   - 200 + body for any of pending/approved/declined/expired/cert_rotated
//   - 401 for an absent or mismatched pollSecret
//   - 404 for an unknown requestID (treated as terminal by iOS)
func (s *Server) pairingPoll(w http.ResponseWriter, r *http.Request) {
	if s.pairing == nil {
		writeError(w, http.StatusNotFound, "pairing_not_supported", errMsgPairingNotSupported)
		return
	}
	id := r.PathValue("requestID")
	secret := extractBearer(r)
	res, err := s.pairing.Poll(id, secret)
	switch {
	case errors.Is(err, pairing.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", errMsgPairingBadSecret)
		return
	case errors.Is(err, pairing.ErrNotFound):
		writeError(w, http.StatusNotFound, "unknown_request",
			"no such pairing request (may have expired or been cleaned up)")
		return
	case err != nil:
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't poll this pairing request", err)
		return
	}
	writeJSON(w, http.StatusOK, pairingPollResponse{
		Status:              res.State.String(),
		TTLSecondsRemaining: res.TTLSecondsRemaining,
		BridgeStartedAt:     s.startedAt.UnixMilli(),
		VerificationCode:    res.VerificationCode,
		Token:               res.Token,
		TokenID:             res.TokenID,
	})
}

// pairingEvents handles GET /v1/pairing/{requestID}/events.
//
// Push-delivery sibling of pairingPoll. iOS connects with the same
// `Authorization: Bearer <pollSecret>` shape as polling and receives
// `pairing.<requestID>` SSE events as the request transitions
// (Approve, Decline, timer-expiry).
//
// Why a separate endpoint and not /v1/events?topics=pairing.<id>:
// /v1/events is bearer-token-authed (a minted `auth.Store` token),
// but the pairing flow has no token until *after* approval — iOS
// can't subscribe with a token it doesn't have yet. The pollSecret
// + requestID pair is what authorises seeing the request's state
// (and its eventual minted token); routing the SSE through this
// pollSecret-authed endpoint lets iOS get push delivery for the
// pairing flow without mixing auth schemes on the bus endpoint.
//
// Auth + initial state: a single `s.pairing.Poll(id, secret)` call
// does double duty — authenticates (404/401 on miss/mismatch) AND
// returns the current state, which we emit as the FIRST event on
// the wire. iOS doesn't have to wait for the next state change to
// know what state the request is in (helpful when the user
// foregrounds the app mid-pairing).
//
// Status mapping:
//   - 200 + SSE stream: authed; state events flow until the client
//     disconnects or the request's lifecycle ends
//   - 401: missing/invalid pollSecret
//   - 404: unknown requestID OR the bridge doesn't have a broker
//     wired (treated as terminal by iOS — falls back to polling)
func (s *Server) pairingEvents(w http.ResponseWriter, r *http.Request) {
	if s.pairing == nil {
		writeError(w, http.StatusNotFound, "pairing_not_supported", errMsgPairingNotSupported)
		return
	}
	if s.eventBroker == nil {
		// Broker not wired (test harness, or feature disabled in
		// a future config). Same back-compat shape as /v1/events
		// — iOS treats 404 as "fall back to polling."
		writeError(w, http.StatusNotFound, "events_not_supported",
			"this bridge does not support push pairing events; client should use the polling endpoint")
		return
	}
	id := r.PathValue("requestID")
	secret := extractBearer(r)
	res, err := s.pairing.Poll(id, secret)
	switch {
	case errors.Is(err, pairing.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", errMsgPairingBadSecret)
		return
	case errors.Is(err, pairing.ErrNotFound):
		writeError(w, http.StatusNotFound, "unknown_request",
			"no such pairing request (may have expired or been cleaned up)")
		return
	case err != nil:
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't poll this pairing request", err)
		return
	}

	// Same defensive headers as /v1/events — see events.go for the
	// rationale on Content-Encoding: identity + X-Accel-Buffering: no
	// (defends against future global gzip middleware and fronting
	// reverse proxies that buffer response bodies).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Content-Encoding", "identity")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)

	// Scope the subscription to JUST this request's topic. The
	// prefix-match contract on the broker means "pairing.<id>"
	// matches exact + any future "pairing.<id>.<subtopic>" without
	// leaking other in-flight pairing requests' state to this
	// caller (which only authenticated for THIS request).
	topic := "pairing." + id
	// Subscribe atomically with respect to the broker's record+fanout
	// cycle (broker docs at internal/api/event_broker.go:304). The
	// subscription is armed BEFORE we re-read state, so any transition
	// that lands in the gap between this subscribe and the re-Poll
	// below is delivered to our channel instead of being silently
	// missed. Gemini + Qodo + CodeRabbit all flagged the prior
	// subscribe-after-Poll shape as a stuck-on-stale-state hazard.
	//
	// Capacity: this route authenticates with the pollSecret from an
	// UNAUTHENTICATED POST /v1/pairing/requests, so one request id must not
	// authorise unlimited streams — hence the per-topic cap on top of the
	// broker-global one.
	sub, _, subErr := s.eventBroker.subscribe([]string{topic}, "", maxPairingSubscribersPerRequest)
	if subErr != nil {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"too many active pairing streams; retry shortly")
		return
	}
	defer s.eventBroker.unsubscribe(sub)

	// Re-Poll AFTER subscribe so the initial state event reflects any
	// transition that landed during the auth-Poll → subscribe gap. The
	// broker also delivers that transition through `sub.ch`, which
	// produces a duplicate event on the wire — iOS's state writes are
	// idempotent, so a duplicate is harmless. (Missing the transition
	// entirely was the real bug: with no replay buffer behind us, an
	// approve/decline that fired before subscribe armed would never
	// reach this client.)
	if refreshed, pollErr := s.pairing.Poll(id, secret); pollErr == nil {
		res = refreshed
	}

	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	// Send the current state as the first event. iOS doesn't have
	// to wait for the next transition to render — same payload
	// shape as the polling endpoint AND the broker's
	// `pairing.<id>` events (they all share the PairingStateEvent
	// wire shape).
	initial := PairingStateEvent{
		Status:              res.State.String(),
		TTLSecondsRemaining: res.TTLSecondsRemaining,
		BridgeStartedAt:     s.startedAt.UnixMilli(),
		VerificationCode:    res.VerificationCode,
		Token:               res.Token,
		TokenID:             res.TokenID,
	}
	if data, jsonErr := json.Marshal(initial); jsonErr == nil {
		// Synthetic event ID "0" — broker IDs are monotonic from 1+
		// so iOS can distinguish the initial-state synth from
		// real broker events when comparing Last-Event-ID. (Today
		// pairing reconnect doesn't use Last-Event-ID — the
		// initial-state-on-connect mechanism replaces it — but
		// the distinction keeps the wire honest, and an empty ID
		// would omit the `id:` field entirely per writeEvent's
		// contract — Gemini caught the comment/code mismatch.)
		if err := writeEvent(w, eventEnvelope{
			Topic: topic,
			Data:  data,
			ID:    "0",
		}); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}

	// Bound the stream's own lifetime to the pairing row's deadline. The loop
	// below selects only on ctx.Done() and sub.ch and never re-checks pairing
	// state, so without this a stream OUTLIVES the request (whose row the
	// store deletes at TTL+grace) and is never reaped — the goroutine,
	// channel, fd and TLS conn stay held indefinitely while nothing can ever
	// send to it again. `pairing.DefaultGrace` matches the store's own
	// terminal-state linger, and the floor keeps an already-approved request
	// (whose remaining TTL may read <= 0) open long enough for the token
	// delivery + DELETE acknowledgment round trip.
	lifetime := time.Duration(res.TTLSecondsRemaining)*time.Second + pairing.DefaultGrace
	if lifetime < minPairingStreamLifetime {
		lifetime = minPairingStreamLifetime
	}
	streamDeadline := time.NewTimer(lifetime)
	defer streamDeadline.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-streamDeadline.C:
			// Past TTL+grace the row is gone from the store; holding the
			// connection open serves nothing. iOS reconnects if it still
			// cares (it re-POSTs a fresh pairing request on expiry).
			return
		case env, ok := <-sub.ch:
			if !ok {
				return
			}
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

// pairingDelete handles DELETE /v1/pairing/{requestID}. Used by iOS for
// both user-cancel and acknowledgment-of-token-receipt. Same auth as Poll.
//
// Idempotent: a second DELETE for the same id returns 204 (the first one
// already removed it). Maps both ErrNotFound (truly gone or already
// cleaned up) and the success path to 204 No Content; only auth failures
// surface as 401.
func (s *Server) pairingDelete(w http.ResponseWriter, r *http.Request) {
	if s.pairing == nil {
		writeError(w, http.StatusNotFound, "pairing_not_supported", errMsgPairingNotSupported)
		return
	}
	id := r.PathValue("requestID")
	secret := extractBearer(r)
	err := s.pairing.Delete(id, secret)
	switch {
	case errors.Is(err, pairing.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", errMsgPairingBadSecret)
		return
	case errors.Is(err, pairing.ErrNotFound), err == nil:
		// Both paths are success at the wire level — the row is gone
		// either way, which is what iOS wants to confirm.
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't delete this pairing request", err)
		return
	}
}

// clientIP extracts the request's source IP. It is used BOTH as the
// pairing rate-limiter key (pairingRateLimiter.allow) and for display in
// the admin pending-requests panel, so it must not collapse distinct
// hosts onto one key. Strips the port and IPv6 brackets.
//
// On a RemoteAddr with no port (a test harness, or an exotic proxy that
// rewrites it) SplitHostPort errors; we fall back to the raw address with
// brackets trimmed rather than "" so the limiter still keys on a stable
// per-host value instead of the empty-key fall-open bucket. A genuinely
// empty RemoteAddr still yields "" (the limiter falls open on that — see
// allow()), which is the right behaviour when the source is truly unknown.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.Trim(r.RemoteAddr, "[]")
	}
	return host
}
