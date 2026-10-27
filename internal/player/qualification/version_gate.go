package qualification

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// GuardedPaths enumerates implementation files whose change invalidates evidence.
var GuardedPaths = []string{"internal/player/engine/transport.go", "internal/player/engine/events.go", "internal/player/engine/backend.go", "internal/player/playback/session.go", "internal/player/playback/open.go", "internal/player/playback/producer.go", "internal/player/playback/reporter.go", "internal/player/format/dsd_pack.go", "internal/alsapcm/uapi.go", "internal/alsapcm/device.go", "internal/alsapcm/params.go", "internal/alsapcm/sink.go", "internal/alsapcm/sink_ring.go", "internal/alsapcm/sink_writer.go", "internal/alsapcm/sink_dsd.go", "internal/alsapcm/sink_dsd_open.go", "internal/alsapcm/sink_dsd_writer.go", "cmd/bridge/output_dsdtest.go", "cmd/bridge/output_dsdtest_run.go", "cmd/bridge/output_dsdtest_input.go", "internal/alsapcm/device_native.go", "internal/alsapcm/sys_linux.go", "internal/alsapcm/sys_other.go", "internal/alsactl/control.go", "internal/alsactl/control_linux.go", "internal/alsactl/uapi.go", "internal/alsa/qualification.go", "internal/player/qualification/inputs.go", "internal/player/qualification/version_gate.go", "cmd/bridge/configpath.go", "cmd/bridge/cli_consts.go", "internal/packaging/packaging.go"}

// Prefixes include the transitive file/identity readers and shared key reader.
var guardedPrefixes = []string{"internal/player/engine/", "internal/player/playback/", "internal/player/binding/", "internal/alsa/", "internal/player/decode/", "internal/player/format/", "internal/player/qualification/", "internal/alsapcm/", "internal/alsactl/", "internal/fsutil/", "internal/usbid/", "internal/config/", "cmd/bridge/output_"}

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
