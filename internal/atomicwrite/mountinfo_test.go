package atomicwrite

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"
)

// parseMountInfo is the mount ID of each mount point a mountinfo file lists
// (proc(5)): the first field of each line, by its fifth, unescaped. A line
// with fewer fields, an ID that does not read as a number, or a mount point
// that is not an absolute path is an error. Where one mount is mounted over
// another at the same point, the later line is the one a path there reaches,
// and wins. The writer tells a mount by the identity the platform gives the
// entry a path reaches, never by a spelling (Codex on rounds 7 and 8), and
// reads the table only for an overlay's upper layer, on the line of the
// root's own mount ID (overlayLayer). The table is the tests' oracle for the
// IDs the kernel gives (TestPlatformMountTableReadsTheKernels).
func parseMountInfo(data []byte) (map[string]uint64, error) {
	ids := map[string]uint64{}
	for n, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.HasPrefix(fields[4], "/") {
			return nil, fmt.Errorf("line %d of the mount table does not read as one", n+1)
		}
		id, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("line %d of the mount table: %w", n+1, err)
		}
		ids[unescapeMountInfo(fields[4])] = id
	}
	return ids, nil
}

// TestParseMountInfo pins the oracle's reading of a mount table: the mount
// ID of each mount point, the point unescaped as the kernel escapes a space,
// a tab, a newline and a backslash, a mount over another at the same point
// winning; and a line that does not read as one, refused, since an oracle
// that misreads the table holds the writer's IDs to nothing.
func TestParseMountInfo(t *testing.T) {
	const table = `22 28 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
28 1 259:2 / / rw,relatime shared:1 - ext4 /dev/root rw
61 28 259:2 /srv/real /data/with\040space rw,relatime shared:1 - ext4 /dev/root rw
62 28 259:2 /srv/back /data/back\134slash rw,relatime - ext4 /dev/root rw
70 28 0:40 / /data/over rw,relatime - tmpfs tmpfs rw
71 70 0:41 / /data/over rw,relatime - tmpfs tmpfs rw
`
	got, err := parseMountInfo([]byte(table))
	if err != nil {
		t.Fatalf("parseMountInfo: %v", err)
	}
	want := map[string]uint64{"/proc": 22, "/": 28, "/data/with space": 61, `/data/back\slash`: 62, "/data/over": 71}
	if !maps.Equal(got, want) {
		t.Fatalf("mount IDs %v, want %v", got, want)
	}
	for _, bad := range []string{"22 28 0:21 /\n", "22 28 0:21 / proc rw - proc proc rw\n", "x 28 0:21 / /proc rw - proc proc rw\n"} {
		if _, err := parseMountInfo([]byte(bad)); err == nil {
			t.Errorf("parseMountInfo(%q) answered no error", bad)
		}
	}
}
