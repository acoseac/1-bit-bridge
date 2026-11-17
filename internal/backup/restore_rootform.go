package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The library form a database-only restore asks of its snapshot (backlog
// B653 PR 2b). The scanner stores a track's path bare under a library of one
// root, and below its root's folder name, filepath.Base(root) + "/" + rel,
// under a library of several, so a database indexed under one form names no
// file of a library configured in the other, and one indexed under several
// roots names its files by those roots' folder names, exactly as written. A
// database restored alone keeps this bridge's config, so the form it was
// indexed under must be the form that config gives now.
//
// That form is decided from the snapshot's own settings and nothing else: its
// bridge.yaml, listed in its manifest with a baseline, read under the lock of
// the folder that holds the snapshot, at most its baseline's size and one
// byte, those bytes hashed as they are read and equal to that baseline in
// size and SHA-256 (readCapturedSettings). The database's verification shows
// neither the file's presence nor its contents, and the files' verdict takes
// no part: a certificate that fails its baseline refuses nothing here, while
// settings that are not the ones captured refuse the restore
// (ErrRootFormUnproven), naming the full restore, which puts the snapshot's
// settings back with its database.
//
// Both sides are read as the file writes them (config.FileLibraryRoots: the
// decoder Load's is, and no BRIDGE_LIBRARY_ROOTS), each root resolved
// against the directory of this bridge's config, never the snapshot's
// folder, so an override in the restoring shell decides neither side and a
// relative root names the folder it names for this bridge. Zero roots and one
// are one form, whatever the root's path or folder name: a single root moved
// is a question of the library's paths, which match the new root's files or
// not as after a hand edit of the root, not of the form. Several roots are
// one form with several only where both sides name the same folder names, a
// multiset compared byte for byte, so a difference of case alone refuses,
// the stored prefix being the exact name, and a folder name that is no plain
// name (a root of the filesystem, an empty root) refuses whatever the other
// side names. Rows an upstream routes take no part: the comparison is of the
// two configs.

// ErrRootFormUnproven is a database-only restore's refusal of a snapshot
// whose settings cannot show the library form its database was indexed under
// (checkRootForm): its manifest lists no bridge.yaml, or none with a
// baseline; the file is not there, is not a regular file, is not the file it
// captured, or does not decode; or one side names, among several roots, one
// with no folder name of its own. Nothing was restored.
var ErrRootFormUnproven = errors.New("the snapshot cannot show which library form its database was indexed under")

// ErrRootFormChanged is a database-only restore's refusal of a snapshot whose
// settings name another library form than this bridge's config
// (checkRootForm): one root or none against several, or several under other
// folder names. Nothing was restored.
var ErrRootFormChanged = errors.New("the snapshot's database was indexed under another library form than this bridge's config gives")

// fullRestoreWay is what each refusal of the form says to do instead.
const fullRestoreWay = "restore the snapshot whole (bridge restore without --database-only), which puts its settings back with its database"

// checkRootForm answers whether the database of the snapshot at snap, whose
// manifest is m, as the restore reads it under the lock of the folder that
// holds it, was indexed under the library form this bridge's config gives
// now: the config at configPath, whose bytes are liveConfig as the restore's
// turn of its lock holds them. It answers nil where the forms are one,
// ErrRootFormUnproven where the snapshot's settings cannot show its form,
// ErrRootFormChanged where they show another, and an error of its own where
// the snapshot's settings could not be read, or this bridge's do not decode.
// It writes nothing, and opens no database.
func checkRootForm(snap string, m Manifest, configPath string, liveConfig []byte) error {
	data, err := readCapturedSettings(snap, m)
	if err != nil {
		return err
	}
	theirs, err := config.FileLibraryRoots(configPath, data)
	if err != nil {
		// FileLibraryRoots' words name this bridge's config, against whose
		// directory it resolves; the cause past them is the snapshot's.
		cause := errors.Unwrap(err)
		if cause == nil {
			cause = err
		}
		return unproven(fmt.Sprintf("its bridge.yaml does not decode (%v)", cause))
	}
	ours, err := config.FileLibraryRoots(configPath, liveConfig)
	if err != nil {
		return fmt.Errorf("read this bridge's library roots: %w; nothing was restored", err)
	}
	return sameRootForm(ours, theirs)
}

// readCapturedSettings is the snapshot's bridge.yaml, in the snapshot at snap
// whose manifest is m, as it was captured: listed in m, with a baseline, a
// regular file by Lstat, read through fsutil.OpenAsFile, which refuses a
// named pipe at once, a Windows sharing violation waited out
// (atomicwrite.ReadWithRetryCtx), at most its baseline's size and one byte,
// and equal to that baseline in size and SHA-256. Anything else is
// ErrRootFormUnproven, saying which; a read that fails otherwise is that
// failure.
func readCapturedSettings(snap string, m Manifest) ([]byte, error) {
	const name = "bridge.yaml"
	if !slices.Contains(m.Files, name) {
		return nil, unproven("its manifest lists no bridge.yaml")
	}
	base, ok := m.baseline(name)
	if !ok {
		return nil, unproven("its manifest records no baseline of its bridge.yaml")
	}
	if base.Size < 0 {
		return nil, unproven("its manifest records no size for its bridge.yaml")
	}
	path := filepath.Join(snap, name)
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, unproven("its bridge.yaml is not there")
	case err != nil:
		return nil, fmt.Errorf("read the snapshot's bridge.yaml: %w; nothing was restored", err)
	case !info.Mode().IsRegular():
		return nil, unproven("its bridge.yaml is not a regular file")
	}
	var data []byte
	var sum string
	err = atomicwrite.ReadWithRetryCtx(context.Background(), func() error {
		f, _, err := fsutil.OpenAsFile(path)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		data, err = io.ReadAll(io.TeeReader(io.LimitReader(f, base.Size+1), h))
		sum = hex.EncodeToString(h.Sum(nil))
		return err
	})
	var notAFile *fsutil.NotAFileError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, unproven("its bridge.yaml is not there")
	case errors.As(err, &notAFile):
		return nil, unproven("its bridge.yaml is not a regular file")
	case err != nil:
		return nil, fmt.Errorf("read the snapshot's bridge.yaml: %w; nothing was restored", err)
	case int64(len(data)) != base.Size || sum != base.SHA256:
		return nil, unproven("its bridge.yaml is not the file it captured")
	}
	return data, nil
}

// sameRootForm answers nil where ours, this bridge's roots, and theirs, the
// snapshot's, each resolved already, are one library form, and its refusal
// otherwise, naming each side's roots by their folder names alone.
func sameRootForm(ours, theirs []string) error {
	multi := len(ours) > 1
	if multi != (len(theirs) > 1) {
		return fmt.Errorf("%w: this bridge's config names %s and the snapshot's names %s; nothing was restored: %s",
			ErrRootFormChanged, rootsWords(ours), rootsWords(theirs), fullRestoreWay)
	}
	if !multi {
		return nil
	}
	a, b := rootNames(ours), rootNames(theirs)
	for _, side := range []struct {
		whose string
		names []string
	}{{"this bridge's config", a}, {"the snapshot's", b}} {
		for _, n := range side.names {
			if !rootFolderName(n) {
				return unproven(fmt.Sprintf("%s names among its library roots one with no folder name of its own (%q), which a library of several roots stores its paths under", side.whose, n))
			}
		}
	}
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return fmt.Errorf("%w: this bridge's config names %s and the snapshot's names %s, and a library of several roots stores its paths under its roots' folder names, exactly as written; nothing was restored: %s",
			ErrRootFormChanged, rootsWords(ours), rootsWords(theirs), fullRestoreWay)
	}
	return nil
}

// rootFolderName reports whether n, filepath.Base of a resolved root, is a
// folder name the scanner can store a path below: not empty, "." (an empty
// root's) or "..", and holding no separator, as the base of a filesystem's
// root does.
func rootFolderName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsRune(n, filepath.Separator) && !strings.ContainsRune(n, '/')
}

// rootNames is the folder name of each root in roots, filepath.Base's.
func rootNames(roots []string) []string {
	names := make([]string, len(roots))
	for i, r := range roots {
		names[i] = filepath.Base(r)
	}
	return names
}

// rootsWords names roots by their count and their folder names, quoted, and
// by nothing else of the config.
func rootsWords(roots []string) string {
	switch len(roots) {
	case 0:
		return "no library root"
	case 1:
		return fmt.Sprintf("one library root (%q)", filepath.Base(roots[0]))
	}
	quoted := make([]string, len(roots))
	for i, n := range rootNames(roots) {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return fmt.Sprintf("%d library roots (%s)", len(roots), strings.Join(quoted, ", "))
}

// unproven is ErrRootFormUnproven, saying why and what to do instead.
func unproven(why string) error {
	return fmt.Errorf("%w: %s; nothing was restored: %s", ErrRootFormUnproven, why, fullRestoreWay)
}
