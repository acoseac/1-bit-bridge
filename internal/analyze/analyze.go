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
// metadata (a waveform sidecar, EBU R128 loudness, and estimated key +
// tempo). It never alters what /v1/download serves — the original file
// streams byte-for-byte as before.
//
// One decode per track at 48 kHz and the SOURCE channel count feeds both
// the peak envelope (mono downmix) and the loudness meter (channel-aware
// R128). 48 kHz is load-bearing for loudness — the BS.1770 K-weighting
// coefficients are defined for 48 kHz — and decoding at the source
// channel count avoids the +3..+6 dB bias a mono downmix imposes on
// multichannel loudness.
package analyze

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
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
	// sidecar is recognised as stale (re-analyzed) without a migration —
	// the scan-skip gate re-enqueues any row whose stamp differs.
	//
	// wf1 → wf2: the decode moved from forced mono (`-c 1`) to the source
	// channel count so EBU R128 loudness is measured channel-aware (a
	// mono downmix reads several dB hot). The 1BWF sidecar BYTES are
	// unchanged — the peak envelope still downmixes to mono, byte-
	// identical to wf1 for mono/stereo sources — so iOS keeps the same
	// `waveformTag` and does NOT re-fetch the sidecar; the bump exists
	// only to trigger the one-time loudness backfill on already-analyzed
	// libraries (a row re-analyzes once, gains its ReplayGain scalar, and
	// stamps wf2 so it isn't re-enqueued even when loudness is
	// unavailable — silence / unprobeable channel layout).
	//
	// wf2 → wf3: the same decode now also estimates musical key
	// (Krumhansl-Schmuckler) + tempo (onset autocorrelation) off the mono
	// downmix. The waveform bytes AND the loudness value are unchanged
	// (key/tempo only ADD consumption of the existing mono stream), so
	// again iOS re-fetches no sidecars and existing rows re-analyze once
	// to backfill key/tempo — stamping wf3 so an un-estimable track (too
	// short / atonal / arrhythmic) isn't re-enqueued forever.
	//
	// wf3 → wf4: the same decode additionally measures true peak
	// (BS.1770-style 4x oversampled, of the 48 kHz analysis rendering)
	// and the community DR score off the interleaved frames, and FLAC
	// sources get an audio-MD5 verification pass against STREAMINFO
	// (a second, native-depth decode of the same file — see flacmd5.go).
	// Waveform bytes, loudness, key and tempo are all unchanged, so iOS
	// re-fetches no sidecars; existing rows re-analyze once to backfill
	// the three scalars and stamp wf4 (an unverifiable file — no stored
	// checksum, odd bit depth — isn't re-enqueued forever).
	WaveformSchemaVersion = "wf4"

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
// key), size, the schema version that produced it, and the signal-
// derived loudness.
type Result struct {
	WaveformPath  string
	WaveformTag   string
	WaveformSize  int64
	SchemaVersion string

	// ReplayGainTrackDB is the EBU R128 / ReplayGain 2.0 track gain in dB
	// (the gain that brings the program to the -18 LUFS reference). Valid
	// only when HasLoudness is true — set when sox reported the channel
	// layout (so loudness wasn't measured off a biased mono downmix) and
	// the program wasn't silence. The caller surfaces it on the wire only
	// when the source carries no ReplayGain tag.
	ReplayGainTrackDB float64
	HasLoudness       bool

	// KeyRoot / KeyMode are the estimated musical key (Krumhansl-
	// Schmuckler): KeyRoot is the tonic 0..11 (C=0), KeyMode is
	// "major"/"minor". Both nil/"" when the estimator saw too little
	// signal. Best-effort — surfaced as an "estimated" key.
	KeyRoot *int
	KeyMode string

	// BPM is the estimated tempo (onset autocorrelation), or nil when no
	// confident estimate. Surfaced only when the source has no BPM tag.
	BPM *int

	// TruePeakDB is the BS.1770-style 4x-oversampled true peak in dB
	// relative to full scale — of the 48 kHz ANALYSIS RENDERING (the
	// package's one-decode invariant; see truepeak.go's honesty note).
	// nil when the program was silence or nothing decoded.
	TruePeakDB *float64

	// DRScore is the community DR (dynamic range) value — the "DR12"
	// convention. nil when the program is too short for the statistic
	// (< ~9 s) or silent. See dr.go.
	DRScore *int

	// AudioMD5State is "" (not verifiable / not FLAC), "verified"
	// (decoded audio matches the STREAMINFO checksum) or "mismatch"
	// (clean decode, different hash — file modified or corrupt). FLAC
	// only; see flacmd5.go for the failure direction.
	AudioMD5State string

	// AudioMD5Retryable qualifies an EMPTY AudioMD5State: true means we
	// could not ask (pipe/spawn failure, a faulted read, a killed
	// child), false means we asked and this file cannot be verified.
	// Meaningless when AudioMD5State is set.
	//
	// Store.UpsertAnalysis turns this into a capped attempt counter, so
	// "could not ask" gets a bounded number of further chances while
	// "cannot be verified" gets none. Without the distinction a
	// one-second I/O blip permanently recorded a healthy file as
	// unverifiable: the row commits with the schema stamp either way,
	// and the scan-skip gate then never looks again.
	AudioMD5Retryable bool
}

// RunAnalysis decodes the source via sox, computes the peak waveform +
// EBU R128 loudness, and writes the sidecar atomically (tmp + rename),
// returning its path, content tag, size, and loudness. The pool fsyncs
// the sidecar before committing the DB row.
//
// **Commit only on clean decode**: a truncated / corrupt file makes sox
// exit non-zero mid-stream; decodeFrames surfaces that as an error and
// RunAnalysis writes nothing (the cleanup defer reaps the tmp on every
// non-success path), so the caller never persists a waveform covering
// only the first N seconds. Cancellation flows via ctx →
// exec.CommandContext (decodeFrames kills + reaps sox).
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

	// One decode at the source channel count feeds both consumers: the
	// peaker (mono downmix — a visual envelope) and the loudness meter
	// (full interleaved frame — channel-aware R128). channelsOK is false
	// when sox couldn't report the layout; we still decode (mono) for the
	// waveform but skip loudness so a biased value is never stored.
	channels, channelsOK, tool, expectedSec := probeChannels(ctx, spec.SourceAbsPath)
	pk := newPeaker(waveformBucketSamples)
	// Allocate the loudness meter only when the channel layout is known — it
	// holds a multi-second sliding K-weighting window (~150 KB), and every
	// meter access below is already channelsOK-gated, so an unknown layout
	// discards the result anyway (Gemini PR #516).
	var meter *loudnessMeter
	if channelsOK {
		meter = newLoudnessMeter(channels)
	}
	kt := newKeyTempoAnalyzer()
	// True peak + DR ride the same interleaved frames regardless of
	// channelsOK — unlike loudness, neither depends on channel SEMANTICS
	// (true peak is a max over every decoded sample; DR averages its
	// per-channel statistic), so a defaulted layout still measures
	// honestly.
	tp := newTruePeakMeter(channels)
	dr := newDRMeter(channels)
	total, err := decodeFrames(ctx, spec.SourceAbsPath, channels, tool, expectedSec, func(frame []float64) {
		mono := downmixFrame(frame)
		pk.add(mono)
		if channelsOK {
			// Skip the per-sample K-weighting biquads when the channel layout
			// is unknown — the loudness result is discarded below in that case.
			meter.addFrame(frame)
		}
		kt.add(mono)
		tp.addFrame(frame)
		dr.addFrame(frame)
	})
	if err != nil {
		return Result{}, err
	}
	pk.finish()
	data := encodeWaveform(pk, AnalysisSampleRate, waveformBucketSamples, total)

	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return Result{}, fmt.Errorf("write waveform tmp: %w", err)
	}
	if err := atomicwrite.RenameWithRetryCtx(ctx, tmpPath, finalPath); err != nil {
		return Result{}, fmt.Errorf("rename waveform: %w", err)
	}
	cleanup = false

	res := Result{
		WaveformPath:  finalPath,
		WaveformTag:   waveformTag(data),
		WaveformSize:  int64(len(data)),
		SchemaVersion: WaveformSchemaVersion,
	}
	if channelsOK {
		if rg, ok := replayGainFromLUFS(meter.integratedLUFS()); ok {
			res.ReplayGainTrackDB = rg
			res.HasLoudness = true
		}
	}
	// Key + tempo are channel-agnostic (estimated off the mono downmix),
	// so they run regardless of channelsOK. Each gates internally and
	// returns ok=false rather than guess from too little signal.
	if root, mode, ok := kt.estimateKey(); ok {
		res.KeyRoot = &root
		res.KeyMode = mode
	}
	if bpm, ok := kt.estimateTempo(); ok {
		res.BPM = &bpm
	}
	if peak, ok := tp.truePeakDB(); ok {
		res.TruePeakDB = &peak
	}
	dr.finish()
	if score, ok := dr.score(); ok {
		res.DRScore = &score
	}
	// FLAC-only audio-MD5 verification — a second, native-depth decode of
	// the same file (the analysis stream above is resampled f32 and is
	// exactly the wrong bytes to hash; see flacmd5.go). Runs after the
	// main decode so a truncated source has already been rejected.
	if strings.EqualFold(filepath.Ext(spec.SourceAbsPath), ".flac") {
		res.AudioMD5State, res.AudioMD5Retryable = verifyFLACAudioMD5(ctx, spec.SourceAbsPath, tool)
	}
	logger.Debug("analyze ok",
		"path", spec.SourceLibraryRel,
		"buckets", pk.count(),
		"bytes", len(data),
		"channels", channels,
		"loudness", res.HasLoudness,
		"hasKey", res.KeyRoot != nil,
		"hasTempo", res.BPM != nil,
		"hasTruePeak", res.TruePeakDB != nil,
		"hasDR", res.DRScore != nil,
		"md5", res.AudioMD5State,
		"md5Retryable", res.AudioMD5Retryable)
	return res, nil
}

// downmixFrame averages an interleaved frame to one mono sample for the
// peak envelope. Mono passes through; stereo is (L+R)/2, matching sox's
// default stereo→mono mix so the waveform bytes stay stable vs the prior
// mono `-c 1` decode (verified byte-identical in
// TestDownmixMatchesMonoDecode).
func downmixFrame(frame []float64) float32 {
	if len(frame) == 1 {
		return float32(frame[0])
	}
	var sum float64
	for _, s := range frame {
		sum += s
	}
	return float32(sum / float64(len(frame)))
}
