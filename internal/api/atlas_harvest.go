package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// AtlasHarvestCredentialSink stores the iOS-provisioned bulk_harvest credential
// (the App-Attest-minted Atlas token + the Atlas base URL). atlasharvest.StateStore
// satisfies it; the api package stays decoupled from the harvest client.
type AtlasHarvestCredentialSink interface {
	SetCredential(token, baseURL string, expiresAt time.Time) error
}

// WithAtlasHarvest wires the credential sink, enabling POST
// /v1/atlas-harvest/credential. Gated on cfg.Atlas.HarvestEnabled by the caller.
//
// `pinnedBaseURL` is cfg.Atlas.CanonicalHarvestBaseURL() — the ONLY Atlas host
// the endpoint will accept a credential for. Empty means unpinned, which stays
// allowed on a non-demo bridge (its bearers are the operator's own paired
// devices) but is REFUSED in demo mode; see refuseUnpinnedHarvestBaseURL.
func (s *Server) WithAtlasHarvest(sink AtlasHarvestCredentialSink, pinnedBaseURL string) *Server {
	s.atlasHarvestCred = sink
	// Canonicalize here rather than trusting the caller. The pin is compared
	// for EQUALITY against the wire value's canonical form, so a raw config
	// string that merely LOOKS equivalent (`https://host:443`, a trailing
	// slash) would produce a pin nothing can ever match — refusing the
	// operator's own bootstrap, which reads as a broken feature rather than a
	// broken comparison. Idempotent: cmd/bridge already passes
	// cfg.Atlas.CanonicalHarvestBaseURL().
	s.atlasHarvestPinnedBase = config.CanonicalHTTPSBase(pinnedBaseURL)
	return s
}

// refuseUnpinnedHarvestBaseURL enforces the harvest base-URL pin.
//
// The credential body carries `atlasBaseUrl`, and the harvest client dials it
// for BOTH submit and fetch — so whoever can set it chooses where the bridge
// pulls "harvested" bios from. Those land in `artist_atlas` and are served to
// every client by GET /v1/atlas-meta, carrying an attacker-chosen `SourceURL`
// the app renders as a "Read more on …" link. That is the same content
// injection refuseAtlasIngestInDemoMode blocks on /v1/atlas-ingest, reached
// through this sibling: that guard's comment reasoned about the TOKEN ("a
// bogus one just fails the harvest — no content injection") and not about the
// base URL travelling in the same request.
//
// Two rules, both load-bearing:
//
//   - Pinned: the request's canonical base must equal it. This is the whole
//     protection, and it deliberately applies in EVERY mode — a non-demo
//     operator who pinned a host meant it.
//   - Unpinned + demo: refuse. A demo bridge's bearer is public by
//     construction (the static `demo.tokenSHA256` ships inside every
//     installed app), so leaving it unpinned there is equivalent to leaving
//     it open. Unpinned on a non-demo bridge stays allowed — that is the
//     back-compatible case, and its bearers are the operator's own devices.
//
// Residual, accepted and NOT closed here: with a correct pin, a public demo
// bearer can still overwrite the TOKEN for the pinned host, which breaks the
// operator's harvest until re-provisioned (denial of function). That is a far
// smaller loss than injection, and closing it needs a "first write wins"
// credential lifecycle this endpoint does not currently have.
//
// Returns true when the request was refused.
func (s *Server) refuseUnpinnedHarvestBaseURL(w http.ResponseWriter, canonicalBase string) bool {
	if s.atlasHarvestPinnedBase != "" {
		if canonicalBase == s.atlasHarvestPinnedBase {
			return false
		}
		writeError(w, http.StatusForbidden, "harvest_base_url_not_allowed",
			"this bridge only accepts harvest credentials for its configured Atlas host")
		return true
	}
	if s.demoMode {
		writeError(w, http.StatusForbidden, "demo_read_only",
			"this demo bridge does not accept harvest credentials without a configured Atlas host")
		return true
	}
	return false
}

const atlasHarvestCredMaxBody = 8 << 10 // tokens + a URL are tiny

// atlasHarvestMaxExpiresInSeconds bounds the client-supplied credential
// lifetime to ~10 years. The value is multiplied by time.Second below,
// and time.Duration is an int64 of nanoseconds — an unbounded client
// int overflows that past ~292 years and would persist a nonsensical
// (possibly negative-wrapped) ExpiresAt. Values above the ceiling AND
// negative values (logically invalid, previously falling through to
// "no expiry") are rejected with 400 (2026-07-21 review, Low).
const atlasHarvestMaxExpiresInSeconds = 10 * 365 * 24 * 3600

type atlasHarvestCredentialRequest struct {
	Token            string `json:"token"`
	AtlasBaseURL     string `json:"atlasBaseUrl"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

// atlasHarvestCredential handles POST /v1/atlas-harvest/credential. The iOS app
// (which alone holds an App-Attest identity) mints a device-bound bulk_harvest
// token at Atlas and hands it here over the authenticated bridge↔app channel;
// the bridge persists it locally (0600, never in the manifest DB) and its
// harvest client uses it. The open-source bridge thus never carries a long-lived
// Atlas secret of its own — the credential is the user's attested device's,
// revocable at Atlas.
func (s *Server) atlasHarvestCredential(w http.ResponseWriter, r *http.Request) {
	if s.atlasHarvestCred == nil {
		writeError(w, http.StatusNotFound, "harvest_not_supported", "this bridge does not accept harvest credentials")
		return
	}
	var req atlasHarvestCredentialRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, atlasHarvestCredMaxBody))
	if err := dec.Decode(&req); err != nil {
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request", "body must be {token, atlasBaseUrl, expiresInSeconds}", err)
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	req.AtlasBaseURL = strings.TrimSpace(req.AtlasBaseURL)
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "token is required")
		return
	}
	// Require a plain https base URL (https://host[:port]): the bridge dials it
	// with the bearer token, and the harvest client appends `/v1/atlas/...`
	// paths. Rejecting http avoids cleartext token transport; rejecting
	// userinfo/query/fragment/path avoids persisting a credential that would
	// always dial the wrong endpoint. The canonical scheme://host form is stored
	// so equivalent inputs (trailing slash) don't churn the sync state.
	// Same reduction the configured pin goes through — config.CanonicalHTTPSBase
	// is shared deliberately: these two values are compared for EQUALITY, so a
	// reduction applied to one and not the other turns a correct pin into a
	// mismatch that fails closed and reads as a broken feature.
	canonicalBase := config.CanonicalHTTPSBase(req.AtlasBaseURL)
	if canonicalBase == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "atlasBaseUrl must be a plain https base URL (https://host[:port])")
		return
	}
	// Pin check BEFORE any persistence: a refused request must not have
	// reset the sync cursor or clobbered the operator's token on its way out
	// (SetCredential does both when the base URL differs).
	if s.refuseUnpinnedHarvestBaseURL(w, canonicalBase) {
		return
	}
	if req.ExpiresInSeconds < 0 || req.ExpiresInSeconds > atlasHarvestMaxExpiresInSeconds {
		writeError(w, http.StatusBadRequest, "bad_request", "expiresInSeconds must be non-negative and at most the maximum (10 years)")
		return
	}
	var expiresAt time.Time
	if req.ExpiresInSeconds > 0 {
		expiresAt = time.Now().Add(time.Duration(req.ExpiresInSeconds) * time.Second)
	}
	if err := s.atlasHarvestCred.SetCredential(req.Token, canonicalBase, expiresAt); err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "persist_failed", "failed to store harvest credential", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
