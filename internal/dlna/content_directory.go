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

	// RelativePath is the track's path relative to the library root,
	// using forward-slash separators. Populated by the bridge-side
	// adapter from `manifest.Track.Path` (which IS the relative
	// path); tests inject it directly. Surface (vs deriving from
	// `AbsolutePath` via LCP) is load-bearing for `BuildFolderIndex`
	// because LCP detection silently strips a top-level folder when
	// every track shares the same containing directory.
	//
	// Empty value MUST be tolerated — `BuildFolderIndex` falls back
	// to LCP-derived relative paths for callers that haven't been
	// updated yet (legacy test fixtures, future adapters that
	// haven't plumbed it through). The fallback is correct in the
	// common case (paths under distinct sub-folders) and degrades
	// gracefully to a flat hierarchy in the single-folder case.
	RelativePath string

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

// allTracksObjectID is the ObjectID for the single "All Tracks"
// container at the root level. Numeric string value (NOT human-
// readable like "all_tracks") to match the convention every other
// MediaServer in the wild uses — empirical evidence captured 2026-
// 05-28 against the Chord 2Go's own MPD-DLNA reference shows it
// uses numeric IDs ("1", "2", "3", "64") on all containers.
// mconnect Player (and similar music-centric controllers based on
// the Cling Java UPnP library) historically parse ObjectID as an
// integer internally; non-numeric strings like "all_tracks" caused
// silent drill-down rejection (mconnect issued root Browse(0)
// repeatedly but NEVER followed up with Browse(all_tracks, ...)
// regardless of class, searchable, or storageUsed shape).
// Per Gemini-rejected hypothesis batch + the 2Go reference shape
// from PR #314's investigation.
//
// **Don't reintroduce a non-numeric string ObjectID** at any new
// container site — re-opens the mconnect-class-of-issue PR #310
// → #313 → #314 → this PR all chased. The empirical fix is the
// numeric format.
const allTracksObjectID = "1"

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
		case "GetSearchCapabilities":
			handleGetSearchCapabilities(w)
		case "GetSortCapabilities":
			handleGetSortCapabilities(w)
		case "GetSystemUpdateID":
			handleGetSystemUpdateID(w)
		default:
			writeSOAPFault(w, UPnPErrInvalidAction)
		}
	}
}

// handleGetSearchCapabilities responds to the spec-mandatory
// CDS:1 GetSearchCapabilities action. Returns an empty SearchCaps
// string — declares that we don't support the Search action (CDS
// spec interprets empty SearchCaps as "Search is not supported";
// non-empty would advertise searchable fields like "dc:title").
//
// Required regardless of whether Search itself is implemented —
// see ContentDirectorySCPDXML docblock for the load-bearing
// rationale (mconnect silent-drill-abort).
func handleGetSearchCapabilities(w http.ResponseWriter) {
	body := SOAPResponseEnvelope(
		ContentDirectoryServiceType,
		"GetSearchCapabilities",
		`<SearchCaps></SearchCaps>`,
	)
	writeSOAPSuccess(w, body)
}

// handleGetSortCapabilities responds to the spec-mandatory
// CDS:1 GetSortCapabilities action. Returns an empty SortCaps
// string — declares that we don't support sortable fields (Browse
// honours SortCriteria="" only; a non-empty SortCaps would list
// sortable fields like "dc:title", "dc:date").
func handleGetSortCapabilities(w http.ResponseWriter) {
	body := SOAPResponseEnvelope(
		ContentDirectoryServiceType,
		"GetSortCapabilities",
		`<SortCaps></SortCaps>`,
	)
	writeSOAPSuccess(w, body)
}

// handleGetSystemUpdateID responds to the spec-mandatory CDS:1
// GetSystemUpdateID action. Returns a static "1" — controllers
// poll this between navigation steps to detect manifest changes;
// a stable "1" tells them "nothing has changed since last poll",
// which matches our wire contract (manifest updates flow through
// bridge sync + SSE, NOT through CDS UpdateID semantics — that
// would require eventing infrastructure we don't have).
//
// Returning a stable value is the correct behaviour for a
// best-effort CDS that doesn't maintain UpdateID state; the spec
// allows it explicitly. The downside is controllers can't detect
// our manifest changes via CDS poll alone — but the iOS client
// is the only consumer that cares about manifest freshness, and
// it has its own SSE path for that signal.
func handleGetSystemUpdateID(w http.ResponseWriter) {
	body := SOAPResponseEnvelope(
		ContentDirectoryServiceType,
		"GetSystemUpdateID",
		`<Id>1</Id>`,
	)
	writeSOAPSuccess(w, body)
}

// handleBrowse processes a SOAP Browse request. ObjectID dispatch:
//
//   - "0"           → root: emits the `all_tracks` storage-folder container
//   - `allTracksObjectID` ("1")  → flat list of every track in the library
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
		case allTracksObjectID:
			selfDIDL = DIDLForContainer(DIDLContainerOpts{
				ID: allTracksObjectID, ParentID: "0", Title: "All Tracks",
				ChildCount: len(lib.ListTrackInfos()),
				// `storageFolder` (reverting PR #313's playlist-
				// Container) — empirical evidence from the Chord
				// 2Go's own MPD-DLNA MediaServer (MiniDLNA-based)
				// captured 2026-05-28 shows storageFolder containers
				// ARE accepted by mconnect Player. The actual
				// blocker was `searchable="0"` (PR #310's choice);
				// the 2Go's reference emits `searchable="1"` on
				// every container + `<upnp:storageUsed>-1</upnp:
				// storageUsed>` for storageFolder spec compliance.
				// See the BrowseDirectChildren root branch below
				// for the full reference-vs-ours diff rationale.
				UPnPClass: "object.container.storageFolder",
			})
		case foldersRootObjectID:
			// "Folders" root container — surfaced alongside All
			// Tracks per PR #316's folder-hierarchy work. Lets users
			// navigate the on-disk folder structure (Artist → Album
			// → Track) rather than scrolling a flat list.
			folderIndex := BuildFolderIndex(lib.ListTrackInfos())
			selfDIDL = DIDLForContainer(DIDLContainerOpts{
				ID: foldersRootObjectID, ParentID: "0", Title: "Folders",
				ChildCount: len(folderIndex.TopLevelFolderIDs) + len(folderIndex.TopLevelTrackIDs),
				UPnPClass:  "object.container.storageFolder",
			})
		default:
			// Could be a hashed folder ObjectID. Build the folder
			// index and look it up before falling through to
			// NoSuchObject.
			folderIndex := BuildFolderIndex(lib.ListTrackInfos())
			if node, ok := folderIndex.Folders[browse.ObjectID]; ok {
				selfDIDL = DIDLForContainer(DIDLContainerOpts{
					ID: node.ObjectID, ParentID: node.ParentID, Title: node.Name,
					ChildCount: len(node.ChildFolderIDs) + len(node.ChildTrackIDs),
					UPnPClass:  "object.container.storageFolder",
				})
				break
			}
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

	// Build the folder index once per Browse call (cheap: O(N) over the
	// cached TrackInfo slice, ~5 ms for a 24k-track library on typical
	// host hardware). Used by the "Folders" root + hashed-folder cases
	// below. Pre-computed here rather than inside each switch arm so
	// the lookup is a free dictionary access in the hashed-folder arm.
	folderIndex := BuildFolderIndex(lib.ListTrackInfos())

	switch browse.ObjectID {
	case "", "0":
		// Root container — emit `all_tracks` AND `folders`.
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
				ID: allTracksObjectID, ParentID: "0", Title: "All Tracks",
				ChildCount: folderIndex.TrackCount(),
				// `storageFolder` — reverting PR #313's playlist-
				// Container change. PR #313 was driven by Gemini's
				// round-3 hypothesis that mconnect filters out
				// `object.container.storageFolder` from music-
				// browse UI. Real-device verification AFTER PR
				// #313 deployed showed mconnect STILL refused to
				// drill in — Gemini's class-blocker hypothesis was
				// wrong (twice across rounds 2 + 3).
				//
				// Empirical evidence from the Chord 2Go's own MPD-
				// DLNA MediaServer (MiniDLNA-based, captured 2026-
				// 05-28 via direct SOAP `curl` against the 2Go's
				// CDS at `http://<2go-ip>:8200/ctl/ContentDir`)
				// confirms that:
				//
				//   1. The 2Go itself emits `object.container.storage-
				//      Folder` containers (Browse Folders / Music /
				//      Pictures / Video). mconnect drills into them
				//      fine. So storageFolder IS accepted by
				//      mconnect — Gemini's class hypothesis was
				//      wrong.
				//
				//   2. The 2Go's containers emit `searchable="1"`
				//      (not "0"). mconnect almost certainly filters
				//      out non-searchable containers from drill-
				//      down candidates — THIS is the actual blocker
				//      our pre-fix shape hit.
				//
				//   3. The 2Go's storageFolder containers emit
				//      `<upnp:storageUsed>-1</upnp:storageUsed>`
				//      per the UPnP CDS spec's mandatory-attribute
				//      contract for storageFolder. We were missing
				//      this; some controllers reject storage-class
				//      containers without it.
				//
				// PR-pending corrective fix:
				//   - Revert class to storageFolder (matches 2Go
				//     reference)
				//   - Flip `searchable` from "0" to "1" via
				//     DIDLContainerOpts (separate field; default
				//     was "0" since PR #310)
				//   - Add `<upnp:storageUsed>-1</upnp:storage-
				//     Used>` emission inside DIDLForContainer when
				//     the class is a storageFolder subtype
				//
				// **Don't revert searchable to "0"** at any new
				// site — re-opens the mconnect class of issue the
				// 2Go reference proves. **Don't reintroduce
				// `playlistContainer`** here — it was a wrong-
				// hypothesis detour from Gemini round-3.
				UPnPClass: "object.container.storageFolder",
			}),
			// "Folders" sibling container — emitted alongside All
			// Tracks per PR #316's folder-hierarchy work. Lets users
			// navigate the on-disk folder structure rather than
			// scrolling a flat list. ChildCount = top-level folder
			// count + tracks-at-library-root count.
			DIDLForContainer(DIDLContainerOpts{
				ID: foldersRootObjectID, ParentID: "0", Title: "Folders",
				ChildCount: len(folderIndex.TopLevelFolderIDs) + len(folderIndex.TopLevelTrackIDs),
				UPnPClass:  "object.container.storageFolder",
			}),
		}
		numberReturned = len(didlElements)
		totalMatches = len(didlElements)

	case foldersRootObjectID:
		// "Folders" drill-down — emit top-level folder containers +
		// any tracks that sit at the library root without a folder
		// prefix. Both are immediate children of the synthetic Folders
		// root container.
		didlElements = browseFolderChildren(
			folderIndex,
			folderIndex.TopLevelFolderIDs,
			folderIndex.TopLevelTrackIDs,
			foldersRootObjectID,
			browse,
			serverURL,
			ua,
		)
		numberReturned = len(didlElements)
		totalMatches = len(folderIndex.TopLevelFolderIDs) + len(folderIndex.TopLevelTrackIDs)

	case allTracksObjectID:
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
			// Pass `allTracksObjectID` ("1") as the parentID so the
			// emitted `<item parentID="1">` reflects the actual
			// container the items live in (PR #309). Strict
			// controllers use the parentID to resolve the
			// container→item relationship; pre-PR the hardcoded
			// "0" caused mconnect Lite to refuse to render the
			// children even though Browse returned 121 items. The
			// PR-pending change to numeric ObjectID also flips
			// this parentID in lockstep so the parent/child IDs
			// remain consistent.
			didlElements = append(didlElements, DIDLForTrack(t.toDIDLOpts(serverURL, ua, allTracksObjectID)))
		}
		numberReturned = len(slice)
		totalMatches = len(tracks)

	default:
		// Could be a hashed folder ObjectID. Look it up in the
		// folder index built earlier; if found, emit its immediate
		// child folders + tracks.
		if node, ok := folderIndex.Folders[browse.ObjectID]; ok {
			didlElements = browseFolderChildren(
				folderIndex,
				node.ChildFolderIDs,
				node.ChildTrackIDs,
				node.ObjectID,
				browse,
				serverURL,
				ua,
			)
			numberReturned = len(didlElements)
			totalMatches = len(node.ChildFolderIDs) + len(node.ChildTrackIDs)
			break
		}
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

// browseFolderChildren emits DIDL for the immediate children of a
// folder-style container — sub-folder containers FIRST, then tracks
// (every reference MediaServer we've inspected uses this ordering;
// alphabetic mix would be acceptable too but would break browsing
// muscle memory for users coming from MiniDLNA / MinimServer).
//
// Pagination is applied via the same uint64-clamp shape the
// `all_tracks` case uses (defends against `RequestedCount = 0xFFFFFFFF`
// overflow on 32-bit builds). `parentID` is the ObjectID of the
// container these children are emitted UNDER — feeds the items' /
// containers' `<parentID>` attribute for strict-controller
// parent/child reconciliation.
func browseFolderChildren(
	folderIndex *FolderIndex,
	childFolderIDs []string,
	childTrackIDs []string,
	parentID string,
	browse browseAction,
	serverURL string,
	ua string,
) []string {
	total := len(childFolderIDs) + len(childTrackIDs)
	if total == 0 {
		return nil
	}
	// uint64 pagination math — see allTracksObjectID case for the
	// rationale.
	n := uint64(total)
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

	// Emit folder containers first, then tracks. Iterate the combined
	// index space directly so pagination cuts at the requested
	// boundary regardless of which side (folders / tracks) it lands
	// on.
	out := make([]string, 0, endIdx-startIdx)
	folderCount := len(childFolderIDs)
	for i := startIdx; i < endIdx; i++ {
		if i < folderCount {
			id := childFolderIDs[i]
			node, ok := folderIndex.Folders[id]
			if !ok {
				continue
			}
			out = append(out, DIDLForContainer(DIDLContainerOpts{
				ID: node.ObjectID, ParentID: parentID, Title: node.Name,
				ChildCount: len(node.ChildFolderIDs) + len(node.ChildTrackIDs),
				UPnPClass:  "object.container.storageFolder",
			}))
			continue
		}
		// Track entry.
		trackIdx := i - folderCount
		id := childTrackIDs[trackIdx]
		t, ok := folderIndex.LookupTrack(id)
		if !ok {
			continue
		}
		out = append(out, DIDLForTrack(t.toDIDLOpts(serverURL, ua, parentID)))
	}
	return out
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
