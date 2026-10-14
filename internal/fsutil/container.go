package fsutil

import "os"

// containerMarkers are the files a container runtime creates in every
// container's root: Docker's /.dockerenv and Podman's /run/.containerenv.
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// InContainer reports whether this process runs inside a container.
//
// Marker files, not cgroup parsing: both markers were verified present inside
// the image (readable by the unprivileged `bridge` user) and absent on the
// host itself. cgroup-based detection is brittle across cgroup v1/v2 and
// rootless runtimes, and every caller only chooses wording or leaves out
// advice addressed to a host shell, so a wrong answer costs a sentence, never
// behaviour.
//
// The console's log hint and `bridge doctor` ask it (backlog B190), so the two
// answer the question the same way. internal/advertise keeps its own
// Docker-only check: the default bridge network it leaves out is Docker's.
func InContainer() bool { return inContainerAt(containerMarkers...) }

// inContainerAt is InContainer over the given marker paths, so the tests read
// a fixture rather than this host's root.
func inContainerAt(markers ...string) bool {
	for _, m := range markers {
		if _, err := os.Stat(m); err == nil {
			return true
		}
	}
	return false
}
