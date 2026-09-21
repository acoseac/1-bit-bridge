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
// mount, a permission change. Three things keep that from being
// recorded as permanent, and they are why this sentinel does not have
// to be narrower than the decoder's own verdict:
//
//  1. A file the bridge cannot stat never reaches a decoder at all.
//     `collectAnalysisCandidates` calls `ResolveChecked` first, and an
//     unresolvable path lands in `res.missing` without being enqueued —
//     so a vanished mount produces no strikes, not wrong ones.
//  2. Suppression takes `manifest.analysisFailureThreshold` CONSECUTIVE
//     strikes against the same file version, on separate sweeps.
//  3. A strike is keyed on (size, mtime_ns) and TTL'd, so repairing the
//     file re-opens it with no operator action and a toolchain upgrade
//     gets a fresh try on its own.
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
// `ProcessState.Exited()` is false for a signal death, which is exactly
// the split wanted: exec.CommandContext kills the child when the job
// context expires, and the OOM killer does the same under memory
// pressure. Neither is the file's fault. A non-ExitError (the wait
// itself failed — an I/O error reaping the process) is not a verdict
// either.
func decoderReachedAVerdict(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	return ee.ProcessState != nil && ee.ProcessState.Exited()
}
