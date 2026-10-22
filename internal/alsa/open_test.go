package alsa

import (
	"go/build"
	"testing"
)

// TestOpenFlagsAreBuiltForEveryUnix: every GOOS the unix build tag stands
// for (Go's own list, syslist.UnixOS) has FIFOs, so each builds
// open_posix.go, whose flags open a FIFO without blocking and a directory
// directory-only, and fifo_test.go, which proves it, and none builds
// open_other.go, whose flags are 0: that file is for the systems with no
// FIFO in their file system. Until round 3 the tags were linux || darwin,
// and a FreeBSD, OpenBSD or NetBSD build opened a FIFO in a tree it read
// and waited for a writer.
func TestOpenFlagsAreBuiltForEveryUnix(t *testing.T) {
	unix := []string{"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "linux", "netbsd", "openbsd", "solaris"}
	for _, goos := range unix {
		for file, want := range map[string]bool{"open_posix.go": true, "fifo_test.go": true, "open_other.go": false} {
			if got := builtFor(t, goos, file); got != want {
				t.Errorf("%s on %s: built %v, want %v", file, goos, got, want)
			}
		}
	}
	for _, goos := range []string{"windows", "plan9", "js", "wasip1"} {
		for file, want := range map[string]bool{"open_posix.go": false, "fifo_test.go": false, "open_other.go": true} {
			if got := builtFor(t, goos, file); got != want {
				t.Errorf("%s on %s: built %v, want %v", file, goos, got, want)
			}
		}
	}
}

// builtFor reports whether a build for goos takes file, one of this
// package's files, by its build constraints.
func builtFor(t *testing.T, goos, file string) bool {
	t.Helper()
	ctxt := build.Default
	ctxt.GOOS, ctxt.GOARCH = goos, "amd64"
	ok, err := ctxt.MatchFile(".", file)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}
