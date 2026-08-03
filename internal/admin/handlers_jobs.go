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

type jobsSnapshotResponse struct {
	Scanner     jobsScanner          `json:"scanner"`
	Analysis    jobsAnalysis         `json:"analysis"`
	Fingerprint *FingerprintJobState `json:"fingerprint,omitempty"`
	Enrichment  jobsEnrichment       `json:"enrichment"`
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
	if root := s.deps.BackupSources.DataDir; root != "" {
		// Newest snapshot's timestamp — covers snapshots from before
		// this process started (the recorder is process-lifetime only).
		if entries, err := backup.List(filepath.Join(root, backup.BackupsDirName)); err == nil && len(entries) > 0 {
			t := entries[0].CreatedAt
			resp.Backups.LastBackupAt = &t
		}
	}

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
