// Package engine is the bridge player's state owner and queue (B470 PR
// 4b1): one goroutine owns the transport, the queue and its IDs, the
// generations, maintenance and the queue's persistence across restarts,
// and drives playback through a backend seam (Backend) that this package's
// tests implement with a fake and PR 4b2 implements over
// internal/player/decode and alsapcm.Sink. Nothing here touches hardware.
//
// # The owner
//
// Run is the owner. Every command (a method), backend event and output
// report reaches it as a message, and it handles them one at a time. A
// command whose caller's context ends before the owner admits it does not
// run; once admitted its effect is applied, or recorded as the pending
// intent while a release is under way, and its reply is sent once that
// effect is decided, which can be after a backend call. The reply is sent
// after the snapshot of the step that decided it, so a caller reads its own
// change as soon as its command returns. A caller that has gone is not
// waited for, and after shutdown every command answers ReasonClosed at
// once. Slow work never runs on the owner: every backend call, reservation
// write and queue file write runs on a goroutine of its own.
//
// A panic on one of those goroutines is recovered there, logged once (where,
// its stack, and its value only when it is the runtime's own error), and is
// the failure of the work it ran, through the path an error of that work
// takes: a backend call fails with ReasonBackend, a reservation write is one
// that failed (the edits that need it refused until one lands), and a queue
// file write is a failed write, the state dirty and the write retried; a
// panic on the writer outside a write ends the writer, the state dirty from
// then on. A panic on the owner itself is Run's caller's to recover: on its
// way out the engine ends every backend call, has its writer write the
// newest revision and exit, and closes Done, so nothing waits on it.
//
// A command that cannot run yet waits, and only behind the waiting commands
// of its own kind: an insert, a delete or a DeleteAll for a reservation
// write, at most reserveTimeout (5 s); Play and Pause for a Pause, a Resume
// or a start under way, and the other transport commands but Stop behind
// them. Stop, the output's reports, the maintenance commands and the modes
// never wait. A Stop, or a report that stops playback, answers the
// transport commands still waiting as it runs (a Pause nil, the rest
// ReasonOvertaken or the output's reason: none of them ran, so none changed
// anything), and EnterMaintenance refuses every waiting command.
//
// After every change the owner publishes an immutable Snapshot, which reads
// take without waiting on the owner; its accessors copy. The generation
// (Generation) advances on every change of the transport state, the queue,
// the modes, the selection, maintenance, the output's availability and an
// unconfirmed release, never on the position, buffering or a starting
// session's first sound, so a command carrying IfGeneration succeeds
// during steady playback. A random run ID makes a generation from another
// run never match.
//
// # The transport
//
// The states are Stopped, Playing (Starting until the new session is
// heard), Paused (holding the paused session, or none: an edit that dropped
// the paused content leaves it paused at the start of the selection),
// Releasing (a session asked to release, its pending intent applied on the
// acknowledgement) and Maintenance. Each command in each state:
//
//	          stopped          playing, starting   paused               paused, no session  releasing             maintenance
//	Play      start (1)        nothing             resume (2)           start (1)           pending: play         refused
//	Pause     nothing          pause               nothing              nothing             nothing               refused
//	Stop      nothing          release, stop       release, stop        stop                pending: stop         nothing
//	Next (3)  next, start      next, release,      next, release,       next, start         next, pending:        refused
//	                           play                play                                     play
//	Previous  previous, start  previous, release,  previous, release,   previous, start     previous, pending:    refused
//	(4)                        play                play                                     play
//	PlayID    select, start    select, release,    select, release,     select, start       select, pending:      refused
//	                           play                play                                     play
//	PlayIndex as PlayID, with the ID at the index, which the owner looks up as it plays it
//
// (1) The selection, or the first entry when there is none; an empty queue
// is refused and nothing changes. (2) At the paused position: the backend
// answers continued or reopened, and a refusal leaves it paused with the
// reason. (3) Past the end (the last entry selected, repeat off): stopped,
// nothing; playing or paused, release and stop with the selection on the
// last entry; paused with no session, stop; releasing, pending: stop. (4)
// At the head Previous restarts the selection. In every state Play, PlayID,
// PlayIndex, Next and Previous are refused while a release is unconfirmed;
// whatever starts a session, or will, is refused while the output is
// unknown or unavailable (Next and Previous with it, past the end aside),
// and an empty queue refuses it; and everything is refused once shutdown
// begins. The policy function, policy, is the table: Next and Previous
// select and play in every state, paused and stopped included, as PlayID
// and PlayIndex do, as controllers use them and SeekId and SeekIndex, and
// as OpenHome's reference Playlist source (ohPipeline's SourcePlaylist)
// plays after both. Release 5a may adjust it after testing named
// controllers.
//
// A command that waits on the transport (a start, a release, a pending
// intent) answers when no start and no release is under way: nil, or, for
// one that asked for playback that did not happen, why (the backend's
// refusal, every entry unplayable, an unconfirmed release, the output, a
// fault, or ReasonSuperseded: a later command or edit, a Stop, a DeleteAll,
// a deletion of the entry to play, replaced it before it began). The answer
// is the command's own, the first outcome recorded for its playback: its
// playback began once the session started for it is heard (its first
// Audible), and from then on it answers nil, whatever stops playback after;
// otherwise the first reason recorded for it stands, so a later Play that
// plays never makes an earlier one's answer nil. The session started for a
// command is the one it starts, or, for the commands that waited behind a
// release or set a pending intent, the one that starts once the release is
// acknowledged, the latest intent's, which all of them share; past an entry
// refused at its start, it is the session that follows. Only that session's
// Audible decides the command, and a Next, Previous, PlayID or PlayIndex
// that replaces it before it is heard answers the command ReasonSuperseded,
// and a Play whose Resume is under way the same; so it does for a command
// carried past an entry refused at its start when one of the four comes
// while the release before the next session is under way and moves the
// pending target (one naming the entry already pending shares that session).
// Pause and a resume answer with the backend's answer; Stop and
// EnterMaintenance once the release is confirmed or its deadline passed.
//
// The selection is what a controller reads as the current ID. During
// playback it moves on the backend's audible report, never on a hand-off;
// a command, a deletion or a skip sets it as the owner applies it, with
// nothing audible until the new session reports. At the end of the last
// entry with repeat off playback stops on it (Play replays it); with
// repeat on the first follows. Deleting the selection selects the entry
// that followed it: while playing it plays, while paused the transport
// stays paused there with the paused content dropped, while stopped it is
// only selected; with nothing after it, playback stops and nothing is
// selected. A command still waiting for the deleted entry's playback,
// behind a release or while its session starts, answers ReasonSuperseded,
// whether the entry that followed plays or nothing follows and playback
// stops: its entry was deleted before it played, and the stop that follows
// records no reason over that one. One whose session was heard before the
// deletion played, and answers nil. An entry the backend refuses before
// any of its content played is skipped with its reason (Snapshot.Skipped);
// a pass that refuses every entry, counted from the last session start or
// queue change, stops with ReasonAllUnplayable, so repeat on makes one lap,
// never a spin. A hand-off made past a skipped entry keeps that skip when a
// queue change starts a new pass: an edit that leaves the entries between
// it and the occurrence before it as they were neither cuts it nor stops
// it. A failure after content played is a fault: recorded
// (Snapshot.Faults), playback stopped, never restarted.
//
// The output's availability is an input (ReportOutput, OutputReport). It
// starts unknown. Its going away, or the bound unit changing, stops
// playback and freezes the position; while nothing plays it drops a pending
// play and what the backend prepared (Backend.Discard). Its return plays
// nothing.
//
// # The queue
//
// OpenHome Playlist:1's semantics without the protocol: at most MaxEntries
// entries and MaxTotalMetadataBytes of metadata, MaxMetadataBytes an entry,
// each a nonzero uint32 ID, a Reference and opaque metadata; insert after 0
// is the head, an insert into an empty queue selects it, and repeat is
// repeat-all. Shuffle is release 7a's and refused. The token advances on
// every change of membership or order.
//
// No ID and no token is handed out twice by runs sharing their durable
// state, a crash included. Restore's separate margin has the assumptions
// documented below.
// Both come from counters whose reservations, a block (idBlock, 4,096) at
// a time, are written to a file of their own (Config.ReservePath) and made
// before anything they cover is handed out: a reservation is made once its
// bytes and every directory entry between it and the data directory
// (Config.DataDir) are fsynced, on every write, whoever made those
// directories (atomicwrite.WriteBytesDurable), and a write whose directory
// fsync fails is a reservation not made, since its rename can roll back at a
// crash. Off an overlay the data directory's own entry is never synced, nor
// needs to be: it holds the player's identity, so a crash that takes it
// makes the player a new device. On an overlay a data directory in the lower
// layer alone is copied up by the first write, and a crash that took the
// copy would keep the identity below it, so there every directory from the
// data directory up to the root of its mount is synced, by New's probe and
// once each reservation lands. New requires the data directory to exist, and the reservation
// file to lie plainly below it, with no ".." element and no symbolic link or
// mount point between them (the data directory itself may be a link), and
// probes those barriers before it reads anything (atomicwrite.ProbeDurable);
// it fails when a directory on the way is not a directory, or cannot be
// synced: on a filesystem whose directory fsync always fails, as some
// network and FUSE mounts' does, every reservation would fail and every edit
// be refused for ever, so the error names the directory that failed and says
// the data directory must move to a filesystem that can sync a directory. A
// platform with no directory fsync (Windows) is refused the same way. A
// directory the probe cannot read (a Stat or an open refused, EACCES or EIO)
// is refused as unreadable, with the probe's error and no advice to move:
// that advice is for a directory fsync that ran and failed
// (atomicwrite.ErrBarrierFailed). A data directory on storage that does not
// keep what is written to it through a reboot is refused too
// (atomicwrite.ErrVolatile: tmpfs, ramfs, an overlay whose upper layer is
// one, as Raspberry Pi OS's "Overlay File System" sets up): every barrier
// answers there, and a reboot takes the reservation and keeps the identity,
// so the IDs and tokens would repeat with no crash at all. The refusal names
// the data directory, says so, and says to put it on persistent storage, and
// so does the refusal of storage that cannot be told to outlive a reboot
// (atomicwrite.ErrMediumUnknown): the orchestrator's decision on round 9's
// review, to fail closed. The next block is reserved in the
// background, so a command waits on a write only when that write has not
// landed by the time the current block runs out, and at most
// reserveTimeout: past it the waiting edits are refused with
// ReasonReservation, and so is every edit that needs a value until the
// write lands. A start begins one block past the larger
// of the saved reservations, the queue file's values and its largest ID, a
// margin for one reservation that storage acknowledged and did not keep,
// and every start advances the token once its own reservation is durable:
// until then the token is 0, none (a start whose reservation failed, or
// whose bad reservation file stays). A start spends at most three blocks
// beyond what it hands out, so the 4,294,967,295 IDs last some 4.29 billion
// inserts, or at least some 349,000 starts (957 years at one a day), and the
// tokens as long; the last is followed by ReasonIDsExhausted, logged once,
// never by a reuse. A reservation that cannot be made durable refuses every
// insert and every other edit that advances the token with
// ReasonReservation; it is retried by the next such edit, never in the
// background as if it were optional.
//
// Reservations are version 2 and name Config.PlayerUUID. Every write merges
// each counter with the live file, whatever UUID it names, and stamps this
// engine's UUID before any value is published. A version 1 reservation has
// no UUID and remains a floor. A start whose first ID or token would exceed
// uint32 refuses; edits refuse at the boundary rather than wrapping.
//
// A backup carries every decodable live reservation, restamped with its
// captured UUID; only a missing reservation is omitted, and an unreadable
// one refuses the backup with repair advice.
// Restore writes max(snapshot, live) per counter plus 2^24 and one uniform
// random 24-bit draw, before writing the UUID or any other identity file.
// The live floor counts even under another UUID; an interrupted restore
// can leave it raised under the old identity, and nothing rolls it back.
// This assumes the source issued fewer than 2^24 of either value since its
// snapshot. Independent restores of an old snapshot rely on the random gap
// exceeding the earlier restore's usage; this is probabilistic, not a global
// uniqueness guarantee. Restore the newest snapshot and run only one box
// with an identity. No queue is backed up.
//
// # Persistence
//
// The queue file (Config.QueuePath) holds the entries, the counters'
// values and reservations, the writer's UUID (version 2), the modes and the selection, never the
// transport state: a start is stopped. A foreign UUID is ReasonQueueIdentity,
// logged once and moved aside without loading any entries. Version 1 has no
// UUID and belongs to the running identity (development boxes only). Restore
// durably removes the destination queue before replacing its identity; a
// restore of the same identity keeps it. One writer goroutine writes the
// newest revision through internal/atomicwrite, fsynced, each write after
// the last has returned, so an older revision never lands after a newer
// one; it waits writeCoalesce after a change and at most writeMaxAge in
// all, and a failed write keeps the state dirty, is logged once and is
// retried with backoff. Both files are opened without waiting and read only
// when they are files (fsutil.OpenAsFile), so a named pipe at either path is
// refused at once rather than waited on. A queue file or reservation file
// that is unreadable, no file (a named pipe, a directory, a device), past its
// cap (maxQueueFileBytes is sized for a full queue's real encoding), of
// another version or invalid is logged and renamed aside with a timestamp,
// never deleted, and the engine starts from what remains; if the rename
// fails the file stays where it is, nothing is written over it and every
// insert and every other token-advancing edit is refused with
// ReasonFileStuck until the operator moves it.
//
// # Maintenance
//
// EnterMaintenance checks the generation and closes the engine to
// mutations in one step, then releases the session and drops pending
// backend work; it answers a Hold whose lease only its caller sees, with
// whether the release was confirmed. Every mutation is then refused with
// ReasonMaintenance (Stop does nothing), reads go on, and events of earlier
// sessions are discarded. ExitMaintenance takes the lease and leaves the
// transport stopped, never resumed. A hold expires after
// Config.MaintenanceTTL unless renewed (RenewMaintenance), which also ends
// one whose holder left before the reply.
//
// # OpenHome
//
// Release 5a maps Playlist:1 onto this API: Play, Pause, Stop, Next and
// Previous; SeekId and SeekIndex to PlayID and PlayIndex; Insert to
// InsertAfter, once the URI is confined and made a Reference; DeleteId,
// DeleteAll; Read, ReadList, IdArray (Snapshot.IDArray, base64 big-endian),
// IdArrayChanged (Snapshot.Token); Repeat, SetRepeat, Shuffle, SetShuffle;
// TracksMax (MaxEntries); Id (Snapshot.Selected) and TransportState
// (Snapshot.State, Pending and Starting); faults 800, 801 and 802 from
// ReasonIDNotFound, ReasonFull and ReasonIndexNotFound. Time:1 reads
// Snapshot.Position and Info:1 counts the audible occurrences.
package engine
