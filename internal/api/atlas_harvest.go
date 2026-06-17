package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AtlasHarvestCredentialSink stores the iOS-provisioned bulk_harvest credential
// (the App-Attest-minted Atlas token + the Atlas base URL). atlasharvest.StateStore
// satisfies it; the api package stays decoupled from the harvest client.
type AtlasHarvestCredentialSink interface {
	SetCredential(token, baseURL string, expiresAt time.Time) error
}

// WithAtlasHarvest wires the credential sink, enabling POST
// /v1/atlas-harvest/credential. Gated on cfg.Atlas.HarvestEnabled by the caller.
func (s *Server) WithAtlasHarvest(sink AtlasHarvestCredentialSink) *Server {
	s.atlasHarvestCred = sink
	return s
}

const atlasHarvestCredMaxBody = 8 << 10 // tokens + a URL are tiny

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
	u, perr := url.Parse(req.AtlasBaseURL)
	if perr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		writeError(w, http.StatusBadRequest, "bad_request", "atlasBaseUrl must be a plain https base URL (https://host[:port])")
		return
	}
	canonicalBase := u.Scheme + "://" + u.Host
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
