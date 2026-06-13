// Package analyze owns the bridge's offline audio analysis: decoding a
// source file to PCM via sox(1), reducing it to a compact peak waveform
// sidecar, and the worker pool the `bridge analyze` CLI and the
// serve-side background pass share.
//
// **Why shell out to sox** — same rationale as internal/transcode:
// keeping the bridge pure-Go preserves single-host cross-compilation to
// all target platforms. sox is already the optional, feature-gated
// dependency the upscaling feature installs; analysis reuses it, so it
// adds no new posture (missing sox ⇒ the feature degrades off, the rest
// of the server runs unchanged).
//
// **Bit-exact mission preserved**: analysis only READS samples to derive
// metadata (a waveform sidecar today; loudness / key / tempo in a later
// phase). It never alters what /v1/download serves — the original file
// streams byte-for-byte as before.
//
// Phase 1 computes the waveform only. The decode target is 48 kHz mono
// (load-bearing for the Phase-2 loudness path: BS.1770 K-weighting
// coefficients are defined for 48 kHz; harmless for peaks).
package analyze

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("analyze")

const (
	// AnalysisSampleRate is the mono PCM rate sox decodes to.
	AnalysisSampleRate = 48000

	// waveformBucketSamples is the fixed time-width peak bucket (0.1 s
	// at 48 kHz). Time-width — not a fixed bucket count — because the
	// streaming decode doesn't know the track length up front.
	waveformBucketSamples = AnalysisSampleRate / 10

	// WaveformSchemaVersion stamps the analysis row. Bump when the
	// decode params or the sidecar binary format change so a prior
	// sidecar is recognised as stale (re-analyzed) without a migration.
	WaveformSchemaVersion = "wf1"

	// WaveformDirSubdir is the fixed subdir under cfg.DataDir where
	// waveform sidecars land (source-path-mirrored beneath it).
	WaveformDirSubdir = "waveforms"

	waveformMagic         = "1BWF"
	waveformFormatVersion = 1
	waveformHeaderLen     = 22
	// maxWaveformBuckets bounds peaker memory against a pathologically
	// long input (≈27 h at 0.1 s buckets). Real tracks are far below it.
	maxWaveformBuckets = 1_000_000
)

// WaveformDirFor returns the absolute directory waveform sidecars are
// written under, given the bridge's dataDir. Pure path arithmetic.
func WaveformDirFor(dataDir string) string {
	return filepath.Join(dataDir, WaveformDirSubdir)
}

// AnalyzeSpec describes one source-to-waveform analysis job. The
// SourceMTimeNS / SourceSize are captured at enqueue time and stored on
// the row so the scan-skip gate and the serving-time freshness check
// can detect a drifted source.
type AnalyzeSpec struct {
	SourceAbsPath    string
	SourceLibraryRel string
	SourceMTimeNS    int64
	SourceSize       int64
	OutputDir        string // <dataDir>/waveforms
}

// SidecarPath returns the absolute on-disk path for this spec's
// waveform sidecar in the source-path-mirrored layout
// (<OutputDir>/<libRel-dir>/<base>.waveform.bin). Mirrors the layout
// internal/transcode uses for variants so operators see one consistent
// on-disk shape.
func (s AnalyzeSpec) SidecarPath() string {
	dir := filepath.Dir(s.SourceLibraryRel)
	base := filepath.Base(s.SourceLibraryRel)
	filename := safeAnalysisFilename(base)
	if dir == "" || dir == "." {
		return filepath.Join(s.OutputDir, filename)
	}
	return filepath.Join(s.OutputDir, dir, filename)
}

// Result is the outcome of a completed analysis job — the written
// sidecar's path, content tag (8 hex of its SHA-256, the iOS cache
// key), size, and the schema version that produced it.
type Result struct {
	WaveformPath  string
	WaveformTag   string
	WaveformSize  int64
	SchemaVersion string
}

// RunAnalysis decodes the source via sox, computes the peak waveform,
// and writes the sidecar atomically (tmp + rename), returning its path,
// content tag, and size. The pool fsyncs the sidecar before committing
// the DB row.
//
// **Commit only on clean decode**: a truncated / corrupt file makes sox
// exit non-zero mid-stream; decodePCM surfaces that as an error and
// RunAnalysis writes nothing (the cleanup defer reaps the tmp on every
// non-success path), so the caller never persists a waveform covering
// only the first N seconds. Cancellation flows via ctx →
// exec.CommandContext (decodePCM kills + reaps sox).
func RunAnalysis(ctx context.Context, spec AnalyzeSpec) (Result, error) {
	finalPath := spec.SidecarPath()
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		return Result{}, fmt.Errorf("mkdir waveform dir: %w", err)
	}
	tmpPath := finalPath + analysisTmpSuffix
	// Clear any stale tmp from a prior interrupted run.
	_ = os.Remove(tmpPath)
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	pk := newPeaker(waveformBucketSamples)
	total, err := decodePCM(ctx, spec.SourceAbsPath, pk.add)
	if err != nil {
		return Result{}, err
	}
	pk.finish()
	data := encodeWaveform(pk, AnalysisSampleRate, waveformBucketSamples, total)

	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return Result{}, fmt.Errorf("write waveform tmp: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return Result{}, fmt.Errorf("rename waveform: %w", err)
	}
	cleanup = false
	logger.Debug("analyze ok",
		"path", spec.SourceLibraryRel,
		"buckets", pk.count(),
		"bytes", len(data))
	return Result{
		WaveformPath:  finalPath,
		WaveformTag:   waveformTag(data),
		WaveformSize:  int64(len(data)),
		SchemaVersion: WaveformSchemaVersion,
	}, nil
}
