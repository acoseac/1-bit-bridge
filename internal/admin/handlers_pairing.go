package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/pairing"
)

// pendingPairingRow is the JSON shape /api/pairing returns for each
// in-flight (and grace-window) request. Mirrors the pairing.Request
// snapshot but stripped to the fields the admin UI actually renders —
// the raw bearer token / pollHash never leave the server, and the
// Store snapshot doesn't include the live timer pointer either.
//
// Status is the lowercase wire form ("pending" / "approved" / etc.) so
// the JS side can class-tag cards without duplicating the State enum.
//
// SecondsUntilExpiry is bridge-relative (decrementing toward zero) —
// avoids the clock-skew footgun of shipping an absolute timestamp that
// the operator's browser would interpret in local time.
type pendingPairingRow struct {
	ID                 string    `json:"id"`
	DeviceName         string    `json:"deviceName"`
	ClientVersion      string    `json:"clientVersion,omitempty"`
	VerificationCode   string    `json:"verificationCode"`
	Status             string    `json:"status"`
	SourceIP           string    `json:"sourceIP,omitempty"`
	FingerprintSuffix  string    `json:"fingerprintSuffix,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	SecondsUntilExpiry int       `json:"secondsUntilExpiry"`
}

// apiPairingList returns the current set of pending + recently-decided
// pairing requests for the admin Devices page. The page polls this on
// the same 3 s tick as the other Devices panels.
//
// When no pairing.Store is wired (e.g. test deployments) the response
// is an empty list — the JS-side renderer treats that as "no pending
// requests", same as a wired-but-empty Store.
func (s *Server) apiPairingList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.getPairingSnapshot())
}

// getPairingSnapshot builds the pending-pairing payload for both
// the REST handler and the SSE handler. Always returns a non-nil
// slice (empty when no Store is wired or no requests are in flight)
// so JSON marshals as `[]`, not `null` — the frontend's
// `Array.isArray(entries) && entries.length === 0` teardown branch
// depends on the array shape.
//
// SecondsUntilExpiry is computed from time.Now() and decrements every
// second while a request is pending. The SSE diff therefore won't
// suppress pairing frames during an in-flight request — by design,
// so the server streams the countdown to the browser without any
// client-side ticker.
func (s *Server) getPairingSnapshot() []pendingPairingRow {
	if s.deps.Pairing == nil {
		return []pendingPairingRow{}
	}
	now := time.Now()
	reqs := s.deps.Pairing.List()
	out := make([]pendingPairingRow, 0, len(reqs))
	for _, req := range reqs {
		row := pendingPairingRow{
			ID:                req.ID,
			DeviceName:        req.DeviceName,
			ClientVersion:     req.ClientVersion,
			VerificationCode:  req.VerificationCode,
			Status:            req.State.String(),
			SourceIP:          req.SourceIP,
			FingerprintSuffix: fingerprintSuffix(req.CertFingerprint),
			CreatedAt:         req.CreatedAt,
		}
		// Pending: countdown to TTL deadline. Terminal: 0 (the row
		// will vanish at TTL+grace via the Store sweeper).
		if req.State == pairing.StatePending {
			deadline := req.CreatedAt.Add(s.deps.Pairing.TTL())
			rem := int(deadline.Sub(now).Seconds())
			if rem < 0 {
				rem = 0
			}
			row.SecondsUntilExpiry = rem
		}
		out = append(out, row)
	}
	return out
}

// apiPairingApprove transitions a Pending request to Approved by
// minting a real bearer token via auth.Store.Mint. The minted token
// is held inside the pairing.Request until the iOS device sends
// DELETE /v1/pairing/{id} as receipt acknowledgment, OR until the
// undelivered-revoke deadline (TTL+grace from creation) at which the
// pairing.Store calls auth.Store.Revoke to prevent orphans.
//
// Cert-rotation guard: if the bridge cert fingerprint has changed
// between request creation and this approve, the Store transitions to
// CertRotated instead of Approved (refusing to mint onto a new cert).
// The iOS client surfaces this as a terminal "please request again"
// state. Returns 409 with `cert_rotated` short-code in that case.
func (s *Server) apiPairingApprove(w http.ResponseWriter, r *http.Request) {
	if s.deps.Pairing == nil {
		writeError(w, http.StatusServiceUnavailable, "pairing_not_wired",
			"this bridge does not have the pairing store wired")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id_required", "pairing request ID is required")
		return
	}
	mint := func(name string) (rawToken, tokenID string, err error) {
		raw, tok, err := s.deps.Auth.Mint(name)
		if err != nil {
			return "", "", err
		}
		return raw, tok.ID, nil
	}

	// `pairing.Store` and `auth.Store` each have their own mutex; the
	// admin `Server.mu` is for serializing config-rewrite paths
	// (`apiRootsAdd` / `apiSettingsPatch` / etc.) and intentionally NOT
	// taken on the pairing path. Holding it through `Mint`'s disk
	// persist would block unrelated admin operations under spam
	// (gemini on PR #104). The bridge's self-signed cert fingerprint
	// captured at admin-server construction is stable for the
	// cert-rotation guard's purpose: rotating the self-signed cert
	// requires a process restart per the CLAUDE.md invariant
	// "rotated server cert requires re-pairing". Tailscale's LE cert
	// is swapped at runtime but is a SECONDARY cert served only on
	// magic-DNS SNI; the pin contract is anchored to the self-signed
	// cert.
	snap, err := s.deps.Pairing.Approve(id, s.deps.Fingerprint, mint)
	switch {
	case errors.Is(err, pairing.ErrNotFound):
		writeError(w, http.StatusNotFound, "unknown_request", id)
		return
	case errors.Is(err, pairing.ErrAlreadyDecided):
		writeError(w, http.StatusConflict, "already_decided",
			"this pairing request was already approved, declined, or expired")
		return
	case errors.Is(err, pairing.ErrCertRotated):
		writeError(w, http.StatusConflict, "cert_rotated",
			"the bridge TLS cert changed since this request was created; the device must re-pair")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "approve_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      snap.ID,
		"tokenId": snap.TokenID,
	})
}

// apiPairingDecline transitions a Pending request to Declined. Same
// 409-on-already-decided contract as Approve.
func (s *Server) apiPairingDecline(w http.ResponseWriter, r *http.Request) {
	if s.deps.Pairing == nil {
		writeError(w, http.StatusServiceUnavailable, "pairing_not_wired",
			"this bridge does not have the pairing store wired")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id_required", "pairing request ID is required")
		return
	}
	// Server.mu is not taken here — see the rationale on apiPairingApprove.
	snap, err := s.deps.Pairing.Decline(id)
	switch {
	case errors.Is(err, pairing.ErrNotFound):
		writeError(w, http.StatusNotFound, "unknown_request", id)
		return
	case errors.Is(err, pairing.ErrAlreadyDecided):
		writeError(w, http.StatusConflict, "already_decided",
			"this pairing request was already approved, declined, or expired")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "decline_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": snap.ID})
}

// fingerprintSuffix returns the last 4 colon-separated bytes of a
// cert fingerprint (or the whole string if it's shorter). Used by the
// admin UI as a glance-only confirmation that the request belongs to
// the bridge the admin is logged into — the verification code is the
// load-bearing identity check.
func fingerprintSuffix(fp string) string {
	const tail = 4
	cnt := 0
	for i := len(fp) - 1; i >= 0; i-- {
		if fp[i] == ':' {
			cnt++
			if cnt == tail {
				return fp[i+1:]
			}
		}
	}
	return fp
}
