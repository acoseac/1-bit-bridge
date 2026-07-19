package dlna

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// -----------------------------------------------------------------------------
// Track ID hashing — stable across scan refreshes (load-bearing invariant)
// -----------------------------------------------------------------------------

// TrackID returns a stable opaque identifier for a track, derived purely
// from (libraryRoot, relativePath). The returned ID is the trackID
// component of `/dlna/file/{trackID}` URLs that DIDL-Lite responses
// embed; the file handler resolves the ID back to an absolute path via
// the manifest store (lookup-by-trackID added in PR 1 task #10).
//
// **Load-bearing stability invariant:** the hash MUST remain a pure
// function of (libraryRoot, relativePath). NEVER incorporate any
// scan-generation data — database row index, autoincrement column,
// scan epoch, file mtime, file size, tag content, anything that
// changes across re-scans. Renderers cache the trackID in their own
// queue state at SetAVTransportURI time; a trackID that changes
// across a re-scan causes the renderer to issue `GET /dlna/file/
// {old_hash}` on the next track transition and receive a terminal
// 404, dropping playback for the user. The
// `Test_TrackID_StableAcrossScanRefresh` regression guard pins this
// invariant by constructing multiple synthetic Track-shaped inputs
// that differ in every field BUT path and asserting the IDs collide.
//
// 16 hex chars (64 bits of entropy) is the truncation length —
// vastly oversized for any single library (the birthday-paradox
// collision boundary is ~4 billion tracks; real libraries top out at
// ~100k). NULL byte separator between root and path prevents
// (root="a", path="bc") from colliding with (root="ab", path="c").
func TrackID(libraryRoot, relativePath string) string {
	h := sha256.Sum256([]byte(libraryRoot + "\x00" + relativePath))
	return hex.EncodeToString(h[:8])
}

// -----------------------------------------------------------------------------
// DIDL-Lite generation
// -----------------------------------------------------------------------------

// DIDLTrackOpts is the input DTO for `DIDLForTrack`. Decoupling the
// dlna package from manifest.Track lets the package test in isolation
// (synthetic literals in tests) AND keeps the import graph
// one-directional (dlna → manifest at adapter sites only, not in this
// pure XML-generation layer).
//
// Field semantics map to UPnP / DLNA conventions:
//   - TrackID — opaque identifier, see `TrackID()` for the stability contract
//   - ServerURL — absolute base URL of the bridge from the renderer's
//     perspective (e.g., "http://192.168.0.14:7790"). Used to construct
//     the absolute file URL and the absolute artwork URL.
//   - UserAgent — incoming renderer UA; consulted for per-vendor MIME
//     selection via the `RendererProfileRegistry`.
//   - FileExtension — lowercase including the leading dot (".dsf"); used
//     for MIME resolution AND for the `<res>` `protocolInfo` attribute.
//
// Pointer-typed optional fields (e.g., the *int / *float64 in
// manifest.Track) are flattened to value types here — the caller is
// responsible for translating nil → 0 / empty / sentinel before calling.
// The XML generator treats zero / empty values as "absent attribute or
// element" (omits them from the output).
type DIDLTrackOpts struct {
	TrackID string // stable hash, see TrackID()
	// ParentID is the ObjectID of the container the item lives
	// in (e.g. "all_tracks", "music/albums/some-album-hash").
	// Per UPnP CDS spec, each `<item>` MUST declare its parent's
	// ObjectID. Strict third-party controllers (mconnect Lite
	// observed 2026-05-28 + Linn Kazoo per UPnP-tester reports)
	// reject Browse responses where the items' parentID doesn't
	// match the ObjectID just Browse'd, treating the inconsistency
	// as a malformed response and silently failing to render the
	// children. Defaults to "0" if the caller leaves it empty so
	// existing call sites that haven't been updated yet still
	// emit valid (if slightly off-spec) DIDL — matches the
	// pre-PR-#309 behavior verbatim.
	ParentID        string  // parent container ObjectID
	Title           string  // dc:title
	Artist          string  // upnp:artist
	AlbumArtist     string  // upnp:artist with role="AlbumArtist" if differs from Artist
	Album           string  // upnp:album
	Composer        string  // upnp:author with role="Composer"
	Genre           string  // upnp:genre
	Year            int     // dc:date — emitted as "YYYY-01-01" if > 0
	TrackNumber     int     // upnp:originalTrackNumber — emitted if > 0
	DiscNumber      int     // not in DLNA standard; skipped
	Size            int64   // <res size="N">
	DurationSeconds float64 // <res duration="H:MM:SS.mmm">; omitted if 0
	SampleRateHz    int     // <res sampleFrequency="N">
	BitsPerSample   int     // <res bitsPerSample="N">
	Channels        int     // <res nrAudioChannels="N">; defaults to 2 if 0
	IsDSD           bool    // hint for sampleFrequency interpretation; informational
	Codec           string  // canonical codec string ("FLAC", "DSF", "ALAC", etc.)
	FileExtension   string  // ".dsf", ".flac", etc. (lowercase with leading dot)
	ArtworkURL      string  // absolute URL for <upnp:albumArtURI>; emitted only if non-empty

	// Variants are offline-cached alternate renderings emitted as
	// additional <res> elements after the source <res>. Empty =
	// source-only (the common case).
	Variants []VariantInfo

	ServerURL string // base URL for constructing the absolute file URL
	UserAgent string // for per-vendor MIME selection
}

// DIDLContainerOpts is the input DTO for `DIDLForContainer`. Containers
// are the hierarchical browse nodes (Music / Artists / Albums / Tracks).
//
// UPnPClass is the canonical DLNA class identifier:
//   - "object.container"                          — generic container
//   - "object.container.album.musicAlbum"         — an album
//   - "object.container.person.musicArtist"       — an artist (or composer)
//   - "object.container.genre.musicGenre"         — a genre
//   - "object.container.storageFolder"            — a filesystem folder
//
// ChildCount is the number of items inside this container. Some
// renderers expect this attribute; missing it can cause the picker UI
// to show "empty" until the user manually enters the container.
// Use -1 to indicate "unknown / dynamic" (rendered as `childCount="0"`
// which is the spec's sentinel for "no advertised count").
type DIDLContainerOpts struct {
	ID         string // ObjectID, e.g. "music" or "music/artists/{hash}"
	ParentID   string // parent ObjectID, e.g. "0" or "music"
	Title      string // dc:title
	ChildCount int    // advertised child count (-1 = unknown)
	UPnPClass  string // see comment above
	ArtworkURL string // optional <upnp:albumArtURI> (e.g. album cover for an album container)
}

// sourceResAttrs builds the conditional `<res>` attribute list for a
// track's SOURCE rendering. Extracted from DIDLForTrack to keep that
// function's cognitive complexity under the threshold (SonarCloud S3776
// on PR #330). Strict renderers reject zero / missing values for some
// attributes (e.g. sampleFrequency=0), so attributes are omitted
// entirely rather than emitted as a literal zero.
func sourceResAttrs(opts DIDLTrackOpts) []string {
	mime := PreferredMIMEFor(opts.UserAgent, opts.FileExtension)
	protocolInfo := fmt.Sprintf(
		"http-get:*:%s:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=%s",
		mime, DLNAFlags,
	)
	attrs := []string{
		fmt.Sprintf(`protocolInfo="%s"`, escapeXMLText(protocolInfo)),
		fmt.Sprintf(`size="%d"`, opts.Size),
	}
	if opts.DurationSeconds > 0 {
		attrs = append(attrs, fmt.Sprintf(`duration=%q`, formatDLNADuration(opts.DurationSeconds)))
	}
	if opts.SampleRateHz > 0 {
		attrs = append(attrs, fmt.Sprintf(`sampleFrequency="%d"`, opts.SampleRateHz))
	}
	// **Defense-in-depth `!opts.IsDSD` co-gate**: DSD tracks have an
	// inherent bit depth of 1 by definition (DSD = 1-bit pulse density
	// modulation), but the DLNA `<res bitsPerSample>` attribute is
	// conventionally PCM-only — sending "1" causes renderer parsers that
	// treat the field as PCM bit-depth to reject the dispatch as "1-bit
	// PCM" (nonsense; observed silent-decline on Chord 2Go 2026-05-28
	// from the iOS-side equivalent before that side added an isDSD gate
	// at its DIDL chokepoint). Companion Mirror-PR with iOS PR #564 — the
	// bridge serves DIDL-Lite directly to third-party UPnP controllers
	// browsing the CDS (e.g. mconnect, Kazoo) so the same protection must
	// live here. Callers that set IsDSD=false while passing
	// BitsPerSample > 0 (PCM tracks) emit the attribute unchanged. Per
	// Gemini cross-codebase audit 2026-05-28.
	if opts.BitsPerSample > 0 && !opts.IsDSD {
		attrs = append(attrs, fmt.Sprintf(`bitsPerSample="%d"`, opts.BitsPerSample))
	}
	// Default to 2 channels (stereo) if unspecified — the bridge's audio
	// focus is stereo content and a missing nrAudioChannels attribute can
	// confuse some renderers.
	channels := opts.Channels
	if channels <= 0 {
		channels = 2
	}
	attrs = append(attrs, fmt.Sprintf(`nrAudioChannels="%d"`, channels))
	return attrs
}

// DIDLForTrack returns the DIDL-Lite `<item>` XML for the given track,
// formatted as a single line with NO surrounding whitespace inside any
// text node. Single-line emission is load-bearing: Phase 0 spike confirmed
// that mConnect Lite reads the `<res>...</res>` text node verbatim and
// fails URL parsing if whitespace surrounds the URL, producing the
// "SetAVTransportURI is null" symptom that blocks playback.
//
// The returned string is the BARE `<item>` element with no surrounding
// `<DIDL-Lite>` wrapper. Callers (SOAP Browse handler, task #9) wrap
// in the appropriate DIDL-Lite namespaces + Result element.
func DIDLForTrack(opts DIDLTrackOpts) string {
	resAttrs := sourceResAttrs(opts)

	fileURL := strings.TrimRight(opts.ServerURL, "/") + "/dlna/file/" + opts.TrackID

	// XML-escape all user-provided text fields. Title can contain
	// apostrophes / quotes / ampersands; album / artist commonly carry
	// & (e.g., "Simon & Garfunkel").
	title := escapeXMLText(opts.Title)
	artist := escapeXMLText(opts.Artist)
	album := escapeXMLText(opts.Album)
	albumArtist := escapeXMLText(opts.AlbumArtist)
	composer := escapeXMLText(opts.Composer)
	genre := escapeXMLText(opts.Genre)

	var sb strings.Builder
	// A populated <item> (id/parentID/title/artist/album/<res>/artwork URI)
	// runs ~500-700 bytes; pre-size to skip the builder's early geometric
	// regrowths during a Browse window. (external review r3)
	sb.Grow(640)
	// `opts.TrackID` is a SHA-256-derived hash (safe ASCII per the
	// TrackID() helper), but defensive XML-attribute escape is cheap
	// and protects against any future caller that passes a custom
	// trackID with `"`/`&`/`<` characters. Same rationale as the
	// container-attribute escape below. Per Gemini Medium-Security
	// finding on PR #303.
	// Default ParentID to "0" when caller leaves it empty —
	// preserves pre-PR-#309 behavior for any call site we missed
	// updating. Callers that emit items into a non-root container
	// (e.g. "all_tracks") MUST set ParentID accordingly so strict
	// controllers can resolve the container→item parent link.
	parentID := opts.ParentID
	if parentID == "" {
		parentID = "0"
	}
	sb.WriteString(fmt.Sprintf(`<item id="%s" parentID="%s" restricted="1">`,
		escapeXMLText(opts.TrackID), escapeXMLText(parentID)))
	sb.WriteString(`<dc:title>`)
	sb.WriteString(title)
	sb.WriteString(`</dc:title>`)
	sb.WriteString(`<upnp:class>object.item.audioItem.musicTrack</upnp:class>`)
	if artist != "" {
		sb.WriteString(`<upnp:artist>`)
		sb.WriteString(artist)
		sb.WriteString(`</upnp:artist>`)
	}
	if albumArtist != "" && albumArtist != artist {
		sb.WriteString(`<upnp:artist role="AlbumArtist">`)
		sb.WriteString(albumArtist)
		sb.WriteString(`</upnp:artist>`)
	}
	if album != "" {
		sb.WriteString(`<upnp:album>`)
		sb.WriteString(album)
		sb.WriteString(`</upnp:album>`)
	}
	if composer != "" {
		sb.WriteString(`<upnp:author role="Composer">`)
		sb.WriteString(composer)
		sb.WriteString(`</upnp:author>`)
	}
	if genre != "" {
		sb.WriteString(`<upnp:genre>`)
		sb.WriteString(genre)
		sb.WriteString(`</upnp:genre>`)
	}
	if opts.Year > 0 {
		// dc:date format per DLNA: "YYYY-MM-DD". We only have the
		// year; emit January 1 as a placeholder month-day (matches
		// what most other DLNA MediaServers do for year-only metadata).
		// Zero-pad the year to 4 digits so a malformed 2/3-digit year
		// (e.g. 99) emits valid ISO 8601 `0099-01-01` rather than
		// `99-01-01`, which strict legacy renderer parsers reject.
		// Mirror-PR with iOS DLNADIDLLiteGenerator (same %04d).
		sb.WriteString(fmt.Sprintf(`<dc:date>%04d-01-01</dc:date>`, opts.Year))
	}
	if opts.TrackNumber > 0 {
		sb.WriteString(fmt.Sprintf(`<upnp:originalTrackNumber>%d</upnp:originalTrackNumber>`, opts.TrackNumber))
	}
	if opts.ArtworkURL != "" {
		sb.WriteString(`<upnp:albumArtURI>`)
		sb.WriteString(escapeXMLText(opts.ArtworkURL))
		sb.WriteString(`</upnp:albumArtURI>`)
	}
	// The <res> element — text content is the file URL with NO
	// surrounding whitespace (load-bearing per Phase 0 finding).
	sb.WriteString(`<res `)
	sb.WriteString(strings.Join(resAttrs, " "))
	sb.WriteString(`>`)
	sb.WriteString(escapeXMLText(fileURL))
	sb.WriteString(`</res>`)

	// Additional <res> elements for offline-cached variants, AFTER the
	// source <res> so the bit-exact original stays the default pick.
	// Ordered optimized → upscaled so a picky / entry-level renderer that
	// rejects a hi-res or DSD source finds the broadly-playable 16-bit
	// FLAC option immediately next.
	for _, v := range orderedVariants(opts.Variants) {
		sb.WriteString(variantResElement(opts, v))
	}

	sb.WriteString(`</item>`)
	return sb.String()
}

// orderedVariants returns the variants in DIDL <res> emission order:
// optimized (the universal 16-bit compatibility floor) first, then
// upscaled (hi-res), then any future kinds — each group preserving input
// order. Two-pass partition (no sort import; the variant count per track
// is tiny).
func orderedVariants(vs []VariantInfo) []VariantInfo {
	if len(vs) <= 1 {
		return vs
	}
	out := make([]VariantInfo, 0, len(vs))
	for _, v := range vs {
		if strings.HasPrefix(v.VariantID, "optimized") {
			out = append(out, v)
		}
	}
	for _, v := range vs {
		if strings.HasPrefix(v.VariantID, "upscaled") {
			out = append(out, v)
		}
	}
	for _, v := range vs {
		if !strings.HasPrefix(v.VariantID, "optimized") && !strings.HasPrefix(v.VariantID, "upscaled") {
			out = append(out, v)
		}
	}
	return out
}

// variantResElement builds the DIDL-Lite <res> for one offline variant.
// Variants are always lossless PCM FLAC, so (unlike the source path)
// there's no DSD bit-depth gate — bitsPerSample is emitted whenever > 0.
// The URL uses a clean PATH SEGMENT (NOT a query string):
//
//	<base>/dlna/file/{trackID}/variant-{variantID}{ext}
//
// Many boutique / legacy UPnP renderers strip query strings before
// handing the URL to their decode buffer; a path segment survives. The
// file handler's extractTrackID already splits on the first '/', so the
// trackID still resolves, and extractVariantID recovers the variant.
func variantResElement(opts DIDLTrackOpts, v VariantInfo) string {
	mime := PreferredMIMEFor(opts.UserAgent, v.FileExtension)
	protocolInfo := fmt.Sprintf(
		"http-get:*:%s:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=%s",
		mime, DLNAFlags,
	)
	attrs := []string{
		fmt.Sprintf(`protocolInfo="%s"`, escapeXMLText(protocolInfo)),
		fmt.Sprintf(`size="%d"`, v.Size),
	}
	if opts.DurationSeconds > 0 {
		attrs = append(attrs, fmt.Sprintf(`duration=%q`, formatDLNADuration(opts.DurationSeconds)))
	}
	if v.SampleRate > 0 {
		attrs = append(attrs, fmt.Sprintf(`sampleFrequency="%d"`, v.SampleRate))
	}
	if v.BitDepth > 0 {
		attrs = append(attrs, fmt.Sprintf(`bitsPerSample="%d"`, v.BitDepth))
	}
	channels := opts.Channels
	if channels <= 0 {
		channels = 2
	}
	attrs = append(attrs, fmt.Sprintf(`nrAudioChannels="%d"`, channels))

	variantURL := strings.TrimRight(opts.ServerURL, "/") +
		"/dlna/file/" + opts.TrackID + "/variant-" + v.VariantID + v.FileExtension

	var sb strings.Builder
	sb.WriteString(`<res `)
	sb.WriteString(strings.Join(attrs, " "))
	sb.WriteString(`>`)
	sb.WriteString(escapeXMLText(variantURL))
	sb.WriteString(`</res>`)
	return sb.String()
}

// DIDLForContainer returns the DIDL-Lite `<container>` XML for the given
// container, formatted as a single line with no surrounding whitespace
// in any text node (same single-line invariant as DIDLForTrack).
func DIDLForContainer(opts DIDLContainerOpts) string {
	upnpClass := opts.UPnPClass
	if upnpClass == "" {
		upnpClass = "object.container"
	}
	childCount := opts.ChildCount
	if childCount < 0 {
		childCount = 0 // DLNA spec: 0 = "unknown / no advertised count"
	}
	var sb strings.Builder
	// XML-attribute escape for caller-supplied ObjectID values.
	// Today's call sites pass safe ASCII ("music", "all_tracks",
	// "music/artists"), but the v1.x hierarchy expansion will
	// produce hash-derived IDs that should be safe-by-construction
	// and arbitrary-string-derived IDs that need defensive escape.
	// Doing it now keeps the helper's invariant clean. Per Gemini
	// Medium-Security finding on PR #303.
	// `searchable="1"` — empirical evidence from the Chord 2Go's
	// own MPD-DLNA MediaServer (MiniDLNA, captured 2026-05-28 via
	// direct SOAP curl) shows ALL containers it emits carry
	// `searchable="1"`. mconnect Player drills into the 2Go's
	// containers fine; mconnect REFUSED to drill into our previous
	// `searchable="0"` emission. The pattern strongly suggests
	// mconnect (and likely other music-centric controllers) filters
	// out non-searchable containers from drill-down candidates —
	// treating them as "data-only sources, not interactive
	// browseables".
	//
	// We don't implement the Search action (returning UPnPErr-
	// InvalidAction if called). The trade-off: false-advertise
	// searchable=1, accepting a runtime error AT search-time over
	// a structural drill-down block at browse-time. Per spec we
	// SHOULD only advertise searchable=1 when we can answer
	// Search requests, but the practical mconnect requirement
	// pressures toward "advertise searchable=1, fail Search
	// gracefully if called". Controllers that DO use the Search
	// action are rare in audio (most use BrowseDirectChildren +
	// SortCriteria for filtering).
	//
	// Per the Gemini PR #310 consult, the `searchable` attribute
	// is REQUIRED on every container element per the UPnP CDS
	// schema regardless of its value. We emit it unconditionally;
	// the only question was the value (0 vs 1). The 2Go's
	// reference resolves that to "1".
	//
	// **Don't revert searchable to "0"** at any new site —
	// re-opens the mconnect-class-of-issue the 2Go reference
	// (2026-05-28) proves.
	sb.WriteString(fmt.Sprintf(`<container id="%s" parentID="%s" restricted="1" searchable="1" childCount="%d">`,
		escapeXMLText(opts.ID), escapeXMLText(opts.ParentID), childCount))
	sb.WriteString(`<dc:title>`)
	sb.WriteString(escapeXMLText(opts.Title))
	sb.WriteString(`</dc:title>`)
	sb.WriteString(`<upnp:class>`)
	sb.WriteString(upnpClass)
	sb.WriteString(`</upnp:class>`)
	// `<upnp:storageUsed>-1</upnp:storageUsed>` is REQUIRED for
	// `object.container.storageFolder` containers AND its subtypes
	// per the UPnP CDS spec. The value `-1` is the spec sentinel
	// for "unknown storage used" — the bridge doesn't track
	// per-container storage usage and shouldn't compute it on the
	// fly. The 2Go's own MPD-DLNA reference emits exactly this
	// shape (captured 2026-05-28). Strict controllers can refuse
	// storageFolder containers that omit the field.
	//
	// `strings.HasPrefix` (NOT exact equality) so any future
	// `object.container.storageFolder.*` subtype (e.g. a
	// hypothetical `storageFolder.movies` or vendor-specific
	// extension) also gets the required emission. No current
	// caller emits a subtype, but the prefix form makes the
	// helper forward-compatible with no downside. Per CodeRabbit
	// + Gemini on PR #314 round-1. Emit only for the storageFolder
	// hierarchy — other classes (musicAlbum, musicArtist,
	// playlistContainer) have their own mandatory-attribute
	// contracts that this helper doesn't touch.
	if strings.HasPrefix(upnpClass, "object.container.storageFolder") {
		sb.WriteString(`<upnp:storageUsed>-1</upnp:storageUsed>`)
	}
	if opts.ArtworkURL != "" {
		sb.WriteString(`<upnp:albumArtURI>`)
		sb.WriteString(escapeXMLText(opts.ArtworkURL))
		sb.WriteString(`</upnp:albumArtURI>`)
	}
	sb.WriteString(`</container>`)
	return sb.String()
}

// WrapDIDLLite wraps a sequence of `<item>` / `<container>` XML strings
// in the canonical DIDL-Lite envelope with the required namespaces.
// The result is the value that goes inside the `<Result>` element of a
// SOAP Browse response. NB: callers responsible for XML-escaping the
// final value when embedding in a SOAP envelope (DIDL-Lite is itself an
// XML payload, and SOAP requires it embedded as an escaped string).
func WrapDIDLLite(elements ...string) string {
	var sb strings.Builder
	// Pre-size to skip the builder's early geometric regrowths — a flat-list
	// Browse wraps hundreds of pre-built ~500-700B item elements at once. ~220B
	// base covers the DIDL-Lite namespace open + close tags. (Mirrors the
	// sb.Grow already in DIDLForTrack.)
	size := 220
	for _, e := range elements {
		size += len(e)
	}
	sb.Grow(size)
	sb.WriteString(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`)
	for _, e := range elements {
		sb.WriteString(e)
	}
	sb.WriteString(`</DIDL-Lite>`)
	return sb.String()
}

// -----------------------------------------------------------------------------
// Pure helpers
// -----------------------------------------------------------------------------

// formatDLNADuration converts a duration in seconds (e.g. 210.799) to the
// DLNA-canonical "H:MM:SS.mmm" format (e.g. "0:03:30.799"). DLNA spec
// uses this format consistently across `<res duration="...">` and SOAP
// transport-state position fields.
func formatDLNADuration(seconds float64) string {
	if seconds <= 0 {
		return "0:00:00.000"
	}
	totalMillis := int64(seconds * 1000)
	hours := totalMillis / (3600 * 1000)
	totalMillis -= hours * 3600 * 1000
	minutes := totalMillis / (60 * 1000)
	totalMillis -= minutes * 60 * 1000
	secs := totalMillis / 1000
	millis := totalMillis - secs*1000
	return fmt.Sprintf("%d:%02d:%02d.%03d", hours, minutes, secs, millis)
}

// escapeXMLText escapes the five XML-significant characters in a text
// node value (per the XML 1.0 spec). Reuses the project's existing
// minimal-escape pattern; using encoding/xml's `xml.EscapeText` would
// add an io.Writer round-trip for what is fundamentally a string
// transformation.
// xmlTextReplacer escapes the five XML metacharacters. Package-level and
// reused across calls: strings.Replacer is safe for concurrent use and builds
// its lookup trie once at init, so DIDLForTrack's ~9 escapeXMLText calls per
// track (× a 1000-item Browse window) no longer each allocate a fresh
// replacer + trie. (external review r3)
var xmlTextReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func escapeXMLText(s string) string {
	if s == "" {
		return ""
	}
	// Drop C0 control bytes XML 1.0 forbids (everything < 0x20 except tab/LF/CR)
	// — they can't appear even as escaped references, so a stray control char in
	// a tag (buggy taggers emit them) would otherwise make a strict renderer
	// reject the WHOLE Browse/Search response, not just the offending item.
	// strings.Map returns s unchanged (no alloc) for the clean common case.
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		return r
	}, s)
	// Fast-path for the common case (no metacharacters to escape).
	if !strings.ContainsAny(s, `&<>"'`) {
		return s
	}
	return xmlTextReplacer.Replace(s)
}

// ExtensionFromPath returns the lowercase file extension (with leading
// dot) for the given path. Trivial wrapper over filepath.Ext + strings.ToLower
// — exists as a named helper so adapter sites that pass `manifest.Track.Path`
// into DIDLTrackOpts construction don't all have to remember the lowercase
// detail.
func ExtensionFromPath(path string) string {
	return strings.ToLower(filepath.Ext(path))
}
