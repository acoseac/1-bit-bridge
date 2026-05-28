package dlna

import (
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// -----------------------------------------------------------------------------
// LibrarySource — the adapter interface ContentDirectory depends on
// -----------------------------------------------------------------------------

// LibrarySource is the data-source interface the ContentDirectory
// service depends on. The bridge's adapter (lands in
// cmd/bridge/main.go via PR 1 task #11) implements this against
// manifest.Store; tests can pass a `staticLibrary` literal for
// deterministic synthetic data.
//
// The interface is intentionally small. Adding new methods is a
// breaking change for adapter implementations (bridge-side adapter
// + any test stubs); think twice before extending.
//
// **No pagination today** — `ListTrackInfos` returns the entire
// library in one slice. At the scale of our reference operator
// libraries (50k tracks ≈ 5 MB of TrackInfo in memory) this is fine.
// A v1.x follow-up will add (StartingIndex, RequestedCount) pagination
// once the SOAP Browse handler accepts those arguments end-to-end
// AND we have field reports of large-library responsiveness pain.
type LibrarySource interface {
	// ListTrackInfos returns every track in the library, in stable order.
	// Stable ordering (e.g., by AbsolutePath) is load-bearing for
	// pagination correctness once it lands — see comment above.
	ListTrackInfos() []TrackInfo

	// GetTrackInfo returns the track with the given TrackID, or
	// (zero-value, false) if not found. Used by the file handler to
	// resolve `/dlna/file/{trackID}` URLs back to absolute paths.
	GetTrackInfo(trackID string) (TrackInfo, bool)
}

// TrackInfo is the bridge's adapter-layer Track shape. Mirrors the
// fields of `manifest.Track` that the DLNA layer needs, minus the
// bridge-API-specific fields (Variants, ArtworkMBID, ReplayGain) and
// PLUS:
//   - `TrackID` — the hash from `TrackID(libraryRoot, relativePath)`
//   - `AbsolutePath` — the on-disk path the file handler serves
//
// Pointer-typed `manifest.Track` optionals (Year, BitsPerSample,
// SampleRate, etc.) are flattened to value types here; the
// bridge-side adapter (PR 1 task #11) handles nil → 0 translation
// before populating this struct.
type TrackInfo struct {
	TrackID      string
	AbsolutePath string

	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Composer    string
	Genre       string
	Codec       string

	// FileExtension is the lowercase extension with leading dot,
	// e.g. ".dsf". Used both for MIME resolution and for the
	// DIDL-Lite <res> protocolInfo attribute.
	FileExtension string

	Size            int64
	DurationSeconds float64
	SampleRateHz    int
	BitsPerSample   int
	Channels        int
	IsDSD           bool
	Year            int
	TrackNumber     int

	// ArtworkURL is the pre-resolved absolute URL (e.g.,
	// "http://192.168.0.14:7790/v1/artwork/{mbid}") that DIDL-Lite
	// embeds as `<upnp:albumArtURI>`. Empty = no artwork. The
	// bridge-side adapter constructs this from manifest.Track's
	// ArtworkMBID + the request's server URL.
	ArtworkURL string
}

// toDIDLOpts converts a TrackInfo + per-request fields into the
// DIDLTrackOpts shape that `DIDLForTrack` expects. Kept private
// because it's a one-line struct copy plus the server URL / UA
// fields — callers shouldn't need to do this themselves.
func (t TrackInfo) toDIDLOpts(serverURL, userAgent, parentID string) DIDLTrackOpts {
	return DIDLTrackOpts{
		TrackID:         t.TrackID,
		ParentID:        parentID,
		Title:           t.Title,
		Artist:          t.Artist,
		AlbumArtist:     t.AlbumArtist,
		Album:           t.Album,
		Composer:        t.Composer,
		Genre:           t.Genre,
		Year:            t.Year,
		TrackNumber:     t.TrackNumber,
		Size:            t.Size,
		DurationSeconds: t.DurationSeconds,
		SampleRateHz:    t.SampleRateHz,
		BitsPerSample:   t.BitsPerSample,
		Channels:        t.Channels,
		IsDSD:           t.IsDSD,
		Codec:           t.Codec,
		FileExtension:   t.FileExtension,
		ArtworkURL:      t.ArtworkURL,
		ServerURL:       serverURL,
		UserAgent:       userAgent,
	}
}

// -----------------------------------------------------------------------------
// ContentDirectory SOAP handler
// -----------------------------------------------------------------------------

// ContentDirectoryServiceType is the canonical UPnP service type string
// for the ContentDirectory:1 service. Surfaced as a constant so the
// handler + the device description + the SCPD XML all reference the
// same value.
const ContentDirectoryServiceType = "urn:schemas-upnp-org:service:ContentDirectory:1"

// browseEnvelope is the input-side XML shape for a SOAP Browse request.
// The XML walks Envelope → Body → Browse → {ObjectID, BrowseFlag, …}.
// `xml:"Browse"` matches regardless of namespace prefix (Go's
// encoding/xml is permissive about namespaces by default).
type browseEnvelope struct {
	XMLName xml.Name   `xml:"Envelope"`
	Body    browseBody `xml:"Body"`
}

type browseBody struct {
	Browse browseAction `xml:"Browse"`
}

type browseAction struct {
	ObjectID       string `xml:"ObjectID"`
	BrowseFlag     string `xml:"BrowseFlag"`
	Filter         string `xml:"Filter"`
	StartingIndex  uint32 `xml:"StartingIndex"`
	RequestedCount uint32 `xml:"RequestedCount"`
	SortCriteria   string `xml:"SortCriteria"`
}

// ContentDirectoryHandler returns an http.HandlerFunc that dispatches
// incoming SOAP requests against the ContentDirectory:1 service. The
// handler:
//
//  1. Validates the request method (must be POST)
//  2. Parses the SOAPAction header to determine the action name
//  3. Dispatches: Browse → handleBrowse; anything else → InvalidAction
//  4. Browse: parses the XML body, dispatches on ObjectID, builds
//     the DIDL-Lite response, wraps in a SOAP envelope, writes
//     200 OK with text/xml.
//
// On parse errors or unsupported actions, returns a SOAPFault envelope
// with the appropriate UPnP error code. HTTP status is always 200 OK
// for successful action dispatch, 500 for SOAPFault (per SOAP 1.1 spec).
//
// `serverURLFunc` is called per-request to determine the absolute server
// URL the renderer should use for file fetches (typically
// `"http://" + r.Host` since the DLNA listener is HTTP-only on LAN).
// Injected as a function rather than a static string so the handler
// adapts to multi-interface deployments (the renderer might dial us
// via a Tailscale IP on one request and the LAN IP on another).
func ContentDirectoryHandler(lib LibrarySource, serverURLFunc func(r *http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}

		_, actionName := ParseSOAPAction(r.Header.Get("SOAPAction"))
		switch actionName {
		case "Browse":
			handleBrowse(w, r, lib, serverURLFunc(r))
		default:
			writeSOAPFault(w, UPnPErrInvalidAction)
		}
	}
}

// handleBrowse processes a SOAP Browse request. ObjectID dispatch:
//
//   - "0"           → root: emits the `all_tracks` storage-folder container
//   - "all_tracks"  → flat list of every track in the library
//   - Anything else → SOAPFault with UPnPErrNoSuchObject
//
// `BrowseFlag == "BrowseMetadata"` returns a single DIDL element
// describing the requested ObjectID itself (load-bearing for strict
// controller drill-down handshakes — see the dedicated branch
// below); `BrowseDirectChildren` returns the container's items.
//
// The dropped `music` ObjectID (PR #309) routes through the `default`
// arm and surfaces as a NoSuchObject SOAP fault — the cleanest signal
// to controllers that cached pre-PR stub IDs that the hierarchy is
// no longer available.
//
// Deeper hierarchy paths (music/artists/{hash}, music/albums/{hash}, etc.)
// are deferred to a v1.x follow-up — the "All Tracks" flat path is what
// Phase 0 confirmed real renderers (Chord 2go via mConnect Lite) walk
// by default, and serves as the minimum viable browse surface for v1.
// maxSOAPBodyBytes caps the SOAP Browse / GetProtocolInfo / Search
// envelope size we'll read into memory. 1 MB is two orders of
// magnitude above any well-formed UPnP SOAP request (typical Browse
// envelope is ~500 bytes); a body larger than that is either a
// renderer bug or a DoS attempt. Without the cap, `io.ReadAll`
// would OOM on a hostile payload.
const maxSOAPBodyBytes = 1 << 20 // 1 MB

func handleBrowse(w http.ResponseWriter, r *http.Request, lib LibrarySource, serverURL string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSOAPBodyBytes))
	if err != nil {
		writeSOAPFault(w, UPnPErrActionFailed)
		return
	}
	var env browseEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		writeSOAPFault(w, UPnPErrInvalidArgs)
		return
	}
	browse := env.Body.Browse
	ua := r.Header.Get("User-Agent")
	// Diagnostic logging for strict-controller investigations
	// (mPlayer Lite "empty All Tracks list" symptom, 2026-05-28).
	// Captures every Browse dispatch with enough detail to
	// reconstruct what each controller asked for + what we
	// returned. INFO level so it lands in `serve.log` by default
	// without needing a debug-mode toggle. Logged at TWO sites:
	// here (request) AND in the response-write path below
	// (NumberReturned / TotalMatches). UA is truncated to 100
	// chars defensively against pathological clients sending
	// unbounded UA strings. Per Gemini consult 2026-05-28.
	logBrowseRequest(r.RemoteAddr, browse, ua)

	var didlElements []string
	var numberReturned, totalMatches int

	// `BrowseMetadata` short-circuit. Per UPnP CDS spec, this flag
	// returns a SINGLE DIDL-Lite element describing the requested
	// ObjectID itself — NOT its children. Strict UPnP controllers
	// (mconnect Lite confirmed 2026-05-28) execute a sequential
	// validation handshake before rendering a container: call
	// BrowseMetadata to resolve parent mapping, permissions, and
	// the UI header title; THEN fire BrowseDirectChildren for
	// items. Empty DIDL here causes their XML parser to crash or
	// stall waiting for missing structural elements — surfacing
	// as an infinite "loading" spinner on the controller's UI
	// even though BrowseDirectChildren on the same ObjectID
	// returns items correctly. Per Gemini consult 2026-05-28.
	//
	// **Handled BEFORE the BrowseDirectChildren switch** so the
	// `all_tracks` case below doesn't pay the cost of building a
	// per-track DIDL slice only to have it discarded. Pre-fix the
	// BrowseDirectChildren path always ran (O(n) XML allocations
	// for n=121-track libraries; pure waste under metadata
	// handshakes that happen on every drill-down). Per Gemini
	// MAJOR + CodeRabbit MAJOR (outside-diff) on PR #310 round-1.
	if browse.BrowseFlag == "BrowseMetadata" {
		var selfDIDL string
		switch browse.ObjectID {
		case "", "0":
			selfDIDL = DIDLForContainer(DIDLContainerOpts{
				ID: "0", ParentID: "-1", Title: "1-bit Bridge",
				ChildCount: 1, // one child: all_tracks
				UPnPClass:  "object.container",
			})
		case "all_tracks":
			selfDIDL = DIDLForContainer(DIDLContainerOpts{
				ID: "all_tracks", ParentID: "0", Title: "All Tracks",
				ChildCount: len(lib.ListTrackInfos()),
				// `playlistContainer` matches the BrowseDirect-
				// Children root-emission class. The two MUST stay
				// in lockstep — a strict controller doing both
				// BrowseMetadata + BrowseDirectChildren on the
				// same ObjectID would otherwise see two different
				// classes for the same container. See the
				// BrowseDirectChildren root branch above for the
				// per-Gemini-round-3 class-selection rationale.
				UPnPClass: "object.container.playlistContainer",
			})
		default:
			// Unknown ObjectID under BrowseMetadata — same `NoSuchObject`
			// signal the BrowseDirectChildren `default` arm produces.
			// Strict controllers + lenient ones agree on this code.
			writeSOAPFault(w, UPnPErrNoSuchObject)
			return
		}
		didlElements = []string{selfDIDL}
		numberReturned = 1
		totalMatches = 1
		didlLite := WrapDIDLLite(didlElements...)
		innerXML := fmt.Sprintf(
			`<Result>%s</Result><NumberReturned>%d</NumberReturned><TotalMatches>%d</TotalMatches><UpdateID>1</UpdateID>`,
			escapeXMLText(didlLite), numberReturned, totalMatches,
		)
		body2 := SOAPResponseEnvelope(ContentDirectoryServiceType, "Browse", innerXML)
		w.Header().Set("Content-Type", SOAPContentType)
		w.Header().Set(SOAPResponseHeader, "")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body2)
		return
	}

	switch browse.ObjectID {
	case "", "0":
		// Root container — emit `all_tracks` only.
		//
		// **Music container removed** (PR #309): the previous root
		// also exposed `Music` (`childCount=4`) but its `music`
		// Browse case returned 4 empty placeholder sub-containers
		// (Artists / Albums / Genres / Composers) with
		// `childCount=0` — the Music hierarchy was never actually
		// implemented (deferred as "v1.x scope" per the prior
		// inline comment). Strict third-party DLNA controllers
		// (mconnect Lite observed 2026-05-28) refused to render
		// the empty placeholder hierarchy AND occasionally bailed
		// to a "Browse failed" state that prevented navigation
		// to `all_tracks` too. Surfacing only the populated
		// `all_tracks` container keeps the CDS minimal-but-correct
		// for strict controllers; the Music hierarchy returns
		// once the by-Artist / by-Album / by-Genre indexes are
		// real. Removing the `music` Browse case below routes any
		// late-arriving cached request (controller cached the
		// stub IDs) to the `default` `NoSuchObject` SOAP fault,
		// which is the cleanest "this container is no longer
		// available" signal.
		didlElements = []string{
			DIDLForContainer(DIDLContainerOpts{
				ID: "all_tracks", ParentID: "0", Title: "All Tracks",
				ChildCount: len(lib.ListTrackInfos()),
				// `playlistContainer` (NOT `object.container.storageFolder`
				// despite Gemini's PR #310 recommendation). PR #310's
				// storageFolder choice was driven by "strict controllers
				// need explicit directory subtype to drill in"; empirical
				// real-device verification via the PR #312 Browse-log
				// diagnostics (2026-05-28) showed mconnect Player
				// receives the storageFolder container at the root then
				// REFUSES to drill in — its internal music-vs-filesystem
				// classifier treats storageFolder as "filesystem,
				// hide from music UI". The downstream
				// `Browse(all_tracks, BrowseDirectChildren)` never
				// fires; the user sees an empty All Tracks tap.
				//
				// `playlistContainer` is the semantically-accurate +
				// cross-controller compatible subtype: music-centric
				// controllers (mconnect, BluOS, Audirvana) recognize
				// it as "appendable queue of audio items" and surface
				// the drill-down affordance; strict structural
				// controllers (Linn Kazoo, older Naim/Cyrus / OpenHome
				// stacks) treat it as a first-class UPnP AV citizen
				// for queue orchestration. Per Gemini consult round-3
				// 2026-05-28.
				//
				// **Don't revert to `storageFolder`** — re-opens the
				// mconnect empty-list class of issue. **Don't revert
				// to generic `object.container`** either — pre-#310
				// strict controllers had drill-in issues against the
				// untyped container; playlistContainer satisfies both
				// camps.
				UPnPClass: "object.container.playlistContainer",
			}),
		}
		numberReturned = len(didlElements)
		totalMatches = len(didlElements)

	case "all_tracks":
		tracks := lib.ListTrackInfos()
		// Apply pagination per the SOAP Browse arguments. A
		// RequestedCount of 0 per UPnP convention means "return as
		// many as you can"; we cap at len(tracks) - StartingIndex.
		//
		// Bound arithmetic happens in uint64 to defend against the
		// `int(uint32)` overflow that would surface on 32-bit builds:
		// a renderer that sends `RequestedCount = 0xFFFFFFFF` would
		// have `startIdx + int(browse.RequestedCount)` produce a
		// negative endIdx and `tracks[startIdx:endIdx]` would panic.
		// Clamp in uint64, then cast at the end where the result is
		// guaranteed in [0, len(tracks)]. Per CodeRabbit Major on
		// PR #303.
		n := uint64(len(tracks))
		startU := uint64(browse.StartingIndex)
		if startU > n {
			startU = n
		}
		endU := n
		if browse.RequestedCount > 0 {
			reqU := uint64(browse.RequestedCount)
			if reqU > n-startU {
				reqU = n - startU
			}
			endU = startU + reqU
		}
		startIdx := int(startU)
		endIdx := int(endU)
		slice := tracks[startIdx:endIdx]
		didlElements = make([]string, 0, len(slice))
		for _, t := range slice {
			// Pass "all_tracks" as the parentID so the emitted
			// `<item parentID="all_tracks">` reflects the actual
			// container the items live in (PR #309). Strict
			// controllers use the parentID to resolve the
			// container→item relationship; pre-PR the hardcoded
			// "0" caused mconnect Lite to refuse to render the
			// children even though Browse(all_tracks) returned
			// 121 items.
			didlElements = append(didlElements, DIDLForTrack(t.toDIDLOpts(serverURL, ua, "all_tracks")))
		}
		numberReturned = len(slice)
		totalMatches = len(tracks)

	default:
		writeSOAPFault(w, UPnPErrNoSuchObject)
		return
	}

	// (BrowseMetadata short-circuited above the BrowseDirectChildren
	// switch — see the dedicated branch near the top of this
	// function. Per Gemini MAJOR + CodeRabbit MAJOR on PR #310
	// round-1, handling BrowseMetadata BEFORE the switch avoids
	// building the per-track DIDL slice on every drill-down
	// handshake.)

	didlLite := WrapDIDLLite(didlElements...)
	innerXML := fmt.Sprintf(
		`<Result>%s</Result><NumberReturned>%d</NumberReturned><TotalMatches>%d</TotalMatches><UpdateID>1</UpdateID>`,
		escapeXMLText(didlLite), numberReturned, totalMatches,
	)
	body2 := SOAPResponseEnvelope(ContentDirectoryServiceType, "Browse", innerXML)

	// Diagnostic logging — response side. Logged alongside the
	// request-side entry above so an operator scanning serve.log
	// for a specific controller's behaviour can correlate
	// (ObjectID, BrowseFlag) → (NumberReturned, TotalMatches,
	// didlBytes) within ~adjacent lines. Per Gemini consult
	// 2026-05-28.
	logBrowseResponse(browse, numberReturned, totalMatches, len(body2))

	w.Header().Set("Content-Type", SOAPContentType)
	w.Header().Set(SOAPResponseHeader, "")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body2)
}

// logBrowseRequest emits an INFO-level structured log line at every
// `Browse` SOAP dispatch. Surfaces the action shape (ObjectID,
// BrowseFlag, pagination window, controller UA) so post-hoc analysis
// of `serve.log` can reconstruct what each controller asked for —
// load-bearing for diagnosing strict-controller behaviour (mPlayer
// Lite, Linn Kazoo) where the rendering UI silently drops items the
// CDS returns. Logged via the file-scope `packageLogger` (defined in
// server.go); INFO level so it lands in `serve.log` by default
// without requiring a debug-mode toggle.
//
// UA truncated at 100 runes defensively — pathological clients
// sending unbounded UA strings would otherwise produce mile-long log
// lines. 100 runes covers every real-world UA we've observed
// (BubbleUPnP / 1-bit / Linn Kazoo / mPlayer Lite / Music Player
// Daemon X.Y.Z all fit comfortably).
//
// Truncate by rune count (NOT bytes) so a multi-byte UTF-8 codepoint
// in a hypothetical exotic UA string can't be cut mid-character,
// which would corrupt the log line + produce invalid UTF-8 in
// downstream JSON parsers consuming the slog handler's output. Per
// CodeRabbit + Gemini on PR #312 round-1.
func logBrowseRequest(remoteAddr string, b browseAction, ua string) {
	uaTrim := ua
	if runes := []rune(uaTrim); len(runes) > 100 {
		uaTrim = string(runes[:100])
	}
	packageLogger.Info("Browse request",
		slog.String("remoteAddr", remoteAddr),
		slog.String("objectID", b.ObjectID),
		slog.String("browseFlag", b.BrowseFlag),
		slog.String("filter", b.Filter),
		slog.Uint64("startingIndex", uint64(b.StartingIndex)),
		slog.Uint64("requestedCount", uint64(b.RequestedCount)),
		slog.String("sortCriteria", b.SortCriteria),
		slog.String("userAgent", uaTrim),
	)
}

// logBrowseResponse emits the response-side companion to
// `logBrowseRequest`. Captures NumberReturned + TotalMatches (the
// fields strict controllers consult for pagination) + the actual
// response body size (so we can detect cases where mPlayer Lite
// expects a chunked / size-capped response and our 100 KB+
// envelope exceeds its parser threshold). Per Gemini consult.
func logBrowseResponse(b browseAction, numberReturned, totalMatches, responseBytes int) {
	packageLogger.Info("Browse response",
		slog.String("objectID", b.ObjectID),
		slog.String("browseFlag", b.BrowseFlag),
		slog.Int("numberReturned", numberReturned),
		slog.Int("totalMatches", totalMatches),
		slog.Int("responseBytes", responseBytes),
	)
}

// writeSOAPFault writes a SOAPFault response with the given UPnP error
// code and HTTP status 500 (per SOAP 1.1 spec for fault responses).
func writeSOAPFault(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", SOAPContentType)
	w.Header().Set(SOAPResponseHeader, "")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(SOAPFaultEnvelope(code))
}

// -----------------------------------------------------------------------------
// Test affordance — minimal in-memory LibrarySource impl
// -----------------------------------------------------------------------------

// StaticLibrary is an in-memory LibrarySource for tests + future use cases
// where the library is short-lived (e.g., a Phase 0 spike server). Not
// the production bridge adapter — that lives in cmd/bridge/main.go and
// queries manifest.Store directly.
//
// Exported (not test-only) so other internal/dlna tests and the eventual
// spike-script integration can use it without re-defining a synthetic
// LibrarySource per call site.
type StaticLibrary struct {
	Tracks []TrackInfo
}

// ListTrackInfos returns a stable-order slice of all tracks. Returns
// a defensive copy so callers can't mutate the internal slice.
func (s *StaticLibrary) ListTrackInfos() []TrackInfo {
	out := make([]TrackInfo, len(s.Tracks))
	copy(out, s.Tracks)
	return out
}

// GetTrackInfo returns the track with the given TrackID, or
// (zero-value, false) if not found. O(n) scan — acceptable at the
// scale this stub is used (test fixtures + spike scripts).
func (s *StaticLibrary) GetTrackInfo(trackID string) (TrackInfo, bool) {
	for _, t := range s.Tracks {
		if t.TrackID == trackID {
			return t, true
		}
	}
	return TrackInfo{}, false
}

// staticServerURL is a default `serverURLFunc` for tests / spike use.
// Production code passes a closure that reads `"http://" + r.Host`.
func staticServerURL(url string) func(r *http.Request) string {
	return func(r *http.Request) string {
		if url != "" {
			return url
		}
		return "http://" + r.Host
	}
}

// Quiet "imported but unused" if a future test removes the only call
// site for staticServerURL or the `strings` import here.
var _ = strings.Builder{}
