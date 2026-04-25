package admin

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// backupsListResponse is the shape returned by GET /api/backups —
// newest-first list of snapshot summaries. The dashboard's Backups
// card renders these as a table; restore stays CLI-only by design
// (the snapshot bundle contains the TLS private key, so a "download
// backup" button would be a needlessly attractive credential
// extraction surface).
type backupsListResponse struct {
	Backups       []backupSummary `json:"backups"`
	BackupsRoot   string          `json:"backupsRoot"`
	SchemaVersion int             `json:"schemaVersion"`
	// Sensitivity is duplicated from `backup.SensitivityNotice` so
	// the admin dashboard can surface the same wording the CLI does.
	Sensitivity string `json:"sensitivity"`
}

type backupSummary struct {
	DirName       string   `json:"dirName"`
	BridgeVersion string   `json:"bridgeVersion"`
	CreatedAt     string   `json:"createdAt"`
	Files         []string `json:"files"`
}

// apiBackupsList: GET /api/backups
//
// Lists every snapshot under `<dataDir>/backups/` newest-first.
// Returns a clean empty list if the dir doesn't yet exist.
func (s *Server) apiBackupsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.BackupSources.DataDir == "" {
		writeError(w, http.StatusServiceUnavailable, "backup-not-wired",
			"admin server constructed without backup sources")
		return
	}
	root := filepath.Join(s.deps.BackupSources.DataDir, backup.BackupsDirName)
	entries, err := backup.List(root)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list-failed", err.Error())
		return
	}
	out := make([]backupSummary, 0, len(entries))
	for _, e := range entries {
		out = append(out, backupSummary{
			DirName:       e.DirName,
			BridgeVersion: e.BridgeVersion,
			CreatedAt:     e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			Files:         e.Files,
		})
	}
	writeJSON(w, http.StatusOK, backupsListResponse{
		Backups:       out,
		BackupsRoot:   root,
		SchemaVersion: backup.SchemaVersion,
		Sensitivity:   backup.SensitivityNotice,
	})
}

// apiBackupsCreate: POST /api/backups
//
// Triggers an on-demand snapshot. The optional `keep` field in the
// JSON body overrides the configured retention for this snapshot
// only — a missing value falls back to `EffectiveKeep()`.
//
// Note: this handler does NOT implement download/export — see the
// list-response comment for why. Operators who need to move
// snapshots offsite use `scp`/`rsync` against `<dataDir>/backups/`.
func (s *Server) apiBackupsCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.BackupSources.DataDir == "" {
		writeError(w, http.StatusServiceUnavailable, "backup-not-wired",
			"admin server constructed without backup sources")
		return
	}
	var body struct {
		Keep *int `json:"keep,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad-json", err.Error())
			return
		}
	}

	// Snapshot uses the request context so a slow VACUUM doesn't
	// outlive a client disconnect. The CLI path uses Background()
	// because there's no parent scope to inherit there.
	src := s.deps.BackupSources
	dst, err := backup.Snapshot(r.Context(), src)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot-failed", err.Error())
		return
	}

	keep := s.deps.Cfg.Backup.EffectiveKeep()
	if body.Keep != nil {
		keep = *body.Keep
	}
	if keep > 0 {
		if _, err := backup.Prune(filepath.Join(src.DataDir, backup.BackupsDirName), keep); err != nil {
			// Snapshot already landed; surface the prune error but
			// don't roll back the snapshot — operator can re-prune
			// from CLI.
			writeJSON(w, http.StatusOK, map[string]any{
				"snapshotDir":  dst,
				"pruneWarning": err.Error(),
			})
			return
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"snapshotDir": dst,
		"sensitivity": backup.SensitivityNotice,
	})
}
