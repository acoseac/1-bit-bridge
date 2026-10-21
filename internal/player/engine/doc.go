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
//	Next (3)  select next      next, release,      next, release,       select next         select next           refused
//	                           play                stay paused
//	Previous  select previous  previous, release,  previous, release,   select previous     select previous       refused
//	(4)                        play                stay paused
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
// unknown or unavailable; and everything is refused once shutdown begins.
// The policy function, policy, is the table: navigation keeps the
// transport's state (Next and Previous play on while playing, stay paused
// while paused with the paused content dropped as a deletion drops it, and
// only select while stopped, so a Next never starts sound on a stopped
// bridge), and PlayID and PlayIndex play in every state, as controllers
// use SeekId and SeekIndex. Release 5a may adjust it after testing named
// controllers.
//
// A command that waits on the transport (a start, a release, a pending
// intent) answers when no start and no release is under way: nil, or, for
// one that asked for playback that did not happen, why (the backend's
// refusal, every entry unplayable, an unconfirmed release, the output, a
// fault). Pause and a resume answer with the backend's answer; Stop and
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
// selected. An entry the backend refuses before any of its content played
// is skipped with its reason (Snapshot.Skipped); a pass that refuses every
// entry, counted from the last session start or queue change, stops with
// ReasonAllUnplayable, so repeat on makes one lap, never a spin. A failure
// after content played is a fault: recorded (Snapshot.Faults), playback
// stopped, never restarted.
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
// No ID and no token is handed out twice, by any run, a crash included.
// Both come from counters whose reservations, a block (idBlock, 4,096) at
// a time, are written to a file of their own (Config.ReservePath) and
// fsynced before anything they cover is handed out; the next block is
// reserved in the background, so a command waits on a write only when that
// write has not landed by the time the current block runs out. A start
// begins one block past the larger of the saved reservations, the queue
// file's values and its largest ID, which covers one reservation write lost
// with its directory entry, and every start advances the token. A start
// spends at most three blocks beyond what it hands out, so the 4,294,967,295
// IDs last some 4.29 billion inserts, or at least some 349,000 starts (957
// years at one a day), and the tokens as long; the last is followed by
// ReasonIDsExhausted, logged once, never by a reuse. A
// reservation that cannot be made durable refuses every insert and every
// other edit that advances the token with ReasonReservation; it is retried
// by the next such edit, never in the background as if it were optional.
//
// # Persistence
//
// The queue file (Config.QueuePath) holds the entries, the counters'
// values and reservations, the modes and the selection, never the
// transport state: a start is stopped. One writer goroutine writes the
// newest revision through internal/atomicwrite, fsynced, each write after
// the last has returned, so an older revision never lands after a newer
// one; it waits writeCoalesce after a change and at most writeMaxAge in
// all, and a failed write keeps the state dirty, is logged once and is
// retried with backoff. A queue file or reservation file that is
// unreadable, past its cap (maxQueueFileBytes is sized for a full queue's
// real encoding), of another version or invalid is logged and renamed
// aside with a timestamp, never deleted, and the engine starts from what
// remains; if the rename fails the file stays where it is, nothing is
// written over it and every insert and every other token-advancing edit is
// refused with ReasonFileStuck until the operator moves it.
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
