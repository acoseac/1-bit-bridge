# Backup and restore

Run `bridge backup --config /path/to/bridge.yaml` to capture the library
database, pairing tokens, TLS pair, configuration, console account, harvest
state, and player identity when present. A running service can be backed up.
Protect the snapshot: it contains credentials that can impersonate the bridge.

Stop the service, then run
`bridge restore --config /path/to/bridge.yaml /path/to/snapshot`.
Restore holds the data directory's exclusive lock and refuses while the
service runs. `--yes` skips the confirmation prompt, not this check.

When restoring onto another box, stop the source first. **Two boxes running
one bridge identity at once are unsupported**, including its TLS pair,
pairing tokens and player UUID. The snapshot's UUID makes the restored player
the device controllers already know. A snapshot without a player UUID leaves
the destination's player identity and reservation unchanged.

Use the box's newest snapshot whenever possible. The player queue is not
backed up. Restoring a different player identity durably discards the
existing queue before replacing the UUID; restoring the same UUID keeps it.
Queue files name their player identity, and a queue from another identity is
moved aside at startup with a warning. Older development queue files without
an identity are read as the running player's queue.

Any readable, valid player reservation is backed up under the captured UUID,
including one left by an interrupted restore, and used to move
both ID and token counters beyond the larger of the snapshot and destination
marks. Restore adds 2^24 plus a cryptographically random value below 2^24,
then makes those marks durable before restoring any identity file. An
interrupted restore may leave a raised reservation; retrying raises it again.
A missing reservation is omitted. An unreadable reservation stops backup or
restore. The remedy for an unreadable reservation or an exhausted destination
mark is the same player reset: **with the service stopped, remove the box's
`player-uuid`, `player-ids.json` and `player-queue.json` together**. The next
start makes a new identity with no queue, a new device to controllers.
**A snapshot's `player-ids.json` is never a valid repair**. Do not delete the
reservation while keeping the player identity. **This reset gives up for
good every player identity the box ran before it, its own and any a restore
gave it: no snapshot of one of those identities, whichever box took it, may
restore its player identity again. Remove `player-uuid` and `player-ids.json`
from every such snapshot, or delete those snapshots**; restoring one would
reuse values that identity already issued. If the reset answers a refused
restore, remove the two files from a copy of that snapshot, then restore that
copy.

This margin assumes the source issued fewer than 2^24 IDs and fewer than
2^24 tokens after the snapshot and before stopping. Two successive restores
of the same old snapshot onto different boxes rely on the random gap being
larger than the earlier restored player's usage. That is probabilistic;
restoring the newest snapshot avoids relying on this second gap. Restore
prints advice when the snapshot is more than a day old according to the
destination's clock, or lacks a matching reservation mark.

Restore refuses before writing if a counter has too little space for the
largest jump plus 2^24 more values. The error identifies the exhausted source:

- For a snapshot mark, remove `player-uuid` and `player-ids.json` from a copy
  of the snapshot and restore the rest. The box keeps or makes its own identity.
- For a destination mark, follow the player reset above, removing all three
  live files together while the service is stopped. This gives up for good every
  player identity the box ran before the reset, its own and any a restore gave
  it: no snapshot of one of those identities, whichever box took it, may restore
  its player identity again. Remove `player-uuid` and `player-ids.json` from
  every such snapshot, or delete those snapshots; restoring one would reuse
  values that identity already issued. For this restore, remove the two files
  from a copy of this snapshot as well, then restore that copy.

Each restore spends less than 2^25 values, so roughly 120 worst-case restores
consume the available space. Ordinary engine starts spend much less.

New snapshots use schema 2 so older bridge versions refuse them before
restoring an identity without advancing its counters. The new bridge still
reads schema 1 snapshots and gives their restored identities the same jump.
