package engine

import (
	"errors"
	"strings"
)

// Reason is a stable code: why the engine refused a command, why playback
// stopped, or why the backend refused an entry or stopped a session.
type Reason string

// The reasons a command is refused. A refused command changes nothing.
const (
	// ReasonClosed is a command after shutdown began: the engine takes no
	// more changes, and after Run returns it answers every command so.
	ReasonClosed Reason = "closed"
	// ReasonCancelled is a command whose caller's context ended before the
	// owner admitted it (it did not run, and the refusal wraps
	// ErrNotAdmitted), or while the caller waited for the reply once it was
	// admitted (it may have run: the snapshot says, and the refusal does not
	// wrap ErrNotAdmitted). ReserveConfiguration is the one exception: a
	// grant its caller's context outlived is let go before the method
	// returns, so its refusal wraps ErrNotAdmitted though the owner admitted
	// the command.
	ReasonCancelled Reason = "cancelled"
	// ReasonConflict is a command whose expected generation is not the
	// engine's: another change came first, or the pair is from another run.
	ReasonConflict Reason = "generation_conflict"
	// ReasonMaintenance is a mutation while maintenance is held, or a second
	// enter.
	ReasonMaintenance Reason = "maintenance"
	// ReasonLease is an exit or a renewal with a lease that is not the
	// hold's: wrong, stale, or of a hold that has expired; and the DSD
	// check's hold that expired before its grant was answered, or read by
	// its caller (EnterCheck).
	ReasonLease Reason = "lease_invalid"
	// ReasonIDNotFound is an ID the queue does not hold (OpenHome fault
	// 800).
	ReasonIDNotFound Reason = "id_not_found"
	// ReasonFull is an insert past MaxEntries or past MaxTotalMetadataBytes
	// (OpenHome fault 801).
	ReasonFull Reason = "full"
	// ReasonIndexNotFound is an index outside the queue (OpenHome fault
	// 802).
	ReasonIndexNotFound Reason = "index_not_found"
	// ReasonMetadataTooLarge is one entry's metadata past MaxMetadataBytes.
	ReasonMetadataTooLarge Reason = "metadata_too_large"
	// ReasonBadReference is a reference that is not one NewReference takes.
	ReasonBadReference Reason = "bad_reference"
	// ReasonTooMany is a ReadList of more than MaxReadList IDs.
	ReasonTooMany Reason = "too_many_ids"
	// ReasonEmptyQueue is a Play with nothing to play.
	ReasonEmptyQueue Reason = "empty_queue"
	// ReasonNotInRelease is shuffle on: release 7a's.
	ReasonNotInRelease Reason = "not_in_this_release"
	// ReasonIDsExhausted is an insert past the last ID a uint32 holds, or an
	// edit past the last token: an ID or a token is never handed out twice.
	ReasonIDsExhausted Reason = "ids_exhausted"
	// ReasonReservation is an insert or another token-advancing edit whose
	// ID or token could not be reserved durably first.
	ReasonReservation Reason = "reservation_not_durable"
	// ReasonQueueIdentity is a persisted queue from another player identity.
	// Its entries are never loaded; the file is moved aside and logged once.
	ReasonQueueIdentity Reason = "queue_identity_mismatch"
	// ReasonFileStuck is an insert or another token-advancing edit while a
	// bad queue or reservation file could not be moved aside: the engine
	// writes no file it would overwrite until the operator moves it.
	ReasonFileStuck Reason = "file_not_moved_aside"
	// ReasonOutputUnknown is a command that would open the device before
	// the output's availability is known.
	ReasonOutputUnknown Reason = "output_unknown"
	// ReasonOutputUnavailable is a command that would open the device while
	// the output is unavailable.
	ReasonOutputUnavailable Reason = "output_unavailable"
	// ReasonReleaseUnconfirmed is a command that would open the device while
	// a session's release is unconfirmed: two of the engine's own sessions
	// never hold or race for the device.
	ReasonReleaseUnconfirmed Reason = "release_unconfirmed"
	// ReasonNotStopped is a configuration reservation, or the DSD check's
	// hold without interrupt, asked while the transport is not stopped with
	// nothing under way: a session starting, playing, paused or releasing, or
	// a release unconfirmed (ReserveConfiguration, B728 B9 PR 3; EnterCheck,
	// B728 B12).
	ReasonNotStopped Reason = "not_stopped"
	// ReasonConfiguring is a command that would start a session, or the DSD
	// check's hold, while a configuration reservation is held: nothing starts
	// until it is let go (ReserveConfiguration, B728 B9 PR 3).
	ReasonConfiguring Reason = "configuring"
	// ReasonBackend is a backend call that failed, for a reason the
	// backend's error carries (a busy device, the identity check, …).
	ReasonBackend Reason = "backend_error"
	// ReasonTimeout is a backend call that did not answer by its deadline.
	ReasonTimeout Reason = "backend_timeout"
	// ReasonSuperseded answers a command that asked for playback, which ran
	// and whose playback a later command or edit replaced before it began:
	// a Stop, a DeleteAll, a deletion of the entry to play, or a Next,
	// Previous, PlayID or PlayIndex that replaced the session started for it,
	// or the Resume under way for it. Whatever else the command did (a
	// PlayID's selection) stands.
	ReasonSuperseded Reason = "superseded"
	// ReasonOvertaken answers a transport command still waiting its turn
	// (behind a Pause or a Resume under way) when a Stop ran: the Stop
	// overtook it, and it did not run and changed nothing, a PlayID's
	// selection included. A Pause so overtaken is answered nil, playback
	// being stopped as it asked.
	ReasonOvertaken Reason = "overtaken"
)

// The reasons a seek within a track is refused (B674). A refused seek
// changes nothing: not the transport, the track heard or its position.
const (
	// ReasonSeekUnavailable is a seek with no track to seek in: the
	// transport stopped, a release that stops or plays another entry, a
	// pause that holds nothing, or a track the backend has reported nothing
	// of yet (Details).
	ReasonSeekUnavailable Reason = "seek_unavailable"
	// ReasonSeekOutOfRange is a seek whose target is at or past the end of
	// the content, or that the arithmetic cannot hold, or in content of no
	// known length or rate.
	ReasonSeekOutOfRange Reason = "seek_out_of_range"
	// ReasonSeekUnsupported is a seek in DSD, native, DoP or converted to
	// PCM (DoP is DSD, whatever its PCM carrier, and so is DSD the player
	// converts): the player does not position DSD in time (TimeSeek).
	ReasonSeekUnsupported Reason = "seek_unsupported"
)

// The reasons playback stops on its own, recorded in Snapshot.LastStop.
const (
	// ReasonStopped is a Stop, a DeleteAll or another command.
	ReasonStopped Reason = "stopped"
	// ReasonEndOfQueue is the end of the last entry with repeat off.
	ReasonEndOfQueue Reason = "end_of_queue"
	// ReasonUnplayable is an entry refused before any of its content played
	// with nothing after it.
	ReasonUnplayable Reason = "unplayable"
	// ReasonAllUnplayable is a pass in which every entry was refused.
	ReasonAllUnplayable Reason = "all_unplayable"
	// ReasonFault is a fault after content played (Snapshot.Faults).
	ReasonFault Reason = "fault"
	// ReasonUnitChanged is the bound unit changing or re-enumerating.
	ReasonUnitChanged Reason = "output_unit_changed"
	// ReasonRemovedAudible is an entry that became audible after an edit
	// had removed it, or made it no longer next, and the cut was too late.
	ReasonRemovedAudible Reason = "removed_entry_audible"
)

// The reasons a backend gives for an entry it refused before any of its
// content played, and for a fault. A backend may give others.
const (
	// ReasonMissing is a file that is not there or cannot be opened.
	ReasonMissing Reason = "missing_file"
	// ReasonRefusedContent is content the decoders refuse.
	ReasonRefusedContent Reason = "refused_content"
	// ReasonDSDOff is DSD content while the session's DSD mode is off. The
	// bridge's backend gives reasons of its own for DSD it refuses, under
	// mode off too (SessionOptions.DSD).
	ReasonDSDOff Reason = "dsd_off"
	// ReasonXrun is an underrun: the sink stopped, and the backend does not
	// recover it on its own.
	ReasonXrun Reason = "xrun"
	// ReasonDisconnected is a device that went away.
	ReasonDisconnected Reason = "disconnected"
	// ReasonDeviceError is any other failure of the device.
	ReasonDeviceError Reason = "device_error"
	// ReasonDecodeFailed is a decode that failed after content played.
	ReasonDecodeFailed Reason = "decode_failed"
)

// ErrNotAdmitted is what every refusal of a command that did not run because
// its context ended wraps (B470 PR 5a4): the context had ended when the method
// was called, it ended before the owner took the command, or the owner took
// it and found it ended, or abandoned by its caller, before admitting it. A
// ReasonCancelled refusal that does not wrap it is of a command the owner
// admitted, which may have run. ReserveConfiguration's refusal of a grant its
// caller's context outlived wraps it too, the one refusal of a command the
// owner admitted that does: the method lets that grant go before it returns,
// so nothing of it stands. errors.Is finds it.
var ErrNotAdmitted = errors.New("not admitted")

// ErrAfterChange is what every refusal of a command answered as a waiter
// wraps (B634): a command answered once the transport settles (wait) was
// admitted and its rule ran (the selection moved, a session released or
// started, the pending intent set) before the refusal was decided, so the
// refusal, whatever its reason, does not show that the command changed
// nothing. A refusal decided before the command changed anything does not
// wrap it. errors.Is finds it.
var ErrAfterChange = errors.New("refused after the command's rule ran")

// afterChange is a waiter's refusal, marked ErrAfterChange.
type afterChange struct{ err error }

func (a afterChange) Error() string   { return a.err.Error() }
func (a afterChange) Unwrap() []error { return []error{a.err, ErrAfterChange} }

// waited is err as a waiter is answered with it: marked ErrAfterChange, nil
// for nil.
func waited(err error) error {
	if err == nil {
		return nil
	}
	return afterChange{err}
}

// Error is a refused command, or a failure the engine reports for one.
type Error struct {
	Reason Reason
	Op     string // "insert", "play", "enter maintenance", …
	Detail string
	// Current is the engine's generation, for ReasonConflict.
	Current Generation
	// Err is the context's or the backend's error, when there is one.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("player: ")
	if e.Op != "" {
		b.WriteString(e.Op + ": ")
	}
	b.WriteString(string(e.Reason))
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	if e.Err != nil {
		b.WriteString(" (" + e.Err.Error() + ")")
	}
	return b.String()
}

// Unwrap returns the context's or the backend's error.
func (e *Error) Unwrap() error { return e.Err }

// ReasonOf returns the Reason of the *Error or the *Unplayable in err's
// chain, or "" when there is none.
func ReasonOf(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	var u *Unplayable
	if errors.As(err, &u) {
		return u.Reason
	}
	return ""
}

// refuse is a refusal of op for reason.
func refuse(op string, reason Reason, detail string) *Error {
	return &Error{Reason: reason, Op: op, Detail: detail}
}
