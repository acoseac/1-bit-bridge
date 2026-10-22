package atomicwrite

import (
	"fmt"
	"strconv"
	"strings"
)

// parseMountInfo is the mount points a mountinfo file lists (proc(5)): the
// fifth field of each line, unescaped. A line with fewer fields, or whose
// mount point is not an absolute path, is an error: a table that does not
// read as the kernel writes one rules no mount out. It is platform-neutral
// so its tests read captured tables everywhere; only Linux reads the
// kernel's (platformMountTable).
func parseMountInfo(data []byte) (map[string]bool, error) {
	points := map[string]bool{}
	for n, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.HasPrefix(fields[4], "/") {
			return nil, fmt.Errorf("line %d of the mount table does not read as one", n+1)
		}
		points[unescapeMountInfo(fields[4])] = true
	}
	return points, nil
}

// unescapeMountInfo undoes mountinfo's escaping of a path: the kernel
// writes a space, tab, newline and backslash as a backslash and three octal
// digits (proc(5), seq_path's "\t\n\\ "). internal/doctor's
// unescapeMountinfo is its twin, for the cgroup mounts.
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
