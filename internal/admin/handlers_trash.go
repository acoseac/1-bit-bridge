package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/trash"
)

// Trash API — the console's delete surface.
//
// This is the first thing in the bridge that removes library content, so it is
// gated separately from uploads (library.allowDelete, default off) and it does
// not unlink: files move to <root>/.bridge-trash/<stamp>/ and stay recoverable
// for the TTL. See internal/trash for why.

type trashEntryDTO struct {
	ID           string `json:"id"`
	OriginalPath string `json:"originalPath"`
	Size         int64  `json:"size"`
	TrashedAt    string `json:"trashedAt"`
	ExpiresAt    string `json:"expiresAt"`
}

type trashOutcomeDTO struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

type trashResultDTO struct {
	OK       int               `json:"ok"`
	Failed   int               `json:"failed"`
	Bytes    int64             `json:"bytes"`
	Outcomes []trashOutcomeDTO `json:"outcomes"`
}

type trashPathsRequest struct {
	Root  string   `json:"root,omitempty"`
	Paths []string `json:"paths"`
}

type trashIDsRequest struct {
	IDs []string `json:"ids"`
}

func (s *Server) trashManager(w http.ResponseWriter) (*trash.Manager, bool) {
	cfg := s.deps.CfgHolder.Load()
	if cfg == nil || !cfg.Library.AllowDelete {
		writeError(w, http.StatusForbidden, "delete_disabled",
			"deleting is turned off; enable it in Settings")
		return nil, false
	}
	if s.deps.TrashManager == nil {
		writeError(w, http.StatusServiceUnavailable, "trash_unavailable",
			"the trash subsystem is not wired on this bridge")
		return nil, false
	}
	return s.deps.TrashManager, true
}

func writeTrashError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, trash.ErrDisabled):
		writeError(w, http.StatusForbidden, "delete_disabled", err.Error())
	case errors.Is(err, trash.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, trash.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, trash.ErrRootUnavailable):
		writeError(w, http.StatusServiceUnavailable, "trash_unavailable", err.Error())
	default:
		logger.Error("trash request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "the request failed")
	}
}

// refuseRoutedPaths splits a delete batch into the paths this bridge owns and
// an itemized refusal for the ones it does not.
//
// The console hides Delete on a routed row, which is the half a person sees;
// this is the half that holds, because the endpoint is a plain authenticated
// request. A routing-lookup FAILURE refuses too — "we could not tell" must not
// resolve to "go ahead and delete", which is the standing rule everywhere a
// classification gates a deletion in this tree.
func (s *Server) refuseRoutedPaths(ctx context.Context, paths []string) (local []string, refused []trashOutcomeDTO) {
	if s.deps.Manifest == nil {
		return paths, nil
	}
	for _, p := range paths {
		rt, err := s.deps.Manifest.GetUPnPRouting(ctx, p)
		switch {
		case err != nil:
			logger.Error("trash: routing lookup", "path", p, "err", err)
			refused = append(refused, trashOutcomeDTO{
				Path: p, Status: "failed",
				Reason: "could not tell whether this track is on an upstream server",
			})
		case rt != nil:
			refused = append(refused, trashOutcomeDTO{
				Path: p, Status: "failed",
				Reason: "this track lives on an upstream server; delete it there",
			})
		default:
			local = append(local, p)
		}
	}
	return local, refused
}

func trashResultDTOOf(res *trash.Result) trashResultDTO {
	out := trashResultDTO{
		OK: res.OK, Failed: res.Failed, Bytes: res.Bytes,
		Outcomes: make([]trashOutcomeDTO, 0, len(res.Outcomes)),
	}
	for _, o := range res.Outcomes {
		out.Outcomes = append(out.Outcomes, trashOutcomeDTO{
			Path: o.Path, Status: o.Status, Reason: o.Reason, Bytes: o.Bytes,
		})
	}
	return out
}

// retireAndRescan retires the rows for changed paths and rescans their folders.
//
// Retirement is IMMEDIATE (threshold 1), not the missing-count debounce: an
// explicit operator delete should not linger for three scans. That path already
// unlinks sidecars and writes manifest_deletions tombstones, so synced clients
// drop the tracks too — no new deletion machinery.
func (s *Server) retireAndRescan(r *http.Request, label string, paths, dirs []string) {
	if len(paths) > 0 && s.deps.Manifest != nil {
		if _, err := s.deps.Manifest.IncrementMissingTracksAndDeleteAtThreshold(r.Context(), paths, 1); err != nil {
			logger.Error("retire trashed rows", "err", err)
		}
	}
	if len(dirs) == 0 {
		return
	}
	if scanDirs, full := planScanDirs(dirs, maxSubtreeScans); full {
		s.spawnBackgroundScan(label)
	} else {
		// Resolved, not joined onto one root: a delete batch can span library
		// roots, and `dirs` is manifest-form, which already names the root.
		s.spawnBackgroundSubtreeScanResolved(label, scanDirs)
	}
}

// --- POST /api/library/trash ---

func (s *Server) apiTrashAdd(w http.ResponseWriter, r *http.Request) {
	m, ok := s.trashManager(w)
	if !ok {
		return
	}
	var req trashPathsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "no paths given")
		return
	}
	// A routed track's bytes live on an upstream UPnP server, and its
	// "path" is a DIDL container path that means nothing on this filesystem.
	// Refused per-path rather than for the whole batch: an album can hold
	// both, and the local half should still be deleted.
	local, refused := s.refuseRoutedPaths(r.Context(), req.Paths)
	res, err := m.Trash(req.Root, local)
	if err != nil {
		writeTrashError(w, err)
		return
	}
	s.retireAndRescan(r, "post-delete scan", res.Paths, res.Dirs)
	dto := trashResultDTOOf(res)
	dto.Outcomes = append(dto.Outcomes, refused...)
	dto.Failed += len(refused)
	writeJSON(w, http.StatusOK, dto)
}

// --- GET /api/library/trash ---

func (s *Server) apiTrashList(w http.ResponseWriter, r *http.Request) {
	m, ok := s.trashManager(w)
	if !ok {
		return
	}
	entries, err := m.List()
	if err != nil {
		writeTrashError(w, err)
		return
	}
	out := make([]trashEntryDTO, 0, len(entries))
	for _, e := range entries {
		out = append(out, trashEntryDTO{
			ID:           e.ID,
			OriginalPath: e.OriginalPath,
			Size:         e.Size,
			TrashedAt:    e.TrashedAt.UTC().Format(time.RFC3339),
			ExpiresAt:    e.TrashedAt.Add(m.TTL()).UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- POST /api/library/trash/restore ---

func (s *Server) apiTrashRestore(w http.ResponseWriter, r *http.Request) {
	m, ok := s.trashManager(w)
	if !ok {
		return
	}
	var req trashIDsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "no entries given")
		return
	}
	res, err := m.Restore(req.IDs)
	if err != nil {
		writeTrashError(w, err)
		return
	}
	// A restored file is new to the manifest again; the subtree scan indexes
	// it. Nothing to retire.
	if len(res.Dirs) > 0 {
		s.retireAndRescan(r, "post-restore scan", nil, res.Dirs)
	}
	writeJSON(w, http.StatusOK, trashResultDTOOf(res))
}

// --- DELETE /api/library/trash ---

func (s *Server) apiTrashPurge(w http.ResponseWriter, r *http.Request) {
	m, ok := s.trashManager(w)
	if !ok {
		return
	}
	var req trashIDsRequest
	// A bodyless DELETE means "empty the trash", which is the only action
	// that actually frees space. decodeOptional: csrfGuard already lets an
	// empty body through, so absence must not be an error here.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
	}
	res, err := m.Purge(req.IDs)
	if err != nil {
		writeTrashError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, trashResultDTOOf(res))
}
