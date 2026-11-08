package analyze

import (
	"context"
	"errors"
	"fmt"
	"io"
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
//
// Every file operation of a repair goes through one handle on its waveform
// directory (repairDir, since round 2): the directory the judgment saw,
// opened as an os.Root once the decode is done and compared with the
// identity the job carries. A root keeps the directory it opened, so nothing
// is made, removed, renamed or synced anywhere but there, whatever comes to
// be at the path; and the path is asked again, against the root, before the
// rename and right before the pool commits, so a curve written into a
// directory no longer at its path is never recorded.

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
	// (from the handle its probe read it with): the directory the repair's
	// root must be (repairDir), and the path is asked against it before the
	// decode. Required: a repair without one writes nothing.
	OutputDirIdentity os.FileInfo
	// Build is the bridge build a failure of the repair is counted against
	// (manifest.Store.RecordWaveformRegenFailure).
	Build string

	// dir is the job's one handle on its waveform directory, which the pool
	// sets so that the runner's writes and the pool's sync and checks before
	// its commit go through the same root. Nil when RunWaveformRepair is
	// called by itself, which then opens one of its own.
	dir *repairDir
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
// opened. No failure of the curve; nothing it writes reaches the directory at
// the path, and nothing it wrote is recorded.
var ErrWaveformDirAway = errors.New("the waveform directory is not the one the repair was admitted on")

// ErrSourceChanged is a repair's refusal when the source is not the version
// its analysis describes, by its size and mtime, the bridge's identity of a
// source everywhere (a rewrite that keeps both is not seen, as the analysis
// skip gate does not see it): a changed source goes through ordinary
// analysis, whose skip gate now offers it, and a curve of it here would
// describe bytes the row's measurements do not. No failure of the curve.
var ErrSourceChanged = errors.New("the source is not the version its analysis describes")

// ErrSourceUnreachable is a repair's refusal when the source cannot be
// stat'ed, opened or read: a mount that dropped, a permission that changed,
// credentials a mount no longer has. No failure of the curve.
var ErrSourceUnreachable = errors.New("the source could not be reached")

// ErrCurveNotWritten marks a repair that rendered the curve and could not
// write it (a directory, its scratch, the rename or the fsync), the waveform
// directory still the one it was admitted on: a failure of the curve, which
// counts toward its stop (CountsAgainstTheCurve).
var ErrCurveNotWritten = errors.New("the curve could not be written")

// CountsAgainstTheCurve reports whether a repair's error is a failure on the
// curve's own account, which counts toward manifest.WaveformRegenStopAfter:
// the decoder's verdict on a source that is still there, unchanged and
// readable (ErrSourceUnreadable), or a curve rendered and not written
// (ErrCurveNotWritten). Everything else is transient and counts nothing: an
// unreachable, unreadable or changed source, the waveform directory gone, a
// decoder that is missing or was killed, a cancellation. The pool also counts
// nothing for its per-job timeout and for a shutdown, before asking.
func CountsAgainstTheCurve(err error) bool {
	return SourceUnreadable(err) || errors.Is(err, ErrCurveNotWritten)
}

// RunWaveformRepair renders spec's curve again and puts it where it belongs,
// the canonical path under the waveform directory, written beside whatever is
// there (a partial copy) and renamed over it, never by deleting it first. The
// pool syncs it through the same root and commits it to the row.
//
// In order, each a refusal that writes nothing: the waveform directory at its
// path is asked whether it is still the one the repair was admitted on,
// before the source is opened or a decoder started; the source whether it is
// still the version the row describes and can be read (sourceReadable); the
// curve is decoded as RunAnalysis decodes it (decodeCurve), and a decoder's
// verdict counts only once the source is asked again and is still there,
// unchanged and readable, since a mount that dropped or a permission that
// changed during the decode answers as a verdict too; the source is asked
// again after a clean decode; the directory is opened as the repair's root
// and compared with the identity the job carries; every directory (never the
// waveform directory itself, mkdirBelow), the scratch, its stale copy's
// removal, its cleanup and the rename go through that root, and the path is
// asked again, against the root, just before the rename.
func RunWaveformRepair(ctx context.Context, spec RepairSpec) (RepairResult, error) {
	return runWaveformRepair(ctx, spec, toolchainSource)
}

// runWaveformRepair is RunWaveformRepair over src.
func runWaveformRepair(ctx context.Context, spec RepairSpec, src curveSource) (RepairResult, error) {
	return runWaveformRepairWith(ctx, spec, src, nil)
}

// repairHooks are a test's view inside one repair: beforeMkdir runs once the
// repair's root is open and checked, before its first directory is made;
// beforeRename once the curve is in its scratch beside its place, before the
// check that precedes the rename; atRename once that check has passed, just
// before the rename itself. Nil in production.
type repairHooks struct {
	beforeMkdir  func()
	beforeRename func(tmp, final string)
	atRename     func()
}

// runWaveformRepairWith is runWaveformRepair with a test's hooks.
func runWaveformRepairWith(ctx context.Context, spec RepairSpec, src curveSource, hooks *repairHooks) (RepairResult, error) {
	d := spec.dir
	if d == nil {
		d = newRepairDir(spec.OutputDir, spec.OutputDirIdentity)
		defer d.close()
	}
	if err := d.atPath(); err != nil {
		return RepairResult{}, err
	}
	if err := sourceReadable(spec.AnalyzeSpec); err != nil {
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
		if SourceUnreadable(err) {
			// Nor is its refusal of a source it could not open: a permission
			// that changed answers as a verdict too, and stats as before.
			if rerr := sourceReadable(spec.AnalyzeSpec); rerr != nil {
				return RepairResult{}, rerr
			}
		}
		return RepairResult{}, err
	}

	root, err := d.open()
	if err != nil {
		return RepairResult{}, err
	}
	finalPath := spec.SidecarPath()
	finalRel, err := d.rel(finalPath)
	if err != nil {
		return RepairResult{}, curveNotWritten{err}
	}
	if hooks != nil && hooks.beforeMkdir != nil {
		hooks.beforeMkdir()
	}
	if err := mkdirBelow(root, filepath.Dir(finalRel)); err != nil {
		return RepairResult{}, writeFailure(ctx, d, err)
	}
	tmpRel := finalRel + AnalysisTmpSuffix
	// Clear any stale tmp from a prior interrupted run, as RunAnalysis does,
	// in the root alone.
	_ = root.Remove(tmpRel)
	cleanup := true
	defer func() {
		if cleanup {
			_ = root.Remove(tmpRel)
		}
	}()
	if err := writeWaveformTmpIn(root, tmpRel, finalRel, data); err != nil {
		return RepairResult{}, writeFailure(ctx, d, err)
	}
	if hooks != nil && hooks.beforeRename != nil {
		hooks.beforeRename(finalPath+AnalysisTmpSuffix, finalPath)
	}
	// Before the curve is published: the rename puts it where a reader of the
	// row finds it, so the directory at the path must still be the root.
	if err := d.atPath(); err != nil {
		return RepairResult{}, err
	}
	if hooks != nil && hooks.atRename != nil {
		hooks.atRename()
	}
	if err := atomicwrite.RenameInRootCtx(ctx, root, tmpRel, finalRel); err != nil {
		return RepairResult{}, writeFailure(ctx, d, err)
	}
	cleanup = false
	return RepairResult{
		WaveformPath: finalPath,
		WaveformTag:  waveformTag(data),
		WaveformSize: int64(len(data)),
	}, nil
}

// repairDir is a repair's one handle on its waveform directory: the
// directory the catalog's judgment saw (admitted), opened as an os.Root at
// most once (open), through which the repair makes every directory, scratch,
// removal, rename and sync, and against which the directory at the path is
// asked again (atPath). A root keeps the directory it opened: once it is
// open, what is at the path can change, the directory the repair writes into
// cannot.
type repairDir struct {
	path     string
	admitted os.FileInfo
	root     *os.Root
	// id is the root's own identity, read from a handle opened through it.
	id os.FileInfo
}

func newRepairDir(path string, admitted os.FileInfo) *repairDir {
	return &repairDir{path: path, admitted: admitted}
}

// open is the repair's root, opened at its first call, and only on the
// directory the job was admitted on: anything else is dirAway, nothing kept
// open.
func (d *repairDir) open() (*os.Root, error) {
	if d.root != nil {
		return d.root, nil
	}
	if d.admitted == nil {
		return nil, dirAway{errors.New("the repair carries no waveform directory")}
	}
	r, err := os.OpenRoot(d.path)
	if err != nil {
		return nil, dirAway{err}
	}
	id, err := rootIdentity(r)
	if err != nil {
		_ = r.Close()
		return nil, dirAway{err}
	}
	if !os.SameFile(id, d.admitted) {
		_ = r.Close()
		return nil, dirAway{fsutil.ErrDirReplaced}
	}
	d.root, d.id = r, id
	return r, nil
}

// rootIdentity is the identity of the directory r holds, stat'ed from a
// handle opened through r: on Windows a stat by path reads the identity at
// the comparison, from whatever the path names then (fsutil.DirIdentity).
func rootIdentity(r *os.Root) (os.FileInfo, error) {
	f, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Stat()
}

// atPath asks whether the directory at the path is still the one the repair
// works through: the root once it is open, which a stale identity could not
// stand in for (the root keeps its directory, so no other can take its
// identity), and the identity the judgment saw before.
func (d *repairDir) atPath() error {
	if d.root == nil {
		return dirStillAdmitted(d.path, d.admitted)
	}
	now, err := fsutil.DirIdentity(d.path)
	if err != nil {
		return dirAway{err}
	}
	if !os.SameFile(now, d.id) {
		return dirAway{fsutil.ErrDirReplaced}
	}
	return nil
}

// rel is path relative to the repair's directory, refused when it is not
// below it.
func (d *repairDir) rel(path string) (string, error) {
	rel, err := filepath.Rel(d.path, path)
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("the curve's path is not below the waveform directory")
	}
	return rel, nil
}

func (d *repairDir) close() {
	if d.root != nil {
		_ = d.root.Close()
	}
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
// row's measurements describe: a regular file, of its size and mtime,
// exactly, as the analysis skip gate compares them. What is no regular file
// is never opened here.
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

// sourceReadable asks whether the source can be read at all: still the
// version the row describes (sourceStillDescribed, which refuses what is no
// regular file before anything opens it), then opened as a file
// (fsutil.OpenAsFile, which waits for no named pipe put there since) and read
// for a byte. A failed open or read is unreachable, never a verdict on the
// curve: a permission that changed leaves the stat as it was, and the
// decoder's refusal of a file it cannot open would otherwise count. An empty
// read of a file whose size said otherwise is another version.
func sourceReadable(spec AnalyzeSpec) error {
	if err := sourceStillDescribed(spec); err != nil {
		return err
	}
	f, _, err := fsutil.OpenAsFile(spec.SourceAbsPath)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrSourceUnreachable, pathless(err))
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	n, err := f.Read(b[:])
	switch {
	case n == 1:
		return nil
	case err == nil || errors.Is(err, io.EOF):
		return ErrSourceChanged
	}
	return fmt.Errorf("%w: %s", ErrSourceUnreachable, pathless(err))
}

// writeFailure classifies an error writing the curve: the directory at the
// path no longer the repair's (its root, or before it opens the identity the
// job carries) is the directory's refusal, a cancellation is the context's,
// and anything else is a failure of the curve (ErrCurveNotWritten).
func writeFailure(ctx context.Context, d *repairDir, err error) error {
	if derr := d.atPath(); derr != nil {
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

// mkdirBelow makes dir, relative to root, and the directories between it and
// root, through root, never root itself: a waveform directory that is not
// there is a volume that went (or was never mounted), and making it would put
// the curve on the disk under the mountpoint, where the next judgment would
// find a directory holding one curve. Each directory is made by
// fsutil.MkdirIn, which gives it its parent's owner when the process is root,
// as RunAnalysis's fsutil.MkdirAll does; one there already is taken only as a
// directory, read through root.
func mkdirBelow(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		err := fsutil.MkdirIn(root, cur, 0o700)
		if err == nil {
			continue
		}
		if errors.Is(err, fs.ErrExist) {
			if info, serr := root.Stat(cur); serr == nil && info.IsDir() {
				continue
			}
		}
		return err
	}
	return nil
}

// writeWaveformTmpIn is writeWaveformTmp through root: the scratch created,
// truncating debris a crash left, 0600, given the owner of the curve it will
// replace, or of its directory, while it is open (fsutil.KeepOwnerIn, a no-op
// unless this process is root). No fsync here: the pool syncs the curve
// through the same root before it commits the row that names it.
func writeWaveformTmpIn(root *os.Root, tmpRel, finalRel string, data []byte) error {
	f, err := root.OpenFile(tmpRel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write waveform tmp: %w", err)
	}
	if err := fsutil.KeepOwnerIn(root, f, finalRel); err != nil {
		_ = f.Close()
		return fmt.Errorf("write waveform tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write waveform tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write waveform tmp: %w", err)
	}
	return nil
}
