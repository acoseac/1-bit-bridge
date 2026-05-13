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
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// --- response shapes ---

// browseFolderRow is the JSON shape for one folder entry in
// /api/library/browse responses.
type browseFolderRow struct {
	Name              string `json:"name"`
	Path              string `json:"path"`
	TrackCount        int    `json:"trackCount"`
	UpscaledCount     int    `json:"upscaledCount"`
	TotalSizeBytes    int64  `json:"totalSizeBytes"`
	UpscaledSizeBytes int64  `json:"upscaledSizeBytes"`
}

// browseTrackRow is the JSON shape for one track entry. Pointer
// fields preserve the "tag absent" vs "tag = 0" distinction that
// the manifest's `Track` wire shape already documents — same
// convention.
type browseTrackRow struct {
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	SizeBytes     int64    `json:"sizeBytes"`
	SampleRate    *float64 `json:"sampleRate,omitempty"`
	BitsPerSample *int     `json:"bitsPerSample,omitempty"`
	Codec         string   `json:"codec,omitempty"`
	IsDSD         *bool    `json:"isDSD,omitempty"`
	IsUpscaled    bool     `json:"isUpscaled"`
}

// browseResponse is the JSON envelope returned by
// GET /api/library/browse.
type browseResponse struct {
	// Path is the normalised library-relative path the response
	// describes; echoed back so the UI can verify what it asked
	// for (especially helpful for traversal-rejected requests
	// that landed on the empty-path branch).
	Path    string            `json:"path"`
	Folders []browseFolderRow `json:"folders"`
	Tracks  []browseTrackRow  `json:"tracks"`
}

// browseProjectionResponse is the JSON envelope returned by
// GET /api/library/browse-projection. Numbers are bytes;
// `wouldFit` carries the verdict against the bridge dataDir
// volume's free space using `transcode.DefaultDiskSafetyMargin`.
type browseProjectionResponse struct {
	Path                    string `json:"path"`
	ProjectedFiles          int    `json:"projectedFiles"`
	AlreadyCoveredFiles     int    `json:"alreadyCoveredFiles"`
	ProjectedSizeBytes      int64  `json:"projectedSizeBytes"`
	AvailableBytes          int64  `json:"availableBytes"`
	WouldFit                bool   `json:"wouldFit"`
	TargetRate              int    `json:"targetRate"`
	TargetBits              int    `json:"targetBits"`
	UnknownFormatFiles      int    `json:"unknownFormatFiles"`
	RequiredBytesWithMargin int64  `json:"requiredBytesWithMargin"`
}

// --- handlers ---

// apiLibraryBrowse handles GET /api/library/browse?path=...
//
// Returns one level of children under `path`. Empty / missing
// `path` returns the top-level folders + any root-level tracks.
func (s *Server) apiLibraryBrowse(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.Query().Get("path")
	normalised, ok := normaliseBrowsePath(rawPath)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path",
			"path contains traversal segments or is otherwise invalid")
		return
	}
	folders, err := s.deps.Manifest.ListChildFolders(r.Context(), normalised)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "browse-folders", err.Error())
		return
	}
	tracks, err := s.deps.Manifest.ListChildTracks(r.Context(), normalised)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "browse-tracks", err.Error())
		return
	}
	resp := browseResponse{
		Path:    normalised,
		Folders: make([]browseFolderRow, 0, len(folders)),
		Tracks:  make([]browseTrackRow, 0, len(tracks)),
	}
	for _, f := range folders {
		resp.Folders = append(resp.Folders, browseFolderRow{
			Name:              path.Base(f.Path),
			Path:              f.Path,
			TrackCount:        f.TrackCount,
			UpscaledCount:     f.UpscaledTrackCount,
			TotalSizeBytes:    f.TotalSizeBytes,
			UpscaledSizeBytes: f.UpscaledSizeBytes,
		})
	}
	for _, t := range tracks {
		resp.Tracks = append(resp.Tracks, browseTrackRow{
			Name:          path.Base(t.Path),
			Path:          t.Path,
			SizeBytes:     t.Size,
			SampleRate:    t.SampleRate,
			BitsPerSample: t.BitsPerSample,
			Codec:         t.Codec,
			IsDSD:         t.IsDSD,
			IsUpscaled:    t.IsUpscaled,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// apiLibraryBrowseProjection handles
// GET /api/library/browse-projection?path=...
//
// Walks every track under `path`, computes per-track projected
// size via the wired closure (transcode.ProjectedSize), probes
// disk space on the bridge's data volume, and returns the
// pre-flight verdict the Library Inspector renders in the action
// drawer.
//
// Tracks that ALREADY have a variant at the active target are
// counted separately (`alreadyCoveredFiles`) and excluded from
// the projection — the operator's pre-flight reflects the work
// that WOULD happen if they hit "Upscale this folder," not the
// theoretical max.
func (s *Server) apiLibraryBrowseProjection(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	rawPath := r.URL.Query().Get("path")
	normalised, ok := normaliseBrowsePath(rawPath)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path",
			"path contains traversal segments or is otherwise invalid")
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
	// Resolve active target: DB-backed setting wins; YAML
	// bootstrap is the fall-back for unseeded installs (will be
	// the case until PR 3's coordinator runs first-boot seed).
	rate, bits, err := s.deps.Manifest.GetUpscaleTarget(r.Context())
	if err != nil && !errors.Is(err, manifest.ErrUpscaleTargetUnset) {
		writeError(w, http.StatusInternalServerError, "read-target", err.Error())
		return
	}
	if errors.Is(err, manifest.ErrUpscaleTargetUnset) {
		rate = cfg.Upscale.EffectiveBootstrapTargetRate()
		bits = cfg.Upscale.EffectiveBootstrapTargetBits()
	}

	projections, err := s.deps.Manifest.ListTrackProjectionsUnderPrefix(r.Context(), normalised)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list-projections", err.Error())
		return
	}

	var (
		totalProjected int64
		projectedFiles int
		coveredFiles   int
		unknownFormat  int
	)
	for _, t := range projections {
		if t.HasVariant {
			coveredFiles++
			continue
		}
		if t.SampleRate <= 0 || t.BitsPerSample <= 0 {
			// Extractor couldn't determine source format — surface
			// these separately so the operator sees them but they
			// don't contribute zero/garbage to the projection.
			unknownFormat++
			continue
		}
		size := s.deps.ProjectedSize(t.Size, t.SampleRate, t.BitsPerSample, rate, bits)
		if size <= 0 {
			continue
		}
		totalProjected += size
		projectedFiles++
	}

	free, err := s.deps.AvailableDiskSpace(cfg.DataDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "disk-probe", err.Error())
		return
	}
	// Required bytes with the project-default safety margin (10%).
	// Mirrors transcode.DiskHasHeadroom's math, kept in lockstep
	// at the wire boundary so the UI's "X GB needed" copy matches
	// what the coordinator will refuse at Submit time.
	required := int64(float64(totalProjected) * 1.10)
	writeJSON(w, http.StatusOK, browseProjectionResponse{
		Path:                    normalised,
		ProjectedFiles:          projectedFiles,
		AlreadyCoveredFiles:     coveredFiles,
		ProjectedSizeBytes:      totalProjected,
		AvailableBytes:          free,
		WouldFit:                required <= free,
		TargetRate:              rate,
		TargetBits:              bits,
		UnknownFormatFiles:      unknownFormat,
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
