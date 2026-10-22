package atomicwrite

import (
	"fmt"
	"path/filepath"
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

// The filesystems the medium check knows by their f_type, Linux's magic
// numbers (TestMediumMagicsAreLinuxs): tmpfs and ramfs keep everything in
// memory, and an overlay keeps what is written to it in its upper layer.
const (
	tmpfsMagic     = 0x01021994
	ramfsMagic     = 0x858458f6
	overlayfsMagic = 0x794c7630
)

// maxOverlayDepth is how many overlays, each the upper layer of the one
// before, the medium check follows before it says it cannot tell.
const maxOverlayDepth = 8

// mediumFrom is the medium check on what statfs, the mount table and the
// mount IDs answer: nil for a root on storage that keeps what is written to
// it through a reboot, ErrVolatile for one that does not, and
// ErrMediumUnknown where that cannot be told, the check failing closed
// (strictPath.medium). id is the root's mount ID, which the writer already
// holds. tmpfs and ramfs are told by their f_type. An overlay keeps what is
// written to it in its upper layer: the one the mount table names on the
// line of the overlay's own mount ID (overlayLayer), never on a line picked
// by its mount point's spelling, and that upper layer's path must reach,
// here, the filesystem the overlay reports, since a statfs of an overlay
// gives its upper layer's totals: a path the mount table gives as its
// mounter named it may reach another filesystem in this mount namespace, or
// none. An overlay mounted volatile skips every sync of its upper layer, so
// nothing written there outlives a crash, and an overlay over an overlay is
// followed by the inner one's mount ID. A read-only overlay, with no upper
// layer, takes nothing: a write there fails by itself (EROFS).
func mediumFrom(root string, id uint64, statfs func(string) (fsStat, error), mountinfo func() ([]byte, error), idOf func(string) (uint64, bool, error)) error {
	st, err := statfs(root)
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
		switch {
		case err != nil:
			return fmt.Errorf("%w: %s is on %san overlay whose upper layer %s, as the mount table names it, cannot be read here: %w", ErrMediumUnknown, root, on, layer.upper, err)
		case upper.Bsize != st.Bsize || upper.Blocks != st.Blocks || upper.Files != st.Files:
			return fmt.Errorf("%w: %s is on %san overlay whose upper layer %s, as the mount table names it, reaches another filesystem here than the one the overlay writes to", ErrMediumUnknown, root, on, layer.upper)
		}
		on += "an overlay whose upper layer " + layer.upper + " is on "
		if upper.Type == overlayfsMagic {
			next, ok, err := idOf(layer.upper)
			switch {
			case err != nil:
				return fmt.Errorf("%w: %s is on %san overlay whose mount cannot be told: %w", ErrMediumUnknown, root, on, err)
			case !ok:
				return fmt.Errorf("%w: %s is on %san overlay the platform gives no mount ID for", ErrMediumUnknown, root, on)
			}
			id = next
		}
		st = upper
	}
}

// overlayMount is what the medium check reads of an overlay's mount in the
// mount table: its upper layer as the table names it, "" for a read-only
// overlay, and whether it was mounted volatile.
type overlayMount struct {
	upper    string
	volatile bool
}

// overlayLayer reads the overlay mounted as id from a mountinfo table
// (proc(5)): the line whose first field is id, the mount ID the writer
// holds, never a line picked by its mount point's spelling; its type, the
// field after the "-" that ends the optional fields, which must be overlay;
// and its super options, the field after the source: upperdir, unescaped as
// the kernel escapes an option's value (a backslash and three octal digits
// for a space, a comma, a backslash and the like), and volatile, an option of
// its own or fsync=volatile. A mount that is not an overlay, one the table
// does not list, and a line that does not read as one are errors.
func overlayLayer(table []byte, id uint64) (overlayMount, error) {
	want := strconv.FormatUint(id, 10)
	for _, line := range strings.Split(string(table), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != want {
			continue
		}
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+3 >= len(fields) {
			return overlayMount{}, fmt.Errorf("the mount table's line for mount %d does not read as one", id)
		}
		if fstype := fields[sep+1]; fstype != "overlay" {
			return overlayMount{}, fmt.Errorf("the mount table names mount %d %s, not an overlay", id, fstype)
		}
		var m overlayMount
		for _, opt := range strings.Split(fields[sep+3], ",") {
			key, value, _ := strings.Cut(opt, "=")
			switch {
			case key == "upperdir":
				m.upper = unescapeMountInfo(value)
			case key == "volatile", key == "fsync" && value == "volatile":
				m.volatile = true
			}
		}
		return m, nil
	}
	return overlayMount{}, fmt.Errorf("the mount table lists no mount %d", id)
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

// medium refuses a root on storage that does not keep what is written to it
// through a reboot (ErrVolatile), or on storage of which that cannot be told
// (ErrMediumUnknown), as the medium check answers for the root and the mount
// it lies on (mediumOf). On such storage every barrier answers, and a reboot
// takes what was written and leaves what was there before: the reservation
// the player wrote goes, and the identity it wrote before the storage became
// volatile stays, so the IDs and tokens the reservation covered would be
// handed out again, with no crash at all (the Claude API on round 9: the
// Raspberry Pi OS "Overlay File System" option, an overlay whose upper layer
// is a tmpfs). The orchestrator's decision is to fail closed there, and so
// where the storage cannot be told. Every entry the write reaches is on the
// root's mount (plain), so the root's storage is every entry's. Where the
// platform gives no mount identity, plain has refused already, and where it
// is not expected to (Windows, which refuses before it gets here), the check
// is not asked.
func (p strictPath) medium() error {
	id, ok, err := mountIDOf(p.root)
	switch {
	case err != nil:
		return err
	case !ok:
		return nil
	}
	return mediumOf(p.root, id)
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
