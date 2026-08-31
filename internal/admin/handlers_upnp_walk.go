package admin

import "net/http"

// Live progress for an upstream UPnP walk.
//
// A walk of a 15,000-track upstream took minutes with nothing on screen:
// no counter, no in-flight marker, and the /upnp page does not re-fetch
// after load, so even the after-the-fact "Last walk" line only appeared
// on a manual reload. The filesystem scanner has published progress since
// it was written; this is its twin for the ingest.

// UPnPWalkStatus is the admin-side view of the walk in flight.
//
// Structurally mirrors upnpingest.WalkStatus; the wiring adapter in
// cmd/bridge converts. Declared here so this package does not import
// internal/upnpingest — the same decoupling UPnPSource uses.
type UPnPWalkStatus struct {
	// Key is the ingest's StableServerKey, NOT the device UDN. It is what
	// upnp_track_routing.server_udn holds, and the only thing that can be
	// matched against a configured row without a second lookup.
	Key     string
	Walking bool
	Items   int64
}

// upnpWalkResponse is the `upnpwalk` SSE event and GET /api/upnp/walk.
//
// SourceID rather than the raw key, so the client can match a row it
// already renders without learning a second identity — the same id the
// sidebar's UPnP rows and the sources event carry.
type upnpWalkResponse struct {
	Walking  bool   `json:"walking"`
	SourceID string `json:"sourceId,omitempty"`
	Items    int64  `json:"items,omitempty"`
}

// getUPnPWalkSnapshot reads the live walk state. Cheap by construction:
// atomic reads only, so it can ride the 500ms tick.
func (s *Server) getUPnPWalkSnapshot() upnpWalkResponse {
	if s.deps.UPnPWalkProgress == nil {
		return upnpWalkResponse{}
	}
	st := s.deps.UPnPWalkProgress()
	if !st.Walking || st.Key == "" {
		return upnpWalkResponse{}
	}
	return upnpWalkResponse{
		Walking:  true,
		SourceID: sourceIDForRow(st.Key),
		Items:    st.Items,
	}
}

// apiUPnPWalk serves GET /api/upnp/walk — the REST twin of the SSE
// event, for curl and tests.
func (s *Server) apiUPnPWalk(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.getUPnPWalkSnapshot())
}
