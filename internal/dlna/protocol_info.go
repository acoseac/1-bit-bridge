package dlna

import "fmt"

// DLNAFlags is the canonical 32-character hex DLNA.ORG_FLAGS value the
// bridge advertises for every served file. The bit positions encode
// streaming-mode + background-transfer + DLNA v1.5 + byte-range support.
// The 0x01700000 high nibble pattern is the documented value used by
// every major DLNA MediaServer for streaming audio.
//
// Default to the FULL 4-param protocolInfo string (not the `*` short
// form) in DIDL-Lite responses and `contentFeatures.dlna.org` headers —
// many strict renderers reject the `*` form. Uniformity also reduces the
// test surface; if Phase 0 surfaces a renderer that DEMANDS the `*`
// form, add an exception via `RendererProfile` rather than flipping the
// default.
const DLNAFlags = "01700000000000000000000000000000"

// ResolveMIMEType returns the Content-Type the bridge should announce in
// the file-handler response, derived from the renderer's User-Agent and
// the source file's extension. Per-vendor branches are necessary because
// there is no spec-canonical DSF MIME — Sony renderers reject
// "audio/x-dsf" and demand "audio/dsd"; Chord/Integra accept "audio/x-dsf";
// Denon doesn't surface DSF at all.
//
// User-Agent matching is case-SENSITIVE because vendor strings are
// case-stable in the wild. Substring containment is used because some
// renderers append firmware versions / model suffixes to a stable vendor
// prefix.
//
// **Phase 0 empirical findings (2026-05-26 spike against Chord 2go):**
//   - The Chord 2go's playback engine sends User-Agent
//     "Music Player Daemon 0.21.26" — NOT "Chord"/"2go"/"Poly".
//     The 2go runs MPD internally and leaks MPD's identity verbatim.
//     A naive "Chord"-only matcher would never fire in production for
//     this device. Multiple Naim/Linn/Lumin models are also MPD-based.
//   - Control-point libavformat (FFmpeg) probes ID3 tags BEFORE the
//     renderer fetches with User-Agent "Lavf/<version>" — does
//     head+tail Range reads that the matcher MUST handle correctly
//     too (wrong MIME on the metadata probe can confuse mConnect
//     before the actual renderer fetch even starts).
//   - mConnect's own control-plane requests carry "mconnect Player"
//     but those don't hit the file handler so don't need matching.
//   - Both MPD and Lavf coincidentally want the same MIME as the
//     default (`audio/x-dsf` for DSF). Explicit branches kept for
//     telemetry clarity AND so future divergence (e.g. MPD ever
//     wanting a different MIME) is a one-line change.
//
// **Branch ordering** is load-bearing: vendor-specific overrides
// (Sony's "audio/dsd" demand) come FIRST, before generic-playback-
// engine branches, so a Sony device that internally uses MPD still
// gets the Sony-correct MIME if its UA prefix contains "Sony".
//
// **DO NOT** add lossy transcoding fallbacks here. If a renderer can't
// consume DSF in any native form, surface as "this renderer cannot play
// DSD" at the iOS picker stage (RendererCapability) rather than silently
// transcoding away the bit-exact contract.
func ResolveMIMEType(userAgent, extension string) string {
	// Delegates to the per-vendor `RendererProfileRegistry` (see
	// `renderer_profile.go`). The branch ordering documented above is
	// reproduced by the registry's `Profiles` slice order: Sony first
	// (vendor override for `audio/dsd`), then Chord/2go/Poly, then
	// Integra/Onkyo, then MPD-generic, then Lavf, then unknown
	// (catch-all that falls through to `defaultMIMEForExtension`).
	//
	// **Adding a new known renderer or refining MIME preferences MUST
	// go through `renderer_profile.go`'s `Profiles` slice, not by
	// editing this function.** Per-vendor data is co-located with the
	// rest of each profile's metadata (known bugs, max safe file size,
	// etc.) so future per-renderer features can build on the same
	// structured catalog without re-discovering vendor identity from
	// scratch.
	return PreferredMIMEFor(userAgent, extension)
}

// ProtocolInfoFor returns the full 4-param DIDL-Lite protocolInfo string
// for the given (User-Agent, extension) pair. The fourth-param flag
// string is always the canonical DLNAFlags constant — see DLNAFlags doc
// for the rationale on defaulting to the full form rather than the `*`
// short form.
//
// DLNA.ORG_OP=01 = range-request supported, time-seek not.
// DLNA.ORG_CI=0  = content NOT transcoded — load-bearing for the
//
//	bit-exact contract; downstream renderers and audio
//	enthusiast tooling key off this flag.
func ProtocolInfoFor(userAgent, extension string) string {
	mime := ResolveMIMEType(userAgent, extension)
	return fmt.Sprintf(
		"http-get:*:%s:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=%s",
		mime, DLNAFlags,
	)
}

// ContentFeaturesHeaderValue returns the value to set on the
// `contentFeatures.dlna.org` response header. The value is the feature
// flags only — the MIME lives in the Content-Type response header
// alongside it (set by the file handler). Some renderers consult this
// header for capability validation independently of (or in addition to)
// the DIDL-Lite hint; serving a different feature-flag value here vs.
// in DIDL-Lite produces inconsistent renderer behavior.
//
// (userAgent, extension) are accepted in the signature so a future
// per-renderer DLNA.ORG_PN profile can be threaded through without a
// caller-site signature change. Current shape uses `*` (no PN) per the
// "no standard PN for DSF" finding documented in the plan.
func ContentFeaturesHeaderValue(userAgent, extension string) string {
	_ = userAgent // reserved for per-renderer DLNA.ORG_PN routing in v1.x
	_ = extension // reserved for codec-derived DLNA.ORG_PN routing in v1.x
	return fmt.Sprintf("DLNA.ORG_PN=*;DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=%s", DLNAFlags)
}
