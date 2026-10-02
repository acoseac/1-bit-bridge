package transcode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// outputFaultFailure is what Run returns when the variants directory refuses
// the sidecar's temp file: the failure processJob must not strike for.
func outputFaultFailure(dir string, errno syscall.Errno) error {
	path := filepath.Join(dir, "01.flac.upscaled-v2-176400-24.flac.0000002a.tmp")
	return markOutputFault(outputVariants, dir,
		fmt.Errorf("create sidecar: %w", &fs.PathError{Op: "open", Path: path, Err: errno}))
}

// TestMarkOutputFaultMarksOnlyTheOutputSidesCauses pins the classification:
// a cause in the platform's table, or a permission, is marked with its kind
// and the directory it was met in, and the mark changes nothing the error
// says; every other error is returned as it is, so it strikes. The table
// itself is pinned per platform (outputFaultErrnosWant).
func TestMarkOutputFaultMarksOnlyTheOutputSidesCauses(t *testing.T) {
	const dir = "/srv/variants/Music/Album"
	if !maps.Equal(outputFaultErrnos, outputFaultErrnosWant()) {
		t.Errorf("outputFaultErrnos = %v, want %v: a cause joins the table, or leaves it, with this test",
			outputFaultErrnos, outputFaultErrnosWant())
	}
	marked := map[syscall.Errno]outputFaultKind{}
	for errno, kind := range outputFaultErrnosWant() {
		marked[errno] = kind
	}
	for _, errno := range permissionErrnos() {
		marked[errno] = outputDenied
	}
	for errno, kind := range marked {
		err := fmt.Errorf("mkdir sidecar dir: %w", &fs.PathError{Op: "mkdir", Path: dir, Err: errno})
		got := markOutputFault(outputVariants, dir, err)
		f, ok := unwritableOutput(got)
		if !ok || f.kind != kind || f.where != outputVariants || f.dir != dir || f.reason != errno.Error() {
			t.Errorf("errno %d (%v): marked %+v, %v; want kind %d in %s, reason %q",
				int(errno), errno, f, ok, kind, dir, errno.Error())
		}
		if got.Error() != err.Error() || !errors.Is(got, errno) {
			t.Errorf("errno %d (%v): the mark changed the error: %q, or does not unwrap to its cause", int(errno), errno, got)
		}
	}
	for _, err := range append(notOutputFaultCauses(),
		context.Canceled,
		context.DeadlineExceeded,
		errors.New("sox FAIL formats: can't open input file"),
		fmt.Errorf("%w: not rendered", ErrSourceChanged),
		&exec.Error{Name: "sox", Err: exec.ErrNotFound},
	) {
		got := markOutputFault(outputVariants, dir, err)
		if _, ok := unwritableOutput(got); ok || got != err {
			t.Errorf("%v was marked as the output side's fault, or changed (%v): it must be returned as it is, and strike", err, got)
		}
	}
}

// TestAnOutputOutageIsReportedWhenItStartsAndWhenAJobProvesItBack pins the
// report: one Warn when an outage starts, the jobs after it at Debug, and one
// Info when a job proves it over, with how many jobs it cost, and no strike
// for any of them.
//
// What proves an outage over depends on its kind. A volume fault (here a
// read-only variants volume, and a full scratch volume) is over at the first
// job that writes on that volume, whichever folder. A permission is a fact
// about one directory: in a variants tree where one album folder is root's,
// every other album still renders, and ending the outage on any success would
// report it over, then back, at every success between two of its failures.
// So it ends only when a job writes in the folder that refused it. And a PCM
// job writes no render scratch, so it proves nothing about one.
func TestAnOutputOutageIsReportedWhenItStartsAndWhenAJobProvesItBack(t *testing.T) {
	const relX, relY, relDSD = "Music/X/01.flac", "Music/Y/01.flac", "Music/Z/01.dsf"
	a := newAnnouncingPool(t, relX, relY, relDSD)
	s := &scripted{}
	a.pool.runner = s.run
	a.pool.fsyncFn = noopFsync // the scripted successes write no sidecar
	x, y, dsd := sourceFile(t, relX), sourceFile(t, relY), dsdSource(t, relDSD)
	y.OutputDir, dsd.OutputDir = x.OutputDir, x.OutputDir
	xDir := filepath.Dir(x.SidecarPath())
	fail := func(where, dir string, errno syscall.Errno) func() (RunResult, error) {
		return func() (RunResult, error) {
			return RunResult{}, markOutputFault(where, dir,
				fmt.Errorf("create sidecar: %w", &fs.PathError{Op: "open", Path: dir, Err: errno}))
		}
	}

	// Album X's folder refuses this user. Y renders between X's failures,
	// which proves nothing about X's folder; X rendering does.
	s.then(fail(outputVariants, xDir, syscall.EACCES), fail(outputVariants, xDir, syscall.EACCES),
		succeededVia(routeSoxDirect),
		fail(outputVariants, xDir, syscall.EACCES), succeededVia(routeSoxDirect))
	a.run(t, x, 2)
	a.run(t, y, 1)
	a.run(t, x, 2)
	// The variants volume goes read-only; Y rendering proves it back.
	s.then(fail(outputVariants, xDir, readOnlyVolumeErrno()), fail(outputVariants, xDir, readOnlyVolumeErrno()),
		succeededVia(routeSoxDirect))
	a.run(t, x, 2)
	a.run(t, y, 1)
	// The scratch volume fills; a PCM job between proves nothing about it.
	scratch := renderScratchDir(dsd.TempDir)
	s.then(fail(outputScratch, scratch, fullVolumeErrno()), fail(outputScratch, scratch, fullVolumeErrno()),
		succeededVia(routeSoxDirect), succeededVia(routeFFmpegDSDPipe))
	a.run(t, dsd, 2)
	a.run(t, y, 1)
	a.run(t, dsd, 1)

	warns := a.lines("WARN", logOutputUnavailable)
	if len(warns) != 3 ||
		!strings.Contains(warns[0], "output="+outputVariants) || !strings.Contains(warns[0], "reason="+syscall.EACCES.Error()) ||
		!strings.Contains(warns[1], "output="+outputVariants) || !strings.Contains(warns[1], "reason="+readOnlyVolumeErrno().Error()) ||
		!strings.Contains(warns[2], "output="+outputScratch) || !strings.Contains(warns[2], "reason="+fullVolumeErrno().Error()) {
		t.Errorf("Warn lines:\n%s\nwant three: the denied folder, the read-only volume, the full scratch",
			strings.Join(warns, "\n"))
	}
	if debug := a.lines("DEBUG", logOutputUnavailable); len(debug) != 4 {
		t.Errorf("Debug lines:\n%s\nwant four: the denied folder's second and third job, and the second job "+
			"of each volume outage", strings.Join(debug, "\n"))
	}
	back := a.lines("INFO", logOutputBack)
	if len(back) != 3 ||
		!strings.Contains(back[0], "output="+outputVariants) || !strings.Contains(back[0], "failedJobs=3") ||
		!strings.Contains(back[1], "output="+outputVariants) || !strings.Contains(back[1], "failedJobs=2") ||
		!strings.Contains(back[2], "output="+outputScratch) || !strings.Contains(back[2], "failedJobs=2") {
		t.Errorf("recovery lines:\n%s\nwant the denied folder back after 3 jobs (when X rendered, not Y), "+
			"the read-only volume after 2 (when Y rendered), the scratch after 2 (when a DSD job rendered)",
			strings.Join(back, "\n"))
	}
	for _, rel := range []string{relX, relY, relDSD} {
		if suppressed, records := a.strikes(t, rel); suppressed != 0 || records != 0 {
			t.Errorf("%s: %d suppressed, %d strike record(s), want none", rel, suppressed, records)
		}
	}
}

// TestAnOutputOutageIsNotOverAtAJobWhoseSidecarFailedItsFsync pins where the
// proof is taken: after the sidecar's fsync. A volume failing its writes can
// still take a rename, so a job whose rename landed and whose fsync then
// failed has shown nothing about the volume, and the outage stays open until
// a job's sidecar is durable.
func TestAnOutputOutageIsNotOverAtAJobWhoseSidecarFailedItsFsync(t *testing.T) {
	const rel = "Music/Album/01.flac"
	a := newAnnouncingPool(t, rel)
	s := &scripted{}
	a.pool.runner = s.run
	fsyncs := 0
	a.pool.fsyncFn = func(string) error {
		fsyncs++
		if fsyncs == 1 {
			return errors.New("synthetic EIO at fsync")
		}
		return nil
	}
	spec := sourceFile(t, rel)
	readOnly := func() (RunResult, error) {
		return RunResult{}, outputFaultFailure(filepath.Dir(spec.SidecarPath()), readOnlyVolumeErrno())
	}

	s.then(readOnly, succeededVia(routeSoxDirect))
	a.run(t, spec, 2)
	if back := a.lines("INFO", logOutputBack); len(back) != 0 {
		t.Fatalf("recovery lines after a job whose fsync failed:\n%s\nwant none", strings.Join(back, "\n"))
	}
	s.then(succeededVia(routeSoxDirect))
	a.run(t, spec, 1)
	if back := a.lines("INFO", logOutputBack); len(back) != 1 || !strings.Contains(back[0], "failedJobs=1") {
		t.Errorf("recovery lines:\n%s\nwant one, once a job's sidecar was durable", strings.Join(back, "\n"))
	}
}

// TestAnOutputOutageThatOutlastsADayIsReportedAgain pins the re-warn for the
// output report: a folder that refused a job and is not written again keeps
// its outage open, so a day of silence is broken by one more Warn carrying
// the count so far.
func TestAnOutputOutageThatOutlastsADayIsReportedAgain(t *testing.T) {
	const rel = "Music/Album/01.flac"
	a := newAnnouncingPool(t, rel)
	s := &scripted{}
	a.pool.runner = s.run
	clock := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	a.pool.outputs.now = func() time.Time { return clock }
	spec := sourceFile(t, rel)
	denied := func() (RunResult, error) {
		return RunResult{}, outputFaultFailure(filepath.Dir(spec.SidecarPath()), syscall.EACCES)
	}

	s.then(denied, denied, denied)
	a.run(t, spec, 1)
	clock = clock.Add(outageRewarnAfter - time.Minute)
	a.run(t, spec, 1)
	clock = clock.Add(time.Minute)
	a.run(t, spec, 1)

	warns := a.lines("WARN", logOutputUnavailable)
	if len(warns) != 2 || !strings.Contains(warns[1], "failedJobs=3") {
		t.Errorf("Warn lines:\n%s\nwant the outage's first job, then its third once a day had passed, "+
			"carrying the count so far", strings.Join(warns, "\n"))
	}
}
