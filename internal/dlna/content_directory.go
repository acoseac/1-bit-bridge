package dlna

import (
	"encoding/xml"
	"fmt"
	"io"
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
func (t TrackInfo) toDIDLOpts(serverURL, userAgent string) DIDLTrackOpts {
	return DIDLTrackOpts{
		TrackID:         t.TrackID,
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
//   - "0"           → root: Music + All Tracks containers
//   - "music"       → category placeholders (Artists/Albums/Genres/Composers)
//   - "all_tracks"  → flat list of every track in the library
//   - Anything else → SOAPFault with UPnPErrNoSuchObject
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

	var didlElements []string
	var numberReturned, totalMatches int

	switch browse.ObjectID {
	case "", "0":
		// Root container — emit Music + All Tracks
		didlElements = []string{
			DIDLForContainer(DIDLContainerOpts{
				ID: "music", ParentID: "0", Title: "Music",
				ChildCount: 4, UPnPClass: "object.container",
			}),
			DIDLForContainer(DIDLContainerOpts{
				ID: "all_tracks", ParentID: "0", Title: "All Tracks",
				ChildCount: len(lib.ListTrackInfos()),
				UPnPClass:  "object.container",
			}),
		}
		numberReturned = len(didlElements)
		totalMatches = len(didlElements)

	case "music":
		// Category placeholders — Artists / Albums / Genres / Composers.
		// Deeper hierarchy under each is v1.x scope; for v1 these
		// containers exist but resolve to empty when browsed-into.
		didlElements = []string{
			DIDLForContainer(DIDLContainerOpts{ID: "music/artists", ParentID: "music", Title: "Artists", ChildCount: 0, UPnPClass: "object.container"}),
			DIDLForContainer(DIDLContainerOpts{ID: "music/albums", ParentID: "music", Title: "Albums", ChildCount: 0, UPnPClass: "object.container"}),
			DIDLForContainer(DIDLContainerOpts{ID: "music/genres", ParentID: "music", Title: "Genres", ChildCount: 0, UPnPClass: "object.container"}),
			DIDLForContainer(DIDLContainerOpts{ID: "music/composers", ParentID: "music", Title: "Composers", ChildCount: 0, UPnPClass: "object.container"}),
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
			didlElements = append(didlElements, DIDLForTrack(t.toDIDLOpts(serverURL, ua)))
		}
		numberReturned = len(slice)
		totalMatches = len(tracks)

	default:
		writeSOAPFault(w, UPnPErrNoSuchObject)
		return
	}

	// `BrowseMetadata` flag returns the metadata of the ObjectID
	// itself rather than its children. Spec-compliant but not
	// exercised by mConnect / Kazoo / 8player in Phase 0 — implement
	// the minimum (return the container's own metadata when known).
	// For now, treat as "no metadata" and let renderers fall back.
	if browse.BrowseFlag == "BrowseMetadata" {
		// Empty DIDL-Lite is a valid response per spec
		didlElements = nil
		numberReturned = 0
		totalMatches = 0
	}

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
