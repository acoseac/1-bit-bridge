package admin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upload"
)

// Upload API.
//
// Every route is admin-surface only: loopback installs inherit the historical
// no-auth posture (anything on the host can already write to the library
// directly), and public installs are gated by sessionMiddleware. None of this
// is on /v1 — the paired-device bearer is read-scoped by design, and the demo
// bridge's static token ships inside every installed app, so an upload endpoint
// there would be an open file-drop.
//
// Client-declared PATHS never appear in a URL. The client gets opaque ids and
// the paths live in the session manifest, which sidesteps the "+ decodes to a
// space" class outright (url.Values form-decoding, the documented /v1
// variant-delete trap) and means a hostile relative path cannot create anything
// anywhere until commit, where it is validated exactly once.

// --- wire DTOs -------------------------------------------------------------

type uploadFileDeclDTO struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Digest string `json:"digest,omitempty"`
}

type uploadCreateRequest struct {
	Root      string              `json:"root,omitempty"`
	Overwrite bool                `json:"overwrite,omitempty"`
	MaxBytes  int64               `json:"maxBytes,omitempty"`
	Files     []uploadFileDeclDTO `json:"files"`
}

type uploadFileDTO struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Offset   int64  `json:"offset"`
	SHA256   string `json:"sha256,omitempty"`
	Complete bool   `json:"complete"`
	// DuplicateOf names an existing library path this file looks like, by
	// (basename, size). A HINT the UI presents — never a decision. A track
	// legitimately on both an album and a compilation is a real library, and
	// that is serve-time duplicate suppression's job, not upload's.
	DuplicateOf string `json:"duplicateOf,omitempty"`
}

type uploadRejectedDTO struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type uploadSessionDTO struct {
	ID         string          `json:"id"`
	Root       string          `json:"root"`
	CreatedAt  string          `json:"createdAt"`
	Overwrite  bool            `json:"overwrite"`
	ChunkBytes int64           `json:"chunkBytes"`
	Files      []uploadFileDTO `json:"files"`
	// Rejected lists declared files the session would not accept. Reported,
	// not fatal — see upload.RejectedFile.
	Rejected []uploadRejectedDTO `json:"rejected,omitempty"`
}

type uploadChunkDTO struct {
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
	Complete bool   `json:"complete"`
	SHA256   string `json:"sha256,omitempty"`
}

type uploadCommitOutcomeDTO struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type uploadCommitDTO struct {
	Committed int                      `json:"committed"`
	Skipped   int                      `json:"skipped"`
	Failed    int                      `json:"failed"`
	Outcomes  []uploadCommitOutcomeDTO `json:"outcomes"`
	ScanDirs  []string                 `json:"scanDirs"`
	FullScan  bool                     `json:"fullScan"`
}

// --- shared helpers --------------------------------------------------------

func (s *Server) uploadManager(w http.ResponseWriter) (*upload.Manager, bool) {
	cfg := s.deps.CfgHolder.Load()
	if cfg == nil || !cfg.Upload.Enabled {
		writeError(w, http.StatusForbidden, "upload_disabled",
			"uploads are turned off; enable them in Settings")
		return nil, false
	}
	if s.deps.Upload == nil {
		writeError(w, http.StatusServiceUnavailable, "upload_unavailable",
			"the upload subsystem is not wired on this bridge")
		return nil, false
	}
	return s.deps.Upload, true
}

// writeUploadError maps the package sentinels onto status codes.
func writeUploadError(w http.ResponseWriter, err error) {
	var mm *upload.OffsetMismatch
	if errors.As(err, &mm) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "offset_mismatch",
			"message": "the server holds a different offset for this file",
			"offset":  mm.Actual,
		})
		return
	}
	var ns *upload.NoSpace
	if errors.As(err, &ns) {
		// reclaimableBytes is what lets the UI turn a dead end into "empty
		// trash and resume" rather than just reporting failure.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInsufficientStorage)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":            "insufficient_storage",
			"message":          "not enough free space for this upload",
			"freeBytes":        ns.FreeBytes,
			"neededBytes":      ns.NeedBytes,
			"reclaimableBytes": ns.ReclaimableBytes,
		})
		return
	}
	switch {
	case errors.Is(err, upload.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such upload session")
	case errors.Is(err, upload.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, upload.ErrDigestMismatch):
		writeError(w, http.StatusBadRequest, "digest_mismatch", err.Error())
	case errors.Is(err, upload.ErrLibraryNotWritable):
		// 503, not 500: the bridge is fine, the deployment cannot accept
		// uploads as configured — the same shape as an unwired subsystem. The
		// message carries the cause and the remedy, because the alternative is
		// an operator reading "upload failed" while the real reason sits in a
		// log they have no reason to open.
		writeError(w, http.StatusServiceUnavailable, "library_read_only", err.Error())
	case errors.Is(err, upload.ErrInvalidPath), errors.Is(err, upload.ErrUnknownRoot):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		logger.Error("upload request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "upload failed")
	}
}

func uploadSessionDTOOf(s *upload.Session, dupes map[string]string) uploadSessionDTO {
	out := uploadSessionDTO{
		ID:         s.ID,
		Root:       s.RootName,
		CreatedAt:  s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Overwrite:  s.Overwrite,
		ChunkBytes: s.ChunkBytes,
		Files:      make([]uploadFileDTO, 0, len(s.Files)),
	}
	for _, r := range s.Rejected {
		out.Rejected = append(out.Rejected, uploadRejectedDTO{Path: r.Path, Reason: r.Reason})
	}
	for _, f := range s.Files {
		out.Files = append(out.Files, uploadFileDTO{
			ID:          f.ID,
			Path:        f.Path,
			Size:        f.Size,
			Offset:      f.Offset,
			SHA256:      f.SHA256,
			Complete:    f.Complete,
			DuplicateOf: dupes[manifest.UploadDupeKey(path.Base(f.Path), f.Size)],
		})
	}
	return out
}

// --- POST /api/upload/sessions ---------------------------------------------

func (s *Server) apiUploadCreate(w http.ResponseWriter, r *http.Request) {
	m, ok := s.uploadManager(w)
	if !ok {
		return
	}
	var req uploadCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	decls := make([]upload.FileDecl, 0, len(req.Files))
	for _, f := range req.Files {
		decls = append(decls, upload.FileDecl{Path: f.Path, Size: f.Size, Digest: f.Digest})
	}
	sess, err := m.Create(decls, upload.CreateOptions{
		Root: req.Root, Overwrite: req.Overwrite, MaxBytes: req.MaxBytes,
	})
	if err != nil {
		writeUploadError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, uploadSessionDTOOf(sess, s.uploadDuplicateHints(r, sess)))
}

// uploadDuplicateHints looks for existing tracks that match an incoming file by
// (basename, size).
//
// This is the answer to the overlapping-upload case: uploading an album folder
// and then an artist folder containing it lands two copies at DIFFERENT paths,
// so nothing collides, both survive on disk, and — because the winner election
// tie-breaks on the SHALLOWER path — the flat copy wins and the properly nested
// one is suppressed. Warning up front is the only place that can be prevented.
//
// Failure is non-fatal: a hint the operator does not get is strictly better
// than a session they cannot start.
func (s *Server) uploadDuplicateHints(r *http.Request, sess *upload.Session) map[string]string {
	if s.deps.Manifest == nil || len(sess.Files) == 0 {
		return nil
	}
	sizes := make([]int64, 0, len(sess.Files))
	want := make(map[string][]string, len(sess.Files))
	seenSize := make(map[int64]struct{}, len(sess.Files))
	for _, f := range sess.Files {
		if f.Size <= 0 {
			continue
		}
		if _, dup := seenSize[f.Size]; !dup {
			seenSize[f.Size] = struct{}{}
			sizes = append(sizes, f.Size)
		}
		k := manifest.UploadDupeKey(path.Base(f.Path), f.Size)
		want[k] = append(want[k], f.ID)
	}
	refs, err := s.deps.Manifest.FindTracksByBasenameAndSize(r.Context(), sizes, want)
	if err != nil {
		logger.Warn("upload duplicate pre-flight", "err", err)
		return nil
	}
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		if _, exists := out[ref.Key]; !exists {
			out[ref.Key] = ref.Path
		}
	}
	return out
}

// --- GET /api/upload/sessions ----------------------------------------------

func (s *Server) apiUploadList(w http.ResponseWriter, r *http.Request) {
	m, ok := s.uploadManager(w)
	if !ok {
		return
	}
	sessions, err := m.List()
	if err != nil {
		writeUploadError(w, err)
		return
	}
	out := make([]uploadSessionDTO, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, uploadSessionDTOOf(sess, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

// --- GET /api/upload/sessions/{sid} -----------------------------------------

func (s *Server) apiUploadStatus(w http.ResponseWriter, r *http.Request) {
	m, ok := s.uploadManager(w)
	if !ok {
		return
	}
	sess, err := m.Get(r.PathValue("sid"))
	if err != nil {
		writeUploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, uploadSessionDTOOf(sess, nil))
}

// --- PUT /api/upload/sessions/{sid}/files/{fid} -----------------------------

func (s *Server) apiUploadChunk(w http.ResponseWriter, r *http.Request) {
	m, ok := s.uploadManager(w)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "bad_offset", "offset must be a non-negative integer")
		return
	}
	digest, err := parseContentDigestSHA256(r.Header.Get("Content-Digest"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_digest", err.Error())
		return
	}

	// The rolling read deadline is what lets a large chunk outlive the admin
	// server's 30s ReadTimeout while a genuinely stalled client still dies.
	body := newUploadBodyReader(w, r.Body)

	next, err := m.WriteChunk(r.PathValue("sid"), r.PathValue("fid"), offset, body, digest, r.ContentLength)
	if err != nil {
		writeUploadError(w, err)
		return
	}
	sess, err := m.Get(r.PathValue("sid"))
	if err != nil {
		writeUploadError(w, err)
		return
	}
	out := uploadChunkDTO{Offset: next}
	for _, f := range sess.Files {
		if f.ID == r.PathValue("fid") {
			out.Size, out.Complete, out.SHA256 = f.Size, f.Complete, f.SHA256
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// parseContentDigestSHA256 reads an RFC 9530 Content-Digest header.
//
// The field is a structured dictionary whose values are byte sequences:
//
//	Content-Digest: sha-256=:BASE64:
//
// Only the sha-256 member is understood; other algorithms are ignored rather
// than refused, which is what the RFC asks of a receiver. The older `Digest:`
// header is deprecated by RFC 9530 and deliberately not accepted — don't "fix"
// this backward.
func parseContentDigestSHA256(h string) ([]byte, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return nil, nil
	}
	for _, member := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(member), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "sha-256") {
			continue
		}
		v = strings.TrimSpace(v)
		// A structured-field member may carry parameters (RFC 8941 §3.1.2):
		// `sha-256=:base64:;q=1`. Without stripping them the byte-sequence
		// check below sees a trailing `1` and rejects a well-formed header.
		if before, _, found := strings.Cut(v, ";"); found {
			v = strings.TrimSpace(before)
		}
		if len(v) < 2 || v[0] != ':' || v[len(v)-1] != ':' {
			return nil, errors.New("Content-Digest sha-256 value must be a byte sequence (:base64:)")
		}
		raw, err := base64.StdEncoding.DecodeString(v[1 : len(v)-1])
		if err != nil {
			return nil, errors.New("Content-Digest sha-256 value is not valid base64")
		}
		if len(raw) != 32 {
			return nil, errors.New("Content-Digest sha-256 value is not 32 bytes")
		}
		return raw, nil
	}
	return nil, nil
}

// --- POST /api/upload/sessions/{sid}/commit ---------------------------------

func (s *Server) apiUploadCommit(w http.ResponseWriter, r *http.Request) {
	m, ok := s.uploadManager(w)
	if !ok {
		return
	}
	res, err := m.Commit(r.PathValue("sid"))
	if err != nil {
		writeUploadError(w, err)
		return
	}
	out := uploadCommitDTO{
		Committed: res.Committed,
		Skipped:   res.Skipped,
		Failed:    res.Failed,
		Outcomes:  make([]uploadCommitOutcomeDTO, 0, len(res.Outcomes)),
	}
	for _, o := range res.Outcomes {
		out.Outcomes = append(out.Outcomes, uploadCommitOutcomeDTO{
			Path: o.Path, Status: o.Status, Reason: o.Reason,
		})
	}
	if res.Committed > 0 {
		dirs, full := planScanDirs(res.ScanDirs, maxSubtreeScans)
		out.ScanDirs, out.FullScan = dirs, full
		if full {
			s.spawnBackgroundScan("post-upload scan")
		} else {
			s.spawnBackgroundSubtreeScan("post-upload scan", res.Root, dirs)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- DELETE /api/upload/sessions/{sid} --------------------------------------

func (s *Server) apiUploadAbandon(w http.ResponseWriter, r *http.Request) {
	m, ok := s.uploadManager(w)
	if !ok {
		return
	}
	if err := m.Abandon(r.PathValue("sid")); err != nil {
		writeUploadError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
