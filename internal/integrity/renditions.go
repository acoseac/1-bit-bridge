package integrity

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// sidecarFamily is what a sidecar directory's guards know of one kind of
// sidecar: how to recognise one of its files by its basename, and the words
// a refusal names the directory and its files with. The variants
// directory's renditions and the waveform directory's curves are the two
// (variantSidecars, waveformSidecars), and the probe, the relocation and
// emptied guard and the identity check are one body for both
// (dirSweepBlock, massMissingRefusal, dirChanged), so they cannot disagree
// about what a tree that holds sidecars looks like (backlog B652).
type sidecarFamily struct {
	// isSidecar reports whether a basename is one of the family's files.
	isSidecar func(name string) bool
	// dir names the directory ("variants directory"), path the path when it
	// names no directory ("variants path"), file one of its files
	// ("rendition"), files more of them ("sidecar files"), and rowFile what
	// a row that has none at either location lacks ("sidecar").
	dir, path, file, files, rowFile string
}

// variantSidecars is the variants directory's family: a rendition in either
// layout looksLikeVariantSidecar recognises.
var variantSidecars = sidecarFamily{
	isSidecar: looksLikeVariantSidecar,
	dir:       "variants directory",
	path:      "variants path",
	file:      "rendition",
	files:     "sidecar files",
	rowFile:   "sidecar",
}

// waveformSidecars is the waveform directory's family: a curve the analysis
// pool writes (ConsiderWaveformSidecar).
var waveformSidecars = sidecarFamily{
	isSidecar: ConsiderWaveformSidecar,
	dir:       "waveform directory",
	path:      "waveform path",
	file:      "waveform",
	files:     "waveform files",
	rowFile:   "curve",
}

// renditionScan is what scanForSidecars found in a sidecar directory.
type renditionScan struct {
	// root is the directory the scan listed, stat'ed from the handle it
	// listed it with (fsutil.DirIdentity says why never os.Stat): the
	// identity SidecarDirBlock.Info carries. Set whenever the root was
	// opened, whatever else the scan found.
	root os.FileInfo
	// entries: the directory itself holds at least one entry, of any kind.
	entries bool
	// rendition: a file the family names (a rendition looksLikeVariantSidecar
	// names, or a curve), regular or a link resolving to a regular file,
	// outside a directory SkipsDerivedTreeDir names (a dot-directory, a NAS
	// recycle bin or snapshot, and the rest).
	rendition bool
	// dirLink: a link to a directory (a symlink, or on Windows a junction or
	// a volume mounted in a folder), not named as SkipsDerivedTreeDir skips,
	// which the scan does not look behind: what it holds, the scan cannot
	// say.
	dirLink bool
}

// renditionScanBatch is how many entries the scan asks of a directory at a
// time. A directory's entries arrive in the filesystem's order, unsorted,
// so the first rendition of a flat 100,000-sidecar tree is found in the
// first batch rather than after a sorted listing of the whole directory
// (filepath.WalkDir's os.ReadDir), which a probe asked once per row by the
// variant delete handler cannot afford.
const renditionScanBatch = 256

// pendingScanDir is a directory the scan has listed and not yet opened,
// with the entry that named it, which IsFilesystemLostFound reads.
type pendingScanDir struct {
	path  string
	entry fs.DirEntry
}

// scanForRenditions looks for one rendition under dir: the one walk behind
// TreeHoldsVariantSidecars (the relocation guard's evidence) and
// VariantsDirSweepBlock (the mount-loss probe), so the two cannot disagree
// about what a tree holds (backlog B223).
//
// It stops at the first rendition, depth first, reading each directory a
// batch at a time. A tree that holds none is read whole, which is the walk
// that proves the negative: on the tree it exists for, the local directory
// an unmount leaves under a mountpoint, that is a handful of entries.
//
// The rules are TreeHoldsVariantSidecars' (its docblock): the root resolved
// before anything is read (resolveSidecarRoot), every directory a library
// walk skips pruned by its name (SkipsDerivedTreeDir: a recycle bin's renditions
// were deleted and a snapshot's are copies, so neither is the tree's own),
// a link counted as a rendition only when it resolves to a regular file, and
// the filesystem's lost+found at the top, which this user cannot list,
// counted as nothing either way (IsFilesystemLostFound). A link to a
// directory is not walked (loops), and is reported as dirLink.
//
// Order-independent, where the WalkDir form it replaces was not: a
// directory it cannot open or list is noted and the scan goes on, a
// rendition found anywhere answers true with no error, and only a scan
// that found none returns the first error it noted. The WalkDir form
// stopped at its first error, so whether a readable rendition was seen
// depended on how the names sorted.
func scanForRenditions(dir string) (renditionScan, error) {
	return scanForSidecars(dir, variantSidecars)
}

// scanForSidecars is scanForRenditions for any family: the same walk, by the
// same rules, looking for one of fam's files. The waveform guard asks it of
// the waveform directory (WaveformDirSweepBlock), so a tree that holds only
// curves is not read as a tree that holds nothing (backlog B652).
func scanForSidecars(dir string, fam sidecarFamily) (renditionScan, error) {
	var scan renditionScan
	if dir == "" {
		// Refused before resolveSidecarRoot: filepath.EvalSymlinks("") is
		// ".", so an unguarded "" would walk the working directory.
		return scan, errors.New("integrity: no " + fam.dir)
	}
	root, err := resolveSidecarRoot(dir)
	if err != nil {
		return scan, err
	}
	f, err := fsutil.OpenDir(root)
	if err != nil {
		return scan, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return scan, err
	}
	scan.root = info

	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	var pending []pendingScanDir
	cur, curPath := f, root
	for {
		if err := scanDirEntries(cur, curPath, curPath == root, fam, &scan, &pending); err != nil {
			note(err)
		}
		_ = cur.Close()
		if scan.rendition {
			return scan, nil
		}
		opened := false
		for !opened && len(pending) > 0 {
			next := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			d, err := fsutil.OpenDir(next.path)
			if err != nil {
				if !IsFilesystemLostFound(root, next.path, next.entry, err) {
					note(err)
				}
				continue
			}
			cur, curPath, opened = d, next.path, true
		}
		if !opened {
			return scan, firstErr
		}
	}
}

// scanDirEntries reads the open directory d, at path, a batch at a time: it
// sets scan.rendition and returns at the first of fam's files, queues each
// directory below it that SkipsDerivedTreeDir does not name, and marks the
// root's entries and any link to a directory. An error ends the listing
// of d alone.
func scanDirEntries(d *os.File, path string, isRoot bool, fam sidecarFamily, scan *renditionScan, pending *[]pendingScanDir) error {
	for {
		ents, err := d.ReadDir(renditionScanBatch)
		for _, e := range ents {
			if isRoot {
				scan.entries = true
			}
			name := e.Name()
			p := filepath.Join(path, name)
			switch t := e.Type(); {
			case t.IsDir():
				if !SkipsDerivedTreeDir(name) {
					*pending = append(*pending, pendingScanDir{path: p, entry: e})
				}
			case t.IsRegular():
				if fam.isSidecar(name) {
					scan.rendition = true
					return nil
				}
			default:
				// A symlink, a Windows junction or mounted folder
				// (ModeIrregular since Go 1.23), or a special file: judged
				// by what it resolves to. os.Stat opens nothing, so a named
				// pipe cannot hold the scan. One that cannot be stat'ed (a
				// dangling link) is nothing, TreeHoldsVariantSidecars' rule.
				st, err := os.Stat(p)
				switch {
				case err != nil:
				case st.IsDir():
					if !SkipsDerivedTreeDir(name) {
						scan.dirLink = true
					}
				case st.Mode().IsRegular() && fam.isSidecar(name):
					scan.rendition = true
					return nil
				}
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}
