package analyze

import (
	"errors"
	"os/exec"
)

// Why one analysis failed: a fact about the FILE, or a fact about
// everything else.
//
// # The defect this exists for
//
// A failed analysis writes no `track_analysis` row, and every candidate
// query selects tracks that LACK a fresh waveform. So a source that can
// never decode is re-selected on every sweep, forever. The operator's
// library holds 30 genuinely-truncated FLACs; on the Azure deployment
// they produced 1,385 WARN lines in 7 days, identical every time, and
// nothing anywhere told the operator which files to replace.
//
// `collectAnalysisCandidates` already skips ZERO-BYTE sources for this
// exact reason, and its comment explains why that check cannot be
// widened: it "stays mtime/size-driven so it can't suppress a real file
// that's only TRANSIENTLY failing (those keep a non-zero size)". A
// truncated file keeps a non-zero size. Telling the two apart needs the
// decoder's verdict, which is what this file carries back.
//
// # The rule
//
// Only a failure that is a property of the FILE may be recorded. That
// is the same rule `internal/enrich` follows for AcoustID (a lookup
// ERROR is a fact about the upstream and never persists, while a
// no-match is a fact about the audio and does) and the same rule
// `manifest.RecordVariantFailure`'s docblock states for the transcode
// debounce. Classify at the site where the fact is KNOWN — never by
// matching the message later, which is how a 4xx whose body mentions
// "HTTP 503" came to retry forever in the enricher.
//
// Everything unclassified is transient. The permissive answer is the
// safe one here: a transient verdict costs another decode, a permanent
// one costs the operator a file that silently stops being analysed.

// ErrSourceUnreadable marks a decode failure the SOURCE FILE caused, as
// opposed to the toolchain, the host, or this process.
//
// Wrapped at exactly two sites in decode.go, both of which are the
// decoder's own verdict on bytes it successfully opened and read:
//
//   - the truncation check — sox and ffmpeg BOTH exit 0 on a truncated
//     stream, so a clean exit whose decoded length falls materially
//     short of the probed duration is the one signal that separates a
//     short file from a short read;
//   - a decoder that RAN and EXITED non-zero, i.e. opened the input,
//     rejected it, and said so. A decoder killed by a signal (the
//     per-job timeout, shutdown, the OOM killer) is excluded — it
//     reached no verdict, and treating a memory-pressure kill as a
//     property of the file is how a healthy library sidelines itself
//     under load.
//
// A non-zero exit can also mean "could not open the input" — a dropped
// mount, a permission change — and this sentinel does not try to be
// narrower than the decoder's own verdict. Three things bound the cost:
//
//  1. A file the bridge cannot STAT never reaches a decoder at all.
//     `collectAnalysisCandidates` calls `ResolveChecked` first, and an
//     unresolvable path lands in `res.missing` without being enqueued —
//     so a vanished mount produces no strikes, not wrong ones. What is
//     left is the narrow case of a file that stats and will not open.
//  2. Suppression takes `manifest.analysisFailureThreshold` CONSECUTIVE
//     strikes against the same file version, on separate sweeps, so a
//     brief fault has to persist across all of them.
//  3. The operator is TOLD. The console's unreadable list carries the
//     decoder's own message, so "Permission denied" reads as itself.
//
// A review proposed narrowing this to the truncation verdict alone, so a
// generic non-zero exit stays transient. Declined on evidence: a file whose
// HEADER is unreadable never reaches the truncation check at all. The probe
// fails, expectedSec is 0, and decodedShortOfDuration is documented as a
// no-op for an unknown duration — so sox and ffmpeg refuse it with a plain
// non-zero exit and nothing else. That is the other half of "permanently
// broken", and on this PR's own live fixture it was ALL of it: four corrupt
// FLACs produced `ffmpeg: exit status 187 (Error opening input: End of file)`,
// never a truncation verdict. Narrowing would have shipped a debounce that
// did not fire on the files it was tested against.
//
// Be precise about what re-opens a suppressed file, because the two
// cases differ. A REPAIRED file changes (size, mtime_ns) and the version
// gate re-offers it with no operator action. A CHMOD changes neither, so
// that one waits for the TTL or takes the explicit route —
// `bridge analyze --retry-failed`, or the list's Retry button.
var ErrSourceUnreadable = errors.New("source unreadable")

// markUnreadable tags err as ErrSourceUnreadable WITHOUT changing what
// it says.
//
// A plain `fmt.Errorf("%w: %w", ErrSourceUnreadable, err)` would prefix
// every message with "source unreadable: ", and these messages are not
// internal: they are logged verbatim, persisted as
// `tracks.analysis_fail_reason`, and rendered in the console's
// unreadable list. "decoded 51.5s of 357.2s probed — source appears
// truncated" already says what happened, in the words the operator has
// been reading in the journal; the classification is for the code.
func markUnreadable(err error) error { return unreadableSource{err} }

// unreadableSource carries the classification beside an error while
// delegating Error() to it. Unwrap keeps errors.Is/As working through
// to the original chain (an *exec.ExitError stays reachable); Is
// answers for the sentinel, which has no place in the message.
type unreadableSource struct{ err error }

func (e unreadableSource) Error() string        { return e.err.Error() }
func (e unreadableSource) Unwrap() error        { return e.err }
func (e unreadableSource) Is(target error) bool { return target == ErrSourceUnreadable }

// SourceUnreadable reports whether err is a verdict about the file
// itself — the one condition under which a caller may persist a
// failure marker against a track.
//
// The pool is the only caller. It also excludes cancellation and the
// per-job timeout BEFORE asking, for the reason
// manifest.RecordVariantFailure's docblock gives: the first says
// nothing about the source, and the second is as likely to mean a hung
// mount as a pathological file.
func SourceUnreadable(err error) bool { return errors.Is(err, ErrSourceUnreadable) }

// decoderReachedAVerdict reports whether a failed *exec.Cmd wait means
// the decoder ran to completion and returned a non-zero status, rather
// than being killed.
//
// `ProcessState.Exited()` is false for a signal death, which is the
// split wanted on POSIX: the OOM killer takes the biggest decode under
// memory pressure, and that is not the file's fault. A non-ExitError
// (the wait itself failed — an I/O error reaping the process) is not a
// verdict either.
//
// # Windows has no signals, so this check cannot make that distinction
//
// A process terminated on Windows exits with a STATUS; `Exited()` is
// true and `TerminateProcess` looks exactly like a program choosing to
// exit non-zero. There is no discriminator to use instead — Go's
// os.Process.Kill passes its own exit code, which any decoder could
// also return. Found by the Windows CI leg, which is the whole reason
// that leg is blocking.
//
// Two things keep the gap narrow, and neither depends on this check.
// The cases the BRIDGE causes never reach here at all: a per-job
// timeout is excluded by processJob's DeadlineExceeded branch, and a
// shutdown by `p.closed`, both platform-independent. What remains is a
// decoder killed by something ELSE entirely — Task Manager, a
// scanner — which on Windows is recorded as a strike where POSIX would
// call it transient. The three mitigations below carry that, and the
// operator sees the decoder's own message in the console list rather
// than a silent suppression.
func decoderReachedAVerdict(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	return ee.ProcessState != nil && ee.ProcessState.Exited()
}
