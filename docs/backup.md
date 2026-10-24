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
again. An unreadable live floor refuses the restore before any write.

A backup captures the UUID before the floor and requires the floor to name
that UUID. A mismatched pair refuses the backup. If the floor is missing while
the live UUID remains, follow the floor-loss remedy below; retry alone cannot
repair it. Retry advice applies when the UUID vanished during capture. Live file opens retry transient Windows sharing failures.
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

`player-initialized` is a versioned local marker, preserved by ordinary reset. Initialization
commits floor, marker, then UUID, each durably. An existing valid floor gains
the marker when the player starts, binds, resets or adopts an identity.
A missing floor beside a UUID, queue or initialization marker is floor loss.
The one upgrade exception is a valid regular UUID with no floor, queue or either
local marker: a DAC bound on an older bridge whose engine never ran. Start,
bind and backup retain that UUID and create its zero floor, then the marker,
without sealing restores. A main-era box whose queue and floor were both deleted
by hand is indistinguishable from that state.
A queue without a UUID needs ordinary reset when the floor remains, or
`--discard-floor` when it is missing. A UUID that is a symlink or does not match
the floor also needs ordinary reset. A version-1 live floor names no UUID;
reset retains its counters and the next start writes a named version-2 floor.
A non-file such as a named pipe is refused promptly with advice to move it
aside; it is never opened waiting for a writer.

For a lost, corrupt or exhausted floor, the explicit escape is:

```sh
bridge reset-player --discard-floor --config /path/to/bridge.yaml
```

This first durably writes `player-floor-discarded.json`, a permanent seal
against **all incoming player identities**, then independently records the
seal in `player-initialized`. Only after both are durable does it clear queue
and UUID,
renames the floor to `player-ids.json.discarded-<random>`, and syncs the
directory. A rerun completes the sequence after any interruption. The next
start creates a fresh floor and UUID. The initialization marker and seal are
never backed up or restored as bytes. Damaged entries conservatively count as
present. Discard renames malformed or non-file markers aside and writes fresh
ones after establishing the seal; a well-formed newer version requires a newer
bridge. Follow a non-file refusal's advice to move the entry aside and run
discard. Do not delete markers to bypass a refusal.

A sealed box still restores the database, tokens and configuration from a
copy of a snapshot with `player-uuid` and `player-ids.json` removed. An
identity-bearing restore refuses before any write and names that remedy.
For permission failures restore read access first; for a newer reservation
format use a newer bridge. **Never copy a snapshot's older floor over the
live floor, or delete the floor while keeping the identity.**

Restore checks both counters for room for the largest jump plus 2^24 values
before writing anything. An exhausted snapshot or destination can still
restore its non-player contents from a copy without `player-uuid` and
`player-ids.json`; the box keeps its identity. Ordinary reset does not refund
counter space. If the engine can no longer start, use `--discard-floor` and
accept the permanent seal. Each restore spends less than 2^25 values, so
roughly 120 worst-case restores consume the available range.

The retained floor protects repeated restores of old identities on the same
box. **Two live holders of a UUID, and restoring a non-latest snapshot onto
another box, are unsupported and not guaranteed.** Stop the source and use
its latest snapshot for migration. The restore margin assumes fewer than
2^24 IDs and tokens were issued after capture and before stopping. Random
separation between independent restores is not a global uniqueness guarantee.
Loss, truncation or replacement of any one local file cannot reopen identity
restores after discard. Deleting both `player-floor-discarded.json` and
`player-initialized` can reopen them; this is the smallest such deletion set.
Whole-directory or storage rollback and the pre-upgrade manual floor loss
described above cannot be detected by these local files.
