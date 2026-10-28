package atomicwrite

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// ReplaceDurable is WriteBytesDurable for a destination that holds an entry
// a rename cannot replace in one step, a directory, empty or not, and whose
// name a failure must not leave empty: the entry at path, whatever it is, is
// kept under aside, a name beside path that holds nothing, and data takes its
// place. After every refusal WriteBytesDurable makes before it writes, data
// is staged beside path and fsynced; only then is the entry renamed aside and
// the staged file renamed into its place, the two renames adjacent, with no
// write and no sync between them; and once both have landed every directory
// from path's up to root is synced (durableChain). A failure before the first
// rename leaves the entry as it was, and nothing staged; a failure of the
// second renames the entry back and syncs the directories again, and one
// that cannot put it back says where it is kept. A crash leaves the entry at
// path or data, but between the two renames, where no rename puts a file in
// a directory's place in one step, path names nothing: a caller that must not
// lose what path records keeps a second record of it, durable before the
// call (the player's discard seal, backup.sealPlayerMarker). A barrier that
// fails once both renames have landed leaves data in place and the write not
// counted durable, as WriteBytesDurable's does.
//
// aside must name a file in path's directory, as written, other than path's
// own name (ErrNotUnderRoot): an entry renamed out of that directory leaves
// the walk the barrier syncs. An entry already at aside is the caller's to
// avoid (a random name); a rename onto it replaces what it can. path must hold
// an entry: none is fs.ErrNotExist, refused before anything is staged.
func ReplaceDurable(root, path, aside string, data []byte, tmpPrefix string) error {
	if syncDir == nil {
		return ErrNoDirectoryBarrier
	}
	p, err := checkPath(root, path)
	if err != nil {
		return err
	}
	switch base := filepath.Base(aside); {
	// EqualFold, not ==: on a case-insensitive filesystem (APFS's default,
	// NTFS) a name differing only in case is the entry itself.
	case NamesDirectory(aside), filepath.Clean(filepath.Dir(aside)) != filepath.Clean(filepath.Dir(path)), strings.EqualFold(base, filepath.Base(path)):
		return fmt.Errorf("%w: %s is not a name beside %s", ErrNotUnderRoot, aside, path)
	}
	asidePath := filepath.Join(p.dir(), filepath.Base(aside))
	n, err := p.plain()
	if err != nil {
		return err
	}
	if n < len(p.dirs) {
		return &fs.PathError{Op: "replace", Path: p.file, Err: fs.ErrNotExist}
	}
	if _, err := lstat(p.file); err != nil {
		return err
	}
	above, err := p.storage()
	if err != nil {
		return err
	}
	chain := p.chain(len(p.dirs))
	staged, err := stageDurable(p.file, data, tmpPrefix)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := renameRetry(ctx, p.file, asidePath); err != nil {
		_ = os.Remove(staged)
		return err
	}
	if err := renameRetry(ctx, staged, p.file); err != nil {
		_ = os.Remove(staged)
		if backErr := renameRetry(ctx, asidePath, p.file); backErr != nil {
			return fmt.Errorf("%w; and the entry it was to replace could not be put back, so it is kept as %s: %w", err, asidePath, backErr)
		}
		if syncErr := durableChain(chain, above); syncErr != nil {
			return fmt.Errorf("%w; the entry it was to replace is back in place, but its directory: %w", err, syncErr)
		}
		return err
	}
	return durableChain(chain, above)
}

// stageDurable writes data to a file beside path, whose name tmpPrefix
// patterns as os.CreateTemp does, gives it the owner of the entry at path
// (fsutil.KeepOwner), fsyncs it and closes it, and returns its name. A
// failure removes it again.
func stageDurable(path string, data []byte, tmpPrefix string) (name string, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPrefix)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := fsutil.KeepOwner(tmp, path); err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	if err := syncFile(tmp); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return tmp.Name(), nil
}
