# Backup and restore

Run `bridge backup --config /path/to/bridge.yaml` to capture the library
database, pairing tokens, TLS pair, configuration, console account, harvest
state and player identity with its counter floor, when present. A running
service can be backed up. Protect snapshots: they contain credentials.

## Verified snapshots

Each snapshot is checked as it is taken. The snapshot records every file's
size and SHA-256 as it writes it, and its `manifest.json` says the check is
pending until the check has run. The check compares the snapshot with those
records, never replacing them, and records two verdicts in the manifest:

- **the database's**: the copy has the size and SHA-256 it was written with;
  SQLite's `quick_check`, run over a read-only open that creates nothing in
  the snapshot, answers exactly `ok` (its pages and records well formed, no
  NULL where a column is NOT NULL); and its `user_version` is one this
  bridge's migrations reach, with that version's tables and columns there.
  `quick_check` does not compare an index with its table:
  `bridge backup --verify <snapshot-dir> --full` runs SQLite's
  `integrity_check` and `foreign_key_check` as well.
- **the other files'** (the configuration, the token store, the TLS pair,
  the console account, the harvest state and the player's files): each is
  there, a regular file, with the size and SHA-256 it was written with.

A verdict is `VERIFIED`, `FAILED` with a reason code (`hash-mismatch`,
`quick-check`, `schema-newer` and so on), or `INCOMPLETE` when the check did
not finish (the bridge stopped, or the check ran past its deadline). A
snapshot whose check is still to come, or under way, reads `PENDING`; one
whose bridge stopped before its check ended reads `INCOMPLETE`. A snapshot
taken by an earlier version records no sizes or hashes and reads
`UNVERIFIED`: neither verified nor known to be damaged, its files are checked
for presence only.

One check runs at a time, at the lowest priority the platform gives a
thread, its reads paced. A check removes nothing, whatever it finds.

`bridge backup` says what it kept and both verdicts, and exits 0 only when
both are `VERIFIED` (for an install with no database yet, when its files
are). `bridge backup [--config bridge.yaml] --verify <snapshot-dir> [--full]`
checks a snapshot already taken in the backups folder of the install the
configuration names, however the path is written (`.` inside the snapshot, a
link to it), and records its verdict; a copy of a snapshot kept elsewhere is
refused, so copy it back into that folder to check it. A quick check never
replaces the verdict of a full one unless it finds the snapshot worse.
`bridge doctor` and the console (Jobs, and Settings → Backups) show the
newest snapshot written and the newest whose database is verified, apart,
with both verdicts; they read the snapshots' manifests and run no check. A
command the doctor suggests gives the configuration as `<bridge.yaml>`: use
the one the doctor read.

A snapshot already verified is marked, durably, as being checked again
before its check begins, and until that check's verdict is written and
durable no reader takes the snapshot for verified, whatever happens to the
write. Only a check that finishes clears that mark: one that is stopped,
runs past its deadline or fails on an error of the bridge's own leaves it,
and so does one given up (below), subject to an operation that already
passed its last check for give-up. Each removal, rename attempt and creation
of the mark before a recheck checks immediately before its call. The check
and call are not atomic: a give-up between them still lets the call run.
A removal, rename attempt or mark creation already begun when the give-up
lands finishes afterward. Removal of the last effective mark exposes what
the finished check found and wrote. Where the storage cannot keep that mark, the snapshot is not checked again: `bridge backup --verify`
says so, and what its earlier check found. If that refusal is cleaning up
a mark it created when it is given up, a removal already begun can finish
after the caller has answered. Successful removal of the last effective
mark restores the earlier check's record as it stood before the recheck;
another mark or a failed removal keeps the conservative reading. The
reported read-back is what was read then. When a verdict cannot be
written, `bridge backup --verify` says what the check found and, apart, what
the record reads. When the record cannot be read back once the check has
written it, or written nothing, `bridge backup --verify`, `bridge backup` and
the scheduled snapshot say so and what the write did, and never take what
the check found for the record; the command then exits 1.

On Windows no folder can be synced, so that mark cannot be kept, and a
re-verification of a snapshot already verified is never recorded there: the
check still runs, and `bridge backup --verify` says what it found, that it
was not recorded, and what the record reads, which keeps the earlier
verdict for the doctor, the console and the prune. A check that records
nothing over a verified snapshot is followed by no prune; the next
snapshot's prune runs as usual.

Every step a check takes while it holds the backups folder (marking a
snapshot as being checked again, the check, writing its verdict, the prune
after it, reading the record back, and letting go of the snapshot and of
the folder's lock) has a bound, counted from the moment the check took the
folder's lock, and so does a prune run on its own. A step that does not
answer within it (storage that stops answering) is given up, and nothing
more is checked or pruned. Each further write attempt checks for give-up
as described above; an operation that passed its check may still finish.
A check given up before its verdict's write is admitted writes no completed
verdict. The record is written once more, where that can still be done, to say the check did not finish,
unless it holds the verdict of an earlier check that finished, which stays.
`bridge backup --verify` and the scheduled snapshot say how far that write
went (it wrote nothing, it wrote its record, or its write did not finish)
and what the record reads, or that it could not be read back, or that its
read-back was given up. If the caller stops waiting before the write's
outcome arrives, it says that the write outcome is unknown: a record may
have been written, or may still be: the verdict, or INCOMPLETE once the
stalled step ends. During shutdown, the scheduled snapshot silences a
cancellation-only error only when the check's verdict is unknown, passing,
or cancellation alone. An outer expiry's cancellation wrapper stays silent
when no verdict has arrived; any accompanying error is reported. A known
`aborted`, `deadline` or FAILED verdict is reported with the write's outcome
and the stop's own error. Without an error, a stopped context silences only
a record whose non-passing verdicts are cancellation alone: the database
must be VERIFIED, UNVERIFIED with only `no-database`, or INCOMPLETE with only
`cancelled`; the files must be VERIFIED or INCOMPLETE with only `cancelled`;
and at least one must be INCOMPLETE with only `cancelled`. The same predicate
decides whether a found verdict beside an error is cancellation alone.
A step that is slow and keeps within its bound changes nothing.

Until a step given up ends, no other check or prune of those snapshots runs
and no scheduled snapshot is taken. In the bridge that runs it they are
refused at once, and `bridge doctor`'s `backup-check` line names the
snapshot, or the prune, and its step, whatever the storage answers. Another
process cannot read that it was given up from storage that does not take
writes: it waits for the folder's lock and answers that the snapshots are
busy. Restarting the bridge, or stopping the `bridge backup --verify` that
runs it, ends the step (backlog B653).

When the bridge starts, it takes no snapshot when one from the last day has
a verified database. Nor does it take one when the newest snapshot from the
last day is still waiting for its check: it runs that check instead, and
takes a new snapshot only when the check finds the database damaged. So a
bridge that keeps restarting before its check ends keeps one snapshot, not
one per start, and the first start that lasts long enough verifies it.

With retention on (`backup.keep` above 0), the prune after each snapshot
keeps the `keep` newest, and beside them, even when older: the newest
snapshot whose database is `VERIFIED`; while none is, the newest one taken
by an earlier version; the newest whose database check `FAILED` or did not
finish, as evidence, one at most; and every snapshot whose check is still to
come or under way, however long it waits its turn. Every other snapshot is
kept or removed by `keep`, as before. The prune runs after every snapshot
taken, whatever its verdict. With retention off, every snapshot stays.

Checks and prunes take a lock in the backups folder, and none runs where
the storage grants no file lock: there the bridge checks and prunes nothing,
takes no scheduled snapshot while the newest one waits for its check, and
`bridge doctor`'s `backup-lock` line says so. Put the data directory on
storage that grants file locks (a local disk, or a network share whose lock
service runs).

## Restore

Stop the service, then run
`bridge restore --config /path/to/bridge.yaml /path/to/snapshot`.
Restore holds the data directory's exclusive lock and refuses while the
service runs. `--yes` skips confirmation, not this check. Like `bridge serve`,
it makes a data directory that is not there yet, and refuses one it cannot
make or use (a regular file or a link to nothing where it goes, or above it),
naming the path, before it writes anything.

The snapshot's player UUID makes the restored player the device controllers
already know. Without a player UUID, restore leaves the destination's player
identity and floor unchanged. The queue is never backed up. Restoring a
different identity durably clears the queue before writing the UUID;
restoring the same identity keeps the queue.

`player-ids.json` is the box's monotonic floor for IDs and tokens across every
player identity it has run. Every floor write takes the larger of the old and
new counters. Restore adds 2^24 plus a random value below 2^24 to the larger of
the snapshot and live floors, durably writes that floor, establishes the local
initialization marker, clears a replaced identity's queue, then writes the
UUID. An interrupted restore may leave a higher floor; retrying raises it
again. An unreadable live floor refuses the restore before any snapshot data
is written.
A lone local discard seal may first be completed as described below.

A snapshot whose manifest lists no floor beside its player UUID, and that holds
none, cannot bound what that identity handed out. One from a bridge older than
this one takes 2^31 + 2^24 as its snapshot floor: an identity below the upgrade
boundary described below, plus a later adoption's random part, stays under it.
The restore still warns that the snapshot carries no mark. Every snapshot this
bridge writes lists the floor beside the UUID and records
`playerFloorRequired` in its manifest, so one of them without its floor was
edited: restore refuses it before writing anything and names the
snapshot-copy remedy. A floor the manifest lists but that is missing or
corrupt refuses too, whoever wrote the snapshot. So does a snapshot that holds
`player-ids.json` its manifest does not list: every bridge that captures the
floor lists it, so the manifest is damaged. Restore only looks at that entry,
never opening or following it, and refuses before writing anything, the
adoption included, naming the snapshot-copy remedy or a newer snapshot of this
box.

A backup captures the UUID before the floor and requires the floor to name
that UUID. A mismatched pair refuses the backup. If the floor is missing while
the live UUID remains, follow the floor-loss remedy below; retry alone cannot
repair it. Retry advice applies when the UUID vanished during capture. Live file opens retry transient Windows sharing failures.
No snapshot this bridge writes carries a player UUID without its floor.
Snapshots remain schema 2; schema 1 remains readable with the same restore
jump. Snapshots without a player UUID carry no floor.

To rotate the bridge player's identity, stop the service and run:

```sh
bridge reset-player --config /path/to/bridge.yaml
```

The command durably removes the queue, then the UUID, leaving
`player-ids.json` byte-identical. The next start or DAC bind makes a new UUID
and restamps the retained floor by max. Rerunning an interrupted reset is
safe: the old UUID may remain with an empty queue, or there may be no UUID.
Reset does not need to read a malformed, unreadable or dangling UUID; a
directory at that path must be moved aside. `--yes` skips the prompt.
`bridge output player` remains the read-only MPD report.

`player-initialized` is a versioned local marker. Ordinary reset preserves its
consumed state and completes an incomplete seal. Initialization commits floor,
marker, then UUID, each durably. An existing valid floor gains
the marker when the player starts, binds, resets or adopts an identity.
A missing floor beside a UUID, queue or initialization marker is floor loss,
with the upgrade and one-shot post-discard exceptions below.
The upgrade exception is a valid regular UUID with no floor, queue or either
local marker.
Serve boot, start, bind, reset, backup and restore adopt this state without
sealing restores. Both counters start at 2^31 plus a random value below 2^24
before the marker is committed. Restore adopts a UUID-only destination first: its
history is unknown, not a zero live bound. It then adds its smaller jump to the
larger of the adopted floor and snapshot bound. Its adoption draws from the
restore's own random source, before the jump does. Reset removes the UUID; restore
installs the snapshot UUID; the other adopters retain the current UUID.
Serve adopts at boot even when playback is disabled. Backup takes the exclusive
server lock for adoption; if adoption is busy, because another process holds
that lock, stop the service and run the backup again. Any other failure to open
or take the lock refuses the backup with its own error, and with the lock file's
remedy below when it is a permission failure. Backup also takes that lock for
seal completion. When the lock is busy, the completion is logged and left to the
next lifecycle operation. Any other failure is logged with its error and advice
by its cause. A permission failure at the data directory alone, an entry's Lstat
or the barrier that syncs the directory, takes one remedy: restore read, write
and search (execute) access to it and each directory above it, or clear an
immutable or append-only flag or ACL on it; on Windows, full control of it. A
permission failure of a step that names an entry, a marker's read or write, or
the open or lock of `server.lock`, takes the union of that remedy and the
entry's, whatever the entry is: a regular file, a link, a named pipe, or nothing
the open could create. On unix that is the access the step needed on the entry
(read for a marker's read, read and write for `server.lock`), the directory's
remedy, any immutable or append-only flag or ACL on the entry and on what it
links to cleared or a link at it replaced with a regular file, and in a sticky
directory the entry owned or the sticky bit cleared. On Windows it is full
control of the data directory and of the entry, its read-only attribute cleared
or a link at it replaced with a regular file, and any program holding it open
closed. A read-only data directory says so, and a marker refusal keeps its own
remedy. Each of these meets the next serve boot or start too, a permission
failure when it runs as the same user. An interrupted or refused lock, or an I/O
error, is logged with its error alone. So is a `server.lock` that is a link
leading out of the data directory, which is never followed: its error ends
`path escapes from parent`, and a regular file in its place clears it. Backup
continues either way.
An older engine can hand out tokens without writing a queue, so losing only
its floor can look like this upgrade state. Adoption covers old counter space
below 2^31, including reserved blocks skipped at restarts, not the number of
values handed out. For example, 4,097 starts without queue edits publish only
4,097 tokens but reach token 33,558,529. Pre-upgrade counter space reaching
2^31 is unsupported. Even the largest adoption draw leaves over 2.13 billion
values of counter space, well above the engine's 4,097-value startup margin.
A queue without a UUID needs ordinary reset when the floor remains, or
`--discard-floor` when it is missing. A UUID that is a symlink or does not match
the floor also needs ordinary reset. A version-1 live floor names no UUID;
reset retains its counters and the next start writes a named version-2 floor.
A non-file at the UUID, queue or floor path, such as a named pipe, is refused
promptly with advice to move it aside; it is never opened waiting for a writer.

For a lost, corrupt or exhausted floor, the explicit escape is:

```sh
bridge reset-player --discard-floor --config /path/to/bridge.yaml
```

This first durably writes `player-floor-discarded.json`, a permanent seal
against **all incoming player identities**, then independently records the
seal in `player-initialized`. Only after both are durable does it clear queue
and UUID, rename the floor to `player-ids.json.discarded-<random>`, and sync
the directory. A rerun completes the sequence after any interruption. The next
start creates a fresh floor, durably records `minted:true` beside `sealed:true`
in `player-initialized`, then publishes the UUID. The missing-floor exemption
is one-shot: ordinary reset preserves this bit, and losing only the floor
afterwards refuses another start. A later explicit discard opens a new exemption
by writing `sealed:true,minted:false` after the queue and UUID unlink barriers,
the floor rename and its directory sync. The initialization marker and seal are
never backed up or restored as bytes. Damaged entries conservatively count as
present. A seal that is not a regular file, such as a directory, a symbolic link
or a named pipe, is still a seal: every lifecycle operation keeps it, never
opening or following it, and records the seal in `player-initialized` beside it.
No advice says to move a seal record aside. Discard replaces a malformed or
non-file marker after establishing the seal: it writes and syncs a fresh one
beside it first, then renames the old entry aside and the fresh one into its
place, and puts the old entry back if that last rename fails. When the seal
file itself is damaged, it seals `player-initialized` first, so an
interruption never leaves the box without a seal record. A discard that stops
there says it discarded nothing; run it again. A well-formed newer
version requires a newer bridge. A damaged initialization marker beside a
readable floor naming the UUID is accepted and kept as present evidence with
any exemption spent. A start's
preflight warns about it once: serve's at each boot, which covers the player that
boot starts, and a DAC bind's. The warning names the file and says to move it
aside: start re-ensures it; beside a seal entry, completion records sealed and
minted. When the marker's sealed bit is the only intact seal, because
`player-floor-discarded.json` is missing or its bytes do not read, the warning
says to keep the marker instead: beside a missing seal file, a marker moved
aside would let the next start write an unsealed marker and reopen identity
restores. A seal entry that is not a regular file counts as a seal here.
`bridge reset-player --discard-floor` rewrites both seal records, at the cost
of the current identity and its queue. Beside a missing floor
nothing warns, and the floor-loss refusal says not to move `player-initialized`
aside. Discard advice is for a missing or unreadable floor. Do not delete markers
to bypass a floor-loss refusal.

A sealed box still restores the database, tokens and configuration from a
copy of a snapshot with `player-uuid` and `player-ids.json` removed. An
identity-bearing restore refuses before writing snapshot data and names that
remedy.
If `player-floor-discarded.json` exists, of whatever kind, without a seal
recorded in `player-initialized`, the next lifecycle operation verifies the
first record is durable and completes the second. Any floor, UUID or queue entry
consumes an unspent sealed marker, including during reset and backup, even when
the seal bit was already set by an older version. A damaged initialization
marker beside a valid seal is left untouched as spent evidence, after verifying
the seal. With a matching readable floor it does not turn the player off. This
local repair can precede a restore refusal; it never imports snapshot bytes. For
permission failures restore read access first; for a newer reservation format
use a newer bridge. **Never copy a snapshot's older floor over the live floor,
or delete the floor while keeping the identity.**

Restore checks both counters for room for the largest jump plus 2^24 values
before writing any snapshot data. An exhausted snapshot or destination can still
restore its non-player contents from a copy without `player-uuid` and
`player-ids.json`; the box keeps its identity. Ordinary reset does not refund
counter space. If the engine can no longer start, use `--discard-floor` and
accept the permanent seal. Each restore's jump spends less than 2^25 values;
adopting a UUID-only destination first skips the lower half of counter space.

The retained floor protects repeated restores of old identities on the same
box. **Two live holders of a UUID, and restoring a non-latest snapshot onto
another box, are unsupported and not guaranteed.** Stop the source and use
its latest snapshot for migration. The restore margin assumes fewer than
2^24 IDs and tokens were issued after capture and before stopping. Random
separation between independent restores is not a global uniqueness guarantee.
After both discard records are established, loss, truncation or replacement of
any one local file cannot reopen identity restores. Deleting both
`player-floor-discarded.json` and
`player-initialized` can reopen them; this is the smallest such deletion set.
Whole-directory or storage rollback cannot be detected by these local files.
The UUID-only upgrade, and a snapshot from an older bridge that lists no floor,
are bounded by the counter space described above.

Player-file adoption, completion and preflight errors turn the player off with
a logged reason; they never stop the bridge from serving its library, even
when playback is disabled. A damaged initialization marker does not block a
backup or an identity-free restore. Other local seal-repair errors are logged
by backup, which still requires a valid captured UUID/floor pair; an
identity-free restore keeps the box's identity and floor and restores ordinary
data.
