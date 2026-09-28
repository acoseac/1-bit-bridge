package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/pairingcode"
)

// PairingCodeTaker redeems the one-time code a pairing link carries.
// *pairingcode.Store satisfies it.
type PairingCodeTaker interface {
	Take(code string) (tokenID string, ok bool)
}

// pairingRedeemMaxBodyBytes caps the body: a 43-character code in a
// one-field JSON object.
const pairingRedeemMaxBodyBytes = 1024

// pairingRedeemRequest is the POST /v1/pairing/redeem body.
type pairingRedeemRequest struct {
	Code string `json:"code"`
}

// pairingRedeemResponse is the 200 body: the device's bearer token, which
// has never travelled in a link, and its ID.
type pairingRedeemResponse struct {
	Token   string `json:"token"`
	TokenID string `json:"tokenId"`
}

// pairingRedeem handles POST /v1/pairing/redeem, the app's half of the
// pairing link's one-time code (internal/pairingcode; the 2026-09-23
// audit's H1). Unauthenticated: the code IS the credential, and the TLS pin
// the link carries is the trust anchor, as for POST /v1/pairing/requests.
//
// The code is taken (removed) before anything else is decided, then the
// token it names is ROTATED: the device receives a fresh secret for the
// same token record (its name, ID and history), and the secret the link
// carried stops validating in the same commit. So a copy of the link is
// dead once the real device has paired, and if a copy is redeemed first,
// the real device's redemption fails where the user can see it.
//
// Every refusal of a code (unknown, used, expired, or naming a token the
// operator revoked since) is the same 410, so the endpoint answers nothing
// about which codes exist. It draws from the per-IP pairing limiter
// POST /v1/pairing/requests uses.
func (s *Server) pairingRedeem(w http.ResponseWriter, r *http.Request) {
	if s.pairingCodes == nil {
		writeError(w, http.StatusNotFound, "pairing_code_not_supported",
			"this bridge does not issue pairing codes")
		return
	}
	if !s.pairingRateLimiter.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"too many pairing requests; try again in a few seconds")
		return
	}
	var req pairingRedeemRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, pairingRedeemMaxBodyBytes)).Decode(&req); err != nil {
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request", "body must be {\"code\": \"...\"}", err)
		return
	}
	if !pairingcode.ValidShape(req.Code) {
		writeError(w, http.StatusBadRequest, "bad_request", "code must be the 43-character pairing code")
		return
	}
	tokenID, ok := s.pairingCodes.Take(req.Code)
	if !ok {
		writeGonePairingCode(w)
		return
	}
	raw, tok, err := s.store.Rotate(tokenID)
	if errors.Is(err, auth.ErrNotFound) {
		writeGonePairingCode(w)
		return
	}
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "persist_failed", "failed to issue the device's token", err)
		return
	}
	// Rotate keeps ExpiresAt, and Validate refuses a token past it, so
	// handing over the fresh secret of an expired token would pair a
	// device that is refused on its first request. Answer as for any
	// other code that cannot be redeemed; the link's token was dead
	// already, so the rotation cost nothing.
	if tok.ExpiresAt != nil && !tok.ExpiresAt.IsZero() && time.Now().After(*tok.ExpiresAt) {
		writeGonePairingCode(w)
		return
	}
	writeJSON(w, http.StatusOK, pairingRedeemResponse{Token: raw, TokenID: tok.ID})
}

// writeGonePairingCode is the one answer to a code that cannot be redeemed.
func writeGonePairingCode(w http.ResponseWriter) {
	writeError(w, http.StatusGone, "pairing_code_invalid",
		"this pairing code has expired or was already used; make a new pairing QR in the bridge console")
}
