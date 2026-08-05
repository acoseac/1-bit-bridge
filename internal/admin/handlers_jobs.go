package admin

// GET /api/jobs — the Jobs page's aggregated background-activity
// snapshot. One polled endpoint (10 s browser chain, page-visible only)
// carrying the slow-moving per-job state: gates, cadences, last/next
// runs, and the analysis coverage split. LIVE counters deliberately do
// NOT ride here — they arrive on the SSE events the page already
// receives (stats → scanner progress, enrichment, analysis → pool +
// sweep, upscale → batches/worker grid, updates), so this handler stays
// cheap and diff-free-by-construction.
//
// Every section degrades independently: a nil closure omits its
// field(s) rather than failing the response — the Jobs page renders
// whatever the bridge actually runs.

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// analysisCoverageCacheTTL bounds how often the coverage query runs
// under polling. Cheap plain-column SQL, but N tabs × 10 s polls have
// no business re-running it each time.
const analysisCoverageCacheTTL = 30 * time.Second

// lastBackupCacheTTL bounds how often the snapshot directory is walked
// for the jobs card. Matched to analysisCoverageCacheTTL — both back the
// same 10s-polled endpoint, and a backup timestamp moves far more slowly
// than a scan does. An operator-triggered snapshot doesn't wait for it:
// invalidateLastBackup clears the entry directly.
const lastBackupCacheTTL = 30 * time.Second

// backupListTimeout bounds one snapshot listing. Larger than
// snapshotDBTimeout because this is filesystem work whose cost scales
// with the number of retained snapshots — and with `backup.keep <= 0`
// the create path skips Prune, so that number has no ceiling.
const backupListTimeout = 5 * time.Second

// jobsScanner — the library scanner card. NextScanDue derives from
// LastFullScan + interval (RunPeriodic's static ticker cadence);
// omitted until a first scan has completed.
type jobsScanner struct {
	IntervalSec    int        `json:"intervalSec"`
	IsScanning     bool       `json:"isScanning"`
	LastFullScan   *time.Time `json:"lastFullScan,omitempty"`
	NextScanDue    *time.Time `json:"nextScanDue,omitempty"`
	WatcherEnabled bool       `json:"watcherEnabled"`
}

// jobsAnalysisCoverage is the analysed-vs-eligible split. Eligible =
// TotalLocal - DSDExcluded - ZeroByteExcluded. APPROXIMATE by design
// (scan-time sizes; mtime-stale rows count as analysed) — the sweep
// recorder's last-run counts are the exact per-run truth, and the UI
// shows both. DSD is a permanent by-design exclusion (sox can't decode
// 1-bit DSD), not a backlog — the whole reason this tile exists.
type jobsAnalysisCoverage struct {
	Eligible         int `json:"eligible"`
	Analysed         int `json:"analysed"`
	Stale            int `json:"stale,omitempty"`
	DSDExcluded      int `json:"dsdExcluded"`
	ZeroByteExcluded int `json:"zeroByteExcluded"`
	TotalLocal       int `json:"totalLocal"`
}

// jobsAnalysis — the audio-analysis card. Sweep/Coverage omitted when
// the feature machinery is off; DegradedReason explains an
// enabled-but-inactive state (sox missing at startup).
type jobsAnalysis struct {
	Enabled        bool                  `json:"enabled"`
	Active         bool                  `json:"active"`
	DegradedReason string                `json:"degradedReason,omitempty"`
	IntervalSec    int                   `json:"intervalSec"`
	Sweep          *AnalysisSweepState   `json:"sweep,omitempty"`
	Coverage       *jobsAnalysisCoverage `json:"coverage,omitempty"`
}

// jobsEnrichment — gates only; live pending/matched/missing counts ride
// the existing SSE `enrichment` event.
type jobsEnrichment struct {
	Source        string `json:"source"`
	HarvestActive bool   `json:"harvestActive"`
}

type jobsSmartMixes struct {
	Enabled          bool         `json:"enabled"`
	IntervalSec      int          `json:"intervalSec,omitempty"`
	AnalysisAssisted bool         `json:"analysisAssisted"`
	Run              *JobRunState `json:"run,omitempty"`
}

type jobsBackups struct {
	// IntervalHours 0 = ticker disabled by the operator (on-demand
	// snapshots stay available).
	IntervalHours int          `json:"intervalHours"`
	Keep          int          `json:"keep"`
	LastBackupAt  *time.Time   `json:"lastBackupAt,omitempty"`
	Run           *JobRunState `json:"run,omitempty"`
}

type jobsUpdates struct {
	CheckIntervalHours int  `json:"checkIntervalHours"`
	AutoInstall        bool `json:"autoInstall"`
}

// jobsMaintenance — display-only flags for the low-key maintenance
// sweepers (config/wiring-derived; no runtime plumbing by design).
type jobsMaintenance struct {
	VariantIntegrityActive bool `json:"variantIntegrityActive"`
	OrphanSidecarGC        bool `json:"orphanSidecarGC"`
	ArtworkCacheLRU        bool `json:"artworkCacheLRU"`
}

type jobsUPnP struct {
	Enabled           bool `json:"enabled"`
	ConfiguredServers int  `json:"configuredServers"`
}

// jobsDuplicates — the duplicate-stamping card: live policy, the last
// pass's headline numbers (from the persisted summary), and the
// on-demand sweeper's run state. Cadence is implicit (every full scan's
// tail) so there is no interval field.
type jobsDuplicates struct {
	Policy     string       `json:"policy"`
	Stamped    bool         `json:"stamped"`
	Groups     int          `json:"groups"`
	Suppressed int          `json:"suppressed"`
	StampedAt  *time.Time   `json:"stampedAt,omitempty"`
	Run        *JobRunState `json:"run,omitempty"`
}

type jobsSnapshotResponse struct {
	Scanner     jobsScanner          `json:"scanner"`
	Analysis    jobsAnalysis         `json:"analysis"`
	Fingerprint *FingerprintJobState `json:"fingerprint,omitempty"`
	Enrichment  jobsEnrichment       `json:"enrichment"`
	Duplicates  jobsDuplicates       `json:"duplicates"`
	SmartMixes  jobsSmartMixes       `json:"smartMixes"`
	Backups     jobsBackups          `json:"backups"`
	Updates     jobsUpdates          `json:"updates"`
	Maintenance jobsMaintenance      `json:"maintenance"`
	UPnP        jobsUPnP             `json:"upnp"`
}

// apiJobs: GET /api/jobs
func (s *Server) apiJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.getJobsSnapshot(r.Context()))
}

func (s *Server) getJobsSnapshot(ctx context.Context) jobsSnapshotResponse {
	cfg := s.deps.CfgHolder.Load()
	var resp jobsSnapshotResponse

	// Scanner.
	resp.Scanner = jobsScanner{
		IntervalSec:    cfg.ScanIntervalSec,
		WatcherEnabled: cfg.LibraryWatch.Enabled,
	}
	if sc := s.deps.Scanner; sc != nil {
		resp.Scanner.IsScanning = sc.IsScanning()
		if last := sc.LastFullScan(); !last.IsZero() {
			l := last
			resp.Scanner.LastFullScan = &l
			if cfg.ScanIntervalSec > 0 {
				due := last.Add(time.Duration(cfg.ScanIntervalSec) * time.Second)
				resp.Scanner.NextScanDue = &due
			}
		}
	}

	// Audio analysis.
	resp.Analysis = jobsAnalysis{
		Enabled:     cfg.Analysis.Enabled,
		IntervalSec: cfg.ScanIntervalSec, // sweeper rides the scan cadence
	}
	if a := s.deps.AnalysisActive; a != nil {
		resp.Analysis.Active = a()
	}
	if resp.Analysis.Enabled && !resp.Analysis.Active {
		resp.Analysis.DegradedReason = "sox_missing"
	}
	if sw := s.deps.AnalysisSweep; sw != nil {
		resp.Analysis.Sweep = sw()
	}
	if resp.Analysis.Active {
		resp.Analysis.Coverage = s.getAnalysisCoverage(ctx)
	}

	// Fingerprint.
	if fp := s.deps.FingerprintState; fp != nil {
		resp.Fingerprint = fp()
	}

	// Enrichment (always-on worker; the card links to Settings for the
	// source picker and shows live counts from the SSE event).
	// HarvestActive is CONFIG-derived — the HarvestForceSubmit closure
	// is wired unconditionally (it returns false when the client isn't
	// running), so closure presence is not a signal here.
	resp.Enrichment.Source, _ = deriveEnrichSource(cfg.Enrich.MusicBrainzBaseURL, cfg.Enrich.CoverArtBaseURL)
	resp.Enrichment.HarvestActive = cfg.Atlas.Enabled && cfg.Atlas.HarvestEnabled

	// Duplicates stamping. Policy is live config; the headline numbers
	// come from the persisted summary (one scan_state row — cheap on
	// the 10 s poll). A load failure degrades to the policy-only card.
	resp.Duplicates = jobsDuplicates{Policy: resolvedDuplicatesFilter(cfg)}
	if sum, derr := s.deps.Manifest.LoadDupeSummary(ctx); derr == nil && sum != nil {
		resp.Duplicates.Stamped = true
		resp.Duplicates.Groups = sum.Groups
		resp.Duplicates.Suppressed = sum.Suppressed
		st := sum.StampedAt
		resp.Duplicates.StampedAt = &st
	}
	if run := s.deps.DuplicatesSweepRun; run != nil {
		resp.Duplicates.Run = run()
	}

	// Smart mixes.
	resp.SmartMixes = jobsSmartMixes{
		Enabled:          cfg.SmartPlaylists.Enabled,
		AnalysisAssisted: resp.Analysis.Active,
	}
	if cfg.SmartPlaylists.Enabled {
		resp.SmartMixes.IntervalSec = int(cfg.SmartPlaylists.EffectiveRegenerateInterval() / time.Second)
	}
	if run := s.deps.SmartMixRun; run != nil {
		resp.SmartMixes.Run = run()
	}

	// Backups.
	resp.Backups = jobsBackups{
		IntervalHours: cfg.Backup.EffectiveIntervalHours(),
		Keep:          cfg.Backup.EffectiveKeep(),
	}
	if run := s.deps.BackupRun; run != nil {
		resp.Backups.Run = run()
	}
	// Newest snapshot's timestamp — covers snapshots from before this
	// process started (the recorder is process-lifetime only). Cached:
	// see getLastBackupAt.
	resp.Backups.LastBackupAt = s.getLastBackupAt(ctx)

	// Updates. CheckIntervalHours 0 means "updater default" (6 h,
	// updater.DefaultCheckInterval) — resolve it here so the card never
	// shows a literal 0 for a running poller.
	resp.Updates = jobsUpdates{
		CheckIntervalHours: cfg.Update.CheckIntervalHours,
		AutoInstall:        cfg.Update.AutoInstall,
	}
	if resp.Updates.CheckIntervalHours <= 0 {
		resp.Updates.CheckIntervalHours = 6
	}

	// Maintenance (display-only).
	upscaleActive := s.deps.UpscaleStats != nil && s.deps.UpscaleStats() != nil
	resp.Maintenance = jobsMaintenance{
		VariantIntegrityActive: upscaleActive && cfg.VariantSweepInterval() > 0,
		OrphanSidecarGC:        upscaleActive && cfg.OrphanSidecarSweepInterval() > 0,
		ArtworkCacheLRU:        cfg.Artwork.CacheMaxBytes > 0,
	}

	// UPnP ingest (trigger + detail live on the UPnP page).
	resp.UPnP = jobsUPnP{
		Enabled:           cfg.UPnPUpstream.Enabled,
		ConfiguredServers: len(cfg.UPnPUpstream.Servers),
	}
	return resp
}

// getLastBackupAt returns the TTL-cached timestamp of the newest
// snapshot, single-flighted so concurrent polls collapse to one listing.
//
// backup.List walks every snapshot directory and reads a manifest out of
// each. /api/jobs is polled every 10s per open admin tab, so doing that
// inline — as this did — put unbounded, uncached filesystem work on a
// hot polling path, right beside a database query the same handler was
// careful to bound at 2s. It is also unbounded in the case that matters
// most: with `backup.keep <= 0` the create path skips Prune entirely, so
// snapshots accumulate without limit and the listing grows with them.
//
// Nil means "no snapshots, or we couldn't tell" — the card just omits
// the line, which is the pre-existing behaviour on a listing error.
func (s *Server) getLastBackupAt(ctx context.Context) *time.Time {
	root := s.deps.BackupSources.DataDir
	if root == "" {
		return nil
	}
	s.lastBackupMu.Lock()
	if !s.lastBackupAt.IsZero() && time.Since(s.lastBackupAt) < lastBackupCacheTTL {
		v := s.lastBackupAtVal
		s.lastBackupMu.Unlock()
		return v
	}
	s.lastBackupMu.Unlock()

	v, _, _ := s.lastBackupSF.Do("lastBackup", func() (any, error) {
		s.lastBackupMu.Lock()
		if !s.lastBackupAt.IsZero() && time.Since(s.lastBackupAt) < lastBackupCacheTTL {
			val := s.lastBackupAtVal
			s.lastBackupMu.Unlock()
			return val, nil
		}
		s.lastBackupMu.Unlock()

		// Detached from the request ctx: the result is shared by every
		// queued caller, so one client's hang-up must not synthesize a
		// failure for the rest (the PR #373 singleflight rule).
		listCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backupListTimeout)
		defer cancel()
		entries, err := backup.ListContext(listCtx, filepath.Join(root, backup.BackupsDirName))
		if err != nil {
			logger.Warn("jobs: backup list", "err", err)
			s.lastBackupMu.Lock()
			val := s.lastBackupAtVal // last good (possibly nil)
			// Stamp on failure too, so a slow or failing listing backs
			// off for a TTL instead of being retried on every poll.
			s.lastBackupAt = time.Now()
			s.lastBackupMu.Unlock()
			return val, nil
		}
		var newest *time.Time
		if len(entries) > 0 {
			t := entries[0].CreatedAt
			newest = &t
		}
		s.lastBackupMu.Lock()
		s.lastBackupAtVal = newest
		s.lastBackupAt = time.Now()
		s.lastBackupMu.Unlock()
		return newest, nil
	})
	if t, ok := v.(*time.Time); ok {
		return t
	}
	return nil
}

// invalidateLastBackup drops the cached snapshot timestamp so the next
// poll re-reads it. Called after an operator-triggered snapshot: waiting
// out a TTL there would trade "why is the jobs page slow" for "did my
// backup actually work", and the second question is the one that gets
// answered with refresh-spam.
//
// A snapshot taken by the SCHEDULER is deliberately not hooked: nobody is
// watching for it, so TTL-bounded staleness is fine.
func (s *Server) invalidateLastBackup() {
	s.lastBackupMu.Lock()
	s.lastBackupAt = time.Time{}
	s.lastBackupMu.Unlock()
}

// getAnalysisCoverage returns the TTL-cached analysed-vs-eligible
// split, single-flighted so concurrent polls collapse to one query.
// Serves last-good (possibly nil) on query failure — the tile just
// hides. Mirrors getCompositionSnapshot's shape.
func (s *Server) getAnalysisCoverage(ctx context.Context) *jobsAnalysisCoverage {
	if s.deps.Manifest == nil || s.deps.AnalysisSchemaVersion == "" {
		return nil
	}
	s.analysisCoverageMu.Lock()
	if !s.analysisCoverageAt.IsZero() && time.Since(s.analysisCoverageAt) < analysisCoverageCacheTTL {
		snap := s.analysisCoverage
		s.analysisCoverageMu.Unlock()
		return snap
	}
	s.analysisCoverageMu.Unlock()
	v, _, _ := s.analysisCoverageSF.Do("coverage", func() (any, error) {
		s.analysisCoverageMu.Lock()
		if !s.analysisCoverageAt.IsZero() && time.Since(s.analysisCoverageAt) < analysisCoverageCacheTTL {
			snap := s.analysisCoverage
			s.analysisCoverageMu.Unlock()
			return snap, nil
		}
		s.analysisCoverageMu.Unlock()
		// Detached from the request ctx: the result is shared by every
		// queued caller, so one client's hang-up must not synthesize a
		// failure for the rest (the PR #373 singleflight rule).
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotDBTimeout)
		defer cancel()
		cov, err := s.deps.Manifest.AnalysisCoverage(dbCtx, s.deps.AnalysisSchemaVersion)
		if err != nil {
			logger.Warn("jobs: analysis coverage", "err", err)
			s.analysisCoverageMu.Lock()
			snap := s.analysisCoverage // last good (possibly nil)
			// Stamp on failure too. Without this the TTL never trips
			// after an error, so every 10s poll re-runs a full-table
			// scan that is already failing — most likely because it is
			// slow, which is exactly when hammering it is worst. The
			// cache then backs off for a TTL and retries once.
			s.analysisCoverageAt = time.Now()
			s.analysisCoverageMu.Unlock()
			return snap, nil
		}
		snap := &jobsAnalysisCoverage{
			Eligible:         cov.TotalLocal - cov.DSDExcluded - cov.ZeroByteExcluded,
			Analysed:         cov.AnalysedFresh,
			Stale:            cov.AnalysedStale,
			DSDExcluded:      cov.DSDExcluded,
			ZeroByteExcluded: cov.ZeroByteExcluded,
			TotalLocal:       cov.TotalLocal,
		}
		s.analysisCoverageMu.Lock()
		s.analysisCoverage = snap
		s.analysisCoverageAt = time.Now()
		s.analysisCoverageMu.Unlock()
		return snap, nil
	})
	if snap, ok := v.(*jobsAnalysisCoverage); ok {
		return snap
	}
	return nil
}

// apiFingerprintSweep: POST /api/fingerprint/sweep — the fingerprint
// twin of apiAnalysisSweep. 202 = queued (nudge coalesces; honored
// after the sweeper's settle window), 503 = feature inactive.
func (s *Server) apiFingerprintSweep(w http.ResponseWriter, _ *http.Request) {
	trigger := s.deps.TriggerFingerprintSweep
	if trigger == nil {
		writeError(w, http.StatusServiceUnavailable, "fingerprint_unavailable", "acoustic fingerprinting is not active on this bridge")
		return
	}
	trigger()
	writeJSON(w, http.StatusAccepted, map[string]bool{"triggered": true})
}
