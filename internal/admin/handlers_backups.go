package admin

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// What the console answers a listing or a snapshot that failed, by what went
// wrong (backupsListRefusal, snapshotRefusal): each says it in words that
// name no path, and what to do. The error itself names the backups folder
// or a file the snapshot copies, the config file among them, and goes to the
// log, where the bridge's own files may be named (B604's second round: the
// body was the error's text).
const (
	errMsgBackupsNotPermitted = "the bridge is not allowed to read its backups folder: give the user the bridge runs as " +
		"read access to the backups folder in its data directory"
	errMsgBackupsNotListed = "the bridge could not list its backups, and the bridge's log says why"
	errMsgSnapshotBusy     = "a file the backup copies stayed busy (another program, such as an antivirus scan, " +
		"had it open), so no snapshot was kept: run the backup again"
	errMsgSnapshotNotPermitted = "the bridge is not allowed to read a file it backs up, or to write in its backups " +
		"folder, so no snapshot was kept: give the user the bridge runs as access to its data directory and its " +
		"config file, then run the backup again"
	errMsgSnapshotStopped = "the backup stopped before it finished (the bridge is shutting down, or the request " +
		"went away), so no snapshot was kept: run the backup again"
	errMsgSnapshotFailed = "the bridge could not take the snapshot and kept none of it: the bridge's log says why, " +
		"and what to do"
)

// backupsListRefusal is what the console says of err, a listing of the
// backups folder that failed: a permission it lacks, or, for anything else,
// read the log.
func backupsListRefusal(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return errMsgBackupsNotPermitted
	}
	return errMsgBackupsNotListed
}

// snapshotRefusal is what the console says of err, a snapshot that failed:
// a file held busy past the read's retries (asked first: on Windows a busy
// read's last error is an access denial, which is also a permission's), a
// stop before the end, a permission it lacks, or, for anything else, read
// the log, which also carries the advice a refusal of the player's identity
// gives (`bridge reset-player`).
func snapshotRefusal(err error) string {
	switch {
	case errors.Is(err, atomicwrite.ErrReadBusy):
		return errMsgSnapshotBusy
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errMsgSnapshotStopped
	case errors.Is(err, fs.ErrPermission):
		return errMsgSnapshotNotPermitted
	}
	return errMsgSnapshotFailed
}

// backupsListResponse is the shape returned by GET /api/backups —
// newest-first list of snapshot summaries, each with the two verdicts of
// its verification record (backlog B653), read from the manifests alone,
// never a verification and never the live database; and apart, the newest
// snapshot written and the newest whose database is verified. It names
// each snapshot by its folder's name, a time stamp, and no path. Restore
// stays CLI-only by design (the snapshot bundle contains the TLS private
// key, so a "download backup" button would be a needlessly attractive
// credential extraction surface).
type backupsListResponse struct {
	Backups []backupSummary `json:"backups"`
	// Newest and NewestVerified name the newest snapshot written and the
	// newest whose database's verdict is VERIFIED; omitted where there is
	// none.
	Newest         *backupRef `json:"newest,omitempty"`
	NewestVerified *backupRef `json:"newestVerified,omitempty"`
	SchemaVersion  int        `json:"schemaVersion"`
	// Sensitivity is duplicated from `backup.SensitivityNotice` so
	// the admin dashboard can surface the same wording the CLI does.
	Sensitivity string `json:"sensitivity"`
}

// backupRef names one snapshot: its folder's name and when it was written.
type backupRef struct {
	DirName   string `json:"dirName"`
	CreatedAt string `json:"createdAt"`
}

// backupSummary is one snapshot and its record's two verdicts, each with
// its reason codes (backup.Reasons), the database's with its check's level
// and time, and the files that failed by their names in the snapshot.
type backupSummary struct {
	DirName         string   `json:"dirName"`
	BridgeVersion   string   `json:"bridgeVersion"`
	CreatedAt       string   `json:"createdAt"`
	Files           []string `json:"files"`
	DatabaseState   string   `json:"databaseState"`
	DatabaseReasons []string `json:"databaseReasons"`
	Level           string   `json:"level,omitempty"`
	CheckedAt       string   `json:"checkedAt,omitempty"`
	FilesState      string   `json:"filesState"`
	FilesReasons    []string `json:"filesReasons"`
	FilesFailed     []string `json:"filesFailed"`
	// Legacy says the snapshot was written before snapshots were verified.
	Legacy bool `json:"legacy"`
}

// wireTime is t as the console's backup payloads write a time.
func wireTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// summarize is s's snapshots as GET /api/backups answers them.
func summarize(st backup.Status) backupsListResponse {
	out := make([]backupSummary, 0, len(st.Snapshots))
	for _, sn := range st.Snapshots {
		v := sn.Verdict
		sum := backupSummary{
			DirName:         sn.DirName,
			BridgeVersion:   sn.BridgeVersion,
			CreatedAt:       wireTime(sn.CreatedAt),
			Files:           sn.Files,
			DatabaseState:   string(v.DatabaseState),
			DatabaseReasons: reasonCodes(v.DatabaseReasons),
			Level:           string(v.Level),
			FilesState:      string(v.FilesState),
			FilesReasons:    reasonCodes(v.FilesReasons),
			FilesFailed:     []string{},
			Legacy:          sn.Legacy,
		}
		if !v.CheckedAt.IsZero() {
			sum.CheckedAt = wireTime(v.CheckedAt)
		}
		for _, f := range v.FilesFailed {
			if f.Name != "" {
				sum.FilesFailed = append(sum.FilesFailed, f.Name)
			}
		}
		out = append(out, sum)
	}
	resp := backupsListResponse{Backups: out, SchemaVersion: backup.SchemaVersion, Sensitivity: backup.SensitivityNotice}
	if sn, ok := st.Newest(); ok {
		resp.Newest = &backupRef{DirName: sn.DirName, CreatedAt: wireTime(sn.CreatedAt)}
	}
	if sn, ok := st.NewestVerified(); ok {
		resp.NewestVerified = &backupRef{DirName: sn.DirName, CreatedAt: wireTime(sn.CreatedAt)}
	}
	return resp
}

// reasonCodes is reasons as the wire carries them, [] for none.
func reasonCodes(reasons []backup.Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, string(r))
	}
	return out
}

// apiBackupsList: GET /api/backups
//
// Lists every snapshot under `<dataDir>/backups/` newest-first, with its
// verdicts. Returns a clean empty list if the dir doesn't yet exist.
func (s *Server) apiBackupsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.BackupSources.DataDir == "" {
		writeError(w, http.StatusServiceUnavailable, "backup-not-wired",
			"admin server constructed without backup sources")
		return
	}
	root := filepath.Join(s.deps.BackupSources.DataDir, backup.BackupsDirName)
	st, err := backup.ReadStatus(r.Context(), root)
	if err != nil {
		logger.Error("admin: the backups could not be listed", "err", err)
		writeError(w, http.StatusInternalServerError, "list-failed", backupsListRefusal(err))
		return
	}
	writeJSON(w, http.StatusOK, summarize(st))
}

// pruneWarningText renders the warning for a completed snapshot whose
// prune reported something, which the console's verification job logs
// (verifyInBackground).
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

// apiBackupsCreate: POST /api/backups
//
// Triggers an on-demand snapshot, and answers 201 once it is written, its
// verification PENDING, naming it by its folder's name and no path: the
// verification and the prune after it run as a job (verifyInBackground).
// The optional `keep` field in the JSON body overrides the configured
// retention for this snapshot only — a missing value falls back to
// `EffectiveKeep()`.
//
// Note: this handler does NOT implement download/export — see the
// list-response comment for why. Operators who need to move
// snapshots offsite use `scp`/`rsync` against `<dataDir>/backups/`.
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

	// The capture uses the request context so a slow VACUUM doesn't
	// outlive a client disconnect. Its verification and the prune after it
	// do not (backlog B653): they run as a job on the server's own context
	// (verifyInBackground), so a request that goes away once its snapshot
	// is written leaves the verdict to come as it would have come.
	src := s.deps.BackupSources
	dst, err := backup.Snapshot(r.Context(), src)
	if err != nil {
		logger.Error("admin: no snapshot was taken", "err", err)
		writeError(w, http.StatusInternalServerError, "snapshot-failed", snapshotRefusal(err))
		return
	}
	// The jobs card caches what the backups folder holds; drop it so the
	// operator who just pressed this sees their snapshot on the next poll
	// rather than up to a TTL later. The job drops it again once its verdict
	// is written.
	s.invalidateLastBackup()

	keep := cfg.Backup.EffectiveKeep()
	if body.Keep != nil {
		keep = *body.Keep
	}
	s.verifyInBackground(dst, keep)
	writeJSON(w, http.StatusCreated, map[string]any{
		"dirName":      filepath.Base(dst),
		"verification": backup.StatePending,
		"sensitivity":  backup.SensitivityNotice,
	})
}

// verifyInBackground runs the kept snapshot in dir's verification and the
// prune after it (backup.Finish) as a job on the server's own context
// (Deps.ScanCtx, which serve's shutdown cancels), never the request's,
// joined in ServeListener's drain with the background scans: queued behind
// this process's other verifications while one runs (backup's slot), and on
// a shutdown its verdict, INCOMPLETE, is written within
// backup.StopWriteBound, so no snapshot is left PENDING with no one to finish
// it. The prune runs whatever the verdict, with keep, the create's
// retention, unconditionally (keep <= 0 disables the keep-policy, not the
// crash-orphan sweep). What it found goes to the log, naming the snapshot
// by its folder's name.
func (s *Server) verifyInBackground(dir string, keep int) {
	ctx := s.scanCtx()
	s.bgBackups.Add(1)
	go func() {
		defer s.bgBackups.Done()
		defer s.invalidateLastBackup()
		res := backup.Finish(ctx, dir, func() int { return keep }, backup.VerifyOptions{})
		name := filepath.Base(dir)
		v := res.Verify.Record
		switch {
		case res.VerifyErr != nil && ctx.Err() != nil:
		case res.VerifyErr != nil:
			logger.Warn("admin: a snapshot's verdict was not recorded", "snapshot", name, "err", res.VerifyErr)
		case v.Passes():
			logger.Info("admin: snapshot verified", "snapshot", name, "database", backup.StateWords(v.DatabaseState, v.DatabaseReasons),
				"files", backup.FilesWords(v))
		default:
			logger.Warn("admin: a snapshot is not a verified recovery point", "snapshot", name,
				"database", backup.StateWords(v.DatabaseState, v.DatabaseReasons), "files", backup.FilesWords(v))
		}
		if warning := pruneWarningText(res.PruneErr, res.Prune.ReapErr); warning != "" && ctx.Err() == nil {
			logger.Warn("admin: the prune after a snapshot reported a problem", "snapshot", name, "warning", warning)
		}
	}()
}

// WaitForBackupJobs waits for every snapshot verification the console
// started, for a test that reads its verdict.
func (s *Server) WaitForBackupJobs() { s.bgBackups.Wait() }
