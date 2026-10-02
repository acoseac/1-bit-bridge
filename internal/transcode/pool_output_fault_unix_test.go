//go:build !windows

package transcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The output-side faults these tests drive are made with permission bits, so
// they are POSIX-only and skip as root, who writes through them. The faults
// a test cannot make (a read-only or full volume, a quota, a mount that is
// gone) are classified from the same errors in output_fault_test.go.

// standInSoxWritesItsOutputs writes every output a run hands it, the way a
// run that gets as far as writing leaves them: a DSD render's Stage A
// scratch, and the temp file of the sidecar in every chain, a whole FLAC
// at the target sourceFile and dsdSource ask for.
var standInSoxWritesItsOutputs = standInSoxHelp + `for a in "$@"; do
  case "$a" in
    *` + renderScratchSuffix + `) printf 'x' > "$a" || exit 2 ;;
    *` + sidecarTmpSuffix + `) ` + shWrites("$a", stereoFLAC(176400, 24)) + ` || exit 2 ;;
  esac
done
`

// standInSoxLocksItsFolder writes the sidecar's temp file, as a run that
// succeeds does, and then takes its folder's write bit away: the folder
// refuses the publish rename, the last step that writes there. /bin/chmod
// by its path, since PATH holds nothing but the stand-ins.
var standInSoxLocksItsFolder = standInSoxHelp + `for a in "$@"; do
  case "$a" in
    *` + sidecarTmpSuffix + `) ` + shWrites("$a", stereoFLAC(176400, 24)) + ` || exit 2; /bin/chmod 555 "${a%/*}" ;;
  esac
done
`

// skipAsRoot skips a test whose fault is a permission bit: root writes
// through it.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes through permission bits; run this as another user")
	}
}

// readOnlyDir makes dir, creating it, refuse new entries to this user, and
// gives it its write bit back when the test ends so t.TempDir's cleanup can
// remove it.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// TestAnOutputDirectoryTheBridgeCannotWriteStrikesNoSource drives the real
// pool and the real runner over a variants or scratch directory this user
// may not write: the state a `sudo bridge render` left before v0.2.1 (a
// root-owned album folder in the variants tree, a root-owned render scratch
// in the shared temp dir), or a variants volume the operator made read-only.
//
// Each job fails, is counted and is announced, as before. What must not
// happen is a strike: three take the file out of every candidate query for
// 30 days, keyed on its size and mtime, which fixing the directory does not
// change. The pool struck on every such failure.
func TestAnOutputDirectoryTheBridgeCannotWriteStrikesNoSource(t *testing.T) {
	skipAsRoot(t)
	for _, tc := range []outputFaultShape{
		{
			name: "variants directory, album folder not made yet", rel: "Music/Album/01.flac", spec: sourceFile,
			lock: func(t *testing.T, s JobSpec) { readOnlyDir(t, s.OutputDir) }, where: outputVariants,
		},
		{
			name: "variants directory, album folder made", rel: "Music/Album/01.flac", spec: sourceFile,
			lock: func(t *testing.T, s JobSpec) { readOnlyDir(t, filepath.Dir(s.SidecarPath())) }, where: outputVariants,
		},
		{
			name: "DSD render, variants album folder made", rel: "Music/Album/01.dsf", spec: dsdSource,
			lock: func(t *testing.T, s JobSpec) { readOnlyDir(t, filepath.Dir(s.SidecarPath())) }, where: outputVariants,
		},
		{
			name: "DSD render, scratch directory not made yet", rel: "Music/Album/01.dsf", spec: dsdSource,
			lock: func(t *testing.T, s JobSpec) { readOnlyDir(t, s.TempDir) }, where: outputScratch,
		},
		{
			name: "DSD render, scratch directory made", rel: "Music/Album/01.dsf", spec: dsdSource,
			lock: func(t *testing.T, s JobSpec) { readOnlyDir(t, renderScratchDir(s.TempDir)) }, where: outputScratch,
		},
		{
			// The folder takes the temp file and refuses the rename: the
			// stand-in locks it once it has written.
			name: "variants album folder locked while the job rendered", rel: "Music/Album/01.flac", spec: sourceFile,
			lock: func(t *testing.T, s JobSpec) {
				album := filepath.Dir(s.SidecarPath())
				t.Cleanup(func() { _ = os.Chmod(album, 0o755) })
			},
			where: outputVariants, sox: standInSoxLocksItsFolder,
		},
	} {
		t.Run(tc.name, func(t *testing.T) { runOutputFaultShape(t, tc) })
	}
}

// outputFaultShape is one way a job's output side refuses this user.
type outputFaultShape struct {
	name, rel string
	spec      func(*testing.T, string) JobSpec
	// lock makes the spec's output side refuse this user.
	lock func(*testing.T, JobSpec)
	// where is the output side the report must name.
	where string
	// sox is the stand-in sox, standInSoxWritesItsOutputs when empty.
	sox string
}

// runOutputFaultShape sends the shape's job three times through the real
// pool and runner, and requires what every shape owes: three counted and
// announced failures, no strike, and one report.
func runOutputFaultShape(t *testing.T, tc outputFaultShape) {
	t.Helper()
	sox := tc.sox
	if sox == "" {
		sox = standInSoxWritesItsOutputs
	}
	a := newToolFreePool(t, standInTools(t, map[string]string{
		"sox": sox, "ffmpeg": standInFFmpeg, "ffprobe": standInFFprobe,
	}), tc.rel)
	spec := tc.spec(t, tc.rel)
	tc.lock(t, spec)
	got := a.run(t, spec, 3)

	if st := a.pool.Stats(); st.Failed != 3 || st.Done != 0 || st.Inflight != 0 {
		t.Errorf("Stats() = %+v, want 3 failed and nothing in flight: the jobs still fail", st)
	}
	for i, g := range got {
		if !strings.HasPrefix(g, "failed: ") {
			t.Errorf("job #%d announced %q, want a failure", i+1, g)
		}
	}
	if suppressed, records := a.strikes(t, tc.rel); suppressed != 0 || records != 0 {
		t.Errorf("after three jobs that could not write their output: %d source(s) suppressed and %d "+
			"strike record(s), want none: a directory the bridge may not write says nothing about the file",
			suppressed, records)
	}
	requireOneOutputReport(t, a, spec, tc.where)
}

// requireOneOutputReport requires exactly one Warn, the output fault's,
// naming where and the permission it was refused, and no report line naming
// the bridge's own directories by their absolute paths: the error is
// redacted as every job failure's is.
func requireOneOutputReport(t *testing.T, a *announcingPool, spec JobSpec, where string) {
	t.Helper()
	warns := a.log.Failures()
	if len(warns) != 1 || !strings.Contains(warns[0], logOutputUnavailable) ||
		!strings.Contains(warns[0], "output="+where) || !strings.Contains(warns[0], "reason=permission denied") {
		t.Errorf("three jobs that could not write their output logged these warnings:\n%s\n"+
			"want exactly one, the output fault's, naming the %s and the permission it was refused",
			strings.Join(warns, "\n"), where)
	}
	report := strings.Join(a.log.Lines(logOutputUnavailable), "\n")
	for _, dir := range []string{spec.OutputDir, spec.TempDir} {
		if strings.Contains(report, dir) {
			t.Errorf("report names %s by its absolute path:\n%s", dir, report)
		}
	}
}

// TestFixingTheOutputDirectoryBringsTheSourceBackAtTheNextJob is the
// reproduction's second half, through the real runner: after three jobs fail
// on a root-owned album folder the source is not suppressed, so the first job
// once the folder is the bridge's again converts it, and that job reports the
// output back with what the outage cost.
func TestFixingTheOutputDirectoryBringsTheSourceBackAtTheNextJob(t *testing.T) {
	skipAsRoot(t)
	const rel = "Music/Album/01.flac"
	a := newToolFreePool(t, standInTools(t, map[string]string{"sox": standInSoxWrites}), rel)
	spec := sourceFile(t, rel)
	album := filepath.Dir(spec.SidecarPath())
	readOnlyDir(t, album)
	a.run(t, spec, 3)

	if err := os.Chmod(album, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := a.run(t, spec, 1); got[0] != "done" {
		t.Fatalf("the job after the folder was fixed announced %q, want done: the source must not be suppressed", got[0])
	}
	if _, err := os.Stat(spec.SidecarPath()); err != nil {
		t.Errorf("the rendition is not in place after the job that converted it: %v", err)
	}
	back := a.lines("INFO", logOutputBack)
	if len(back) != 1 || !strings.Contains(back[0], "output="+outputVariants) || !strings.Contains(back[0], "failedJobs=3") {
		t.Errorf("recovery lines:\n%s\nwant one, the variants output back after 3 failed jobs",
			strings.Join(back, "\n"))
	}
}
