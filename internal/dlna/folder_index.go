package dlna

import (
	"crypto/sha256"
	"encoding/binary"
	"path"
	"sort"
	"strconv"
	"strings"
)

// foldersRootObjectID is the reserved ObjectID for the "Folders"
// root container surfaced alongside "All Tracks". Numeric per the
// PR #315 invariant — mconnect / Cling-based controllers historically
// parse ObjectID as an integer internally; non-numeric IDs at any
// drill-down level can cause silent rejection.
const foldersRootObjectID = "2"

// folderIDReservedFloor is the lowest numeric ObjectID a hashed
// folder can produce. Reserves [0..9] for our own static IDs:
//
//	"0" — root
//	"1" — all_tracks
//	"2" — folders root
//	"3"-"9" — reserved for future static containers
//
// Hashed folder IDs land in [10, 2^64-1]. Collision probability with
// the reserved range is ~10/2^64 per hash — effectively zero — but the
// defensive guard costs nothing and surfaces intent. If a hash lands
// in [0..9], we bump it by `folderIDReservedFloor` so the result is
// stable AND outside the reserved range.
const folderIDReservedFloor = 1000

// FolderNode represents a single folder in the library hierarchy.
// One node per unique parent path encountered when walking every
// track's relative path. Built by `BuildFolderIndex`.
type FolderNode struct {
	// ObjectID is the stable numeric string ID surfaced in DIDL
	// `<container id>` + Browse responses. Derived from `RelPath`
	// via `FolderObjectID` so the same path always produces the
	// same ID across bridge restarts.
	ObjectID string

	// ParentID is the ObjectID of the parent container. For top-
	// level folders (no path separator), parent is `foldersRootObjectID`
	// ("2"). For nested folders, parent is the ObjectID derived from
	// the parent's RelPath.
	ParentID string

	// RelPath is the folder's relative path from the library root,
	// using forward-slash separators. Top-level folders have a single
	// component (e.g. "Artist A"); nested folders use multi-component
	// paths (e.g. "Artist A/Album X"). NEVER carries a leading slash
	// (matches the bridge's manifest convention).
	RelPath string

	// Name is the last path component — what controllers display in
	// the folder list (e.g. "Album X" rather than the full path).
	Name string

	// ChildFolderIDs is the set of immediate sub-folder ObjectIDs,
	// in sorted-by-Name order. Used by `Browse(folderID,
	// BrowseDirectChildren)` to emit child container DIDL.
	ChildFolderIDs []string

	// ChildTrackIDs is the set of immediate child track TrackIDs
	// (NOT folder ObjectIDs — these are the hashed TrackInfo.TrackID
	// values), in sorted-by-path order. Used by Browse to emit child
	// item DIDL.
	ChildTrackIDs []string
}

// FolderIndex is the lookup structure built once per Browse from the
// flat `[]TrackInfo` list. Carries everything the Browse handler
// needs to dispatch folder / sub-folder / item requests.
//
// Build cost: O(N × D) where N = track count, D = average folder
// depth. For a 24k-track library with D≈3 that's ~72k iterations
// per Browse — measured at <5 ms on the bridge's typical host hardware.
// If profiling shows it as a hot path later, the adapter can cache
// the FolderIndex alongside the TrackInfo list with the same TTL.
type FolderIndex struct {
	// Folders is the canonical lookup map: ObjectID → FolderNode.
	// Includes both top-level and nested folders.
	Folders map[string]FolderNode

	// TopLevelFolderIDs is the set of immediate children of the
	// `foldersRootObjectID` container, in sorted-by-Name order. The
	// "Folders" root container's `BrowseDirectChildren` response
	// iterates this slice.
	TopLevelFolderIDs []string

	// TopLevelTrackIDs is the set of TrackIDs for tracks that sit
	// AT the library root (no folder prefix in their RelPath).
	// Surfaced as items at the same level as TopLevelFolderIDs
	// under `foldersRootObjectID`.
	TopLevelTrackIDs []string

	// trackByID gives `Browse(folderID, BrowseDirectChildren)` an
	// O(1) lookup from the ChildTrackIDs slice back to TrackInfo
	// for DIDL emission.
	trackByID map[string]TrackInfo
}

// LookupTrack returns the TrackInfo for the given TrackID if present
// in this index. Used by Browse to convert a folder's ChildTrackIDs
// into emittable DIDL items.
func (fi *FolderIndex) LookupTrack(trackID string) (TrackInfo, bool) {
	if fi == nil || fi.trackByID == nil {
		return TrackInfo{}, false
	}
	t, ok := fi.trackByID[trackID]
	return t, ok
}

// TrackCount returns the total number of tracks indexed. Used by the
// All Tracks container's `childCount` attribute — equivalent to
// `len(lib.ListTrackInfos())` but reuses the already-built index.
func (fi *FolderIndex) TrackCount() int {
	if fi == nil {
		return 0
	}
	return len(fi.trackByID)
}

// FolderObjectID returns the stable numeric ObjectID for a folder's
// relative path. SHA-256 truncated to 64 bits, reinterpreted as a
// big-endian uint64, then formatted as a decimal string. Pure numeric
// per the PR #315 invariant.
//
// Empty relPath returns the reserved `foldersRootObjectID` ("2") so a
// caller that accidentally hashes the root never produces a
// collision-prone "0".
//
// Collision probability with the reserved IDs [0..9]: ~10/2^64. The
// defensive bump (`+ folderIDReservedFloor` when the hash lands low)
// costs one comparison per call and removes the corner case entirely.
func FolderObjectID(relPath string) string {
	if relPath == "" {
		return foldersRootObjectID
	}
	h := sha256.Sum256([]byte(relPath))
	n := binary.BigEndian.Uint64(h[:8])
	if n < folderIDReservedFloor {
		n += folderIDReservedFloor
	}
	return strconv.FormatUint(n, 10)
}

// BuildFolderIndex walks every track in `tracks`, derives the folder
// hierarchy from each `TrackInfo.AbsolutePath` (or, when absent, from
// the relative path the bridge stored on the manifest — surfaced via
// the helper `relPathForTrack`), and returns a fully-populated
// FolderIndex.
//
// **Source-of-truth for "relative path"**: the bridge's `TrackInfo`
// doesn't carry a clean relative-path field today, so we derive it
// from `AbsolutePath` by taking the filename plus its enclosing
// directory components AFTER the deepest known library-root prefix
// matches. The current `LibrarySource` doesn't surface library roots
// to the DLNA layer either (intentionally — the adapter encapsulates
// that detail). To avoid plumbing libraryRoots through the interface
// for this PR, we use the simpler shape: take `AbsolutePath`'s
// directory portion AND base it on the longest shared prefix between
// every track's AbsolutePath. That gives a stable hierarchy that
// matches the on-disk layout WITHOUT needing a library-root
// configuration plumb-through.
//
// Empty `tracks` → empty FolderIndex (handled gracefully by all
// callers; the "Folders" container will surface `childCount=0`).
//
// **Stable ordering**: TopLevelFolderIDs / each folder's
// ChildFolderIDs / ChildTrackIDs are all sorted at build time so
// successive Browse calls produce identical orderings (load-bearing
// for pagination correctness and for controllers' cached-result
// reconciliation).
func BuildFolderIndex(tracks []TrackInfo) *FolderIndex {
	fi := &FolderIndex{
		Folders:   make(map[string]FolderNode),
		trackByID: make(map[string]TrackInfo, len(tracks)),
	}

	if len(tracks) == 0 {
		return fi
	}

	// Determine the per-track relative path. Preferred source:
	// `TrackInfo.RelativePath` (populated by the production adapter
	// from `manifest.Track.Path`). Fallback: longest-common-prefix
	// stripping of `TrackInfo.AbsolutePath` — works in the common
	// case (paths under distinct sub-folders) but degrades to a flat
	// hierarchy when every track shares a single parent. The RelPath
	// path produces correct results in both shapes; the LCP fallback
	// is kept for legacy test fixtures that haven't migrated yet.
	useRelPath := allRelativePathsPopulated(tracks)
	var libRoot string
	if !useRelPath {
		libRoot = longestCommonPathPrefix(tracksToAbsPaths(tracks))
	}

	// Collect tracks grouped by their containing folder's relative
	// path. Top-level tracks (no folder component) accumulate under
	// the empty key "".
	tracksByFolder := make(map[string][]TrackInfo, len(tracks))
	for _, t := range tracks {
		fi.trackByID[t.TrackID] = t
		var relParent string
		if useRelPath {
			relParent = relParentDirFromRelPath(t.RelativePath)
		} else {
			relParent = relParentDir(t.AbsolutePath, libRoot)
		}
		tracksByFolder[relParent] = append(tracksByFolder[relParent], t)
	}

	// Walk every unique folder relative path and instantiate the
	// FolderNode, including all ancestors up to (but not including)
	// the root. Even folders that contain only tracks (no sub-folders)
	// need a FolderNode so Browse(folderID) can dispatch correctly.
	//
	// The loop bound covers two sentinels: `""` means we've walked
	// past the synthetic root, `"."` is `path.Dir`'s return for any
	// single-component input (e.g. `path.Dir("Artist A") == "."`).
	// Both must terminate the walk WITHOUT being registered as a
	// folder — otherwise "." leaks into the index and surfaces as a
	// phantom top-level entry.
	seenFolders := make(map[string]bool, len(tracksByFolder))
	for folderRelPath := range tracksByFolder {
		if folderRelPath == "" {
			// Top-level tracks — handled below via TopLevelTrackIDs.
			continue
		}
		for cur := folderRelPath; cur != "" && cur != "."; cur = path.Dir(cur) {
			if seenFolders[cur] {
				break
			}
			seenFolders[cur] = true
		}
	}

	// Materialize every registered folder into a FolderNode with
	// parent + name resolved. Children are filled in a second pass
	// below. `FolderObjectID` (SHA-256) is computed ONCE per node and
	// reused — pre-fix it was called twice (key + struct field).
	// Per Gemini Medium on PR #317.
	for folderRelPath := range seenFolders {
		objID := FolderObjectID(folderRelPath)
		fi.Folders[objID] = FolderNode{
			ObjectID: objID,
			ParentID: parentObjectID(folderRelPath),
			RelPath:  folderRelPath,
			Name:     path.Base(folderRelPath),
		}
	}

	// Wire up ChildTrackIDs: every track in `tracksByFolder[relParent]`
	// becomes a child of the FolderNode at that relParent (or, when
	// relParent == "", of the synthetic "Folders" root container —
	// which is captured separately as TopLevelTrackIDs).
	for folderRelPath, trackList := range tracksByFolder {
		// Stable sort by path so successive Browse calls produce
		// identical orderings.
		sort.Slice(trackList, func(i, j int) bool {
			return trackList[i].AbsolutePath < trackList[j].AbsolutePath
		})
		ids := make([]string, 0, len(trackList))
		for _, t := range trackList {
			ids = append(ids, t.TrackID)
		}
		if folderRelPath == "" {
			fi.TopLevelTrackIDs = ids
			continue
		}
		nodeID := FolderObjectID(folderRelPath)
		node := fi.Folders[nodeID]
		node.ChildTrackIDs = ids
		fi.Folders[nodeID] = node
	}

	// Wire up ChildFolderIDs: for every FolderNode, find its direct
	// sub-folders by iterating the Folders map and matching ParentID.
	// Could be done in a single pass with a temporary
	// parent→children map; this two-pass shape is fine for typical
	// library sizes and keeps the build logic readable.
	childrenByParent := make(map[string][]string)
	for id, node := range fi.Folders {
		childrenByParent[node.ParentID] = append(childrenByParent[node.ParentID], id)
	}
	for parentID, childIDs := range childrenByParent {
		// Stable sort child IDs by their Name (the user-visible folder
		// label) — empirically matches what every reference MediaServer
		// (MiniDLNA, MinimServer) does and aligns with what mconnect
		// expects.
		sort.Slice(childIDs, func(i, j int) bool {
			return fi.Folders[childIDs[i]].Name < fi.Folders[childIDs[j]].Name
		})
		if parentID == foldersRootObjectID {
			fi.TopLevelFolderIDs = childIDs
			continue
		}
		if node, ok := fi.Folders[parentID]; ok {
			node.ChildFolderIDs = childIDs
			fi.Folders[parentID] = node
		}
	}

	return fi
}

// parentObjectID computes the parent-folder ObjectID for the given
// relative path. Top-level folders (single path component) parent to
// the synthetic Folders root ("2"); nested folders parent to the
// FolderObjectID of their immediate parent path.
func parentObjectID(folderRelPath string) string {
	parent := path.Dir(folderRelPath)
	if parent == "." || parent == "" || parent == folderRelPath {
		return foldersRootObjectID
	}
	return FolderObjectID(parent)
}

// relParentDir returns the relative-parent-directory portion of an
// absolute path, with the library-root prefix stripped. Examples
// (libRoot = "/lib/"):
//
//	"/lib/Artist/Album/track.flac" → "Artist/Album"
//	"/lib/standalone.flac"         → "" (top-level)
//	"/elsewhere/track.flac"        → "" (libRoot mismatch falls
//	                                     through as top-level — defensive
//	                                     for tracks that resolve outside
//	                                     the detected root, e.g.
//	                                     symlinked aliases)
//
// Forward-slash separators throughout, matching the bridge's
// manifest convention.
func relParentDir(absPath, libRoot string) string {
	if absPath == "" {
		return ""
	}
	stripped := absPath
	if libRoot != "" && strings.HasPrefix(absPath, libRoot) {
		stripped = strings.TrimPrefix(absPath, libRoot)
	}
	// Normalize OS-specific path separators to forward slashes —
	// the manifest convention uses '/' for portability.
	stripped = strings.ReplaceAll(stripped, "\\", "/")
	stripped = strings.TrimPrefix(stripped, "/")
	dir := path.Dir(stripped)
	if dir == "." || dir == "" {
		return ""
	}
	return dir
}

// longestCommonPathPrefix returns the longest path prefix shared by
// every entry in `paths`, with a trailing separator. Used by
// BuildFolderIndex to detect the library-root prefix without needing
// it plumbed through the LibrarySource interface.
//
// Empty input or single-entry input behaves naturally: empty input
// returns ""; a single entry returns its directory plus separator.
func longestCommonPathPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) == 1 {
		// Single track: return its directory + separator.
		d := path.Dir(strings.ReplaceAll(paths[0], "\\", "/"))
		if d == "." || d == "" {
			return ""
		}
		return d + "/"
	}
	// LCP is bounded by the lexicographically smallest + largest entries, so
	// a single O(N) min/max pass replaces the prior O(N log N) sort (it only
	// ever needed the two extremes). Normalize separators inline — the same
	// ReplaceAll the old per-entry `norm` slice applied, now without the
	// intermediate allocation. len(paths) >= 2 here (the len 0/1 cases
	// returned above), so paths[0] is safe; the component-wise comparison
	// below is unchanged. (external review r3)
	a := strings.ReplaceAll(paths[0], "\\", "/")
	b := a
	for _, p := range paths[1:] {
		n := strings.ReplaceAll(p, "\\", "/")
		if n < a {
			a = n
		}
		if n > b {
			b = n
		}
	}
	// Walk component-by-component, NOT character-by-character — a
	// character walk would produce mid-component prefixes (e.g.
	// "/lib/Artist" vs "/lib/Artists" sharing "/lib/Artist" as a
	// substring even though the components are distinct).
	aParts := strings.Split(a, "/")
	bParts := strings.Split(b, "/")
	var common []string
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if aParts[i] != bParts[i] {
			break
		}
		common = append(common, aParts[i])
	}
	if len(common) == 0 {
		return ""
	}
	out := strings.Join(common, "/")
	// Don't include the filename portion if the prefix happens to
	// equal a complete path (would happen only if all input paths
	// are identical, which shouldn't happen with distinct tracks
	// but is handled defensively).
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

// tracksToAbsPaths is a tiny helper extracting the AbsolutePath field
// from a TrackInfo slice — saves one allocation by sizing the output
// slice exactly.
func tracksToAbsPaths(tracks []TrackInfo) []string {
	out := make([]string, len(tracks))
	for i, t := range tracks {
		out[i] = t.AbsolutePath
	}
	return out
}

// allRelativePathsPopulated returns true when every TrackInfo carries
// a non-empty RelativePath. Used to pick between the explicit-rel-path
// derivation (preferred, correct in all shapes) and the LCP fallback
// (legacy compatibility for fixtures / adapters that haven't been
// updated).
func allRelativePathsPopulated(tracks []TrackInfo) bool {
	for _, t := range tracks {
		if t.RelativePath == "" {
			return false
		}
	}
	return true
}

// relParentDirFromRelPath returns the relative-parent-directory
// portion of a relative path. Mirrors `relParentDir` but skips the
// library-root stripping step (RelativePath is already root-relative).
//
// Examples:
//
//	"Artist/Album/track.flac" → "Artist/Album"
//	"loose.flac"              → "" (top-level)
//	"/Artist/track.flac"      → "Artist" (leading slash tolerated)
func relParentDirFromRelPath(relPath string) string {
	if relPath == "" {
		return ""
	}
	normalized := strings.ReplaceAll(relPath, "\\", "/")
	normalized = strings.TrimPrefix(normalized, "/")
	dir := path.Dir(normalized)
	if dir == "." || dir == "" {
		return ""
	}
	return dir
}
