# Backup and restore

Run `bridge backup --config /path/to/bridge.yaml` to capture the library
database, pairing tokens, TLS pair, configuration, console account, harvest
state, player identity and player history when present. A running service can be backed up.
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
restore. If read access to `player-ids.json` failed, restore read access first,
then rerun `bridge output player reset`. A malformed reservation or an exhausted
destination mark also requires a player reset: **with the service stopped, run
`bridge output player reset --config /path/to/bridge.yaml`**. It asks for
confirmation; `--yes` skips only that prompt. The command records the
identities given up before removing the queue, UUID and reservation, in that
order. Rerun the command to finish an interrupted reset; the player will not
start until the reset finishes, including after a reboot. The next
start makes a new identity with no queue, a new device to controllers.
**A snapshot's `player-ids.json` is never a valid repair**. Do not delete the
reservation while keeping the player identity.
A reset gives up for good the identity it removes and any the box ran
earlier (one an earlier restore replaced on this box), unless, after this
box last ran it, another box took it over from this box, by restoring a
snapshot this box made, and has not reset it since, running or powered off. Their values were held only by the floor the reset drops. No snapshot
of a given-up identity taken before the reset, on this box or another, may
restore its player identity again: remove `player-uuid` and
`player-ids.json` from every such snapshot, or delete those snapshots. Any
other box that still holds a given-up identity's files (one powered off
since a migration, say) must be reset too before it runs again. An identity
another box took over from this box by a restore, after this box last ran
it, and has not reset since, is not given up: that box's floor holds it, and its snapshots stay valid.
If the reset answers a refused restore, remove `player-uuid` and
`player-ids.json` from a copy of that snapshot, then restore that copy.

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
- For a destination mark, run the player reset command above while
  the service is stopped.
  A reset gives up for good the identity it removes and any the box ran
  earlier (one an earlier restore replaced on this box), unless, after this
  box last ran it, another box took it over from this box, by restoring a
  snapshot this box made, and has not reset it since, running or powered off. Their values were held only by the floor the reset drops. No snapshot
  of a given-up identity taken before the reset, on this box or another, may
  restore its player identity again: remove `player-uuid` and
  `player-ids.json` from every such snapshot, or delete those snapshots. Any
  other box that still holds a given-up identity's files (one powered off
  since a migration, say) must be reset too before it runs again. An
  identity another box took over from this box by a restore, after this box
  last ran it, and has not reset since, is not given up: that box's floor holds it, and its snapshots stay valid.
  If the reset answers a refused restore, remove
  `player-uuid` and `player-ids.json` from a copy of that snapshot, then
  restore that copy.

Each restore spends less than 2^25 values, so roughly 120 worst-case restores
consume the available space. Ordinary engine starts spend much less.

New snapshots use schema 3 so older bridge versions refuse them before
ignoring the player history. The new bridge still reads schema 1 and 2
snapshots and gives their restored identities the same jump.

`player-history.json` stays in the data directory through a reset. It records
identities replaced by restore whose values the live floor covers, and the
identities a reset gives up. Backup lists and carries the history even when
there is no player UUID. If the player's identity or history changes during
a backup, backup refuses and asks you to run it again. Normal advances of the
reservation counters do not prevent a live backup. Restore merges the
snapshot's history with the box's
history before installing an identity; it never replaces the live list with
an older one. Do not delete or edit the history to bypass a refusal. A history
that is corrupt, unreadable or not a file refuses backup, restore or reset.
For a reservation that cannot be read, restore read access to `player-ids.json`
and then rerun `bridge output player reset`; a malformed reservation can be
reset, retaining every UUID it still names.

Restore refuses a UUID given up in either history before writing any file.
Remove `player-uuid` and `player-ids.json` from a copy of the snapshot to
restore the rest; the box keeps or makes its own player identity. Keep the
history in that copy. This check fails closed: an identity another box took
over after this box last ran it still has valid snapshots on that other box,
as described above, but this box refuses it after recording a reset. It
cannot establish what the other box still holds. Refusing is safe; reissuing
an old ID or token is not.
