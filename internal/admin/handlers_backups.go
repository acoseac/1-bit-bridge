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
	DirName       string `json:"dirName"`
	BridgeVersion string `json:"bridgeVersion"`
	CreatedAt     string `json:"createdAt"`
	Files         []string `json:"files"`
}

// apiBackupsList: GET /api/backups
//
// Lists every snapshot under `<dataDir>/backups/` newest-first.
// Returns a clean empty list if the dir doesn't yet exist.
func (s *Server) apiBackupsList(w http.ResponseWriter, r *http.Request) {
	root := filepath.Join(s.deps.Cfg.DataDir, backup.BackupsDirName)
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
// JSON body overrides `cfg.Backup.Keep` for this snapshot only — a
// zero or missing value falls back to the configured default.
//
// Note: this handler does NOT implement download/export — see the
// list-response comment for why. Operators who need to move
// snapshots offsite use `scp`/`rsync` against `<dataDir>/backups/`.
func (s *Server) apiBackupsCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keep *int `json:"keep,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad-json", err.Error())
			return
		}
	}

	src := s.backupSourcesForAdmin()
	dst, err := backup.Snapshot(src)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "snapshot-failed", err.Error())
		return
	}

	keep := s.deps.Cfg.Backup.Keep
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

// backupSourcesForAdmin builds the file-path set the admin handler
// passes to `backup.Snapshot`. Mirrors `cmd/bridge/backup.go`'s
// `buildBackupSources` but stays inside the admin package so we
// don't have to import cmd/. The two functions have to walk the
// same fields; see the test in admin_backups_test.go.
func (s *Server) backupSourcesForAdmin() backup.Sources {
	cfg := s.deps.Cfg
	certPath, keyPath := cfg.TLSCertPath, cfg.TLSKeyPath
	if certPath == "" || keyPath == "" {
		// Hard-code the default paths here rather than importing
		// internal/tls — the admin package staying free of the TLS
		// import keeps the dependency graph one-direction (cmd
		// imports both, neither imports the other).
		certPath = filepath.Join(cfg.DataDir, "server.crt")
		keyPath = filepath.Join(cfg.DataDir, "server.key")
	}
	return backup.Sources{
		DataDir:    cfg.DataDir,
		ManifestDB: filepath.Join(cfg.DataDir, "bridge.db"),
		TokensJSON: filepath.Join(cfg.DataDir, "tokens.json"),
		ServerCert: certPath,
		ServerKey:  keyPath,
		BridgeYAML: s.deps.CfgPath,
	}
}
