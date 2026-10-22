package alsa

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// tree is a directory a reading reads through and nowhere else: the /proc
// reader's two roots and the sysfs reader's one. Every open, listing and
// look is made relative to the descriptor os.Root holds, so what it reaches
// is what the tree holds at that moment, whatever a check saw before, and a
// link that would leave the tree, an absolute one or one whose ".." climbs
// past the root, is refused at the open itself, even one swapped in after
// resolve looked. resolve follows each link itself first, so in a tree that
// holds still a link that leaves it is refused by name (errLinkAbsolute,
// errLinkLeavesRoot), and a loop is bounded.
type tree struct {
	root *os.Root
	// links bounds the links resolve follows in one path (more is a loop),
	// and, with hops, the components it walks.
	links, hops int
}

// The bounds a tree is resolved with: more than maxLinks links in one path
// is a loop, and a path is walked to 4*maxHops+maxLinks*maxHops components.
// The sysfs reader also stops a walk up a device's ancestors after maxHops.
const (
	maxLinks = 40
	maxHops  = 32
)

// errOutsideRoot is a path refused because following it would leave the
// tree: errLinkAbsolute and errLinkLeavesRoot, which errors.Is reports as
// it. Nothing past it is opened, so a link in a --proc-root or
// --sysfs-root tree cannot make --raw print a file from the rest of the
// filesystem, and no message names the link's target.
var errOutsideRoot = errors.New("path leaves the root")

// outsideRoot is one reason a path leaves the tree.
type outsideRoot string

func (e outsideRoot) Error() string { return string(e) }

func (e outsideRoot) Is(target error) bool { return target == errOutsideRoot }

// The reasons a path is refused.
var (
	errLinkLeavesRoot error = outsideRoot("a link leaves the root")
	errLinkAbsolute   error = outsideRoot("an absolute link is refused")
	errLinkLoop             = errors.New("too many links: a loop")
	errTooDeep              = errors.New("too many path components")
)

// osOpenRoot is os.OpenRoot, the one open of a tree's root. It is a seam
// at that open and nowhere before it: the test that swaps a FIFO in for the
// root does so when this is handed the root's name, so the swap lands
// after anything openTree does first, which is where a look before the
// open would leave its window (TestARootSwappedForAFIFOBeforeItsOpenIsRefusedPromptly).
var osOpenRoot = os.OpenRoot

// openTree opens the directory at dir as a tree. os.OpenRoot opens its path
// with a plain open, which a FIFO there would hold until a writer came, so
// it is handed rootPath(dir): on unix dir/., whose resolution fails at once
// (ENOTDIR) unless dir is a directory, so that one open never opens
// anything else, whatever is swapped in for the root. A look before the
// open would leave the instant between the two (round 3's directory-only
// look did). Everything below the root is checked at its own open.
func openTree(dir string, links, hops int) (*tree, error) {
	root, err := osOpenRoot(rootPath(dir))
	if err != nil {
		return nil, err
	}
	return &tree{root: root, links: links, hops: hops}, nil
}

func (t *tree) close() { _ = t.root.Close() }

// resolve follows every symlink in rel, component by component, inside the
// root, and returns the path with none left, slash-separated and relative
// to the root.
func (t *tree) resolve(rel string) (string, error) {
	todo := strings.Split(rel, "/")
	var out []string
	links, steps := 0, 0
	for len(todo) > 0 {
		steps++
		if steps > 4*t.hops+t.links*t.hops {
			return "", errTooDeep
		}
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(out) == 0 {
				return "", errLinkLeavesRoot
			}
			out = out[:len(out)-1]
			continue
		}
		next := path.Join(path.Join(out...), c)
		fi, err := t.root.Lstat(filepath.FromSlash(next))
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			out = append(out, c)
			continue
		}
		links++
		if links > t.links {
			return "", errLinkLoop
		}
		text, err := t.root.Readlink(filepath.FromSlash(next))
		if err != nil {
			return "", err
		}
		text = filepath.ToSlash(text)
		if path.IsAbs(text) || filepath.IsAbs(text) || filepath.VolumeName(text) != "" {
			return "", errLinkAbsolute
		}
		todo = append(strings.Split(text, "/"), todo...)
	}
	return path.Join(out...), nil
}

// opener opens rel, a slash-separated path under the root, with the flags
// it is handed.
func (t *tree) opener(rel string) func(flag int) (*os.File, error) {
	return func(flag int) (*os.File, error) { return t.root.OpenFile(filepath.FromSlash(rel), flag, 0) }
}

// readFile reads the regular file rel names, its links followed inside the
// tree, to limit bytes. before, when it is set, runs between the resolve and
// the open. What opens is what the root holds then, and it is read only
// when the opened descriptor is a regular file (openRegular).
func (t *tree) readFile(rel string, limit int64, before func()) ([]byte, error) {
	resolved, err := t.resolve(rel)
	if err != nil {
		return nil, err
	}
	if before != nil {
		before()
	}
	f, err := openRegular(t.opener(resolved))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readAllCapped(f, limit)
}

// readDir lists the directory rel names, its links followed inside the
// tree, sorted by name, as os.ReadDir does: with an error, the entries read
// before it. before, when it is set, runs between the resolve and the open,
// which is directory-only (openDirectory).
func (t *tree) readDir(rel string, before func()) ([]fs.DirEntry, error) {
	resolved, err := t.resolve(rel)
	if err != nil {
		return nil, err
	}
	if before != nil {
		before()
	}
	f, err := openDirectory(t.opener(resolved))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, err
}

// stat describes what rel names, its links followed inside the tree.
func (t *tree) stat(rel string) (fs.FileInfo, error) {
	resolved, err := t.resolve(rel)
	if err != nil {
		return nil, err
	}
	return t.root.Lstat(filepath.FromSlash(resolved))
}

// readAllCapped reads r to its end, refusing more than limit bytes.
func readAllCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("longer than %d bytes", limit)
	}
	return b, nil
}
