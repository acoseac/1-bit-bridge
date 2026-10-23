package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// fsStat is what the medium check reads of a filesystem: its type, statfs's
// f_type (a Linux magic number, kept to 32 bits as 32-bit kernels give it),
// and the totals a statfs reports, which an overlay takes from its upper
// layer: the block size, the blocks and the inodes (f_bsize, f_blocks,
// f_files). Device is the path's st_dev, rendered as major:minor on Linux.
type fsStat struct {
	Type   uint32
	Bsize  int64
	Blocks uint64
	Files  uint64
	Device string // Linux st_dev as major:minor; empty where unavailable.
}

// sameTotals reports whether two statfs answers have equal totals.
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

// totalsRounds bounds retries for an agreement with a moving filesystem.
// Exhausting them is inconclusive, never proof of another filesystem.
const totalsRounds = 3

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
// block total with the pool's use), so where the two answers differ they are
// read again, for up to three rounds (upperAsReached). Totals that moved
// are no tmpfs's or ramfs's, which no write moves. A sustained mismatch is
// inconclusive: the upper path's mount ID selects its own mountinfo line,
// identifying the filesystem visible here, not the overlay's backing in the
// mounter's namespace. Even a confirmed XFS/ZFS may give repeated A,B,A
// samples. The error describes that evidence and suggests a retry, never
// claims another filesystem. An overlay mounted volatile skips every
// sync of its upper layer, so nothing written there outlives a crash; an
// overlay over an overlay is followed by the inner one's mount ID; and a
// root on a read-only overlay, with no upper layer, takes nothing, since a
// write there fails by itself (EROFS). A read-only overlay reached below
// that root cannot be a writable overlay's upper layer and stays unknown.
// FUSE is taken but for fuse-overlayfs (the Claude
// API on round 10), whose upper layer, what rootless Podman writes a
// container's files to, the table does not show.
//
// A root whose mount is a bind of a directory inside an overlay, its line's
// root field another than "/", is refused wherever a writable overlay would
// be taken (ErrOverlayBind, also matching ErrMediumUnknown). A write below
// a data directory in the lower layer alone
// copies up the directories above it inside the overlay, which this mount
// cannot reach to sync (strictPath.storage), and so would a crash take them.
// An operator's systemd ProtectSystem=strict with ReadWritePaths makes such a
// bind of the data directory, as do a chroot and a container's bind of an
// overlay host's directory; the fix is a data directory on a filesystem that
// is not an overlay, or on the overlay's own mount (the orchestrator's
// decision on Codex's High and the Claude API's Medium on round 11, which
// rejected syncing through another mount of the superblock and syncfs, whose
// errors overlayfs reports only on recent kernels). Our own unit adds no
// sandboxing, and an overlay root's own mount, Volumio's shape, is taken.
func mediumFrom(root string, id uint64, statfs func(string) (fsStat, error), mountinfo func() ([]byte, error), idOf func(string) (uint64, bool, error)) error {
	path := root
	st, err := statfs(path)
	if err != nil {
		return err
	}
	// on is the overlays on the way down to the storage, each "an overlay
	// whose upper layer U is on ".
	on := ""
	// bound is the root field of the root's own mount, an overlay's, when
	// that is a bind of a directory inside it: taken is the verdict where
	// the storage under it would be taken.
	bound := ""
	taken := func() error {
		if bound == "" {
			return nil
		}
		return fmt.Errorf("%w: %s is on a bind of %s, a directory inside an overlay, not the overlay's own mount: a copy-up the write makes creates entries above it that this mount cannot reach to sync: put the data directory on a filesystem that is not an overlay, or on the overlay's own mount", ErrOverlayBind, root, bound)
	}
	for depth := 0; ; depth++ {
		switch st.Type {
		case tmpfsMagic:
			return fmt.Errorf("%w: %s is on %stmpfs", ErrVolatile, root, on)
		case ramfsMagic:
			return fmt.Errorf("%w: %s is on %sramfs", ErrVolatile, root, on)
		case fuseMagic:
			if err := fuseMedium(root, on, id, mountinfo); err != nil {
				return err
			}
			return taken()
		case overlayfsMagic:
		default:
			return taken()
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
		case depth == 0 && layer.mountRoot != "/":
			bound = layer.mountRoot
		}
		switch {
		case layer.upper == "":
			if depth == 0 {
				return nil // The root's own mount is read-only; a write fails EROFS.
			}
			return fmt.Errorf("%w: %s is on %sa read-only overlay: the upper path reaches a read-only overlay, which cannot be a writable overlay's upper layer", ErrMediumUnknown, root, on)
		case !strings.HasPrefix(layer.upper, "/"):
			// The mount table is Linux's, so a path in it is absolute by its
			// leading slash, never by what filepath says on the system the
			// tests run on (on Windows it wants a drive).
			return fmt.Errorf("%w: %s is on %san overlay whose upper layer the mount table names by a relative path, %s, which reaches nothing certain here", ErrMediumUnknown, root, on, layer.upper)
		}
		upper, err := upperAsReached(root, on, path, layer.upper, st, statfs, func(u fsStat) (string, error) {
			return upperMountEvidence(layer.upper, u.Device, table, idOf)
		})
		if err != nil {
			return err
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

// upperAsReached reads an overlay's upper path and retries differing totals.
// Agreement or movement of the overlay's totals keeps the existing bounded
// medium check; a tmpfs/ramfs path cannot explain moving overlay totals.
// Exhausted retries remain ErrMediumUnknown. Evidence identifies only the
// filesystem visible at upper, not an otherwise unreachable backing layer.
func upperAsReached(root, on, path, upper string, first fsStat, statfs func(string) (fsStat, error), evidence func(fsStat) (string, error)) (fsStat, error) {
	o := first
	for round := 1; ; round++ {
		u, err := statfs(upper)
		if err != nil {
			return fsStat{}, fmt.Errorf("%w: %s is on %san overlay whose upper layer %s, as the mount table names it, cannot be read here: %w", ErrMediumUnknown, root, on, upper, err)
		}
		if u.sameTotals(o) {
			return u, nil
		}
		again, err := statfs(path)
		switch {
		case err != nil:
			return fsStat{}, fmt.Errorf("%w: %s is on %san overlay that cannot be read again: %w", ErrMediumUnknown, root, on, err)
		case u.sameTotals(again):
			return u, nil
		case !again.sameTotals(o) && (u.Type == tmpfsMagic || u.Type == ramfsMagic):
			return fsStat{}, fmt.Errorf("%w: %s is on %san overlay whose totals move with what is written to it, while its upper layer %s, as the mount table names it, reaches tmpfs or ramfs here, whose totals do not", ErrMediumUnknown, root, on, upper)
		case !again.sameTotals(o):
			return u, nil
		case round == totalsRounds:
			detail, evidenceErr := evidence(u)
			if evidenceErr != nil {
				return fsStat{}, fmt.Errorf("%w: %s is on %san overlay whose upper layer %s has differing totals after %d reads; the result is inconclusive and its visible filesystem cannot be confirmed: %w; retry when filesystem activity has settled, and check that the upper path is visible in the service's mount namespace", ErrMediumUnknown, root, on, upper, totalsRounds, evidenceErr)
			}
			return fsStat{}, fmt.Errorf("%w: %s is on %san overlay whose upper layer %s is visible here as %s, but its totals differ after %d reads; the result is inconclusive, since totals may change between reads: retry when filesystem activity has settled, and check that the upper path is visible in the service's mount namespace", ErrMediumUnknown, root, on, upper, detail, totalsRounds)
		}
		o = again
	}
}

// upperMountEvidence names the filesystem visible at upper by its own mount
// ID's line. Device numbers are detail only: a btrfs subvolume's st_dev can
// differ from mountinfo's superblock device. A matching mount-point spelling
// is not identity; neither is this evidence of the overlay's backing across
// mount namespaces. Device numbers never decide strictPath's mount boundaries.
func upperMountEvidence(upper, device string, table []byte, idOf func(string) (uint64, bool, error)) (string, error) {
	id, ok, err := idOf(upper)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no mount ID for %s", upper)
	}
	rec, err := mountLine(table, id)
	if err != nil {
		return "", err
	}
	if device == "" {
		return fmt.Sprintf("%s (mount %d, mount device %s)", rec.fstype, id, rec.device), nil
	}
	return fmt.Sprintf("%s (mount %d, mount device %s, path device %s)", rec.fstype, id, rec.device, device), nil
}

// fuseMedium is the medium check of a FUSE filesystem, the mount id: taken,
// as the check cannot see what a FUSE daemon writes to, but for
// fuse-overlayfs, whose upper layer the mount table does not show, told by
// its type's subtype or, for a build that sets none, by its source, and a
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
	case rec.fstype == "fuse.fuse-overlayfs", rec.source == "fuse-overlayfs":
		return fmt.Errorf("%w: %s is on %sfuse-overlayfs, whose upper layer the mount table does not show", ErrMediumUnknown, root, on)
	}
	return nil
}

// mountRecord is one line of a mountinfo table, as the medium check reads it:
// the directory of its filesystem the mount shows as its own root ("/" for
// the filesystem's root, another for a bind of a directory inside it), its
// device (major:minor), filesystem type, source, and super options, split
// on their commas, each still escaped as the filesystem wrote it.
type mountRecord struct {
	device string
	root   string
	fstype string
	source string
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
// and the super options, none of them empty but the source, which the kernel
// writes empty for a filesystem mounted with none (the Claude API on round
// 11). A line cut short, with a field too many or an empty one, and an id the
// table does not list are errors.
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
		if sep < 0 || len(fields) != sep+4 || slices.Contains(fields[:sep+2], "") || fields[sep+3] == "" {
			return mountRecord{}, fmt.Errorf("the mount table's line for mount %d does not read as one", id)
		}
		return mountRecord{
			device: fields[2],
			root:   unescapeMountInfo(fields[3]),
			fstype: fields[sep+1],
			source: unescapeMountInfo(fields[sep+2]),
			super:  strings.Split(fields[sep+3], ","),
		}, nil
	}
	return mountRecord{}, fmt.Errorf("the mount table lists no mount %d", id)
}

// overlayMount is what the medium check reads of an overlay's mount in the
// mount table: its upper layer as the table names it, "" for a read-only
// overlay, whether it was mounted volatile, and the directory of the overlay
// the mount shows as its root, "/" for the overlay's own mount.
type overlayMount struct {
	upper     string
	volatile  bool
	mountRoot string
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
	m := overlayMount{mountRoot: rec.root}
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

// copyUpsFrom is the directories above root a copy-up can create entries in,
// on what statfs and the mount IDs answer (platformCopyUps wires the
// kernel's): on an overlay, every one up to the root of root's mount, id
// (ancestorsOnMount); on anything else none, as nothing copies up, and no walk.
// A statfs or a walk that fails is its error. It is platform-neutral, as
// mediumFrom is, so that its tests hold the default where the namespace tests
// cannot run (the Claude API on round 11).
func copyUpsFrom(root string, id uint64, statfs func(string) (fsStat, error), idOf func(string) (uint64, bool, error)) ([]string, error) {
	st, err := statfs(root)
	if err != nil {
		return nil, err
	}
	if st.Type != overlayfsMagic {
		return nil, nil
	}
	return ancestorsOnMount(root, id, idOf)
}

// syncAbove syncs the directories above the root a copy-up can create entries
// in (copyUpsFrom), after the chain, in a write that landed and in the
// probe. A permission error names the directory and advises read access;
// other open errors are wrapped without that advice. A directory that will
// not sync is the barrier's failure, marked as ever.
func syncAbove(above []string) error {
	for _, d := range above {
		switch err := syncDirAt(d); {
		case err == nil:
		case errors.Is(err, ErrBarrierFailed):
			return fmt.Errorf("the directory %s, above the root on an overlay, cannot be synced: %w", d, err)
		case errors.Is(err, fs.ErrPermission):
			return fmt.Errorf("the directory %s, above the root on an overlay, could not be opened for its sync, which a copy-up below it needs: give the service read access to it, or put the data directory on a filesystem that is not an overlay: %w", d, err)
		default:
			return fmt.Errorf("the directory %s, above the root on an overlay, could not be opened for its sync, which a copy-up below it needs: %w", d, err)
		}
	}
	return nil
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
