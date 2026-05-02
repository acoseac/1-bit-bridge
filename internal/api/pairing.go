package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"github.com/acoseac/1-bit-bridge/internal/pairing"
)

// pairingCreateRequest is the POST /v1/pairing/requests body shape iOS sends.
type pairingCreateRequest struct {
	DeviceName     string `json:"deviceName"`
	ClientVersion  string `json:"clientVersion,omitempty"`
	PollSecretHash string `json:"pollSecretHash"` // hex SHA-256 of the iOS-generated 32-byte secret
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
type pairingPollResponse struct {
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
		writeError(w, http.StatusNotFound, "pairing_not_supported",
			"this bridge does not support tap-to-pair")
		return
	}
	// Per-IP rate limit (burst=5, ~1 req / 5 s steady). The pairing
	// flow is unauthenticated, so an unauthenticated rate limit at the
	// entry point is the right shape — a noisy LAN attacker can't
	// burn through the operator's 16-pending admin queue alone. Empty
	// IP (RemoteAddr parse failure) falls open; see allow().
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
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.DeviceName == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "deviceName is required")
		return
	}

	out, err := s.pairing.CreateRequest(
		req.DeviceName,
		req.ClientVersion,
		req.PollSecretHash,
		ip,
		s.fingerprint,
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
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
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
		writeError(w, http.StatusNotFound, "pairing_not_supported",
			"this bridge does not support tap-to-pair")
		return
	}
	id := r.PathValue("requestID")
	secret := extractBearer(r)
	res, err := s.pairing.Poll(id, secret)
	switch {
	case errors.Is(err, pairing.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized",
			"missing or invalid poll secret")
		return
	case errors.Is(err, pairing.ErrNotFound):
		writeError(w, http.StatusNotFound, "unknown_request",
			"no such pairing request (may have expired or been cleaned up)")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
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

// pairingDelete handles DELETE /v1/pairing/{requestID}. Used by iOS for
// both user-cancel and acknowledgment-of-token-receipt. Same auth as Poll.
//
// Idempotent: a second DELETE for the same id returns 200 (the first one
// already removed it). Maps both ErrNotFound (truly gone or already
// cleaned up) and the success path to 200; only auth failures surface as
// 401.
func (s *Server) pairingDelete(w http.ResponseWriter, r *http.Request) {
	if s.pairing == nil {
		writeError(w, http.StatusNotFound, "pairing_not_supported",
			"this bridge does not support tap-to-pair")
		return
	}
	id := r.PathValue("requestID")
	secret := extractBearer(r)
	err := s.pairing.Delete(id, secret)
	switch {
	case errors.Is(err, pairing.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized",
			"missing or invalid poll secret")
		return
	case errors.Is(err, pairing.ErrNotFound), err == nil:
		// Both paths are success at the wire level — the row is gone
		// either way, which is what iOS wants to confirm.
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
}

// clientIP extracts the request's source IP for display in the admin
// pending-requests panel. Strips port and bracket notation. Returns ""
// if the RemoteAddr is unparseable — the field is display-only so an
// empty value just means "unknown".
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}
