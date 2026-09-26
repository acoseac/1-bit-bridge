//go:build !windows

package proctest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// unreleasedStatus is the status a held shell exits with once nobody is
// left to release it.
const unreleasedStatus = 3

// HoldUntilReleased returns the lines of a /bin/sh script that record the
// shell's pid in pidFile and then wait until the file release exists. A
// test uses it to hold a process it started indirectly, such as a fake CLI
// behind a wrapper: it reads the pid to ask Exited about the process, and
// creates release to let the script go on with its work.
//
// The wait also ends once nobody is left to release the process: when
// release's directory is gone, or when the test binary that called
// HoldUntilReleased is. The script then exits with status 3 and runs
// nothing after these lines. Before, a failing run of
// TestServeLeavesNoTailscaleCLIRunning or
// TestCancelStopsTheWholeCLIProcessTree left the fake it could not stop
// looping forever. The test's cleanup created release, and t.TempDir's
// own cleanup, registered earlier and so run next, removed the directory
// straight after. A shell that looks every 20 ms rarely saw the file in
// that window: 23 of 25 fakes left behind in one measurement, each
// reparented to init and forking `sleep` 50 times a second. A test binary
// that dies before its cleanups (a timeout, a Ctrl-C) creates no release
// file at all.
//
// Neither condition holds while the test runs, so a held process that
// outlives what the test does to it is still there to be found, and a
// released one still does its work.
func HoldUntilReleased(pidFile, release string) string {
	tmp := shellQuote(pidFile + ".tmp")
	return "echo $$ > " + tmp + " && mv " + tmp + " " + shellQuote(pidFile) + "\n" +
		"while [ ! -e " + shellQuote(release) + " ]; do\n" +
		"\t[ -d " + shellQuote(filepath.Dir(release)) + " ] && kill -0 " + strconv.Itoa(os.Getpid()) +
		" 2>/dev/null || exit " + strconv.Itoa(unreleasedStatus) + "\n" +
		"\tsleep 0.02\n" +
		"done\n"
}

// shellQuote quotes s as one /bin/sh word: in single quotes, with each
// single quote in s closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
