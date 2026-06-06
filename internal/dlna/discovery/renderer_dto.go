package discovery

import "time"

// RendererInfo is the wire shape returned by `GET /v1/renderers`.
// Mirrors the iOS-side `DLNARenderer` struct fields consumed by
// `BridgeRendererDiscovery` (PR 6).
//
// **JSON encoding stability**: field order + JSON tags are part of
// the public API surface. New fields are additive (omitempty
// applied to optional ones); existing field shapes can NOT change
// without a `ProtocolVersion` bump. PR 5+6 adds the endpoint
// itself + this struct; subsequent iterations are gated on the
// Mirror-PR convention.
//
// **`LastSeenAt`** is included so iOS can apply its own staleness
// gating layer if needed (a bridge that responds fast but lost
// SSDP contact with a renderer ~50s ago would still surface that
// renderer in the response until the bridge's own TTL eviction
// fires). Default bridge TTL is 60s; iOS can compare against its
// own threshold.
type RendererInfo struct {
	// UDN — UPnP unique device name (`uuid:...`). The stable key
	// across restarts + the dedup key when iOS merges this list
	// with its own SSDP-discovered renderers (`RendererSourceMerger`
	// already keys on UDN).
	UDN string `json:"udn"`

	// FriendlyName — operator-visible name. Drives the row label
	// in `OutputPickerSheet`.
	FriendlyName string `json:"friendlyName"`

	// Manufacturer / ModelDescription / ModelName — surfaced as
	// secondary metadata for power-user disambiguation when
	// multiple renderers from the same vendor are on the LAN.
	Manufacturer     string `json:"manufacturer,omitempty"`
	ModelDescription string `json:"modelDescription,omitempty"`
	ModelName        string `json:"modelName,omitempty"`

	// ControlURL — absolute URL of the AVTransport service's SOAP
	// control endpoint. iOS dispatches SetAVTransportURI / Play /
	// Pause / Seek against this URL.
	ControlURL string `json:"controlURL"`

	// EventURL — absolute URL of the AVTransport service's GENA
	// subscription endpoint. iOS SUBSCRIBE-s here for push state
	// updates (PR 4's predictive GENA failover would consume this).
	EventURL string `json:"eventURL,omitempty"`

	// RenderingControlURL — absolute URL of the RenderingControl
	// service's SOAP control endpoint, when the renderer advertises
	// it (already parsed by ParseDeviceDescription into
	// Services[ServiceRenderingControl]). iOS dispatches SetMute /
	// SetVolume here. Empty when the renderer has no RenderingControl
	// service (iOS skips the renderer-volume slider AND the DSD-pause
	// SetMute ring-suppression for that renderer). Additive field —
	// pre-existing clients ignore it.
	RenderingControlURL string `json:"renderingControlURL,omitempty"`

	// SinkProtocolInfos — the renderer's advertised
	// `GetProtocolInfo` Sink list, raw strings as the renderer
	// returned them. iOS's `RendererCapability` consumes these
	// to gate MIME / codec / rate decisions. Empty slice when the
	// renderer was offline during our GetProtocolInfo probe (the
	// next M-SEARCH cycle refetches).
	SinkProtocolInfos []string `json:"sinkProtocolInfos,omitempty"`

	// LastSeenAt — most recent SSDP observation (M-SEARCH response
	// or NOTIFY ssdp:alive). RFC3339 in JSON.
	LastSeenAt time.Time `json:"lastSeenAt"`
}

// RenderersResponse is the top-level shape of `GET /v1/renderers`.
// Wrapping the slice in an object (vs returning a bare array) keeps
// the door open for additive fields like pagination cursors / cache
// freshness metadata without a protocol bump.
type RenderersResponse struct {
	Renderers []RendererInfo `json:"renderers"`
}
