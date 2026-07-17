// Admin Library Inspector browse endpoints (v1.3). Two surfaces:
//
//   - GET /api/library/browse?path=...
//     Lists one level of folders + tracks under `path`. Empty path
//     returns the top-level folders (root basenames in multi-root
//     mode, album folders in single-root mode). Each folder row
//     carries the recursive rollup (track / upscaled counts +
//     sizes) so the UI's status ring renders without a follow-up
//     call per row.
//
//   - GET /api/library/browse-projection?path=...
//     Computes the operator-facing pre-flight: walks every track
//     under `path`, runs each through `transcode.ProjectedSize`
//     (wired via deps closures), probes free disk space on the
//     bridge's data volume, returns (projected_files,
//     projected_size_bytes, available_bytes, would_fit).
//
// Both endpoints are admin-only (loopback-enforced at the
// listener layer). Path normalisation reuses
// `manifest.NormalizePathForLookup` semantics; traversal (`..`)
// rejected before the SQL call so a maliciously-crafted query
// can't escape the library root via the parameter.

package admin

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylist"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// pathHash returns a stable 8-character lowercase hex digest of the
// library-relative path via CRC32 (IEEE polynomial). Used as a
// transient HTML identifier for the per-tile popover menus
// (`id="menu-<hash>"` + `popovertarget="menu-<hash>"`). Library
// paths regularly carry spaces, `/`, `.`, parentheses, and non-ASCII
// (`Édith Piaf/Mon Légionnaire (Remastered).flac`) — those
// characters are illegal or unstable as HTML `id` / `popovertarget`
// tokens, so a sanitized-string approach is fragile. CRC32 collision
// probability across the few-thousand-tile inspector view is
// vanishingly small; if a collision ever fires the popover opens
// for the wrong tile (no data corruption). JS reads `data-path`
// (the raw value) for any API call — `pathHash` is strictly an
// HTML-attribute escape hatch.
func pathHash(p string) string {
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(p)))
}

// --- response shapes ---

// browseFolderRow is the JSON shape for one folder entry in
// /api/library/browse responses. Variant counters / sizes split
// by kind (upscale vs CarPlay-optimized) so the inspector tile
// renders dual coverage bars without a follow-up call.
type browseFolderRow struct {
	Name               string `json:"name"`
	Path               string `json:"path"`
	TrackCount         int    `json:"trackCount"`
	UpscaledCount      int    `json:"upscaledCount"`
	OptimizedCount     int    `json:"optimizedCount"`
	TotalSizeBytes     int64  `json:"totalSizeBytes"`
	UpscaledSizeBytes  int64  `json:"upscaledSizeBytes"`
	OptimizedSizeBytes int64  `json:"optimizedSizeBytes"`
	PathHash           string `json:"pathHash"`
	// *EligibleCount are the coverage-bar DENOMINATORS: tracks that
	// have a variant of the kind OR are currently eligible to get one
	// (manifest.EligibleCountsForFolders). Tracks that need nothing —
	// at the CarPlay floor, at the upscale target, DSD, unknown
	// geometry — drop out so a fully-processed mixed folder reads
	// 100%, with the tile showing "N need nothing" for the rest.
	//
	// POINTERS, not plain ints: nil (field omitted) = "counts
	// unavailable" (store error — degrade) and the JS falls back to
	// TrackCount; a present 0 = genuinely nothing eligible (all-DSD
	// folder) and renders as "all set". A non-pointer 0 couldn't
	// distinguish the two.
	UpscaleEligibleCount  *int `json:"upscaleEligibleCount,omitempty"`
	OptimizeEligibleCount *int `json:"optimizeEligibleCount,omitempty"`
}

// browseTrackRow is the JSON shape for one track entry. Pointer
// fields preserve the "tag absent" vs "tag = 0" distinction that
// the manifest's `Track` wire shape already documents — same
// convention. `IsOptimized` mirrors `IsUpscaled` for the CarPlay
// variant kind.
type browseTrackRow struct {
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	SizeBytes     int64    `json:"sizeBytes"`
	SampleRate    *float64 `json:"sampleRate,omitempty"`
	BitsPerSample *int     `json:"bitsPerSample,omitempty"`
	Codec         string   `json:"codec,omitempty"`
	IsDSD         *bool    `json:"isDSD,omitempty"`
	IsUpscaled    bool     `json:"isUpscaled"`
	IsOptimized   bool     `json:"isOptimized"`
	PathHash      string   `json:"pathHash"`
	// SkipReason is the kind-agnostic, target-independent reason this
	// track can never be upscaled/optimized (dsd_bitstream / lossy_source
	// / unknown_format), or "" when it has no hard block. The inspector
	// badges each tile with it inline — see fundamentalSkipReason.
	SkipReason string `json:"skipReason,omitempty"`
}

// browseResponse is the JSON envelope returned by
// GET /api/library/browse.
//
// v1.4 PR C: cursor-based pagination. Callers can pass
// `?after=&limit=` (defaults: ""/500); response surfaces the
// total counts (immediate-children only, no rollup) AND the
// next cursors so the UI can render a "Load more (N remaining)"
// sentinel. Empty `nextFolderCursor` / `nextTrackCursor` signals
// "no more pages" for the respective collection.
type browseResponse struct {
	// Path is the normalised library-relative path the response
	// describes; echoed back so the UI can verify what it asked
	// for (especially helpful for traversal-rejected requests
	// that landed on the empty-path branch).
	Path             string            `json:"path"`
	Folders          []browseFolderRow `json:"folders"`
	Tracks           []browseTrackRow  `json:"tracks"`
	TotalFolders     int               `json:"totalFolders"`
	TotalTracks      int               `json:"totalTracks"`
	NextFolderCursor string            `json:"nextFolderCursor,omitempty"`
	NextTrackCursor  string            `json:"nextTrackCursor,omitempty"`
	Limit            int               `json:"limit"`
	// Recursive subtree rollup for the ENTIRE node — every track under
	// `path`, not just the paginated folder/track page in Folders/Tracks.
	// TotalTracks above is immediate loose tracks only; these are the
	// numbers the action-panel coverage header must use. Summing the
	// returned page client-side under-counts whenever a node has more
	// children than one page (e.g. a 647-folder root showed ~13k of 25k
	// tracks). Computed via RollupByPrefix — same source as the Dashboard's
	// "Library composition" tile, so the two always agree.
	//
	// Populated on the FIRST page only (omitempty): the client caches the
	// first-page response and reads these totals from there; load-more
	// pages don't update it, so re-walking the whole subtree on every
	// follow-up page would be wasted work on exactly the large-node case
	// pagination exists for (Gemini + CodeRabbit on PR #343).
	SubtreeTracks    int   `json:"subtreeTracks,omitempty"`
	SubtreeUpscaled  int   `json:"subtreeUpscaled,omitempty"`
	SubtreeOptimized int   `json:"subtreeOptimized,omitempty"`
	SubtreeSizeBytes int64 `json:"subtreeSizeBytes,omitempty"`
	// Subtree*Eligible mirror the per-folder eligible denominators for
	// the ENTIRE node (manifest.EligibleRollupByPrefix) — the action
	// panel's coverage header uses these. First page only, like the
	// other Subtree fields. Pointers for the same nil-vs-genuine-zero
	// reason as browseFolderRow's eligible counts.
	SubtreeUpscaleEligible  *int `json:"subtreeUpscaleEligible,omitempty"`
	SubtreeOptimizeEligible *int `json:"subtreeOptimizeEligible,omitempty"`
	// KeyFilter / KeyName are set ONLY on the harmonic-key filter view
	// (?camelot=8A): the Camelot code echoed back (upper-cased) and its
	// human pitch name ("A minor"), driving the inspector's "filtering by
	// key" banner. Empty on a normal folder browse.
	KeyFilter string `json:"keyFilter,omitempty"`
	KeyName   string `json:"keyName,omitempty"`
}

// browseProjectionResponse is the JSON envelope returned by
// GET /api/library/browse-projection. Numbers are bytes;
// `wouldFit` carries the verdict against the free space of the
// volume holding the effective variants dir (the write target)
// using `transcode.DefaultDiskSafetyMargin`.
type browseProjectionResponse struct {
	Path string `json:"path"`
	// Kind echoes the resolved variant kind ("upscale" / "optimize")
	// so the UI knows which section of the drawer this payload
	// populates.
	Kind                string `json:"kind"`
	ProjectedFiles      int    `json:"projectedFiles"`
	AlreadyCoveredFiles int    `json:"alreadyCoveredFiles"`
	ProjectedSizeBytes  int64  `json:"projectedSizeBytes"`
	AvailableBytes      int64  `json:"availableBytes"`
	WouldFit            bool   `json:"wouldFit"`
	TargetRate          int    `json:"targetRate"`
	TargetBits          int    `json:"targetBits"`
	// UnknownFormatFiles counts genuinely-skipped tracks (DSD, lossy,
	// unknown geometry); AlreadyAtTargetFiles counts tracks that need
	// NOTHING (PCM at/below the CarPlay floor for optimize; at/above
	// the target for upscale). The UI renders the former as a warning
	// hint and the latter as neutral "nothing to do" copy.
	UnknownFormatFiles      int   `json:"unknownFormatFiles"`
	AlreadyAtTargetFiles    int   `json:"alreadyAtTargetFiles"`
	RequiredBytesWithMargin int64 `json:"requiredBytesWithMargin"`
}

// isLossyCodecLabel answers "is this a lossy encode?" for the
// per-track skip badge AND the projection walk's upscale gate.
// Delegates to manifest.IsLossyCodec — the single source of truth
// shared with Coordinator.Submit / EnqueueOne and mirrored by
// upscaleEligibleSQL — so the badge and the gates can't drift.
// codec "" is unknown, NOT lossy, so it stays unknown_format.
func isLossyCodecLabel(codec string) bool {
	return manifest.IsLossyCodec(codec)
}

// fundamentalSkipReason returns the kind-agnostic, target-independent
// reason a track can NEVER be upscaled or optimized — or "" when it has
// no such hard block (it may still be a soft, kind-specific skip like
// "already at target", which only the per-kind projection can decide).
// These are the cases that make an operator think the bridge is
// malfunctioning ("why are these tracks missing?!"), so the inspector
// badges them inline on every track tile, straight from the row's own
// data — no projection round-trip. Pure + table-tested.
//
//   - dsd_bitstream  : DSD (1-bit) — the SoX PCM pipeline rejects it
//   - lossy_source   : a known lossy codec (MP3/AAC/OGG/OPUS/WMA)
//   - unknown_format : extractor couldn't determine sample rate / bit depth
func fundamentalSkipReason(isDSD bool, codec string, sampleRate *float64, bitsPerSample *int) string {
	switch {
	case isDSD:
		return "dsd_bitstream"
	case isLossyCodecLabel(codec):
		return "lossy_source"
	case sampleRate == nil || *sampleRate <= 0 || bitsPerSample == nil || *bitsPerSample <= 0:
		return "unknown_format"
	default:
		return ""
	}
}

// --- handlers ---

// apiLibraryBrowse handles GET /api/library/browse?path=...
//
// Returns one level of children under `path`. Empty / missing
// `path` returns the top-level folders + any root-level tracks.
func (s *Server) apiLibraryBrowse(w http.ResponseWriter, r *http.Request) {
	// Harmonic-key filter view (the coverage wheel's click-to-scope
	// deep-link): a flat, library-wide list of analyzed tracks in one
	// key. Same browseResponse shape (Folders empty) so the inspector
	// reuses its tile renderer + cursor pagination unchanged.
	if camelot := strings.TrimSpace(r.URL.Query().Get("camelot")); camelot != "" {
		s.browseByKey(w, r, camelot)
		return
	}
	rawPath := r.URL.Query().Get("path")
	normalised, ok := normaliseBrowsePath(rawPath)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path",
			"path contains traversal segments or is otherwise invalid")
		return
	}
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manifest store not wired")
		return
	}
	// Cursor-based pagination (PR C). Folders and tracks paginate
	// INDEPENDENTLY — each has its own ordering scope on the
	// underlying table, so the client passes a separate cursor per
	// collection. The presence (not value) of each cursor param
	// signals "keep paginating this collection." A request that
	// omits `afterFolder` means folders are exhausted on the client
	// side; the server skips that query entirely. Mirror logic for
	// tracks. Initial-page request omits BOTH cursors → server
	// fetches the first page of each (treat as exhausted-of-nothing).
	q := r.URL.Query()
	hasAfterFolder := q.Has("afterFolder")
	hasAfterTrack := q.Has("afterTrack")
	afterFolder := q.Get("afterFolder")
	afterTrack := q.Get("afterTrack")
	isFirstPage := !hasAfterFolder && !hasAfterTrack
	fetchFolders := isFirstPage || hasAfterFolder
	fetchTracks := isFirstPage || hasAfterTrack

	limit := 500
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 2000 {
				n = 2000
			}
			limit = n
		}
	}

	var (
		folders []manifest.ChildFolderRollup
		tracks  []manifest.ChildTrack
		err     error
	)
	if fetchFolders {
		folders, err = s.deps.Manifest.ListChildFoldersPage(r.Context(), normalised, afterFolder, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "browse-folders", err.Error())
			return
		}
	}
	if fetchTracks {
		tracks, err = s.deps.Manifest.ListChildTracksPage(r.Context(), normalised, afterTrack, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "browse-tracks", err.Error())
			return
		}
	}
	totalFolders, err := s.deps.Manifest.CountChildFolders(r.Context(), normalised)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "count-folders", err.Error())
		return
	}
	totalTracks, err := s.deps.Manifest.CountChildTracks(r.Context(), normalised)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "count-tracks", err.Error())
		return
	}
	resp := browseResponse{
		Path:         normalised,
		Folders:      make([]browseFolderRow, 0, len(folders)),
		Tracks:       make([]browseTrackRow, 0, len(tracks)),
		TotalFolders: totalFolders,
		TotalTracks:  totalTracks,
		Limit:        limit,
	}
	// Eligible-denominator counts for the coverage bars. Target
	// resolution and the counts themselves DEGRADE rather than fail
	// the browse: on any error the denominators stay zero and the JS
	// falls back to trackCount (the pre-eligibility rendering). A
	// zero/unresolved upscale target likewise collapses the upscale
	// arm to covered-only inside the SQL — see upscaleEligibleSQL.
	targetRate, targetBits := 0, 0
	if cfg := s.deps.CfgHolder.Load(); cfg != nil {
		if tr, tb, err := s.resolveUpscaleTarget(r.Context(), cfg); err == nil {
			targetRate, targetBits = tr, tb
		} else {
			logger.Warn("browse: upscale target unresolved; eligible counts degrade to covered-only", "err", err)
		}
	}
	var eligible map[string]manifest.EligibleCounts
	if len(folders) > 0 {
		paths := make([]string, len(folders))
		for i, f := range folders {
			paths[i] = f.Path
		}
		eligible, err = s.deps.Manifest.EligibleCountsForFolders(r.Context(), paths, targetRate, targetBits)
		if err != nil {
			logger.Warn("browse: eligible counts unavailable; tiles fall back to all-tracks denominators", "err", err)
			eligible = nil
		}
	}
	// Recursive subtree rollup for the whole node — only on the first page
	// (the client caches it from there; load-more pages don't use it, so
	// re-walking the subtree per follow-up page would be wasted work).
	if isFirstPage {
		rollup, err := s.deps.Manifest.RollupByPrefix(r.Context(), normalised)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "browse-rollup", err.Error())
			return
		}
		resp.SubtreeTracks = rollup.TrackCount
		resp.SubtreeUpscaled = rollup.UpscaledTrackCount
		resp.SubtreeOptimized = rollup.OptimizedTrackCount
		resp.SubtreeSizeBytes = rollup.TotalSizeBytes
		if ec, err := s.deps.Manifest.EligibleRollupByPrefix(r.Context(), normalised, targetRate, targetBits); err == nil {
			resp.SubtreeUpscaleEligible = intPtr(ec.Upscale)
			resp.SubtreeOptimizeEligible = intPtr(ec.Optimize)
		} else {
			logger.Warn("browse: eligible rollup unavailable; panel header falls back", "err", err)
		}
	}
	for _, f := range folders {
		row := browseFolderRow{
			Name:               path.Base(f.Path),
			Path:               f.Path,
			TrackCount:         f.TrackCount,
			UpscaledCount:      f.UpscaledTrackCount,
			OptimizedCount:     f.OptimizedTrackCount,
			TotalSizeBytes:     f.TotalSizeBytes,
			UpscaledSizeBytes:  f.UpscaledSizeBytes,
			OptimizedSizeBytes: f.OptimizedSizeBytes,
			PathHash:           pathHash(f.Path),
		}
		if ec, ok := eligible[f.Path]; ok {
			row.UpscaleEligibleCount = intPtr(ec.Upscale)
			row.OptimizeEligibleCount = intPtr(ec.Optimize)
		}
		resp.Folders = append(resp.Folders, row)
	}
	for _, t := range tracks {
		resp.Tracks = append(resp.Tracks, toBrowseTrackRow(t))
	}
	// Next-page cursors: a "full page" (len == limit) MIGHT have
	// more; signal continuation via the last row's path. A
	// short page (len < limit) is the final page — no cursor.
	// The exact-multiple-of-limit boundary case (page size == limit
	// AND exactly that many rows remain) costs one wasted round
	// trip — the next request will return 0 rows and the frontend
	// stops; acceptable v1 vs the alternative of an extra COUNT
	// query per page advance.
	if len(folders) == limit && fetchFolders {
		resp.NextFolderCursor = folders[len(folders)-1].Path
	}
	if len(tracks) == limit && fetchTracks {
		resp.NextTrackCursor = tracks[len(tracks)-1].Path
	}
	writeJSON(w, http.StatusOK, resp)
}

// pitchNames maps keyRoot 0..11 (C=0) to its pitch-class name for the
// inspector's harmonic-key banner. Sharps match the Camelot wheel labels.
var pitchNames = [12]string{"C", "C♯", "D", "D♯", "E", "F", "F♯", "G", "G♯", "A", "A♯", "B"}

// camelotKeyName renders a (keyRoot, mode) as a human pitch name, e.g.
// "A minor" / "C major". Empty for an out-of-range root.
func camelotKeyName(keyRoot int, mode string) string {
	if keyRoot < 0 || keyRoot > 11 {
		return ""
	}
	return pitchNames[keyRoot] + " " + mode
}

// toBrowseTrackRow projects a manifest.ChildTrack into the wire row shape.
// Shared by the folder browse and the harmonic-key filter so the
// skip-reason + pointer-field mapping lives in one place.
func toBrowseTrackRow(t manifest.ChildTrack) browseTrackRow {
	return browseTrackRow{
		Name:          path.Base(t.Path),
		Path:          t.Path,
		SizeBytes:     t.Size,
		SampleRate:    t.SampleRate,
		BitsPerSample: t.BitsPerSample,
		Codec:         t.Codec,
		IsDSD:         t.IsDSD,
		IsUpscaled:    t.IsUpscaled,
		IsOptimized:   t.IsOptimized,
		PathHash:      pathHash(t.Path),
		SkipReason:    fundamentalSkipReason(t.IsDSD != nil && *t.IsDSD, t.Codec, t.SampleRate, t.BitsPerSample),
	}
}

// browseByKey serves the harmonic-key filter view: a flat, library-wide,
// cursor-paginated list of analyzed tracks whose key matches the Camelot
// code. Folders are always empty (a key view has no hierarchy); the
// inspector renders it with its existing tile + load-more machinery and
// shows a "filtering by key" banner from KeyFilter/KeyName. Track-cursor
// pagination only (afterTrack); afterFolder doesn't apply.
func (s *Server) browseByKey(w http.ResponseWriter, r *http.Request, camelot string) {
	keyRoot, keyMode, ok := smartplaylist.FromCamelot(camelot)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-key",
			"camelot must be a wheel code like 8A (minor) or 8B (major)")
		return
	}
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manifest store not wired")
		return
	}
	q := r.URL.Query()
	afterTrack := q.Get("afterTrack")
	isFirstPage := !q.Has("afterTrack")
	limit := 500
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 2000 {
				n = 2000
			}
			limit = n
		}
	}
	tracks, err := s.deps.Manifest.ListTracksByKeyPage(r.Context(), keyRoot, keyMode, afterTrack, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "browse-by-key", err.Error())
		return
	}
	total, err := s.deps.Manifest.CountTracksByKey(r.Context(), keyRoot, keyMode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "count-by-key", err.Error())
		return
	}
	resp := browseResponse{
		Path:        "",
		KeyFilter:   strings.ToUpper(strings.TrimSpace(camelot)),
		KeyName:     camelotKeyName(keyRoot, keyMode),
		Folders:     []browseFolderRow{},
		Tracks:      make([]browseTrackRow, 0, len(tracks)),
		TotalTracks: total,
		Limit:       limit,
	}
	// The flat key list IS its own subtree; surface the total on the first
	// page so the action-panel coverage header has a denominator.
	if isFirstPage {
		resp.SubtreeTracks = total
	}
	for _, t := range tracks {
		resp.Tracks = append(resp.Tracks, toBrowseTrackRow(t))
	}
	if len(tracks) == limit {
		resp.NextTrackCursor = tracks[len(tracks)-1].Path
	}
	writeJSON(w, http.StatusOK, resp)
}

// intPtr boxes an int for the optional (nil = unavailable) JSON
// fields on browse rows.
func intPtr(v int) *int { return &v }

// resolveUpscaleTarget resolves the ACTIVE upscale target: the
// DB-backed setting wins; the YAML bootstrap is the fallback for
// unseeded installs. Shared by the projection endpoint (which fails
// the request on error) and the browse endpoint's eligible-count
// computation (which degrades to covered-only denominators instead).
func (s *Server) resolveUpscaleTarget(ctx context.Context, cfg *config.Config) (int, int, error) {
	rate, bits, err := s.deps.Manifest.GetUpscaleTarget(ctx)
	switch {
	case err == nil:
		return rate, bits, nil
	case errors.Is(err, manifest.ErrUpscaleTargetUnset):
		return cfg.Upscale.EffectiveBootstrapTargetRate(),
			cfg.Upscale.EffectiveBootstrapTargetBits(), nil
	default:
		return 0, 0, err
	}
}

// apiLibraryBrowseProjection handles
// GET /api/library/browse-projection?path=...&kind=upscale|optimize
//
// Walks every track under `path`, computes per-track projected
// size via the wired closure (transcode.ProjectedSize), probes
// disk space on the volume holding the effective variants dir
// (the actual write target — NOT the bridge data volume), and
// returns the pre-flight verdict the Library Inspector renders in
// the action drawer.
//
// `?kind=upscale` (default when omitted, back-compat) projects
// against the active upscale target (rate/bits resolved from
// scan_state). `?kind=optimize` projects against per-track
// family-preserving 16/44.1 or 16/48 targets (via the wired
// `TargetRateForOptimize` closure); tracks failing
// `OptimizeEligible` (DSD, lossy, already-at-CarPlay-floor) roll
// into `unknownFormatFiles` so the UI's "X skipped" copy
// reconciles with the JSON payload.
//
// Tracks that ALREADY have a variant OF THE REQUESTED KIND are
// counted separately (`alreadyCoveredFiles`) and excluded from
// the projection. **Kind-scoped HasVariant** (senior-review fix):
// a track with only an upscale variant correctly shows as
// eligible (not "already covered") under kind=optimize, and
// vice versa.
func (s *Server) apiLibraryBrowseProjection(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	rawPath := r.URL.Query().Get("path")
	normalised, ok := normaliseBrowsePath(rawPath)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path",
			"path contains traversal segments or is otherwise invalid")
		return
	}
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manifest store not wired")
		return
	}
	if s.deps.ProjectedSize == nil || s.deps.AvailableDiskSpace == nil {
		// Closures not wired (test harness, or upscale feature off
		// at boot). Surface a clean 503 rather than a typed-nil
		// panic so test harnesses can be parsed.
		writeError(w, http.StatusServiceUnavailable, "upscale-disabled",
			"upscale feature is not configured on this bridge")
		return
	}

	// Resolve kind (back-compat default = "upscale").
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	switch kind {
	case "", "upscale":
		kind = "upscale"
	case "optimize":
		// Optimize requires both eligibility + target-rate closures.
		if s.deps.OptimizeEligible == nil || s.deps.TargetRateForOptimize == nil {
			writeError(w, http.StatusServiceUnavailable, "optimize-disabled",
				"carplay-optimize feature is not configured on this bridge")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "invalid-kind",
			`unknown kind: `+kind+` (expected "upscale" or "optimize")`)
		return
	}

	// Resolve active upscale target: DB-backed setting wins; YAML
	// bootstrap is the fall-back for unseeded installs.
	// Used by kind=upscale; kind=optimize derives per-track instead.
	var (
		rate, bits int
		err        error
	)
	if kind == "upscale" {
		rate, bits, err = s.resolveUpscaleTarget(r.Context(), cfg)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "read-target", err.Error())
			return
		}
	}

	// Kind-scoped HasVariant: pass the prefix matching the requested
	// kind so a track with only an upscale variant correctly shows
	// as eligible under kind=optimize, and vice versa.
	variantPrefix := manifest.VariantKindPrefixUpscaled
	if kind == "optimize" {
		variantPrefix = manifest.VariantKindPrefixOptimized
	}
	projections, err := s.deps.Manifest.ListTrackProjectionsUnderPrefix(r.Context(), normalised, variantPrefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list-projections", err.Error())
		return
	}

	var (
		totalProjected int64
		projectedFiles int
		coveredFiles   int
		unknownFormat  int
		atTarget       int
	)
	for _, t := range projections {
		if t.HasVariant {
			coveredFiles++
			continue
		}
		if t.IsDSD {
			// DSD (DSF/DFF) is 1-bit modulated and the SoX upscale
			// pipeline rejects it — there's no meaningful "upscale"
			// from a delta-sigma stream to a PCM resampler target.
			// Pre-fix DSD tracks fell through to the rate>0 / bits>0
			// branch (DSF reports e.g. rate=2822400, bits=1) AND
			// ProjectedSize returned a non-zero value, so the UI
			// surfaced an active "Upscale this folder" button. The
			// submit then enqueued 0 tracks ("Batch enrolled · 0
			// tracks queued"). Folding DSD into `unknownFormat` is
			// honest: the UI already labels that bucket as "DSD,
			// lossy, or unknown — they'll be skipped." User-reported
			// on the v1.4 followup.
			unknownFormat++
			continue
		}
		if t.SampleRate <= 0 || t.BitsPerSample <= 0 {
			// Extractor couldn't determine source format — surface
			// these separately so the operator sees them but they
			// don't contribute zero/garbage to the projection.
			unknownFormat++
			continue
		}
		// Per-track target derivation for optimize: family-preserving
		// 16/44.1 or 16/48 depending on source rate. Upscale uses the
		// static outer-scope target resolved above.
		//
		// The at-target split (alreadyAtTargetFiles): tracks that need
		// NOTHING are counted separately from the genuinely-skipped
		// bucket. Pre-split, an at-floor CD track (optimize) landed in
		// unknownFormat — rendered as "DSD, lossy, or unknown — skipped",
		// which read as a failure for a track that is simply already
		// CarPlay-ready — and an at/above-target track (upscale) vanished
		// from every bucket via the ProjectedSize<=0 fall-through,
		// under-reporting the panel. The UI renders atTarget as a
		// neutral hint, not a warning.
		var trackRate, trackBits int
		if kind == "optimize" {
			if !s.deps.OptimizeEligible(t.Path, t.Codec, t.SampleRate, t.BitsPerSample) {
				// fundamentalSkipReason distinguishes hard blocks
				// (DSD / lossy / unknown geometry) from "PCM but
				// already at/below the CarPlay floor" — the latter
				// is the at-target case. Reuses the tile badges'
				// classifier so the two surfaces can't disagree.
				sr := float64(t.SampleRate)
				bps := t.BitsPerSample
				if fundamentalSkipReason(t.IsDSD, t.Codec, &sr, &bps) == "" {
					atTarget++
				} else {
					unknownFormat++
				}
				continue
			}
			trackRate = s.deps.TargetRateForOptimize(t.SampleRate)
			trackBits = 16
			if trackRate <= 0 {
				unknownFormat++
				continue
			}
		} else {
			if isLossyCodecLabel(t.Codec) {
				// Lossy sources are never upscaled (Submit's gate,
				// PROTOCOL.md "PCM") — genuinely skipped, so they
				// belong in the unknownFormat bucket the UI labels
				// "DSD / lossy / unknown format", NOT at-target.
				// Same shared predicate as the tile badge (the local
				// wrapper delegates to manifest.IsLossyCodec).
				unknownFormat++
				continue
			}
			if t.SampleRate > rate || t.BitsPerSample > bits ||
				(t.SampleRate == rate && t.BitsPerSample == bits) {
				// At or above the upscale target on either axis
				// (Submit's never-downsample gate) or exactly at it
				// (no-op) — nothing to generate for this track.
				atTarget++
				continue
			}
			trackRate = rate
			trackBits = bits
		}
		size := s.deps.ProjectedSize(t.Size, t.SampleRate, t.BitsPerSample, trackRate, trackBits)
		if size <= 0 {
			continue
		}
		totalProjected += size
		projectedFiles++
	}

	// Probe the volume the variants are actually WRITTEN to — the
	// effective variants dir may live on a different (much larger)
	// volume than the bridge data dir (field case: bridge.ars.md's
	// B2-mounted variants vs a 29 GB root disk; grading DataDir
	// refused batches that fit easily). The wired closure routes
	// through AvailableDiskSpaceNearest, so a lazily-created /
	// not-yet-existing variants dir is graded by its closest
	// existing ancestor.
	free, err := s.deps.AvailableDiskSpace(cfg.Upscale.EffectiveVariantsDir(cfg.DataDir))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "disk-probe", err.Error())
		return
	}
	// Required bytes, from the SAME helper Coordinator.Submit's
	// pre-flight uses. This endpoint's entire contract is predicting
	// what Submit will refuse, so it can't restate the arithmetic —
	// a local copy drifts the moment DefaultDiskSafetyMargin moves and
	// the operator gets a green panel followed by a 507. The helper
	// carries the overflow guard (saturate to MaxInt64, never wrap
	// negative — the hazard the earlier hand-rolled integer form was
	// avoiding).
	required := transcode.RequiredBytesWithMargin(totalProjected, transcode.DefaultDiskSafetyMargin)
	// For kind=optimize, surface TargetBits=16 (always) but
	// TargetRate=0 to signal "per-track family-preserved" — the UI
	// renders "Target: 16-bit / 44.1k or 48k (family-preserved)"
	// rather than a single static rate that would lie about mixed-
	// family scopes.
	respTargetRate := rate
	respTargetBits := bits
	if kind == "optimize" {
		respTargetRate = 0
		respTargetBits = 16
	}
	writeJSON(w, http.StatusOK, browseProjectionResponse{
		Path:                    normalised,
		Kind:                    kind,
		ProjectedFiles:          projectedFiles,
		AlreadyCoveredFiles:     coveredFiles,
		ProjectedSizeBytes:      totalProjected,
		AvailableBytes:          free,
		WouldFit:                required <= free,
		TargetRate:              respTargetRate,
		TargetBits:              respTargetBits,
		UnknownFormatFiles:      unknownFormat,
		AlreadyAtTargetFiles:    atTarget,
		RequiredBytesWithMargin: required,
	})
}

// normaliseBrowsePath cleans the user-supplied `path` query
// parameter into the library-relative form the manifest store's
// `ListChild*` helpers expect (no leading slash, no `.`/`..`
// segments, no trailing slash). Returns ok=false when the input
// contains a parent-traversal segment that would escape the
// library root, which indicates either a buggy frontend or a
// deliberate attempt to break scope.
//
// Empty input returns ("", true) — that's the root-level browse.
//
// We deliberately call `path.Clean` WITHOUT a leading slash so
// the function preserves any unresolved `..` segments. A `..`
// at the root of the cleaned result means the input would
// escape; rooting the path with a slash before cleaning would
// silently absorb such cases (path.Clean("/../etc") returns
// "/etc", losing the escape signal).
func normaliseBrowsePath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	// Forward slashes only — manifest stores forward-slash paths
	// exclusively. Reject the input rather than silently
	// normalising backslashes.
	if strings.Contains(raw, `\`) {
		return "", false
	}
	// Strip ALL leading slashes so a UI that sent
	// "/MusicA", "//MusicA", or "///MusicA" lands in the same
	// shape as "MusicA". Pre-fix the helper used `TrimPrefix`
	// which only strips one — an input like `//../etc` would
	// post-trim to `/../etc`, slip past path.Clean's
	// resolution (which still saw a leading slash anchor), and
	// could evade the `..`-traversal refusal. Per CodeRabbit
	// med-sec on PR #203 round 2.
	raw = strings.TrimLeft(raw, "/")
	if raw == "" {
		return "", true
	}
	cleaned := path.Clean(raw)
	// path.Clean("") and path.Clean(".") both return "." — both
	// mean "the root we already are at." Map to empty so callers
	// that compare `==""` don't have to handle a special sentinel.
	if cleaned == "." {
		return "", true
	}
	// `..` at the head of the cleaned result means the input
	// would resolve above the library root. path.Clean leaves
	// such cases intact when no absolute anchor is provided:
	//   path.Clean("..")             = ".."
	//   path.Clean("../etc/passwd")  = "../etc/passwd"
	//   path.Clean("foo/../../bar")  = "../bar"
	// All three are escapes; the cleaned-result-starts-with-`..`
	// check handles all of them in one place.
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}
