// Package discovery is the bridge-side SSDP M-SEARCH client + renderer
// cache that powers `GET /v1/renderers`. Companion to the
// `internal/dlna/` MediaServer package — this package CONSUMES SSDP
// announcements from MediaRenderers on the LAN; the sibling package
// EMITS SSDP NOTIFY for the bridge's own MediaServer.
//
// **Why a separate package**: keeps the orientation clear — server-
// side advertising and client-side discovery share only the byte-
// shape primitives (`ssdp_packet.go`) and the LAN-eligibility helper
// (`interfaces.go`). Both are re-exported from `internal/dlna/` so
// this package imports them without introducing a circular dep.
//
// **Lifecycle (v1, M-SEARCH-only)**: `SSDPDiscoveryClient.Start(ctx)`
// binds a single UDP socket to wildcard `0.0.0.0:0` (ephemeral
// port) for sending M-SEARCH packets + receiving unicast responses,
// fires an immediate M-SEARCH, then loops every `MSearchInterval`
// (default 30s). On first discovery of a new UDN: fetches
// `/description.xml` (unicast HTTP), parses the device + service
// list, fetches `GetProtocolInfo` from the renderer's
// ConnectionManager, caches the populated `RendererInfo` DTO.
// Cache evicts via staleness (no observation within `RendererTTL`,
// default 60s).
//
// **v1 does NOT listen for spontaneous NOTIFY broadcasts** because
// the well-known multicast port (1900) is already bound by the
// bridge's own server-side advertiser in the same process.
// Renderers that come or go between cycles surface within the
// 30s M-SEARCH interval; explicit byebye fast-eviction is
// deferred to a future PR (would need cross-package wiring to
// the server-side advertiser to multiplex packet observation).
//
// **Bind to LAN-eligible interfaces only** — reuses
// `internal/dlna.IsLANEligibleInterface`. SSDP multicast on a
// public-mode VPS subnet is undefined behaviour; the public-mode
// gate refuses discovery entirely (caller wiring in
// `cmd/bridge/dlna_wiring.go` enforces).
//
// **Wire shape** (`/v1/renderers`): bearer-authed JSON response
// matching the `RenderersResponse` struct in `renderer_dto.go`.
// iOS consumes via `BridgeRendererDiscovery` (PR 6) merging into
// the existing SSDP + manual sources in `OutputPickerSheet`.
//
// **Additive — no protocol version bump.** Feature gate is the
// new `"rendererDiscovery"` flag in `/v1/health.features` (alpha-
// sorted between `pushEventsSupported` and `upscaleCompleteEvents`).
// Old iOS clients without `BridgeRendererDiscovery` simply ignore
// the flag + the endpoint.
package discovery
