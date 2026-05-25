// Package dlna implements a UPnP/DLNA MediaServer surface that lets
// spec-compliant network audio renderers (Chord 2go, Lumin, dCS Network
// Bridge, Auralic Aries, Bluesound, Eversolo, …) discover and stream the
// bridge's library bit-exact. The package is wired in addition to — never
// in place of — the existing bearer-authed `/v1/*` API used by the 1-bit
// iOS app; iOS remains the control point that drives the renderer via
// UPnP AVTransport while the data plane (bridge → renderer over HTTP) is
// served from this package.
//
// # Posture and gating
//
// The DLNA listener is bound ONLY to LAN-eligible interfaces (RFC1918,
// link-local, and the operator-opted-in Tailscale tsnet interface). Public
// deployment mode REFUSES to bind DLNA regardless of the operator-supplied
// `cfg.DLNA.Enabled` value — exposing an unauthenticated ContentDirectory
// + file endpoint on a public-internet bridge would let any internet user
// browse and download the library. The `shouldEnableDLNA` gate enforces
// this invariant; don't add an override flag. See `config_gate.go`.
//
// # Bit-exact contract
//
// Files are served via `http.ServeContent` (Range and 206 Partial Content
// preserved) wrapped in `AdaptiveResponseWriter` for per-connection chunk
// sizing — NEVER through any transcoding, transformation, or re-encoding
// path. The bridge's mission is "bit-exact or silent failure"; the DLNA
// data path inherits the same contract.
//
// # Per-vendor MIME resolution
//
// The renderer's `GET /dlna/file/{trackID}` is issued completely
// asynchronously from the iOS control point's `SetAVTransportURI` SOAP
// call. The bridge has no session state from iOS to consult on this
// request — Content-Type is resolved per-request from the User-Agent
// string via `ResolveMIMEType`. The DIDL-Lite MIME hint iOS supplied is
// advisory; the actual response Content-Type is what determines whether
// e.g. Sony renders DSD or rejects-then-skips. See `protocol_info.go`.
//
// This Go-side per-vendor matcher MUST stay aligned with the iOS-side
// `RendererProfileRegistry` matcher. Both consult the same interop table
// in the design doc; drift between them produces silent renderer-specific
// playback failures that bypass every test gate.
//
// # Telemetry
//
// Per-connection request logging captures User-Agent, Accept, Range
// patterns, response codes, byte volumes, and RTT samples in a bounded
// ring buffer exposed via `/v1/admin/dlna/telemetry`. Forms the empirical
// dataset for tuning the adaptive chunk allocator and populating the
// per-renderer profile registry. Sub-millisecond overhead per request.
package dlna
