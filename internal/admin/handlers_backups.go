package admin

import (
	"net/http"
	"path/filepath"
	"strings"

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
// pruneWarningText renders the operator-facing warning for a completed
// snapshot whose prune reported something.
//
// Both conditions are surfaced when BOTH occur. They are independent —
// `PruneResult.ReapErr` is split out from the error return precisely
// because an un-classifiable orphan directory is permanent and is not a
// failure of the keep-policy prune — so returning on the first one would
// hide the other. Neither is fatal here: the snapshot already landed, and
// the operator can re-prune from the CLI.
func pruneWarningText(pruneErr, reapErr error) string {
	var warnings []string
	if pruneErr != nil {
		warnings = append(warnings, pruneErr.Error())
	}
	if reapErr != nil {
		warnings = append(warnings, "orphan sweep: "+reapErr.Error())
	}
	return strings.Join(warnings, "; ")
}

func (s *Server) apiBackupsCreate(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	if s.deps.BackupSources.DataDir == "" {
		writeError(w, http.StatusServiceUnavailable, "backup-not-wired",
			"admin server constructed without backup sources")
		return
	}
	var body struct {
		Keep *int `json:"keep,omitempty"`
	}
	// Use the shared optional-body helper rather than gating on
	// r.ContentLength — chunked uploads commonly arrive with
	// ContentLength == -1 even when a real body is present, and the
	// gate would silently drop the `keep` override (CodeRabbit on PR
	// #99). The helper internally treats io.EOF as "no body" and is
	// already wrapped with the 1 MiB MaxBytesReader cap from the
	// admin-wide PR-#99 hardening pass.
	if !decodeOptionalJSONBody(w, r, &body) {
		return
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
	// The jobs card caches the newest snapshot's timestamp; drop it so
	// the operator who just pressed this sees their snapshot on the next
	// poll rather than up to a TTL later.
	s.invalidateLastBackup()

	keep := cfg.Backup.EffectiveKeep()
	if body.Keep != nil {
		keep = *body.Keep
	}
	// Unconditional (keep <= 0 disables the keep-policy, not the
	// crash-orphan sweep) and ctx-bound so a browser disconnect doesn't
	// leave the prune running — same rationale as backupCmd / the ticker.
	res, pruneErr := backup.PruneContext(r.Context(), filepath.Join(src.DataDir, backup.BackupsDirName), keep)
	if warning := pruneWarningText(pruneErr, res.ReapErr); warning != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"snapshotDir":  dst,
			"pruneWarning": warning,
		})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"snapshotDir": dst,
		"sensitivity": backup.SensitivityNotice,
	})
}
