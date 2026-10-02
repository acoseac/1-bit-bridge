package transcode

import (
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"
	"time"
)

// Why one job could not write its output: a fact about this HOST's output
// side, never about the source it was rendering.
//
// # The defect this exists for
//
// A render writes in two places the source has no say in: the sidecar's
// directory under the variants directory, and, for a DSD source, the Stage A
// scratch under the temp dir. processJob struck the source for every runner
// failure that was not a timeout, a shutdown, a missing tool or a changed
// source, so a directory this user may not write struck every file rendered
// into it, and three strikes take a file out of every candidate query for 30
// days, keyed on its size and mtime, which fixing the directory does not
// change. The ordinary way to get there was a `sudo bridge render` before
// v0.2.1, which left root-owned album folders in the variants tree and a
// root-owned render scratch in the shared temp dir (CLAUDE.md, the B17
// bullet); a variants volume made read-only, a quota spent and a mount that
// went away do the same. Measured through the real pool and Run (backlog
// B211): one source suppressed after three jobs, for each of five shapes.
//
// # The rule
//
// tool_unavailable.go's, for the other side of a job: classify where the
// fact is known, by the error's TYPE, never from a message later. Each step
// that writes to the output side marks its own failure (markOutputFault):
// making the sidecar's directory and the scratch directory, creating the
// sidecar's temp file and the scratch before any tool runs (createOutput),
// and the publish rename and the stat after it. The mark carries the error
// unchanged, so the batch row, the jobFailed event and the log say what they
// said. A marked failure is counted and announced like any other and
// strikes nothing.
//
// Only a cause that describes the directory or its volume is marked
// (hostOutputFault): a permission the directory refuses this user, a
// read-only or full volume, a quota, and an I/O error or a mount gone. A
// cause the source's NAME produces (a name too long for the filesystem, or
// in an encoding it refuses) is not marked and keeps its strike: retrying
// never fixes it.
//
// Creating the tool's output before the tool runs is what puts the common
// shape at a typed site. A directory that exists and refuses new files (the
// root-owned album folder) used to fail in sox's own open, whose message is
// all the pool sees. Created here, it fails in the bridge's open, before any
// decode: a DSD render no longer spends its Stage A on a sidecar that could
// never be written.
//
// A write a tool makes after its output exists (a volume that fills during
// the render) the bridge does not see; since B264 it reads the output the
// tool left and asks the volume (rendition_complete.go). On Windows the tool
// still creates its own output (createOutput does nothing there), so a
// directory whose ACL refuses new files keeps its strike; making a directory
// and the publish rename are marked on every platform.

// The two places a job writes, as the outage report names them.
const (
	outputVariants = "variants"
	outputScratch  = "scratch"
)

// outputFaultKind is what a marked failure says about the output side. Its
// one use beyond the log line is the proof that ends an outage: a permission
// is a fact about one directory, every other kind a fact about the volume.
type outputFaultKind int

const (
	// outputDenied: the directory's permissions refuse this user (EACCES,
	// EPERM; ERROR_ACCESS_DENIED on Windows). Proven over only by a job
	// that writes in the directory that refused: in a variants tree where
	// one album folder is root's, every other album still renders.
	outputDenied outputFaultKind = iota + 1
	outputReadOnly
	outputFull
	outputQuota
	// outputGone: an I/O error on the volume, or a mount gone (a FUSE
	// daemon that died, a stale NFS handle).
	outputGone
)

// outputFault is a failure of the output side: where the job was writing,
// what kind of fault, the directory it needed (the sidecar's own directory,
// or the render scratch directory), and the cause in the operating system's
// words.
type outputFault struct {
	where  string
	kind   outputFaultKind
	dir    string
	reason string
}

// outputFaultError marks an error as a fault of the output side. It changes
// nothing the error says: the mark is for processJob, the message for the
// batch row, the jobFailed event and the log (toolUnavailableError's shape).
type outputFaultError struct {
	fault outputFault
	err   error
}

func (e *outputFaultError) Error() string { return e.err.Error() }
func (e *outputFaultError) Unwrap() error { return e.err }

// markOutputFault marks err, from a step that writes where (outputVariants or
// outputScratch) in dir, as a fault of this host when its cause is one
// hostOutputFault names. Any other error is returned as it is, and strikes.
func markOutputFault(where, dir string, err error) error {
	kind, reason, ok := hostOutputFault(err)
	if !ok {
		return err
	}
	return &outputFaultError{fault: outputFault{where: where, kind: kind, dir: dir, reason: reason}, err: err}
}

// unwritableOutput reports the fault when err says a job could not write its
// output for a fact about this host, and false otherwise.
func unwritableOutput(err error) (outputFault, bool) {
	var marked *outputFaultError
	if errors.As(err, &marked) {
		return marked.fault, true
	}
	return outputFault{}, false
}

// hostOutputFault classifies the cause of err, read by type. A permission is
// asked of the whole chain through fs.ErrPermission, which names EACCES and
// EPERM on unix and ERROR_ACCESS_DENIED on Windows, and which any wrapper
// that reports a permission keeps; the reason is the operating system's
// words when the chain carries a syscall.Errno. The volume's faults are the
// first syscall.Errno in the chain, looked up in the platform's table
// (outputFaultErrnos). Any other error (a cancelled context, a cause outside
// the table) is no fault of the output side.
func hostOutputFault(err error) (kind outputFaultKind, reason string, ok bool) {
	var errno syscall.Errno
	hasErrno := errors.As(err, &errno)
	if errors.Is(err, fs.ErrPermission) {
		if hasErrno {
			return outputDenied, errno.Error(), true
		}
		return outputDenied, fs.ErrPermission.Error(), true
	}
	if !hasErrno {
		return 0, "", false
	}
	if kind, ok := outputFaultErrnos[errno]; ok {
		return kind, errno.Error(), true
	}
	return 0, "", false
}

// The output report's two messages. The per-job line after the first is
// logged at Debug under the same message as the Warn, so a test parked on
// the Warn (loggingtest.ParkOn) holds the first job only.
const (
	logOutputUnavailable = "pool: rendition output unavailable"
	logOutputBack        = "pool: rendition output available again"
)

// outputHints says where to look, per place a job writes.
var outputHints = map[string]string{
	outputVariants: "check that the variants directory (upscale.variantsDir) is mounted, has room, and lets the user the bridge runs as create files in every folder",
	outputScratch:  "check that the render scratch directory under upscale.tempDir (or the OS temp dir) has room and lets the user the bridge runs as create files",
}

// outputKey is one output outage: where, and what kind of fault. A read-only
// variants volume and a root-owned album folder in it are two outages, each
// reported and each proven over on its own.
type outputKey struct {
	where string
	kind  outputFaultKind
}

// outputOutages is the pool's report of the output faults its jobs met,
// toolOutages' shape over outputKey: one Warn when an outage starts, the jobs
// after it at Debug, one Info when a job proves it over, and a re-Warn after
// a day of silence. A queue behind a read-only variants volume logged one
// "pool: sox failed" WARN per job, and struck each source; it logs one line.
type outputOutages struct {
	streaks outageStreaks[outputKey, outputFault]
	// now is the clock, time.Now when nil; a test sets it before any job.
	now func() time.Time
}

// fail records one job whose output side failed with f, and logs it: the
// Warn when the outage starts (or has been silent for outageRewarnAfter),
// Debug otherwise. msg is the job's redacted failure reason, which names the
// directory relative to the variants directory or as a placeholder
// (redactSoxErr); path is its library-relative source.
func (o *outputOutages) fail(f outputFault, path, msg string) {
	s, warn := o.streaks.note(outputKey{where: f.where, kind: f.kind}, f, clockNow(o.now))
	if !warn {
		logger.Debug(logOutputUnavailable, "output", f.where, "reason", f.reason, "path", path, "failedJobs", s.failed)
		return
	}
	logger.Warn(logOutputUnavailable,
		"output", f.where,
		"reason", f.reason,
		"path", path,
		"err", msg,
		"failedJobs", s.failed,
		"since", s.since,
		"hint", outputHints[f.where],
		"note", "every job that writes there fails without a strike against its source; the next line is when a job proves it back")
}

// outputWrite is one place a successful job wrote: where, and the directory.
type outputWrite struct {
	where, dir string
}

// writtenBy names the places a successful job wrote: its sidecar's directory
// under the variants directory, and for a run that took the DSD route (read
// from the settings it recorded, as toolsProvenBy reads them) the render
// scratch directory its Stage A wrote.
func writtenBy(spec JobSpec, sidecarPath, settings string) []outputWrite {
	w := []outputWrite{{where: outputVariants, dir: filepath.Dir(sidecarPath)}}
	if v, ok := ParseSoxSettings(settings); ok && v.Decoder == routeFFmpegDSDPipe.String() {
		w = append(w, outputWrite{where: outputScratch, dir: renderScratchDir(spec.TempDir)})
	}
	return w
}

// proven ends the outages a successful job's writes prove over, logging each
// at Info with how many jobs it cost. A write proves its place's volume back,
// which ends every kind but a permission there; a permission outage ends only
// when a job writes in the directory that refused it. Free when no outage is
// open, which is every success on a healthy host: written is called only
// then, and once.
func (o *outputOutages) proven(written func() []outputWrite) {
	wrote := lazily(written)
	ended := o.streaks.end(func(key outputKey, first outputFault) bool {
		for _, w := range wrote() {
			if w.where == key.where && (key.kind != outputDenied || w.dir == first.dir) {
				return true
			}
		}
		return false
	})
	for _, e := range ended {
		logger.Info(logOutputBack, "output", e.key.where, "reason", e.first.reason, "failedJobs", e.failed, "since", e.since)
	}
}
