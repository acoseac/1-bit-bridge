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
	"os"
	"path/filepath"
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
	//
	// `>=` not `>`: float64(math.MaxInt64) rounds UP to 2^63, so a
	// projected value of exactly 2^63 slips a `>` check and then
	// int64(math.Round(...)) wraps to a negative size. Matches the
	// DiskHasHeadroom guard below.
	if projected >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Round(projected))
}

// AvailableDiskSpaceNearest probes the free space for `dir`, falling
// back to the closest EXISTING ancestor when `dir` itself doesn't
// exist yet. The variants dir is created lazily (sox writes the first
// sidecar's parents on demand) and a custom `upscale.variantsDir` may
// simply not be mounted — a bare statfs on either returns ENOENT and
// would fail the pre-flight for a batch that is actually fine.
// Probing the nearest ancestor reports the volume the directory WILL
// land on once created.
//
// filepath.Clean first so trailing slashes / relative segments can't
// stall the ancestor walk; termination is the filepath.Dir fixed
// point (`/` on POSIX, `C:\` on Windows). A missing configured dir is
// logged at Warn — on a host whose variants volume failed to mount,
// that line is the operator's signal that the check is now grading
// the wrong (parent) volume.
func AvailableDiskSpaceNearest(dir string) (int64, error) {
	dir = filepath.Clean(dir)
	probe := dir
	for {
		_, err := os.Stat(probe)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			// Only NON-EXISTENCE walks up: any other stat failure
			// (permission flap, transient I/O) means the path may
			// well exist — walking past it would grade the wrong
			// parent volume and mask the real fault. Stat the
			// configured dir itself so AvailableDiskSpace surfaces
			// the genuine error to the caller.
			probe = dir
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			// Volume root — stat it anyway and let AvailableDiskSpace
			// surface the real error if even the root is unreadable.
			break
		}
		probe = parent
	}
	if probe != dir {
		logger.Warn("disk probe: directory missing; probing nearest existing ancestor",
			"dir", dir, "ancestor", probe)
	}
	return AvailableDiskSpace(probe)
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
		float64(e.RequiredBytes)/float64(max(e.ProjectedBytes, 1)),
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
// The probe routes through AvailableDiskSpaceNearest, so a `dir`
// that doesn't exist yet (lazily-created variants dir) is graded by
// its closest existing ancestor's volume. Remaining probe errors
// (Statfs failure on an existing path) surface as the returned
// `err`; callers should treat those as "can't check, refuse the
// batch" — silently proceeding risks a disk-full mid-batch.
func DiskHasHeadroom(dir string, projectedBytes int64, safetyMargin float64) (ok bool, freeBytes int64, err error) {
	if projectedBytes <= 0 {
		// No work projected — pair with a successful disk probe so
		// callers always get a meaningful freeBytes value.
		freeBytes, err = AvailableDiskSpaceNearest(dir)
		if err != nil {
			return false, 0, err
		}
		return true, freeBytes, nil
	}
	if safetyMargin < 0 || math.IsNaN(safetyMargin) {
		safetyMargin = 0
	}
	freeBytes, err = AvailableDiskSpaceNearest(dir)
	if err != nil {
		return false, 0, err
	}
	required := RequiredBytesWithMargin(projectedBytes, safetyMargin)
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

// RequiredBytesWithMargin returns the free space a batch of
// projectedBytes needs after applying safetyMargin — i.e.
// ceil(projected × (1 + margin)).
//
// This is THE definition of the margin, exported because the admin
// Library Inspector's projection endpoint has to predict exactly what
// Submit will refuse. Any surface that renders a "needs X, have Y"
// verdict MUST route through here rather than restating the
// arithmetic: a second copy drifts silently the moment
// DefaultDiskSafetyMargin moves, and the operator gets a green panel
// followed by a 507.
//
// A negative or NaN margin is clamped to 0; a non-positive projection
// yields 0.
//
// The float-to-int64 conversion needs an overflow guard. The
// multiplication can produce +Inf / NaN / values past MaxInt64 for
// adversarial inputs (giant projectedBytes near MaxInt64, the margin
// pushing it over the float-exponent ceiling). A direct `int64(...)`
// wraps to a NEGATIVE value on overflow, which then passes a
// `required > free` check silently — exactly the disk-full-mid-write
// hazard this exists to prevent, so overflow saturates to MaxInt64
// (refuse) rather than wrapping. Gemini high on PR #199.
//
// (int64 → float64 is exact below 2^53 ≈ 9 PB; projections are many
// orders of magnitude under that, and anything approaching it
// saturates to a refusal anyway.)
func RequiredBytesWithMargin(projectedBytes int64, safetyMargin float64) int64 {
	if projectedBytes <= 0 {
		return 0
	}
	if safetyMargin < 0 || math.IsNaN(safetyMargin) {
		safetyMargin = 0
	}
	requiredF := math.Ceil(float64(projectedBytes) * (1 + safetyMargin))
	switch {
	case math.IsNaN(requiredF) || math.IsInf(requiredF, 1) || requiredF >= float64(math.MaxInt64):
		return math.MaxInt64
	case requiredF < 0:
		return 0
	default:
		return int64(requiredF)
	}
}
