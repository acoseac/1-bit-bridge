package analyze

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The waveform-only repair (backlog B652): a curve proven missing at its
// recorded path and at its canonical one (internal/integrity's
// DecideWaveform), or a partial copy at the canonical path, is rendered again
// from a source its analysis row still describes, and recorded on that row
// waveform only (manifest.Store.CommitWaveformRepair). The row's other
// measurements are kept as they are: a repair renders the curve and nothing
// else, so a decode that could not measure what the analysis measured does
// not take the measurement away, and a build that would measure it otherwise
// does not rewrite it behind a curve.

// RepairSpec is one waveform-only repair.
type RepairSpec struct {
	// AnalyzeSpec names the source and where its curve goes:
	// SourceLibraryRel is the analysis row's own source path (the pool's
	// dedup key, so a repair and an analysis of one source never run at
	// once), SourceMTimeNS and SourceSize the version the row's measurements
	// describe, which the source must still be, and OutputDir the waveform
	// directory.
	AnalyzeSpec
	// Base is the analysis row the repair started from: its commit writes
	// only while the row is still this one.
	Base manifest.AnalysisRow
	// OutputDirIdentity is the waveform directory the catalog's judgment saw
	// (from the handle its probe read it with), asked again before the
	// repair makes a directory or a file and before it renames the curve into
	// place (fsutil.DirStillIs). Required: a repair without one writes
	// nothing.
	OutputDirIdentity os.FileInfo
	// Build is the bridge build a failure of the repair is counted against
	// (manifest.Store.RecordWaveformRegenFailure).
	Build string
}

// RepairResult is the curve a repair wrote.
type RepairResult struct {
	WaveformPath string
	WaveformTag  string
	WaveformSize int64
}

// ErrWaveformDirAway is a repair's refusal when the waveform directory is not
// the one it was admitted on: gone, another directory at the path (a volume
// unmounted, its mountpoint the local directory again), or one that cannot be
// opened. It makes no directory and no file, and is no failure of the curve.
var ErrWaveformDirAway = errors.New("the waveform directory is not the one the repair was admitted on")

// ErrSourceChanged is a repair's refusal when the source is not the version
// its analysis describes: a changed source goes through ordinary analysis,
// whose skip gate now offers it, and a curve of it here would describe bytes
// the row's measurements do not. No failure of the curve.
var ErrSourceChanged = errors.New("the source is not the version its analysis describes")

// ErrSourceUnreachable is a repair's refusal when the source cannot be
// stat'ed: a mount that dropped, a permission that changed. No failure of
// the curve.
var ErrSourceUnreachable = errors.New("the source could not be reached")

// ErrCurveNotWritten marks a repair that rendered the curve and could not
// write it (a directory, its scratch, the rename or the fsync), the waveform
// directory still the one it was admitted on: a failure of the curve, which
// counts toward its stop (CountsAgainstTheCurve).
var ErrCurveNotWritten = errors.New("the curve could not be written")

// CountsAgainstTheCurve reports whether a repair's error is a failure on the
// curve's own account, which counts toward manifest.WaveformRegenStopAfter:
// the decoder's verdict on a source that is still there and unchanged
// (ErrSourceUnreadable), or a curve rendered and not written
// (ErrCurveNotWritten). Everything else is transient and counts nothing: an
// unreachable or changed source, the waveform directory gone, a decoder that
// is missing or was killed, a cancellation. The pool also counts nothing for
// its per-job timeout and for a shutdown, before asking.
func CountsAgainstTheCurve(err error) bool {
	return SourceUnreadable(err) || errors.Is(err, ErrCurveNotWritten)
}

// RunWaveformRepair renders spec's curve again and puts it where it belongs,
// the canonical path under the waveform directory, written beside whatever is
// there (a partial copy) and renamed over it, never by deleting it first. The
// pool fsyncs it and commits it to the row.
//
// In order, each a refusal that writes nothing: the waveform directory is
// asked whether it is still the one the repair was admitted on, before the
// source is opened or a decoder started; the source whether it is still the
// version the row describes; the curve is decoded as RunAnalysis decodes it
// (decodeCurve), and a decoder's verdict counts only once the source is
// asked again and is still there unchanged, since a mount that dropped during
// the decode answers as a verdict too; the source is asked again after a
// clean decode; the directory again before a directory is made (never the
// waveform directory itself, mkdirBelow) and again before the rename.
func RunWaveformRepair(ctx context.Context, spec RepairSpec) (RepairResult, error) {
	return runWaveformRepair(ctx, spec, toolchainSource)
}

// runWaveformRepair is RunWaveformRepair over src.
func runWaveformRepair(ctx context.Context, spec RepairSpec, src curveSource) (RepairResult, error) {
	return runWaveformRepairWith(ctx, spec, src, nil)
}

// repairHooks are a test's view inside one repair: beforeRename runs once the
// curve is in its scratch beside its place, before the check that precedes
// the rename. Nil in production.
type repairHooks struct {
	beforeRename func(tmp, final string)
}

// runWaveformRepairWith is runWaveformRepair with a test's hooks.
func runWaveformRepairWith(ctx context.Context, spec RepairSpec, src curveSource, hooks *repairHooks) (RepairResult, error) {
	root := spec.OutputDir
	if err := dirStillAdmitted(root, spec.OutputDirIdentity); err != nil {
		return RepairResult{}, err
	}
	if err := sourceStillDescribed(spec.AnalyzeSpec); err != nil {
		return RepairResult{}, err
	}
	pr := src.probe(ctx, spec.SourceAbsPath)
	data, _, err := decodeCurve(ctx, src, spec.SourceAbsPath, pr, nil)
	if serr := sourceStillDescribed(spec.AnalyzeSpec); serr != nil {
		// A curve of bytes that changed, or went, under the decode describes
		// nothing, and a decoder's refusal of a source that went is no verdict
		// about it.
		return RepairResult{}, serr
	}
	if err != nil {
		return RepairResult{}, err
	}

	finalPath := spec.SidecarPath()
	if err := dirStillAdmitted(root, spec.OutputDirIdentity); err != nil {
		return RepairResult{}, err
	}
	if err := mkdirBelow(root, filepath.Dir(finalPath)); err != nil {
		return RepairResult{}, writeFailure(ctx, spec, err)
	}
	tmpPath := finalPath + AnalysisTmpSuffix
	// Clear any stale tmp from a prior interrupted run, as RunAnalysis does.
	_ = os.Remove(tmpPath)
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := writeWaveformTmp(tmpPath, finalPath, data); err != nil {
		return RepairResult{}, writeFailure(ctx, spec, err)
	}
	if hooks != nil && hooks.beforeRename != nil {
		hooks.beforeRename(tmpPath, finalPath)
	}
	// Before the curve is published: the rename puts it where a reader of
	// the row finds it.
	if err := dirStillAdmitted(root, spec.OutputDirIdentity); err != nil {
		return RepairResult{}, err
	}
	if err := atomicwrite.RenameWithRetryCtx(ctx, tmpPath, finalPath); err != nil {
		return RepairResult{}, writeFailure(ctx, spec, err)
	}
	cleanup = false
	return RepairResult{
		WaveformPath: finalPath,
		WaveformTag:  waveformTag(data),
		WaveformSize: int64(len(data)),
	}, nil
}

// dirStillAdmitted asks whether root is still the waveform directory the
// repair was admitted on.
func dirStillAdmitted(root string, admitted os.FileInfo) error {
	if admitted == nil {
		return dirAway{errors.New("the repair carries no waveform directory")}
	}
	if err := fsutil.DirStillIs(root, admitted); err != nil {
		return dirAway{err}
	}
	return nil
}

// dirAway is ErrWaveformDirAway with its cause, which names no path.
type dirAway struct{ cause error }

func (e dirAway) Error() string {
	return ErrWaveformDirAway.Error() + ": " + pathless(e.cause)
}

// pathless describes err without the paths a file operation's error names
// (fsutil.ErrorWithoutPath), a rename's two included (*os.LinkError): a
// repair's error is the reason a failure records, which `bridge doctor`
// prints beside the track's library-relative path, and the waveform
// directory's path is the data directory's.
func pathless(err error) string {
	var le *os.LinkError
	if errors.As(err, &le) && le != nil {
		return le.Op + ": " + fmt.Sprint(le.Err)
	}
	return fsutil.ErrorWithoutPath(err)
}
func (e dirAway) Unwrap() error        { return e.cause }
func (e dirAway) Is(target error) bool { return target == ErrWaveformDirAway }

// sourceStillDescribed asks whether the source is still the version the
// row's measurements describe: its size and mtime, exactly, as the analysis
// skip gate compares them.
func sourceStillDescribed(spec AnalyzeSpec) error {
	info, err := os.Stat(spec.SourceAbsPath)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrSourceUnreachable, pathless(err))
	}
	if !info.Mode().IsRegular() || info.ModTime().UnixNano() != spec.SourceMTimeNS || info.Size() != spec.SourceSize {
		return ErrSourceChanged
	}
	return nil
}

// writeFailure classifies an error writing the curve: the waveform directory
// gone since its last check is its refusal, a cancellation is the context's,
// and anything else is a failure of the curve (ErrCurveNotWritten).
func writeFailure(ctx context.Context, spec RepairSpec, err error) error {
	if derr := dirStillAdmitted(spec.OutputDir, spec.OutputDirIdentity); derr != nil {
		return derr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return curveNotWritten{err}
}

// curveNotWritten is ErrCurveNotWritten with the write's error under it.
type curveNotWritten struct{ cause error }

func (e curveNotWritten) Error() string {
	return ErrCurveNotWritten.Error() + ": " + pathless(e.cause)
}
func (e curveNotWritten) Unwrap() error        { return e.cause }
func (e curveNotWritten) Is(target error) bool { return target == ErrCurveNotWritten }

// mkdirBelow makes dir and the directories between it and root, never root
// itself: a waveform directory that is not there is a volume that went (or
// was never mounted), and making it would put the curve on the disk under the
// mountpoint, where the next judgment would find a directory holding one
// curve. Each directory is made by fsutil.Mkdir, which gives it its parent's
// owner when the process is root, as RunAnalysis's fsutil.MkdirAll does.
func mkdirBelow(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("the curve's directory is not below the waveform directory")
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		err := fsutil.Mkdir(cur, 0o700)
		if err == nil {
			continue
		}
		if errors.Is(err, fs.ErrExist) {
			if info, serr := os.Stat(cur); serr == nil && info.IsDir() {
				continue
			}
		}
		return err
	}
	return nil
}
