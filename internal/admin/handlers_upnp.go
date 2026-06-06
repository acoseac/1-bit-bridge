package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// UPnPUpstreamProvider is the operator-facing read interface for the
// upstream-MediaServer feature. Production wiring is the bridge's
// upnpUpstreamLifecycle; tests pass a stub. Nil-safe — when the
// provider isn't wired (operator hasn't enabled the feature) the
// admin handlers all surface 404 with a stable error code so the
// frontend can hide the surface cleanly.
type UPnPUpstreamProvider interface {
	// ConfiguredServers returns the operator-configured servers
	// merged with their live discovery state. Order is the YAML
	// config order so the UI is deterministic.
	ConfiguredServers() []UPnPUpstreamServerState

	// ForceRescan triggers an immediate Ingester.Run for the matching
	// UDN (or "" for all configured servers). Returns nil on success
	// + a typed error otherwise; the handler maps to JSON.
	ForceRescan(ctx context.Context, udn string) error
}

// UPnPUpstreamServerState is one row in the per-server status surface.
// Wire fields mirror the operator's mental model: "did the bridge see
// my 2Go? did the last walk succeed? how many tracks did it ingest?"
type UPnPUpstreamServerState struct {
	Name             string    `json:"name"`
	ConfiguredUDN    string    `json:"configuredUDN,omitempty"`
	ManualURL        string    `json:"manualDescriptionURL,omitempty"`
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
func (s *Server) apiUPnPServers(w http.ResponseWriter, _ *http.Request) {
	resp := upnpServersResponse{Servers: []UPnPUpstreamServerState{}}
	if s.deps.UPnPUpstream != nil {
		resp.Enabled = true
		resp.Servers = s.deps.UPnPUpstream.ConfiguredServers()
	}
	writeJSON(w, http.StatusOK, resp)
}

// apiUPnPRescan serves POST /api/upnp/rescan?udn=<UDN>. An empty udn
// triggers a force-rescan across every configured server. Returns
// 202 Accepted on success; 404 when the feature isn't wired; 503 when
// no server matches the requested UDN.
func (s *Server) apiUPnPRescan(w http.ResponseWriter, r *http.Request) {
	if s.deps.UPnPUpstream == nil {
		writeError(w, http.StatusNotFound, "upnp_disabled",
			"the upstream UPnP feature isn't enabled on this bridge")
		return
	}
	udn := strings.TrimSpace(r.URL.Query().Get("udn"))
	if err := s.deps.UPnPUpstream.ForceRescan(r.Context(), udn); err != nil {
		switch {
		case errors.Is(err, ErrUPnPNoSuchServer):
			writeError(w, http.StatusNotFound, "no_such_server",
				"no configured upstream server matches the requested UDN")
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

// ErrUPnPNoSuchServer is returned by ForceRescan when the caller passed
// a UDN that doesn't match any configured server.
var ErrUPnPNoSuchServer = errors.New("admin: no such upnp upstream server")

// ErrUPnPRescanInFlight is returned by ForceRescan when an ingest run
// is already executing — we don't overlap runs (the cmd/bridge ticker
// uses a single goroutine).
var ErrUPnPRescanInFlight = errors.New("admin: upnp rescan already in flight")
