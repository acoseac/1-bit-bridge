package manifest

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// NameCollision is one group of names in one folder that the app reads as
// one name: equal under renameFold, the key the deletion pass pairs a rename
// by (NFC, then the Unicode lowercase unicode_lower applies), and different
// as stored.
type NameCollision struct {
	// Paths are the library-relative paths of the group's spellings, the
	// folder or file each one names, in byte order. Always two or more.
	Paths []string
}

// NameCollisionReport is FindNameCollisions' answer.
type NameCollisionReport struct {
	// Paths is how many track paths were compared.
	Paths int
	// Groups counts every group found.
	Groups int
	// First holds the first groups in path order (by each group's first
	// path), at most the bound the caller passed.
	First []NameCollision
}

// FindNameCollisions finds the names, in paths, that the app reads as one
// name: siblings in one folder, as stored, whose names are equal under
// renameFold and differ as stored. `bridge doctor`'s name-collisions check
// asks it of the indexed track paths (backlog B420).
//
// The app folds a bridge share's paths, NFC then lowercase
// (`share.normalize(path:)`, which unicode_lower mirrors), so "Album" beside
// "ALBUM", or an NFC "Café" beside an NFD one, is one folder there, and two
// such files are one track. The scanner keeps both: the rename reap leaves
// two spellings the walk saw both of (CLAUDE.md's Speller bullet). They are
// almost always copies, made by a copy or sync onto a filesystem that keeps
// both spellings of a name.
//
// Each path component is compared with its siblings only, so a group sits
// at the level where its paths first differ: "Artist/Album/01.flac" beside
// "artist/Album/01.flac" is the one group {"Artist", "artist"}, never one per
// track below it. With several roots every stored path begins with its
// root's folder name, so a path is compared only with paths in its own root;
// the roots' folder names are siblings at the top and are compared with each
// other, since the app folds them with the rest of the path.
//
// paths need not be sorted; a sorted slice (TrackPathsLocal's order) is not
// copied. The walk keeps one folder's names per level of the path it is on,
// so its memory is the widest folders on one branch, not the library.
// maxGroups bounds First; Groups counts past it.
func FindNameCollisions(paths []string, maxGroups int) NameCollisionReport {
	out := NameCollisionReport{Paths: len(paths)}
	if !slices.IsSorted(paths) {
		paths = slices.Clone(paths)
		slices.Sort(paths)
	}
	w := collisionWalk{out: &out, max: maxGroups, stack: []*collisionFolder{{}}}
	for _, p := range paths {
		w.add(p)
	}
	for len(w.stack) > 0 {
		w.close()
	}
	return out
}

// collisionFolder is one open folder of the walk: its stored path ("" for
// the library), its own name, and its children's names by fold key, each
// list in the order the sorted walk met them.
type collisionFolder struct {
	dir      string
	name     string
	children map[string][]string
}

func (f *collisionFolder) addChild(name string) {
	key := nameCollisionKey(name)
	names := f.children[key]
	// A name met twice is one spelling: a stale file row beside the folder
	// that replaced it ("x" before "x b/…" and "x/…" in byte order).
	if slices.Contains(names, name) {
		return
	}
	if f.children == nil {
		f.children = map[string][]string{}
	}
	f.children[key] = append(names, name)
}

// nameCollisionKey is the name a folder's children are compared by:
// renameFold, the deletion pass's key, and so the app's. The check's one
// call of it, so a change to what it compares is made here.
//
// An ASCII name takes strings.ToLower, which is renameFold's answer for
// ASCII and costs no normalization: NFC leaves ASCII as it is, and the only
// ASCII letters with a case are A to Z, which both lowercases map alike
// (TestNameCollisionKeyIsTheRenameFold). Every other name takes renameFold
// itself, never strings.ToLower, whose simple mapping joins U+0130 to a
// plain "i" (renameFold's docblock). Measured over the 112,000 names of a
// synthetic 100,000-track library (one artist in ten accented), renameFold
// took ~35 ms and this ~7 ms.
func nameCollisionKey(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] >= utf8.RuneSelf {
			return renameFold(name)
		}
	}
	return strings.ToLower(name)
}

// collisionWalk walks sorted paths with the chain of folders the current
// path is in open, and settles a folder's groups when the walk leaves it:
// a folder's paths are contiguous in byte order, so its list of names is
// complete then.
type collisionWalk struct {
	out   *NameCollisionReport
	max   int
	stack []*collisionFolder
}

// add takes the next path: the folders of it already open stay open, the
// ones the walk has left are settled, and the rest are opened. start is
// where the unread part of p begins; a folder's path is the prefix of p that
// ends with its name.
func (w *collisionWalk) add(p string) {
	open, start := 0, 0
	for open+1 < len(w.stack) {
		i := strings.IndexByte(p[start:], '/')
		if i < 0 || w.stack[open+1].name != p[start:start+i] {
			break
		}
		open, start = open+1, start+i+1
	}
	for len(w.stack) > open+1 {
		w.close()
	}
	for {
		i := strings.IndexByte(p[start:], '/')
		if i < 0 {
			break
		}
		name := p[start : start+i]
		w.stack[len(w.stack)-1].addChild(name)
		w.stack = append(w.stack, &collisionFolder{dir: p[:start+i], name: name})
		start += i + 1
	}
	w.stack[len(w.stack)-1].addChild(p[start:])
}

// close settles the innermost open folder: every fold key with two or more
// spellings is a group.
func (w *collisionWalk) close() {
	f := w.stack[len(w.stack)-1]
	w.stack = w.stack[:len(w.stack)-1]
	for _, names := range f.children {
		if len(names) < 2 {
			continue
		}
		g := NameCollision{Paths: make([]string, len(names))}
		for i, n := range names {
			g.Paths[i] = joinCollisionPath(f.dir, n)
		}
		slices.Sort(g.Paths)
		w.keep(g)
	}
}

// keep counts g and holds it in First when it is among the first max groups
// by first path. The walk settles a folder after its subfolders, and a
// folder's keys come out of a map, so the order groups arrive in is not the
// order they are named in. No two groups share a path, so first paths are
// distinct.
func (w *collisionWalk) keep(g NameCollision) {
	w.out.Groups++
	if w.max <= 0 {
		return
	}
	first := w.out.First
	i, _ := slices.BinarySearchFunc(first, g.Paths[0], func(c NameCollision, p string) int {
		return strings.Compare(c.Paths[0], p)
	})
	switch {
	case len(first) < w.max:
		w.out.First = slices.Insert(first, i, g)
	case i < w.max:
		w.out.First = slices.Insert(first[:w.max-1], i, g)
	}
}

func joinCollisionPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
