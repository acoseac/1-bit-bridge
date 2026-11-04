package qualification

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// GuardedPaths enumerates the files outside guardedPrefixes whose change
// invalidates the harness's evidence: since B728's B1, the DSD byte path and
// the harness alone (the native DSD and DoP packers, the player's DSD open and
// its plans' edges, the ALSA qualification identity, the control UAPI and
// reader).
var GuardedPaths = []string{"internal/player/format/dsd_pack.go", "internal/player/format/dop_pack.go", "internal/player/playback/native.go", "internal/player/playback/edges.go", "internal/alsa/qualification.go", "internal/alsactl/control.go", "internal/alsactl/control_linux.go", "internal/alsactl/uapi.go"}

// Prefixes are the ALSA PCM backend and its sinks, the qualification package
// (the harness's readers and inputs, and this checker) and the dsd-test
// harness.
var guardedPrefixes = []string{"internal/alsapcm/", "internal/player/qualification/", "cmd/bridge/output_dsdtest"}

// CheckVersion compares read-only Git refs; guarded rename/deletion counts too.
func CheckVersion(dir, base, head string) error {
	git, e := exec.LookPath("git")
	if e != nil {
		return e
	}
	run := func(args ...string) ([]byte, error) {
		c := exec.Command(git, args...)
		c.Dir = dir
		return c.CombinedOutput()
	}
	b, e := run("diff", "--name-only", "--no-renames", base, head, "--")
	if e != nil {
		return fmt.Errorf("git diff: %w", e)
	}
	guarded := false
	for _, name := range strings.Fields(string(b)) {
		for _, p := range guardedPrefixes {
			if strings.HasPrefix(name, p) {
				guarded = true
			}
		}
		for _, p := range GuardedPaths {
			if name == p {
				guarded = true
			}
		}
	}
	if !guarded {
		return nil
	}
	readVersion := func(ref string) (int, error) {
		b, e := run("show", ref+":internal/player/qualification/version.go")
		if e != nil {
			listed, le := run("ls-tree", ref, "--", "internal/player/qualification/version.go")
			if le == nil && len(listed) == 0 {
				return 0, nil
			}
			return 0, e
		}
		m := regexp.MustCompile(`qualificationVersion\s*=\s*([0-9]+)`).FindSubmatch(b)
		if len(m) != 2 {
			return 0, fmt.Errorf("qualificationVersion missing at %s", ref)
		}
		return strconv.Atoi(string(m[1]))
	}
	before, e := readVersion(base)
	if e != nil {
		return e
	}
	after, e := readVersion(head)
	if e != nil {
		return e
	}
	if after <= before {
		return fmt.Errorf("guarded code changed without qualificationVersion increase (%d -> %d)", before, after)
	}
	return nil
}
