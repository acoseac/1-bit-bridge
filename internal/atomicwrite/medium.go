package atomicwrite

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// fsStat is what the medium check reads of a filesystem: its type, statfs's
// f_type (a Linux magic number, kept to 32 bits as 32-bit kernels give it),
// and the totals a statfs reports, which an overlay takes from its upper
// layer: the block size, the blocks and the inodes (f_bsize, f_blocks,
// f_files).
type fsStat struct {
	Type   uint32
	Bsize  int64
	Blocks uint64
	Files  uint64
}

// sameTotals reports whether two statfs answers give one filesystem's totals.
func (a fsStat) sameTotals(b fsStat) bool {
	return a.Bsize == b.Bsize && a.Blocks == b.Blocks && a.Files == b.Files
}

// The filesystems the medium check knows by their f_type, Linux's magic
// numbers (TestMediumMagicsAreLinuxs): tmpfs and ramfs keep everything in
// memory, an overlay keeps what is written to it in its upper layer, and FUSE
// is whatever its daemon serves, fuse-overlayfs among them.
const (
	tmpfsMagic     = 0x01021994
	ramfsMagic     = 0x858458f6
	overlayfsMagic = 0x794c7630
	fuseMagic      = 0x65735546
)

// maxOverlayDepth is how many overlays, each the upper layer of the one
// before, the medium check follows before it says it cannot tell.
const maxOverlayDepth = 8

// mediumFrom is the medium check on what statfs, the mount table and the
// mount IDs answer: nil for a root on storage the check finds no volatile
// layer in, ErrVolatile for one that does not keep what is written to it
// through a reboot, and ErrMediumUnknown where it cannot read what it must,
// failing closed (strictPath.storage). It refuses what it can tell is
// volatile and what it cannot read, and does not try to prove every stack
// persistent, which no check from outside can: ext4 on a zram device or on a
// loop image kept in tmpfs, and an NFS export written async, read as a disk
// does (the orchestrator's scope on round 10's review); the data directory
// must be on persistent storage, and those are the operator's to avoid.
//
// id is the root's mount ID, which the writer already holds. tmpfs and ramfs
// are told by their f_type. An overlay keeps what is written to it in its
// upper layer: the one the mount table names on the line of the overlay's
// own mount ID (overlayLayer), never on a line picked by its mount point's
// spelling. The path the table gives is as the overlay's mounter named it, so
// it must reach, here, the filesystem the overlay reports, since a statfs of
// an overlay gives its upper layer's totals; but some filesystems' totals
// move with ordinary writes (XFS's inode total with its free blocks, ZFS's
// block total with the pool's use), so where the two answers differ the
// overlay is read again: totals that held still while the path's differ name
// another filesystem, or none (ErrMediumUnknown), and totals that moved are
// no tmpfs's or ramfs's, which no write moves, so the upper layer is taken
// for what the path reaches, unless that is tmpfs or ramfs itself (both
// reviewers on round 10: round 10 refused XFS and ZFS upper layers at
// random). An overlay mounted volatile skips every sync of its upper layer,
// so nothing written there outlives a crash; an overlay over an overlay is
// followed by the inner one's mount ID; and a read-only overlay, with no
// upper layer, takes nothing, since a write there fails by itself (EROFS).
// FUSE is taken but for fuse-overlayfs (the Claude API on round 10), whose
// upper layer, what rootless Podman writes a container's files to, the
// table does not show.
func mediumFrom(root string, id uint64, statfs func(string) (fsStat, error), mountinfo func() ([]byte, error), idOf func(string) (uint64, bool, error)) error {
	path := root
	st, err := statfs(path)
	if err != nil {
		return err
	}
	// on is the overlays on the way down to the storage, each "an overlay
	// whose upper layer U is on ".
	on := ""
	for depth := 0; ; depth++ {
		switch st.Type {
		case tmpfsMagic:
			return fmt.Errorf("%w: %s is on %stmpfs", ErrVolatile, root, on)
		case ramfsMagic:
			return fmt.Errorf("%w: %s is on %sramfs", ErrVolatile, root, on)
		case fuseMagic:
			return fuseMedium(root, on, id, mountinfo)
		case overlayfsMagic:
		default:
			return nil
		}
		if depth == maxOverlayDepth {
			return fmt.Errorf("%w: %s is on %soverlays more than %d deep", ErrMediumUnknown, root, on, maxOverlayDepth)
		}
		table, err := mountinfo()
		if err != nil {
			return fmt.Errorf("%w: %s is on %san overlay, and the mount table that names its upper layer cannot be read: %w", ErrMediumUnknown, root, on, err)
		}
		layer, err := overlayLayer(table, id)
		switch {
		case err != nil:
			return fmt.Errorf("%w: %s is on %san overlay: %w", ErrMediumUnknown, root, on, err)
		case layer.volatile:
			return fmt.Errorf("%w: %s is on %san overlay mounted volatile, which skips every sync of its upper layer, so nothing written there outlives a crash", ErrVolatile, root, on)
		case layer.upper == "":
			return nil
		case !filepath.IsAbs(layer.upper):
			return fmt.Errorf("%w: %s is on %san overlay whose upper layer the mount table names by a relative path, %s, which reaches nothing certain here", ErrMediumUnknown, root, on, layer.upper)
		}
		upper, err := statfs(layer.upper)
		if err != nil {
			return fmt.Errorf("%w: %s is on %san overlay whose upper layer %s, as the mount table names it, cannot be read here: %w", ErrMediumUnknown, root, on, layer.upper, err)
		}
		if !upper.sameTotals(st) {
			again, err := statfs(path)
			switch {
			case err != nil:
				return fmt.Errorf("%w: %s is on %san overlay that cannot be read again: %w", ErrMediumUnknown, root, on, err)
			case again.sameTotals(st):
				return fmt.Errorf("%w: %s is on %san overlay whose upper layer %s, as the mount table names it, reaches another filesystem here than the one the overlay writes to", ErrMediumUnknown, root, on, layer.upper)
			case upper.Type == tmpfsMagic || upper.Type == ramfsMagic:
				return fmt.Errorf("%w: %s is on %san overlay whose totals move with what is written to it, while its upper layer %s, as the mount table names it, reaches tmpfs or ramfs here, whose totals do not", ErrMediumUnknown, root, on, layer.upper)
			}
		}
		on += "an overlay whose upper layer " + layer.upper + " is on "
		if upper.Type == overlayfsMagic || upper.Type == fuseMagic {
			next, ok, err := idOf(layer.upper)
			switch {
			case err != nil:
				return fmt.Errorf("%w: %s is on %sa filesystem whose mount cannot be told: %w", ErrMediumUnknown, root, on, err)
			case !ok:
				return fmt.Errorf("%w: %s is on %sa filesystem the platform gives no mount ID for", ErrMediumUnknown, root, on)
			}
			id = next
		}
		path, st = layer.upper, upper
	}
}

// fuseMedium is the medium check of a FUSE filesystem, the mount id: taken,
// as the check cannot see what a FUSE daemon writes to, but for
// fuse-overlayfs, whose upper layer the mount table does not show, and a
// mount whose line cannot be read, refused as storage that cannot be told
// (ErrMediumUnknown).
func fuseMedium(root, on string, id uint64, mountinfo func() ([]byte, error)) error {
	table, err := mountinfo()
	if err != nil {
		return fmt.Errorf("%w: %s is on %sFUSE, and the mount table that names its filesystem cannot be read: %w", ErrMediumUnknown, root, on, err)
	}
	rec, err := mountLine(table, id)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %s is on %sFUSE: %w", ErrMediumUnknown, root, on, err)
	case rec.fstype == "fuse.fuse-overlayfs":
		return fmt.Errorf("%w: %s is on %sfuse-overlayfs, whose upper layer the mount table does not show", ErrMediumUnknown, root, on)
	}
	return nil
}

// mountRecord is one line of a mountinfo table, as the medium check reads it:
// its filesystem type, and its super options, split on their commas, each
// still escaped as the filesystem wrote it.
type mountRecord struct {
	fstype string
	super  []string
}

// mountLine reads the line of a mountinfo table (proc(5)) whose first field
// is id, the mount ID the writer holds, never a line picked by its mount
// point's spelling. The kernel separates the fields with one ASCII space and
// escapes a space, a tab, a newline and a backslash in each, so the line is
// split on that space alone: a Unicode white space in a path, which the
// kernel leaves as it is, stays inside its field (Codex on round 10:
// strings.Fields split U+00A0 out of a lowerdir, and the upperdir and
// volatile after it were lost). The record must be whole: six fields, any
// optional fields, the "-" that ends them, then exactly the type, the source
// and the super options, none of them empty. A line cut short, with a field
// too many or an empty one, and an id the table does not list are errors.
func mountLine(table []byte, id uint64) (mountRecord, error) {
	want := strconv.FormatUint(id, 10)
	for _, line := range strings.Split(string(table), "\n") {
		fields := strings.Split(line, " ")
		if fields[0] != want {
			continue
		}
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || len(fields) != sep+4 || slices.Contains(fields, "") {
			return mountRecord{}, fmt.Errorf("the mount table's line for mount %d does not read as one", id)
		}
		return mountRecord{fstype: fields[sep+1], super: strings.Split(fields[sep+3], ",")}, nil
	}
	return mountRecord{}, fmt.Errorf("the mount table lists no mount %d", id)
}

// overlayMount is what the medium check reads of an overlay's mount in the
// mount table: its upper layer as the table names it, "" for a read-only
// overlay, and whether it was mounted volatile.
type overlayMount struct {
	upper    string
	volatile bool
}

// overlayLayer reads the overlay mounted as id from a mountinfo table, on the
// line of its mount ID (mountLine), whose type must be overlay: its super
// options give the upper layer (upperdir, unescaped as the kernel escapes an
// option's value, a backslash and three octal digits for a space, a comma, a
// backslash and the like) and volatile (an option of its own, or
// fsync=volatile). The kernel writes the superblock's ro or rw first among
// them, and only an overlay that is read-only there may name no upper layer:
// a writable one that names none is an error, never taken for read-only.
func overlayLayer(table []byte, id uint64) (overlayMount, error) {
	rec, err := mountLine(table, id)
	if err != nil {
		return overlayMount{}, err
	}
	if rec.fstype != "overlay" {
		return overlayMount{}, fmt.Errorf("the mount table names mount %d %s, not an overlay", id, rec.fstype)
	}
	var m overlayMount
	for _, opt := range rec.super {
		key, value, _ := strings.Cut(opt, "=")
		switch {
		case key == "upperdir":
			m.upper = unescapeMountInfo(value)
		case key == "volatile", key == "fsync" && value == "volatile":
			m.volatile = true
		}
	}
	if m.upper == "" && rec.super[0] != "ro" {
		return overlayMount{}, fmt.Errorf("the mount table names no upper layer for mount %d, an overlay that is not read-only", id)
	}
	return m, nil
}

// unescapeMountInfo undoes mountinfo's escaping of a path: the kernel writes
// a space, tab, newline and backslash as a backslash and three octal digits
// (proc(5), seq_path's "\t\n\\ "), and a filesystem's option values the same
// way (seq_show_option, which escapes a comma and an equals sign too).
// internal/doctor's unescapeMountinfo is its twin, for the cgroup mounts.
func unescapeMountInfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ancestorsOnMount is every directory above root while it lies on root's
// mount, id, by the mount ID the platform gives each (idOf), never by its
// spelling: the parent first, the mount's own root last, and none for a root
// that is its mount's root or the filesystem's. root is resolved, so each is
// a directory, never a link. A directory on the way the platform gives no
// mount ID for is ErrMountsUnknown, and its error is returned as it is.
func ancestorsOnMount(root string, id uint64, idOf func(string) (uint64, bool, error)) ([]string, error) {
	var above []string
	for d := root; ; {
		parent := filepath.Dir(d)
		if parent == d {
			return above, nil
		}
		m, ok, err := idOf(parent)
		switch {
		case err != nil:
			return nil, err
		case !ok:
			return nil, fmt.Errorf("%w: the platform gives no mount ID for %s, above %s", ErrMountsUnknown, parent, root)
		case m != id:
			return above, nil
		}
		above = append(above, parent)
		d = parent
	}
}

// storage asks the platform what the writer must know of the storage under
// root, by the mount root lies on: the medium check (mediumOf), and the
// directories above root, up to the root of its mount, that a copy-up can
// create entries in (copyUpAncestorsOf), which a write that lands, and the
// probe, sync once the chain is synced. On an overlay a write below a root
// found in the lower layer alone copies the root up, and every directory
// above it up to the overlay's own root, creating entries the chain never
// syncs, so a crash could take the copy and the reservation in it while the
// player's identity below it stayed (Codex on round 10; the orchestrator
// chose to sync every directory from the data directory up to its mount's
// root). Every entry the write reaches is on root's mount (plain), so root's
// storage is every entry's. Where the platform gives no mount identity, plain
// has refused already, and where it is not expected to (Windows, which
// refuses before it gets here), neither is asked.
func (p strictPath) storage() ([]string, error) {
	id, ok, err := mountIDOf(p.root)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, nil
	}
	if err := mediumOf(p.root, id); err != nil {
		return nil, err
	}
	return copyUpAncestorsOf(p.root, id)
}

// SetMediumForTest swaps the strict writer's and the probe's medium check for
// a test in another package: fn is handed the root, its links resolved, and
// its answer is the check's: nil for storage that outlives a reboot, an error
// wrapping ErrVolatile or ErrMediumUnknown otherwise. It returns what puts the
// check before it back. The seam is process-global: a test that uses it MUST
// NOT call `t.Parallel()`, and restores it in a `t.Cleanup`. Public only
// because the player engine's tests drive its probe and its reservation
// writes through the real writer, in a temporary directory that may itself be
// on tmpfs.
func SetMediumForTest(fn func(root string) error) (restore func()) {
	prev := mediumOf
	mediumOf = func(root string, _ uint64) error { return fn(root) }
	return func() { mediumOf = prev }
}
