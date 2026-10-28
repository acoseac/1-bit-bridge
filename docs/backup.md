# Backup and restore

Run `bridge backup --config /path/to/bridge.yaml` to capture the library
database, pairing tokens, TLS pair, configuration, console account, harvest
state and player identity with its counter floor, when present. A running
service can be backed up. Protect snapshots: they contain credentials.

Stop the service, then run
`bridge restore --config /path/to/bridge.yaml /path/to/snapshot`.
Restore holds the data directory's exclusive lock and refuses while the
service runs. `--yes` skips confirmation, not this check.

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
