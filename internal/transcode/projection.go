// Projection helpers for operator-driven upscale (v1.3). Two
// responsibilities:
//
//  1. ProjectedSize — pure function estimating the on-disk size of a
//     FLAC variant given the source's (rate, bits, size) and the
//     target's (rate, bits) plus a format-dependent compression
//     factor.
//  2. DiskHasHeadroom — combines a per-platform AvailableDiskSpace
//     probe (see projection_unix.go / projection_windows.go) with
//     a safety margin so the admin Library Inspector can refuse a
//     batch before the worker pool starts writing.
//
// Both surfaces are exercised by the admin pre-flight (Phase 4) and
// by the batch coordinator's Submit path (Phase 3). PR 1 ships the
// helpers so Phase 2's `/api/library/browse-projection` endpoint can
// consume them without further plumbing.

package transcode

import (
	"errors"
	"fmt"
	"math"
)

// DefaultDiskSafetyMargin is the headroom fraction applied on top of
// the projected variant size before comparing against AvailableDiskSpace.
// 0.10 = refuse a batch when projected + 10% would consume the free
// space — leaves the bridge dataDir's WAL, sox temp file, and any
// concurrent enrichment writes some breathing room. Operators can
// override per-batch via the Submit API; this is the admin Library
// Inspector's pre-flight default.
const DefaultDiskSafetyMargin = 0.10

// Compression factors for ProjectedSize. Validated empirically against
// existing track_variants samples in the dev fixture; revisit if real-
// world ratios drift outside these bounds.
//
//   - 16-bit FLAC tends toward ~0.55× the raw PCM size on typical
//     dynamic-range material (classical / jazz). Lower with very
//     sparse signals, higher with electronic / heavily-compressed
//     masters.
//   - 24-bit FLAC carries more LSB noise that FLAC's predictor can't
//     fully model, so the ratio creeps up to ~0.65. The four
//     additional bits per sample are roughly orthogonal to the
//     predictor's residual coding, so the size delta vs 16-bit is
//     close to the bit-depth ratio.
//   - 32-bit cases are rare in this pipeline (target rarely 32);
//     same factor as 24-bit is a defensible upper-bound estimate.
//
// The factor is multiplicative on top of the raw PCM size projection
// — it is NOT the compression ratio relative to source FLAC bytes.
// ProjectedSize composes them correctly.
const (
	FLACCompressionFactor16Bit = 0.55
	FLACCompressionFactor24Bit = 0.65
)

// DefaultCompressionFactor returns the FLAC compression factor matching
// the given target bit depth. Defaults to the 24-bit factor for any
// non-16 value — the larger factor is the conservative direction for
// pre-flight space checks (over-estimates by a few percent at most;
// under-estimating risks the disk-full-mid-batch failure the helper
// exists to prevent).
func DefaultCompressionFactor(targetBits int) float64 {
	if targetBits == 16 {
		return FLACCompressionFactor16Bit
	}
	return FLACCompressionFactor24Bit
}

// ProjectedSize estimates the on-disk size of a FLAC variant produced
// at (targetRate, targetBits) from a source of (sourceSize, sourceRate,
// sourceBits).
//
// The model: rate ratio × bits ratio × compressionFactor × source size.
// Each factor is independent — sample rate scales linearly with byte
// count; bit depth scales linearly within a single PCM frame;
// compression factor captures FLAC's predictor + Rice coding
// efficiency.
//
// All arithmetic is performed in float64 to avoid wrap-around on
// gigabyte-scale sources; the result is converted back to int64 at
// the end. Negative or zero source parameters return 0 — the caller
// (typically the Coordinator's batch walk) should filter those rows
// before calling.
//
// Pure, allocation-free, suitable for hot loops over thousands of
// tracks.
func ProjectedSize(
	sourceSize int64,
	sourceRate, sourceBits int,
	targetRate, targetBits int,
	compressionFactor float64,
) int64 {
	if sourceSize <= 0 || sourceRate <= 0 || sourceBits <= 0 ||
		targetRate <= 0 || targetBits <= 0 || compressionFactor <= 0 {
		return 0
	}
	rateRatio := float64(targetRate) / float64(sourceRate)
	bitsRatio := float64(targetBits) / float64(sourceBits)
	projected := float64(sourceSize) * rateRatio * bitsRatio * compressionFactor
	// math.Round → int64 conversion guards against floating-point drift
	// on edge inputs. Result clamped at MaxInt64 in the (unrealistic)
	// case the projection overflows.
	if projected > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(math.Round(projected))
}

// ErrInsufficientDiskSpace is returned by DiskHasHeadroom when the
// projected size (after applying the safety margin) exceeds the free
// space. The error embeds the numbers so the coordinator can surface
// them in the operator-facing response without re-running the probe.
var ErrInsufficientDiskSpace = errors.New("insufficient disk space")

// InsufficientDiskSpaceError carries the projection details for
// rendering operator-facing messages ("needs 8.2 GB, 4.1 GB free").
// Returned alongside ErrInsufficientDiskSpace via errors.As so
// callers that don't care about the numbers can still match on the
// sentinel.
type InsufficientDiskSpaceError struct {
	ProjectedBytes int64
	RequiredBytes  int64 // projected × (1 + safetyMargin)
	AvailableBytes int64
	Dir            string
}

func (e *InsufficientDiskSpaceError) Error() string {
	return fmt.Sprintf(
		"%s: needs %d bytes (%.2f safety margin) on %q, %d available",
		ErrInsufficientDiskSpace.Error(),
		e.RequiredBytes,
		float64(e.RequiredBytes)/float64(max64(e.ProjectedBytes, 1)),
		e.Dir,
		e.AvailableBytes,
	)
}

func (e *InsufficientDiskSpaceError) Unwrap() error { return ErrInsufficientDiskSpace }

// DiskHasHeadroom probes the free space on the volume containing
// `dir` and returns (ok, freeBytes, err). It refuses (`ok=false` with
// an *InsufficientDiskSpaceError) if projectedBytes × (1 + safetyMargin)
// > freeBytes. A negative or NaN safetyMargin is clamped to 0; a
// zero or negative projectedBytes is treated as "no work to do" and
// always returns ok=true.
//
// Probe errors (Statfs failure, missing directory) surface as the
// returned `err`; callers should treat those as "can't check, refuse
// the batch" — silently proceeding risks a disk-full mid-batch.
func DiskHasHeadroom(dir string, projectedBytes int64, safetyMargin float64) (ok bool, freeBytes int64, err error) {
	if projectedBytes <= 0 {
		// No work projected — pair with a successful disk probe so
		// callers always get a meaningful freeBytes value.
		freeBytes, err = AvailableDiskSpace(dir)
		if err != nil {
			return false, 0, err
		}
		return true, freeBytes, nil
	}
	if safetyMargin < 0 || math.IsNaN(safetyMargin) {
		safetyMargin = 0
	}
	freeBytes, err = AvailableDiskSpace(dir)
	if err != nil {
		return false, 0, err
	}
	// Float-to-int64 conversion needs an overflow guard. The
	// multiplication can produce +Inf / NaN / values past MaxInt64
	// for adversarial inputs (giant projectedBytes near MaxInt64,
	// the margin pushing it over the float-exponent ceiling).
	// Direct `int64(...)` wraps to a negative value on overflow,
	// which then passes the `required > freeBytes` check silently
	// — exactly the disk-full-mid-write hazard the helper exists
	// to prevent. Gemini high on PR #199.
	requiredF := math.Ceil(float64(projectedBytes) * (1 + safetyMargin))
	var required int64
	switch {
	case math.IsNaN(requiredF) || math.IsInf(requiredF, 1) || requiredF >= float64(math.MaxInt64):
		required = math.MaxInt64
	case requiredF < 0:
		required = 0
	default:
		required = int64(requiredF)
	}
	if required > freeBytes {
		return false, freeBytes, &InsufficientDiskSpaceError{
			ProjectedBytes: projectedBytes,
			RequiredBytes:  required,
			AvailableBytes: freeBytes,
			Dir:            dir,
		}
	}
	return true, freeBytes, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
