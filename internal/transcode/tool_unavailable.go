package transcode

import (
	"errors"
	"io/fs"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Why one job failed: a fact about its SOURCE, or a fact about this HOST.
//
// # The defect this exists for
//
// processJob records a strike against the source for a runner failure
// (manifest.RecordVariantFailure), and three strikes on one file version take
// it out of every candidate query for 30 days (variantFailureSuppressedSQL).
// Until 2026-09-28 every runner error but a timeout and a shutdown struck,
// a missing sox included. A missing tool is the one failure that does not go
// away between strikes, so every job queued behind it struck its file, three
// sweeps suppressed the whole eligible library, and installing the tool
// changed nothing: the suppression is keyed on the file's size and mtime,
// which the install does not touch (measured on #1067: six of six files
// suppressed 40 s after the first sweep, and still suppressed after a
// restart with sox on PATH). The live upscale gate keeps new work away
// from a host without sox, but it re-probes every 30 s and refuses only NEW
// work: a job already queued when the tool goes, or one queued inside that
// window, still reaches the runner.
//
// # The rule
//
// internal/analyze's (failure.go), from the other side: classify where the
// fact is known, never from the message later. That pool strikes only on a
// verdict it has classified; this one strikes on everything EXCEPT what is
// classified here, because its debounce predates the classification and a
// runner that ran and failed on the file is the common case. Three things
// are classified, and each is a failure no tool reached a verdict in:
//
//   - exec could not find the tool, or found one it may not run: an
//     *exec.Error, which exec.Command's PATH lookup produces and nothing
//     else does (not found, not executable, or found relative to the
//     working directory);
//   - the operating system could not start the tool at the absolute path
//     resolveBin found: os.StartProcess's *fs.PathError, Op "fork/exec"
//     (the binary gone since the lookup, a missing interpreter, the wrong
//     architecture). The source path is only an argument to that call, so
//     nothing in it can have caused the failure;
//   - the decoder probe gave the source no route on this host's toolchain
//     (Run marks it, markToolUnavailable): a DSF / DSDIFF source when ffmpeg
//     or ffprobe is missing, or the build lacks the dsd_* decoders, or its
//     decoder listing could not be read; an MP4 source when sox has no MP4
//     reader and ffmpeg or ffprobe is missing.
//
// Everything else keeps its strike: a tool that ran and refused the file is
// what the debounce is for. A job that fails for want of a tool is still
// counted and announced, so its batch row and its jobFailed event say what
// happened; what it does not do is sideline the file.

// Tool names in the outage report, and the keys a successful job's chain is
// matched against (toolsProvenBy).
const (
	toolSox     = "sox"
	toolFFmpeg  = "ffmpeg"
	toolFFprobe = "ffprobe"
	// toolDSDDecoders is the capability rather than a binary: ffmpeg can be
	// on PATH and still route no DSD source.
	toolDSDDecoders = "ffmpeg's DSD decoders"
)

// The outage report's two messages. The per-job line after the first is
// logged at Debug under the same message as the Warn, so a test parked on
// the Warn (loggingtest.ParkOn) holds the first job only.
const (
	logToolUnavailable = "pool: tool unavailable"
	logToolBack        = "pool: tool available again"
)

// missingTool is what a failure says this host lacks: the tool, by the name
// the outage report keys on, and why, in the report's words.
type missingTool struct {
	name, reason string
}

// toolUnavailableError marks a failure the decoder probe decided: Run found no
// route for the source on this host's toolchain. It carries the mark beside
// the error without changing what the error says, the markUnreadable shape in
// internal/analyze: the message reaches the batch row, the jobFailed event and
// the log, and the classification is for the code.
type toolUnavailableError struct {
	tool missingTool
	err  error
}

func (e *toolUnavailableError) Error() string { return e.err.Error() }
func (e *toolUnavailableError) Unwrap() error { return e.err }

// markToolUnavailable tags err as a failure for want of tool, for the reason
// given.
func markToolUnavailable(tool, reason string, err error) error {
	return &toolUnavailableError{tool: missingTool{name: tool, reason: reason}, err: err}
}

// unavailableTool reports what this host lacks when err says a job could not
// run a tool it needs, and false when the failure may be about the source.
//
// A tool that could not start is asked about first: a route mark wraps sox's
// own failure on an MP4 source, and if sox itself vanished between the probe
// and the run, the tool to name is sox.
func unavailableTool(err error) (missingTool, bool) {
	var lookup *exec.Error
	if errors.As(err, &lookup) {
		return missingTool{name: toolName(lookup.Name), reason: causeText(lookup.Err)}, true
	}
	var start *fs.PathError
	if errors.As(err, &start) && start.Op == "fork/exec" {
		return missingTool{name: toolName(start.Path), reason: causeText(start.Err)}, true
	}
	var marked *toolUnavailableError
	if errors.As(err, &marked) {
		return marked.tool, true
	}
	return missingTool{}, false
}

// causeText is an exec failure's cause as the report states it. The os and
// exec packages always set one; a value built without it must not panic the
// worker that classifies it.
func causeText(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

// toolName is the name the report gives the program an exec named: its base
// name, without Windows' .exe.
func toolName(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); strings.EqualFold(ext, ".exe") {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// missingDecodeTool names what this host lacks for a source the decoder probe
// gave no route, from the ffmpeg snapshot the route was decided on. For an MP4
// source that is always a missing binary, beside a sox with no MP4 reader
// (with both binaries present it routes through the pipe); a DSD source also
// routes nowhere when the binaries are there and the decoders are not.
func missingDecodeTool(ff FFmpegInfo, class decodeClass) (tool, reason string) {
	switch {
	case len(ff.MissingBinaries) > 0:
		tool, reason = ff.MissingBinaries[0], strings.Join(ff.MissingBinaries, " and ")+" not found on PATH"
	case !ff.DecodersKnown:
		tool, reason = toolDSDDecoders, "the ffmpeg decoder listing could not be read"
		if ff.ProbeErr != "" {
			reason += ": " + ff.ProbeErr
		}
	default:
		tool, reason = toolDSDDecoders, "this ffmpeg build lacks the dsd_* decoders"
	}
	if class == classMP4 {
		reason = "sox has no MP4 reader, and " + reason
	}
	return tool, reason
}

// toolsProvenBy names the tools a successful run's chain started and saw
// through, read from the settings the run recorded: the decoder field is the
// route the run actually took (Run records it for exactly that reason). Every
// chain ends in sox; the pipe routes also ran ffprobe and ffmpeg, and the DSD
// route ran ffmpeg's DSD decoders. Settings that do not parse prove nothing.
func toolsProvenBy(settings string) []string {
	v, ok := ParseSoxSettings(settings)
	if !ok || v.Decoder == "" {
		return nil
	}
	switch v.Decoder {
	case routeFFmpegDSDPipe.String():
		return []string{toolSox, toolFFmpeg, toolFFprobe, toolDSDDecoders}
	case routeFFmpegPipe.String():
		return []string{toolSox, toolFFmpeg, toolFFprobe}
	default:
		// "sox", and "none": a source the probe gave no route that sox
		// read after all. sox ran either way.
		return []string{toolSox}
	}
}

// toolOutages is the pool's report of the tools its jobs could not run, the
// M-SEARCH rule (discovery.SendFailureLog) applied per tool: one Warn when a
// tool's outage starts, one Info when a job proves the tool back, and the jobs
// between at Debug. A queue of 5,000 jobs (the default QueueCap) behind a
// missing sox logged 5,000 identical WARNs; it logs one. The streaks are kept
// by outageStreaks, which the output report (outputOutages) shares.
//
// Keyed per tool because tools come and go apart: with ffmpeg missing and sox
// present, a FLAC job succeeds between two DSD failures, and ending every
// outage on any success would report ffmpeg back, then missing again, once
// per DSD job. A success ends only the outages of the tools its own chain ran.
type toolOutages struct {
	streaks outageStreaks[string, missingTool]
	// now is the clock, time.Now when nil. A test drives the re-warn with
	// it; set it before any job runs.
	now func() time.Time
}

// fail records one job that failed for want of t, and logs it: the Warn when
// the outage starts (or has been silent for outageRewarnAfter), Debug
// otherwise. msg is the job's redacted failure reason; path is its
// library-relative source.
func (o *toolOutages) fail(t missingTool, path, msg string) {
	s, warn := o.streaks.note(t.name, t, clockNow(o.now))
	if !warn {
		logger.Debug(logToolUnavailable, "tool", t.name, "path", path, "failedJobs", s.failed)
		return
	}
	logger.Warn(logToolUnavailable,
		"tool", t.name,
		"reason", t.reason,
		"path", path,
		"err", msg,
		"failedJobs", s.failed,
		"since", s.since,
		"note", "every job that needs it fails without a strike against its source; the next line is when a job proves it back")
}

// proven ends the outages of the tools a successful run's chain ran, logging
// each at Info with how many jobs it cost. Free when no outage is open, which
// is every success on a healthy host.
func (o *toolOutages) proven(settings string) {
	ran := lazily(func() []string { return toolsProvenBy(settings) })
	ended := o.streaks.end(func(tool string, _ missingTool) bool {
		return slices.Contains(ran(), tool)
	})
	for _, b := range ended {
		logger.Info(logToolBack, "tool", b.key, "failedJobs", b.failed, "since", b.since)
	}
}
