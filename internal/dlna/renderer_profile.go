package dlna

import "strings"

// RendererBug is a stable identifier for a documented per-renderer behavior
// quirk. Used by telemetry labeling AND as a future gating point for
// file-handler decisions (e.g., a renderer with the >2GB DSF parser overflow
// bug should not be served files larger than 2^31-1 bytes — `MaxSafeFileSize`
// drives that decision).
//
// Naming convention: `BugFeature[ConditionDetail]` in CamelCase, with a
// matching stable string value in camelCase that serves as the wire-stable
// identifier across logs and (future) admin-console exposure.
type RendererBug string

const (
	// BugID3OffsetOverflowOver2GB — many renderer firmwares use signed 32-bit
	// integers for the ID3v2 metadata offset pointer in DSF metadata. DSF
	// files larger than 2^31-1 bytes (~2.147 GB) overflow the signed range
	// and produce blank track listings, infinite socket hangs, or silent
	// skip-to-previous on the affected renderer. Documented for Chord 2go
	// via field reports; not empirically confirmed in Phase 0 (no >2GB DSF
	// available in the test library). Production design assumes the bug
	// per the conservative-default principle (`MaxSafeFileSize = 2^31-1`).
	BugID3OffsetOverflowOver2GB RendererBug = "id3OffsetOverflowOver2GB"

	// BugRapidPauseResumeArtifact — Hugo 2 DAC's analog output mute relay
	// disengages on every I2S clock-resume cycle (PLL relock transient =
	// audible click). Cumulative analog-stage settling state from repeated
	// rapid pause/resume cycles produces a persistent low ringing artifact
	// that doesn't fully decay between cycles. Empirically confirmed in
	// Phase 0 (2026-05-26). Outside our software's reach — the bridge
	// doesn't own the audio thread; iOS doesn't either (the 2go does).
	// PR 4 surfaces as a one-time UX hint on first DLNA pairing with a
	// Chord-family renderer.
	BugRapidPauseResumeArtifact RendererBug = "rapidPauseResumeArtifact"
)

// RendererProfile captures the per-vendor behaviors and quirks the bridge
// has observed empirically for DLNA renderers it serves files to. The
// matching is by HTTP User-Agent substring (case-sensitive — vendor strings
// are case-stable in the wild).
//
// **Two parallel registries by necessity** (see plan): the bridge-side
// profile here governs the actual file-handler response Content-Type
// (resolved per-request from the User-Agent the renderer sends). The
// iOS-side `RendererProfileRegistry` governs DIDL-Lite generation (the
// MIME hint the renderer is told to fetch). Both consult the same
// per-vendor interop table; they cover different decision points.
//
// **Branch ordering matters.** Sony-specific overrides come FIRST in the
// `Profiles` slice, before generic playback-engine profiles, so a Sony
// device that internally uses MPD or libavformat still hits the
// Sony-correct branch (`audio/dsd` for DSF) rather than the MPD default.
// The catch-all `unknown` profile MUST be LAST — its empty-string
// matcher catches everything that didn't match an earlier profile.
//
// Bridge-side fields are a subset of the iOS-side struct; subscription
// lifecycle / gapless / GENA fields don't apply to the file-serve layer
// (they're iOS-Control-Point concerns).
type RendererProfile struct {
	// ID — stable identifier for telemetry / logs. camelCase, no whitespace.
	ID string

	// DisplayName — human-friendly name for admin console / docs.
	DisplayName string

	// UserAgentMatchers — substrings to match against incoming UA strings.
	// First substring match wins (within the profile AND across profiles —
	// the `Profiles` slice is iterated in order, first profile-match wins).
	// Case-sensitive. The `unknown` catch-all profile uses `[""]` here so
	// `strings.Contains(anything, "")` returns true for every UA.
	UserAgentMatchers []string

	// PreferredMIME — per-file-extension Content-Type this renderer prefers.
	// Keys are lowercase extensions WITH the leading dot (".dsf", ".flac",
	// etc.). Missing entries fall through to `defaultMIMEForExtension`.
	PreferredMIME map[string]string

	// KnownBugs — documented per-vendor quirks. Informational for now;
	// future v1.x features may consult this map for runtime decisions
	// (e.g., `MaxSafeFileSize`-driven 413 response).
	KnownBugs map[RendererBug]bool

	// MaxSafeFileSize — if >0, the file handler refuses to serve files
	// larger than this many bytes for this renderer (returns 413 Payload
	// Too Large). 0 = no size limit. For renderers carrying
	// `BugID3OffsetOverflowOver2GB`, set to `2147483647` (2^31-1).
	//
	// NOT consulted yet by the file handler (the production design hides
	// oversized DSF files at the iOS picker stage via
	// `RendererCapability.canPlay`). Field exists for future use AND for
	// documentation parity with the iOS-side registry.
	MaxSafeFileSize int64
}

// Profiles is the ordered registry of known renderer profiles. Iterated in
// order on every `MatchProfile` call; first profile whose
// `UserAgentMatchers` matches the incoming UA wins. The catch-all
// `unknown` profile MUST be the LAST entry — its empty-string matcher
// matches every UA, so it serves as the structural fallback.
//
// Initial entries seeded from Phase 0 empirical findings (2026-05-26)
// against Chord 2go via mConnect Lite + spike script.
var Profiles = []RendererProfile{
	profileSony(),
	profileChordFamily(),
	profileIntegraOnkyo(),
	profileMPDGeneric(),
	profileLavf(),
	profileUnknown(),
}

// MatchProfile returns the first profile in `Profiles` whose
// `UserAgentMatchers` substring-matches the given UA. The `unknown`
// catch-all profile guarantees a match for every input (via its
// empty-string matcher), so this never returns a zero-value struct.
func MatchProfile(userAgent string) RendererProfile {
	for _, p := range Profiles {
		for _, m := range p.UserAgentMatchers {
			if strings.Contains(userAgent, m) {
				return p
			}
		}
	}
	// Defense in depth — the unknown profile's empty-string matcher should
	// always catch anything that reached here. If the registry was somehow
	// mutated to drop the unknown profile, return a zero-value profile
	// rather than panicking; downstream callers (PreferredMIMEFor) handle
	// nil PreferredMIME gracefully.
	return RendererProfile{ID: "unknown-fallback"}
}

// PreferredMIMEFor consults the registry to determine the Content-Type the
// bridge should announce in the file-handler response for the given (UA,
// extension) pair. Matched profile's `PreferredMIME` entry wins; missing
// entries fall through to `defaultMIMEForExtension`.
func PreferredMIMEFor(userAgent, extension string) string {
	ext := strings.ToLower(extension)
	profile := MatchProfile(userAgent)
	if mime, ok := profile.PreferredMIME[ext]; ok {
		return mime
	}
	return defaultMIMEForExtension(ext)
}

// defaultMIMEForExtension returns the spec-canonical Content-Type for the
// given file extension, used as the fallback when no profile specifies a
// per-vendor override. Extension MUST be lowercased before calling (matches
// the convention `PreferredMIMEFor` uses).
//
// For DSF/DFF there's no spec-canonical MIME (the format predates the
// audio/x-dsf / audio/x-dff conventions adopted by Chord/Integra/Onkyo);
// the "default" here is the highest-interop choice per Phase-0 verified
// field reports — most renderers in the field accept `audio/x-dsf` for
// DSF and `audio/x-dff` for DFF. Renderers that demand a different MIME
// (e.g., Sony's `audio/dsd`) carry an explicit override in their profile.
func defaultMIMEForExtension(extension string) string {
	switch extension {
	case ".dsf":
		return "audio/x-dsf"
	case ".dff":
		return "audio/x-dff"
	case ".flac":
		return "audio/x-flac"
	case ".m4a", ".mp4":
		return "audio/mp4"
	case ".wav":
		return "audio/wav"
	case ".aiff", ".aif":
		return "audio/aiff"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg":
		return "audio/ogg"
	default:
		return "application/octet-stream"
	}
}

// -----------------------------------------------------------------------------
// Profile constructors. Each returns a fresh RendererProfile struct seeded
// with the empirical findings from Phase 0 + documented field reports. Kept
// as constructors (rather than package-level var literals) so tests can
// construct fresh copies without sharing mutable map state between cases.
// -----------------------------------------------------------------------------

// profileSony — Sony renderers reject `audio/x-dsf` and require `audio/dsd`
// for both DSF and DFF. Documented across multiple Sony streaming products
// (SRS-HG1, SRS-HG10, etc.). MUST be matched FIRST in the `Profiles` slice
// so a Sony device that internally uses MPD or libavformat still hits the
// Sony-correct branch rather than the generic MPD/Lavf default.
func profileSony() RendererProfile {
	return RendererProfile{
		ID:                "sony",
		DisplayName:       "Sony (SRS-HG1 family)",
		UserAgentMatchers: []string{"Sony"},
		PreferredMIME: map[string]string{
			".dsf": "audio/dsd",
			".dff": "audio/dsd",
		},
	}
}

// profileChordFamily — Chord 2go / Poly / Hugo TT (with network module).
// Empirically confirmed in Phase 0 (2026-05-26) playing DSD256 bit-exact
// via `audio/x-dsf`. **The 2go's playback engine identifies as
// "Music Player Daemon 0.21.26" in HTTP headers** (the 2go runs MPD
// internally and leaks MPD's identity verbatim) — so the Chord profile's
// matchers won't fire for the 2go's actual file-fetch requests. Those go
// to the `mpdGeneric` profile below. The Chord matchers here cover any
// future renderer that DOES advertise as "Chord" / "2go" / "Poly" in the
// User-Agent (e.g., a hypothetical Chord-branded network stack).
//
// Hardware bug: `BugRapidPauseResumeArtifact` documented from Phase 0 —
// Hugo 2 DAC's PLL relock + mute-relay artifacts on every play/pause
// cycle. Outside the bridge's reach (we don't own the audio thread).
func profileChordFamily() RendererProfile {
	return RendererProfile{
		ID:                "chordFamily",
		DisplayName:       "Chord 2go / Poly / Hugo (network)",
		UserAgentMatchers: []string{"Chord", "2go", "Poly"},
		PreferredMIME: map[string]string{
			".dsf": "audio/x-dsf",
			".dff": "audio/x-dff",
		},
		KnownBugs: map[RendererBug]bool{
			BugID3OffsetOverflowOver2GB: true, // assumed per field reports; not empirically confirmed in Phase 0
			BugRapidPauseResumeArtifact: true, // confirmed Phase 0 — Hugo 2 PLL relock + mute relay
		},
		MaxSafeFileSize: 2147483647, // 2^31-1; defensive cap per BugID3OffsetOverflowOver2GB assumption
	}
}

// profileIntegraOnkyo — Integra DRX-3 and Onkyo TX-NR686-class receivers.
// Documented to accept `audio/x-dsf` / `audio/x-dsd` / `audio/x-dff` for
// DSD content. Same DSF/DFF defaults as Chord; included as a separate
// profile for telemetry granularity + future per-vendor divergence.
func profileIntegraOnkyo() RendererProfile {
	return RendererProfile{
		ID:                "integraOnkyo",
		DisplayName:       "Integra / Onkyo",
		UserAgentMatchers: []string{"Integra", "Onkyo"},
		PreferredMIME: map[string]string{
			".dsf": "audio/x-dsf",
			".dff": "audio/x-dff",
		},
	}
}

// profileMPDGeneric — generic Music Player Daemon based renderers.
// **Most common in the wild — covers Chord 2go (which leaks MPD's UA
// verbatim), many Naim / Linn / Lumin models, plus any DIY MPD-based
// streaming endpoint.** Empirically confirmed in Phase 0 against Chord
// 2go: `User-Agent: "Music Player Daemon 0.21.26"` with no Range header
// for the actual playback fetch, sequential streaming.
//
// MPD's connection-hold behavior on pause: holds the HTTP connection
// alive for ~28 seconds idle before closing client-side. After timeout,
// control points typically issue a fresh SetAVTransportURI on resume —
// driving the renderer to re-fetch from byte 0 and fast-forward to the
// paused position. iOS production design pins a ~15s keepalive
// `GetPositionInfo` poll during pause to prevent the timeout (PR 2).
func profileMPDGeneric() RendererProfile {
	return RendererProfile{
		ID:                "mpdGeneric",
		DisplayName:       "Music Player Daemon (Chord 2go, Naim/Linn/Lumin)",
		UserAgentMatchers: []string{"Music Player Daemon"},
		PreferredMIME: map[string]string{
			".dsf": "audio/x-dsf",
			".dff": "audio/x-dff",
		},
	}
}

// profileLavf — FFmpeg libavformat. Used by many UPnP control points
// (mConnect, BubbleUPnP, Roon Bridge) for metadata extraction probes
// BEFORE the renderer fetches for actual playback. Empirically confirmed
// in Phase 0: `User-Agent: "Lavf/58.45.100"` doing head + tail Range
// reads (`bytes=0-` then `bytes=594960476-` for ID3 at file tail). Wrong
// MIME on this probe path can break mConnect's metadata extraction
// before the renderer fetch even starts.
func profileLavf() RendererProfile {
	return RendererProfile{
		ID:                "lavf",
		DisplayName:       "FFmpeg libavformat (control-point metadata probe)",
		UserAgentMatchers: []string{"Lavf"},
		PreferredMIME: map[string]string{
			".dsf": "audio/x-dsf",
			".dff": "audio/x-dff",
		},
	}
}

// profileUnknown — catch-all for renderers we haven't seen before. MUST
// be the LAST entry in the `Profiles` slice. The empty-string matcher
// makes `strings.Contains(anything, "")` return true for every UA, so
// this profile catches everything that didn't match an earlier profile.
//
// No PreferredMIME overrides — every extension falls through to
// `defaultMIMEForExtension`. This is the "highest-interop conservative
// default" path for unrecognized renderers; if a specific unknown
// renderer turns out to need a different MIME, add it as its own
// profile rather than mutating this catch-all.
func profileUnknown() RendererProfile {
	return RendererProfile{
		ID:                "unknown",
		DisplayName:       "Unknown renderer (catch-all)",
		UserAgentMatchers: []string{""}, // empty string matches every UA (substring contains "" is always true)
	}
}
