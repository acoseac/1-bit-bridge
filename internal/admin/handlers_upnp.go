package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// UPnPUpstreamProvider is the operator-facing interface for the
// upstream-MediaServer feature. Production wiring is the bridge's
// upnpUpstreamLifecycle; tests pass a stub. Nil-safe — when the
// provider isn't wired (operator hasn't enabled the feature) the
// admin handlers all surface 404 with a stable error code so the
// frontend can hide the surface cleanly.
//
// **Read vs write semantics**: `DiscoveredServers` is a cheap in-memory
// read. `ConfiguredServers` is NOT free — it issues one routed-track
// COUNT(*) per configured upstream — so it takes a ctx and every caller
// must pass one that cancels (see the method's own doc).
// `ForceRescan` debounces concurrent calls via inFlight. The CRUD
// trio (`AddServer` / `RemoveServer` / `UpdateServer`) writes
// bridge.yaml via the same `Config.Save` path the rest of the admin
// PATCH surface uses; callers must surface `restartRequired: true`
// in the response so the operator knows to restart for the new
// server set to take effect (v1 matches the established
// Sox/DLNA/Tailscale-mode precedent).
type UPnPUpstreamProvider interface {
	// ConfiguredServers returns the operator-configured servers
	// merged with their live discovery state. Order is the YAML
	// config order so the UI is deterministic.
	//
	// This hits the DB: one `SELECT COUNT(*) FROM upnp_track_routing`
	// per configured upstream. It is reached from the SSE `sources`
	// publisher on the 30 s tick, once per open connection, so the ctx
	// must be the caller's real one — an uncancellable
	// context.Background() (what this used before 2026-08-06) keeps a
	// disconnected client's queries running to completion and serialises
	// every other slow-tick publisher behind them.
	ConfiguredServers(ctx context.Context) []UPnPUpstreamServerState

	// DiscoveredServers returns SSDP-cached MediaServers that are
	// NOT in the operator's configured list — i.e. candidates the
	// user can one-click "Configure…" into the manifest ingest
	// path. Each row carries enough info to prefill the add form
	// (UDN, friendlyName, manufacturer, modelName, controlURL,
	// lastSeenAt). Order is friendlyName ASC for deterministic
	// rendering; ties broken by UDN.
	DiscoveredServers() []UPnPDiscoveredServer

	// ForceRescan triggers an immediate Ingester.Run for the matching
	// UDN (or "" for all configured servers). Returns nil on success
	// + a typed error otherwise; the handler maps to JSON.
	ForceRescan(ctx context.Context, udn string) error

	// AddServer appends a new server entry to upnpUpstream.servers and
	// persists bridge.yaml via Config.Save. Returns ErrUPnPDuplicateUDN
	// when the UDN OR ManualDescriptionURL collides with an existing
	// configured row, ErrUPnPValidation on shape errors (missing name,
	// no UDN/manualURL, etc.), or the underlying save error on
	// persistence failure. The added entry's live discovery state
	// surfaces via the next ConfiguredServers read; the actual ingest
	// walk requires a bridge restart (caller writes
	// `restartRequired: true` in the response).
	AddServer(ctx context.Context, req UPnPServerAddRequest) error

	// RemoveServer drops the server entry whose UDN matches and
	// persists bridge.yaml. Returns ErrUPnPNoSuchServer when no row
	// matches. The routed tracks the removed server contributed to
	// the manifest remain until the next restart's reconcile sweep —
	// caller writes `restartRequired: true`.
	RemoveServer(ctx context.Context, udn string) error

	// UpdateServer edits the operator-visible fields of an existing
	// row (Name / PathPrefix / RootObjectID / SkipTopLevelContainers).
	// UDN is identity and NOT editable. Returns ErrUPnPNoSuchServer
	// when no row matches; ErrUPnPValidation on shape errors.
	UpdateServer(ctx context.Context, udn string, req UPnPServerUpdateRequest) error
}

// UPnPDiscoveredServer is the wire shape of one row in the
// "Discovered on LAN" surface. Carries enough info to pre-fill the
// add form when the operator clicks "Configure…" — friendlyName +
// manufacturer + modelName for human identification; UDN for the
// stored config row; controlURL is informational (the bridge derives
// it at runtime from SSDP, not the YAML row).
type UPnPDiscoveredServer struct {
	UDN              string    `json:"udn"`
	FriendlyName     string    `json:"friendlyName,omitempty"`
	Manufacturer     string    `json:"manufacturer,omitempty"`
	ModelName        string    `json:"modelName,omitempty"`
	ModelDescription string    `json:"modelDescription,omitempty"`
	ControlURL       string    `json:"contentDirectoryControlURL,omitempty"`
	LastSeenAt       time.Time `json:"lastSeenAt"`
}

// UPnPServerAddRequest is the wire shape of POST /api/upnp/servers.
// Either UDN OR ManualDescriptionURL is required; supplying both is
// allowed (SSDP-paired manual fallback) and the handler stores both.
// PathPrefix defaults to a sanitized form of Name when omitted (same
// rule the YAML loader applies). RootObjectID defaults to "64" (the
// MiniDLNA Browse Folders convention).
type UPnPServerAddRequest struct {
	Name                   string   `json:"name"`
	UDN                    string   `json:"udn,omitempty"`
	ManualDescriptionURL   string   `json:"manualDescriptionURL,omitempty"`
	PathPrefix             string   `json:"pathPrefix,omitempty"`
	RootObjectID           string   `json:"rootObjectID,omitempty"`
	SkipTopLevelContainers []string `json:"skipTopLevelContainers,omitempty"`
}

// UPnPServerUpdateRequest is the wire shape of PATCH /api/upnp/servers/{udn}.
// All fields are pointers so omitted fields preserve the current
// value (mirrors the `apiSettingsPatch` pointer-discriminator
// convention). Empty-string is a valid "clear this field" value for
// PathPrefix / RootObjectID — the YAML loader fills the defaults.
type UPnPServerUpdateRequest struct {
	Name                   *string   `json:"name,omitempty"`
	PathPrefix             *string   `json:"pathPrefix,omitempty"`
	RootObjectID           *string   `json:"rootObjectID,omitempty"`
	SkipTopLevelContainers *[]string `json:"skipTopLevelContainers,omitempty"`
}

// UPnPUpstreamServerState is one row in the per-server status surface.
// Wire fields mirror the operator's mental model: "did the bridge see
// my 2Go? did the last walk succeed? how many tracks did it ingest?"
//
// PathPrefix / RootObjectID / SkipTopLevelContainers are the EDITABLE
// YAML fields, echoed here so the admin edit modal can prefill them.
// They are deliberately named to match `UPnPServerUpdateRequest` — the
// modal reads a row, shows those values, and PATCHes them back. Before
// they were exposed the modal rendered them blank and its submit
// handler PATCHed the blanks, so a plain rename silently cleared all
// three: RootObjectID fell back to "64" (the wrong subtree for any
// non-MiniDLNA upstream) and PathPrefix fell back to the raw Name,
// moving every routed track's manifest path into a new namespace — the
// reconcile sweep reaps the old rows and the re-inserted ones come back
// with enriched_at = 0, i.e. a full MB/CAA/Deezer re-crawl of the whole
// upstream plus a full re-sync to every paired device. Keep them on the
// DTO: a modal that cannot see a field must not be allowed to write it.
type UPnPUpstreamServerState struct {
	Name                   string   `json:"name"`
	ConfiguredUDN          string   `json:"configuredUDN,omitempty"`
	ManualURL              string   `json:"manualDescriptionURL,omitempty"`
	PathPrefix             string   `json:"pathPrefix,omitempty"`
	RootObjectID           string   `json:"rootObjectID,omitempty"`
	SkipTopLevelContainers []string `json:"skipTopLevelContainers,omitempty"`

	// StableKey is upnpingest.StableServerKey for this row — the value
	// upnp_track_routing.server_udn holds, which is NOT the device UDN
	// (see admin.UPnPSource). Carried so the sources snapshot can derive
	// the same source id the sidebar links to, without a second lookup
	// that could disagree.
	StableKey        string    `json:"stableKey,omitempty"`
	Discovered       bool      `json:"discovered"`
	ResolvedUDN      string    `json:"resolvedUDN,omitempty"`
	FriendlyName     string    `json:"friendlyName,omitempty"`
	Manufacturer     string    `json:"manufacturer,omitempty"`
	ControlURL       string    `json:"contentDirectoryControlURL,omitempty"`
	LastSeenAt       time.Time `json:"lastSeenAt,omitempty"`
	LastWalkStarted  time.Time `json:"lastWalkStartedAt,omitempty"`
	LastWalkFinished time.Time `json:"lastWalkFinishedAt,omitempty"`
	LastWalked       int       `json:"lastWalkedCount,omitempty"`
	LastReaped       int       `json:"lastReapedCount,omitempty"`
	LastWalkErr      string    `json:"lastWalkErr,omitempty"`
	RoutedTracks     int       `json:"routedTracks"`
}

// upnpServersResponse is the wire shape of GET /api/upnp/servers.
type upnpServersResponse struct {
	Enabled bool                      `json:"enabled"`
	Servers []UPnPUpstreamServerState `json:"servers"`
}

// apiUPnPServers serves GET /api/upnp/servers.
//
// When the feature isn't wired (deps.UPnPUpstream == nil), we still
// return 200 with enabled=false so the frontend can render an
// informational empty state — a 404 here would force every visit to
// the Devices page to swallow a console error.
func (s *Server) apiUPnPServers(w http.ResponseWriter, r *http.Request) {
	resp := upnpServersResponse{Servers: []UPnPUpstreamServerState{}}
	if s.deps.UPnPUpstream != nil {
		resp.Enabled = true
		// Keep the pre-initialized [] when the provider returns nil — a
		// nil slice marshals to `"servers": null`, which breaks the
		// frontend's array iteration. Empty (non-nil) marshals to `[]`.
		if servers := s.deps.UPnPUpstream.ConfiguredServers(r.Context()); servers != nil {
			resp.Servers = servers
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// apiUPnPRescan serves POST /api/upnp/rescan?udn=<UDN>. An empty udn
// triggers a force-rescan across every configured server. Returns
// 202 Accepted on success; 404 when the feature isn't wired OR when no
// server matches the requested UDN; 409 when a rescan is already in
// flight; 500 on any other failure.
func (s *Server) apiUPnPRescan(w http.ResponseWriter, r *http.Request) {
	if s.deps.UPnPUpstream == nil {
		writeError(w, http.StatusNotFound, "upnp_disabled",
			upnpDisabledMsg)
		return
	}
	udn := strings.TrimSpace(r.URL.Query().Get("udn"))
	if err := s.deps.UPnPUpstream.ForceRescan(r.Context(), udn); err != nil {
		switch {
		case errors.Is(err, ErrUPnPNoSuchServer):
			writeError(w, http.StatusNotFound, "no_such_server",
				upnpNoSuchServerMsg)
		case errors.Is(err, ErrUPnPRescanInFlight):
			writeError(w, http.StatusConflict, "rescan_in_flight",
				"an ingest run is already in flight; try again shortly")
		default:
			writeError(w, http.StatusInternalServerError, "internal",
				"the bridge couldn't trigger a rescan: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// Wire-error message constants. Extracted because each one appears in
// 3-4 handler 404/400 paths and SonarCloud S1192 flags the
// duplication. Kept lowercase to match the existing handler-side
// envelope-message convention.
const (
	upnpDisabledMsg     = "the upstream UPnP feature isn't enabled on this bridge"
	upnpNoSuchServerMsg = "no configured upstream server matches the requested UDN"
)

// ErrUPnPNoSuchServer is returned by ForceRescan / RemoveServer /
// UpdateServer when the caller passed a UDN that doesn't match any
// configured server.
var ErrUPnPNoSuchServer = errors.New("admin: no such upnp upstream server")

// ErrUPnPRescanInFlight is returned by ForceRescan when an ingest run
// is already executing — we don't overlap runs (the cmd/bridge ticker
// uses a single goroutine).
var ErrUPnPRescanInFlight = errors.New("admin: upnp rescan already in flight")

// ErrUPnPDuplicateUDN is returned by AddServer when the requested UDN
// (or ManualDescriptionURL) collides with an existing configured row.
// Two configured rows sharing identity would race on the same routing
// path namespace + manifest entries on every walk.
var ErrUPnPDuplicateUDN = errors.New("admin: upnp upstream UDN or manualDescriptionURL already configured")

// ErrUPnPValidation is returned by AddServer / UpdateServer when the
// payload fails shape validation (empty name, no UDN AND no manualURL,
// path-prefix shape, etc.). The handler maps to 400 Bad Request.
var ErrUPnPValidation = errors.New("admin: upnp upstream validation failed")

// upnpDiscoveredResponse is the wire shape of GET /api/upnp/discovered.
type upnpDiscoveredResponse struct {
	Enabled bool                   `json:"enabled"`
	Servers []UPnPDiscoveredServer `json:"servers"`
}

// apiUPnPDiscovered serves GET /api/upnp/discovered. Returns SSDP-
// cached MediaServers that are NOT in the operator's configured list
// (i.e. candidates the user can one-click "Configure…"). Symmetrical
// with apiUPnPServers — when the feature isn't wired we surface 200
// + enabled=false + an empty list so the frontend can render a tidy
// informational state.
func (s *Server) apiUPnPDiscovered(w http.ResponseWriter, _ *http.Request) {
	resp := upnpDiscoveredResponse{Servers: []UPnPDiscoveredServer{}}
	if s.deps.UPnPUpstream != nil {
		resp.Enabled = true
		// Keep the pre-initialized [] when the provider returns nil (see
		// apiUPnPServers) so the wire carries `[]`, not `null`.
		if servers := s.deps.UPnPUpstream.DiscoveredServers(); servers != nil {
			resp.Servers = servers
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// upnpServerCRUDResponse is the post-CRUD wire shape. `restartRequired`
// is always true (matches the established Sox/DLNA/Tailscale-mode
// precedent — the upnpUpstreamLifecycle constructs its Ingester at
// startup from the configured server list, so a runtime change needs
// a restart to take effect). The frontend surfaces the badge.
type upnpServerCRUDResponse struct {
	OK              bool   `json:"ok"`
	UDN             string `json:"udn,omitempty"`
	RestartRequired bool   `json:"restartRequired"`
}

// apiUPnPServerAdd serves POST /api/upnp/servers. Body is
// UPnPServerAddRequest. Returns 201 + restartRequired:true on success,
// 400 on validation, 409 on duplicate UDN, 500 on save failure, 404
// when the feature isn't wired.
func (s *Server) apiUPnPServerAdd(w http.ResponseWriter, r *http.Request) {
	if s.deps.UPnPUpstream == nil {
		writeError(w, http.StatusNotFound, "upnp_disabled",
			upnpDisabledMsg)
		return
	}
	var req UPnPServerAddRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return // decodeJSONBody already wrote the error envelope
	}
	if err := s.deps.UPnPUpstream.AddServer(r.Context(), req); err != nil {
		switch {
		case errors.Is(err, ErrUPnPValidation):
			writeError(w, http.StatusBadRequest, "validate", err.Error())
		case errors.Is(err, ErrUPnPDuplicateUDN):
			writeError(w, http.StatusConflict, "duplicate", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, errCodeSaveConfig, err.Error())
		}
		return
	}
	udn := strings.TrimSpace(req.UDN)
	writeJSON(w, http.StatusCreated, upnpServerCRUDResponse{
		OK: true, UDN: udn, RestartRequired: true,
	})
}

// apiUPnPServerRemove serves DELETE /api/upnp/servers?udn=<UDN>.
// Returns 200 + restartRequired:true on success, 404 when no row
// matches OR the feature isn't wired, 500 on save failure.
//
// Identity rides on the query string, not a path segment — `udn` MAY
// be a ManualDescriptionURL containing `/` for SSDP-unreachable
// servers, which would never match a single-segment path wildcard
// after Go's net/http multiplexer's `%2F`→`/` unescaping + path-
// cleaning. Per Gemini HIGH on PR #357 round-2.
func (s *Server) apiUPnPServerRemove(w http.ResponseWriter, r *http.Request) {
	if s.deps.UPnPUpstream == nil {
		writeError(w, http.StatusNotFound, "upnp_disabled",
			upnpDisabledMsg)
		return
	}
	udn := strings.TrimSpace(r.URL.Query().Get("udn"))
	if udn == "" {
		writeError(w, http.StatusBadRequest, "validate",
			"missing `udn` query parameter")
		return
	}
	if err := s.deps.UPnPUpstream.RemoveServer(r.Context(), udn); err != nil {
		switch {
		case errors.Is(err, ErrUPnPNoSuchServer):
			writeError(w, http.StatusNotFound, "no_such_server",
				upnpNoSuchServerMsg)
		default:
			writeError(w, http.StatusInternalServerError, errCodeSaveConfig, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, upnpServerCRUDResponse{
		OK: true, UDN: udn, RestartRequired: true,
	})
}

// apiUPnPServerUpdate serves PATCH /api/upnp/servers?udn=<UDN>. Body
// is UPnPServerUpdateRequest (pointer-discriminator partial update;
// UDN is identity, NOT editable). Returns 200 + restartRequired:true
// on success, 400 on validation, 404 when no row matches OR feature
// isn't wired, 500 on save failure. Identity is on the query string —
// see `apiUPnPServerRemove` for the rationale (ManualDescriptionURL
// fallback breaks single-segment path wildcards).
func (s *Server) apiUPnPServerUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.UPnPUpstream == nil {
		writeError(w, http.StatusNotFound, "upnp_disabled",
			upnpDisabledMsg)
		return
	}
	udn := strings.TrimSpace(r.URL.Query().Get("udn"))
	if udn == "" {
		writeError(w, http.StatusBadRequest, "validate",
			"missing `udn` query parameter")
		return
	}
	var req UPnPServerUpdateRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	if err := s.deps.UPnPUpstream.UpdateServer(r.Context(), udn, req); err != nil {
		switch {
		case errors.Is(err, ErrUPnPValidation):
			writeError(w, http.StatusBadRequest, "validate", err.Error())
		case errors.Is(err, ErrUPnPNoSuchServer):
			writeError(w, http.StatusNotFound, "no_such_server",
				upnpNoSuchServerMsg)
		default:
			writeError(w, http.StatusInternalServerError, errCodeSaveConfig, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, upnpServerCRUDResponse{
		OK: true, UDN: udn, RestartRequired: true,
	})
}

// decodeJSONBody is a thin wrapper that respects the global
// adminMaxBodyBytes ceiling and surfaces the canonical bad-json
// error envelope. Mirrors the inline decode/MaxBytesReader pattern
// used in apiRootsAdd / apiRootsRemove / etc; extracted here so the
// three CRUD handlers above don't duplicate the boilerplate.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes))
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, err.Error())
		return err
	}
	return nil
}
