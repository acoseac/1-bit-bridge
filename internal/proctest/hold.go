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
// nothing after these lines. Before, when
// TestServeLeavesNoTailscaleCLIRunning or
// TestCancelStopsTheWholeCLIProcessTree failed because their fake had
// survived, the fake went on looping forever. The test's cleanup created
// release, and t.TempDir's own cleanup, registered earlier and so run
// next, removed the directory straight after. A shell that looks every
// 20 ms rarely saw the file in that window: under a mutation that let the
// fake survive, 23 of 25 were left behind on macOS and 6 of 8 on Linux,
// each reparented to init and forking `sleep` 50 times a second. And a
// test binary that dies before its cleanups, on a -test.timeout or a
// SIGINT, creates no release file and removes no directory.
//
// Neither condition holds while the test runs, so a held process that
// outlives what the test does to it is still there to be found, and a
// released one still does its work. The test binary is watched with
// kill -0, which finds a zombie and a reused pid alike: a binary that dies
// with nothing left to reap it (its `go` command killed too, under a PID 1
// that reaps nothing), or whose pid the kernel hands to another process of
// this user before the shell's next look, keeps the shell waiting until
// that one is gone.
//
// Relative paths are made absolute against this process's working
// directory first. A bare name's directory is ".", which a shell finds for
// as long as its working directory exists, removed or not, so the
// directory would never be seen to go.
func HoldUntilReleased(pidFile, release string) string {
	pidFile, release = mustAbs(pidFile), mustAbs(release)
	tmp := shellQuote(pidFile + ".tmp")
	return "echo $$ > " + tmp + " && mv " + tmp + " " + shellQuote(pidFile) + "\n" +
		"while [ ! -e " + shellQuote(release) + " ]; do\n" +
		"\t[ -d " + shellQuote(filepath.Dir(release)) + " ] && kill -0 " + strconv.Itoa(os.Getpid()) +
		" 2>/dev/null || exit " + strconv.Itoa(unreleasedStatus) + "\n" +
		"\tsleep 0.02\n" +
		"done\n"
}

// mustAbs returns path made absolute against this process's working
// directory. filepath.Abs fails only when it cannot read that directory,
// which it never needs to for an absolute path.
func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic("proctest: " + err.Error())
	}
	return abs
}

// shellQuote quotes s as one /bin/sh word: in single quotes, with each
// single quote in s closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
