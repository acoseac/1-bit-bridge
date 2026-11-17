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
of a veto checks immediately before its call. Both the step and its hold
are checked before each veto creation attempt, even its existence lookup;
the fallback in the backups folder is attempted only if the bundle attempt
failed. The check
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

On Windows no folder can be synced, so a re-verification of a snapshot
already verified does not replace its manifest's verdict. The check still
runs and reports what it found separately from what readers now read. If a
component check finishes FAILED where the record held it VERIFIED, even
when the other component is INCOMPLETE, it puts a veto in the snapshot,
or beside it in the backups folder. All readers,
including the doctor, console, startup decision and future prunes, then read
the old VERIFIED as INCOMPLETE (`unsynced`). The console's cached reading is
invalidated by a separate stamp of the backups folder. That stamp also
checks give-up first: if skipped, the error says the record is vetoed but
the console may retain its cached reading until its cache expires or the
backups folder changes. This is not a failed veto. An INCOMPLETE component
disproves nothing; it does not negate a completed failure of the other one.
The veto cannot be made crash-durable on Windows. If both placement
attempts fail, the error explicitly says the disproved record could not be vetoed
and this verification skips its prune; a later process can still trust the
stale VERIFIED. If give-up stops the attempts before every location was
tried, the error instead says "the veto could not be confirmed": an
uninspected location may already hold one. This also skips the prune.
A recheck that records no new verdict is followed by no prune. After a successful veto, later prunes apply the usual retention policy
below, protecting the older trustworthy recovery point without keeping every
intermediate failure.

A FAILED database keeps the doctor's damaged-database advice first, even
when the files read `unsynced`. Otherwise, on a platform without folder
sync, the doctor's advice for `unsynced` is to take a fresh snapshot with `bridge backup --config <bridge.yaml>`, using the
config the doctor read. The record cannot be trusted or re-recorded here.
`unsynced` alone does not prove damage: a successful check can leave a veto
when its removal fails or its process ends, and a failed veto lookup also
makes readers conservative.

A failure while reading the schema's tables or columns is `open-failed`,
not a finding that a column is missing. Cancellation leaves the check
INCOMPLETE. The doctor's advice blames the database only when its verdict
is FAILED; apart from the no-folder-sync `unsynced` case above, an unfinished
database or files check gets retry advice. Failed files in a snapshot
with no database are described without claiming
a database recovery point.

When no lock is granted, the backups are busy, or an earlier check still
holds them, a verification that never started says so with its reason in
the command, scheduled snapshot and console log. A cancellation that wrote
INCOMPLETE still reports that write's actual outcome.

The staged manifests and copied files have their permissions set through
their open files before sync and close. SQLite's captured database is opened
without following a final-component symbolic link or reparse point and
validated as a regular file before its permissions are changed through
that descriptor. On Windows, directories, directory symlinks and junctions
are opened as entries and refused as "not a regular file".

This protects the staged and vacuum files' permission changes against
final-component symlink or reparse-point redirection. It does not protect
against replacement with another regular file or a hard link, a parent
directory replaced with a link, or SQLite's own `VACUUM INTO` write, which
uses a path. A root-run backup or restore over a tree an untrusted account
can rename is outside this protection. A stuck-check marker removes an
existing entry and creates a new file exclusively, refusing a racing
replacement at that final entry; its parent traversal is not anchored.

Every step a check takes while it holds the backups folder (marking a
snapshot as being checked again, the check, writing its verdict, the prune
after it, reading the record back, and letting go of the snapshot and of
the folder's lock) has a bound, counted from the moment the check took the
folder's lock, and so does a prune run on its own. A step that does not
answer within it (storage that stops answering) is given up, and nothing
more is checked or pruned. Each further write attempt checks for give-up
as described above; an operation that passed its check may still finish.
A check given up before its verdict's write is admitted writes no completed
verdict. A separate recovery step writes INCOMPLETE over a pending or
incomplete record, then reads it back. Its own give-up signal controls the
cache update: the earlier give-up does not prevent that update. If the
recovery step itself is given up after its write and sync finish, and
the publication's result is received, its record is still reported as
written, with a separate warning that the
console may retain its cached reading until expiry or a backups-folder
change. A failed sync still means the write is not known to survive a crash.
A completed verdict stays. If either component is VERIFIED and the merge
would change the other, recovery needs a veto; the earlier give-up prevents
that veto from being confirmed or created, even if one already exists.
Recovery then writes nothing and reports that refusal beside the read-back.
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

Until a step given up ends, no other check or prune of those snapshots runs,
none of them is restored, and no scheduled snapshot is taken. In the bridge
that runs it they are refused at once, and `bridge doctor`'s `backup-check` line names the
snapshot, or the prune, and its step, whatever the storage answers. Another
process cannot read that it was given up from storage that does not take
writes: it waits for the folder's lock and answers that the snapshots are
busy. Restarting the bridge, or stopping the `bridge backup --verify` that
runs it, ends the step (backlog B653). A `bridge restore` given up while it
holds the snapshots' lock holds them the same way, and is named a restore of
its snapshot: stopping that command ends it.

When the bridge starts, it takes no snapshot when one from the last day has
a verified database. Nor does it take one when the newest snapshot from the
last day is still waiting for its check: it runs that check instead, and
takes a new snapshot whenever the check finds the database FAILED, whether
or not its record could be written. A recheck that cannot record a verdict
on this platform also takes a replacement when the read-back remains vetoed
and its own finding has a FAILED component, including VERIFIED database /
FAILED files. A leftover veto
alone, with a passing check, takes no replacement: failed veto removal
must not add a snapshot at every start. A bridge that keeps
restarting before its check ends keeps one snapshot, not
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

Restore also refuses, before it asks you to confirm and with nothing
restored, while any bridge command has the library database open: a `bridge
scan`, `analyze` or `upscale`, a `bridge library add` or `remove`, a `bridge
backup`, or `bridge doctor`. Stop it, then restore again. It refuses as well
where the data directory's storage grants no file locks, since it cannot then
show that nothing has the database open: put the data directory on storage
that grants them. While restore runs, every other bridge command refuses to
open the database. A bridge from before this lock was added takes no lock
that restore can see, so stop every bridge process of an earlier version,
and every command one of them runs, before you restore.

While an update the bridge installed waits to be confirmed after a start that
did not end cleanly, restore refuses to replace the database, with nothing
restored. A restore made while an update is on probation keeps that update
from being rolled back: the previous version may not read the database the
restore put in place. For the same reason, while an update is on probation,
the version it replaced (a `bridge` you kept, or the update's backup) refuses
to restore the database, with nothing restored: restore with the updated
bridge, or once the update is confirmed or rolled back. While restore replaces
the database, an update waits for it to finish (`bridge update` says to try
again if the restore takes longer than its wait), and a restore that an update
being installed keeps waiting is refused the same way, with nothing restored.

Before it asks you to confirm, restore looks at the snapshot. It refuses at
once, with nothing restored, while a check, a prune or a restore of those
snapshots that did not end still holds them, and it refuses a snapshot whose
storage does not answer within about 20 seconds.

Once you have confirmed, restore also takes the lock of the folder that holds
the snapshot, the lock the bridge that keeps those snapshots checks and prunes
them under, and holds it until it ends, so no prune removes the snapshot while
it is restored. It does not hold it across the prompt. Before it writes
anything, it waits up to 15 seconds for a check or prune of those snapshots
to end, and refuses past that with nothing restored. It refuses at once while
a check or a prune that did not end still holds them: restart the bridge that
keeps them, or stop the `bridge backup --verify` that runs it, then restore
again. A restore that did not end holds them the same way, and is named as
one: stop that `bridge restore`, then restore again. Under that lock it reads
the snapshot again, and refuses one that a prune removed while you were asked
to confirm. A copy of a snapshot kept elsewhere restores the same way: nothing
tells a copy from a live backups folder for certain, so its folder is locked
as a backups folder is, with the lock file made there if it is missing. A
snapshot on read-only media, on storage that grants no file locks, or in a
folder you cannot write is refused with nothing restored: copy the snapshot's
folder to a local disk, and restore from the copy.

A restore that you stop, or whose snapshot's storage stops answering, before
it begins to write is refused with nothing restored, the destination as it
was. If the snapshot's storage stopped answering once restore held its
folder's lock, the folder stays locked until that read answers or the
`bridge restore` process ends, and meanwhile the snapshots read as held by a
restore that did not end. Once restore has begun to write, it runs to its
end.

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

### Restore the database alone

When the library database is damaged, or you want an earlier library index
back while keeping everything else (paired devices, the TLS certificate, the
settings, the console account and the player), restore the database alone:

```sh
bridge restore --config /path/to/bridge.yaml --database-only /path/to/snapshot
```

Give the flags before the snapshot. It takes the database of a snapshot whose
database check reads `VERIFIED` (`bridge doctor`'s `backup-verified` line
names the newest one), and refuses any other before it asks you to confirm,
with nothing restored: a snapshot from a newer bridge, one whose database
failed or did not finish its check, and one whose record cannot be trusted.
The other files' verdict is reported and never required. With `--full` after
`--database-only`, the database it prepares is checked with SQLite's
`integrity_check` and `foreign_key_check` rather than `quick_check`.

It restores nothing else: the token store, the TLS pair, `bridge.yaml`, the
console account, the player's files and the library stay as they are. The
harvest's sync position is rewound, so the next harvest submits the restored
library again, and its credential is kept. Paired apps see the restored
database as a new library history and make a full sync. The paragraphs above
on the data directory's lock, on commands that have the database open and on
an update on probation apply to it as they do to the full restore.

The library's roots must have the form the snapshot's database was indexed
under: one root or none on both sides, or several on both with the same
folder names, exactly as written. The snapshot's own `bridge.yaml`, unchanged
since it was captured, says which form that was. A snapshot without it, or
whose roots have another form, is refused with nothing restored; the full
restore, without `--database-only`, puts the settings back with the database.
A root set through `BRIDGE_LIBRARY_ROOTS` takes no part in the comparison, and
a single root moved elsewhere is restored, its paths matching the files under
the new root or not.

The files it replaces are kept, never deleted: `bridge.db`, and its
`bridge.db-wal`, `bridge.db-shm` and `bridge.db-journal` where they were
there, move unopened into a folder of their own under `restore-originals/` in
the data directory, beside `backups/`, with a record of each (`restore.json`:
its name, size and SHA-256). Nothing the bridge runs removes them; delete such
a folder once you no longer need it. The restore needs free space for twice
the snapshot's database, and refuses a data directory on storage that cannot
sync a directory (some network and FUSE mounts), with nothing restored. On
Windows no directory can be synced, so each of its steps is atomic and not
known to survive a power loss (backlog B920).

#### When a restore of the database alone does not finish

While it moves files, the restore keeps a journal, `bridge.db-restore.json`
beside the database. While that journal stands the bridge refuses to start,
every command refuses to open the database, and `bridge doctor` names the way
out, which is one command, with no snapshot:

```sh
bridge restore --config /path/to/bridge.yaml --database-only
```

Run it with the bridge version that began the restore: another version
refuses, with nothing moved. It reads no snapshot, so one removed since is no
obstacle. If the restored database was in place, it completes the restore;
otherwise it puts every original file back where it was, each checked by its
SHA-256, and removes the journal. It asks you to confirm, naming what it
found, unless you give `--yes`.

It stops with the journal kept and nothing moved, saying where every original
is, wherever what it finds is not what the restore recorded:

- an entry at a name where none was due (a file put there, or a `-wal` beside
  the restored database): move that entry out of the data directory, then run
  the command again;
- an original that is neither at its name nor in its `restore-originals/`
  folder, or one whose content changed: put the originals back by hand, below;
- a journal it cannot read, or one another bridge version began whose version
  you cannot run: put the originals back by hand, below.

A restore that stopped once its journal was gone is finished: what it left at
the names it stages under (`bridge.db-restore` and its sidecars) the next
restore removes.

#### Put the originals back by hand

When the command cannot run, put the originals back by hand, with every bridge
process stopped:

1. Open the folder under `restore-originals/` that the restore's words name
   and read its `restore.json`: it lists each original and whether it was
   there.
2. Move whatever stands at `bridge.db`, `bridge.db-wal`, `bridge.db-shm` and
   `bridge.db-journal` in the data directory into a folder of your own.
3. Move each original in that folder back to its name in the data directory,
   an original the bridge could not read whole included.
4. Remove `bridge.db-restore.json`, then start the bridge.

To keep the restored database instead, leave it at `bridge.db`, move any
`-wal` or `-shm` beside it away, and remove the journal.

`rm bridge.db*` is no recovery: it removes the journal and the staged copy
with the database, and the lock files beside it, which every bridge process
that has the database open holds, so never run it while one runs. The
originals stay in `restore-originals/`.

What the restore leaves open is tracked as backlog B919, B920 and B921.
