package backup

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// RestoreOptions supplies the restore's warnings and randomness. Zero uses
// crypto/rand.Reader and discards warnings. Random is for deterministic tests;
// production callers leave it nil. It is read once per restored identity.
type RestoreOptions struct {
	Random io.Reader
	Warn   func(string)
	// ConfigAsRead is the bytes of the config file at Targets.BridgeYAML that
	// the caller read the config from when it worked out the targets: nil
	// when the caller read none.
	ConfigAsRead []byte
	// beforeWrites, when set, runs once the restore holds its locks and has
	// read the snapshot, just before its first write; beforeConfig just
	// before it writes the snapshot's bridge.yaml, in its turn of the file's
	// lock. They are the windows a test lands another writer in
	// (WithBeforeWritesForTest, WithBeforeConfigForTest). Nil in production.
	beforeWrites, beforeConfig func()
}

// restoreMargin is the minimum jump and the amount of running room left
// after the largest possible jump. The random part is uniform in [0, margin).
const restoreMargin uint64 = 1 << 24

// playerResetAdvice is the single remedy for giving up an exhausted or
// unreadable live floor. With the service stopped, remove player-uuid,
// player-ids.json and player-queue.json together; the next start makes a
// new identity with no queue (a v1 queue has no identity stamp). A reset
// gives up for good the identity it removes and any the box ran earlier
// (one an earlier restore replaced on this box), unless, after this box last
// ran it, another box took it over from this box, by restoring a snapshot
// this box made, and has not reset it since, running or powered off.
// Their values were held only by the floor the reset drops. No snapshot of
// a given-up identity taken before the reset, on this box or another, may
// restore its player identity again: remove player-uuid and player-ids.json
// from every such snapshot, or delete it. Any other box that still holds a
// given-up identity's files (one powered off since a migration, say) must
// be reset too before it runs again. An identity another box took over from
// this box by a restore, after this box last ran it, and has not reset since, is not given up: that box's
// floor holds it, and its snapshots stay valid. If the reset answers a
// refused restore, remove the two files from a copy of that snapshot, then
// restore that copy. Only the restore refusal carries this step. A snapshot's
// player-ids.json is never a repair; never delete the reservation while
// keeping the identity. B524 will enforce this operator rule before 5a
// ships.
func playerResetAdvice(restoring bool) string {
	advice := fmt.Sprintf("with the service stopped, reset the player: remove the box's %s, %s and player-queue.json together", PlayerUUIDFileName, engine.ReservationFileName)
	advice += "; this reset gives up for good the identity it removes and any the box ran earlier, unless, after this box last ran it, another box took it over from this box, by restoring a snapshot this box made, and has not reset it since, running or powered off; no snapshot of one of them taken before the reset, on this box or another, may restore its player identity again; remove player-uuid and player-ids.json from every such snapshot, or delete those snapshots; reset any other box that still holds one of them before it runs again; an identity another box took over from this box, after this box last ran it, and has not reset since, keeps its snapshots"
	if restoring {
		advice += fmt.Sprintf("; remove %s and %s from a copy of the snapshot as well, then restore that copy", PlayerUUIDFileName, engine.ReservationFileName)
	}
	return advice + "; the next start makes a new identity with no queue, a new device to controllers; a snapshot's player-ids.json is never a valid repair; do not delete the reservation while keeping the player identity"
}

// snapshotIdentityRemedy is the remedy for a snapshot whose player identity
// cannot be restored — a listed reservation that will not read, or a v2 mark
// naming another player. The damage is in the snapshot, not the live box, so
// the operator strips the identity from a copy of the snapshot and restores
// that, keeping the box's own identity. It is NOT playerResetAdvice, which
// gives up the box's identity, queue and every earlier snapshot of it.
func snapshotIdentityRemedy() string {
	return fmt.Sprintf("remove %s and %s from a copy of the snapshot to restore the rest; the box keeps or makes its own player identity", PlayerUUIDFileName, engine.ReservationFileName)
}

// snapshotReservationFloor reads the snapshot's already captured and restamped
// mark when the manifest lists it. A listed reservation is part of the bundle:
// if it cannot be read (corruption, a truncated copy, a hand-edit that deleted
// the file but left the manifest entry) the restore refuses, because treating
// it as "no mark" would drop the snapshot's floor and let the restored
// identity reissue IDs and tokens below its true high-water mark (CodeRabbit on
// PR #1331, reproduced). A v2 mark must name the identity being restored —
// Snapshot always writes the two together, so a mismatch is tampering — while a
// v1 mark has no UUID and is a floor whatever it names, as a live floor is. The
// decoded version decides, not the UUID alone, so a value that reads clean yet
// is neither a v1 mark nor a matching v2 mark refuses rather than passing as a
// zero floor. A reservation the manifest does not list is a genuine absence (a
// snapshot from before reservations, or one an operator stripped), handled by
// the caller's unmarked path. Live reservations never come here: every
// decodable live floor covers the box, whatever UUID or supported version it
// names.
func snapshotReservationFloor(snapshotDir, id string, m Manifest) (engine.Reservation, bool, error) {
	if !slices.Contains(m.Files, engine.ReservationFileName) {
		return engine.Reservation{}, false, nil
	}
	r, err := engine.ReadReservation(filepath.Join(snapshotDir, engine.ReservationFileName))
	if err != nil {
		return engine.Reservation{}, false, fmt.Errorf("restore: the snapshot lists %s but it cannot be read: %w; nothing was restored; %s", engine.ReservationFileName, err, snapshotIdentityRemedy())
	}
	switch {
	case r.Version == 1:
		return r, true, nil
	case r.Version == 2 && r.UUID == id:
		return r, true, nil
	default:
		return engine.Reservation{}, false, fmt.Errorf("restore: the snapshot's %s names player %q, not the identity %s being restored; nothing was restored; %s", engine.ReservationFileName, r.UUID, id, snapshotIdentityRemedy())
	}
}

// restorePlayerReservation preflights both counters, draws once, then makes
// the new floor durable before the caller writes any identity file.
func restorePlayerReservation(snapshotDir, target, id string, m Manifest, opts RestoreOptions) error {
	snap, marked, err := snapshotReservationFloor(snapshotDir, id, m)
	if err != nil {
		return err
	}
	dataDir := filepath.Dir(target)
	livePath := filepath.Join(dataDir, engine.ReservationFileName)
	live, err := engine.ReadReservation(livePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("restore: read the box's player reservation: %w; %s; nothing was restored", err, playerResetAdvice(true))
	}
	// Test each source separately so both remedies are named if both are
	// exhausted. This bound is independent of the random draw.
	var exhausted []error
	const limit = math.MaxUint32 - 3*restoreMargin
	for _, c := range []struct {
		name           string
		snapshot, live uint32
	}{
		{"IDs", snap.IDs, live.IDs}, {"tokens", snap.Tokens, live.Tokens},
	} {
		if uint64(c.snapshot) > limit {
			exhausted = append(exhausted, fmt.Errorf("snapshot's player %s mark leaves no room for a restore: remove player-uuid and %s from the snapshot to restore the rest; the box keeps or makes its own identity", c.name, engine.ReservationFileName))
		}
		if uint64(c.live) > limit {
			exhausted = append(exhausted, fmt.Errorf("box's player %s mark leaves no room for a restore: %s", c.name, playerResetAdvice(true)))
		}
	}
	if err := errors.Join(exhausted...); err != nil {
		return fmt.Errorf("%w; nothing was restored", err)
	}
	var draw [3]byte
	if _, err := io.ReadFull(opts.randomSource(), draw[:]); err != nil {
		return fmt.Errorf("restore: draw the player reservation jump: %w; nothing was restored", err)
	}
	jump := restoreMargin + (uint64(draw[0])<<16 | uint64(draw[1])<<8 | uint64(draw[2]))
	want := engine.Reservation{UUID: id,
		IDs:    uint32(uint64(max(snap.IDs, live.IDs)) + jump),
		Tokens: uint32(uint64(max(snap.Tokens, live.Tokens)) + jump),
	}
	if opts.Warn != nil && (!marked || time.Since(m.CreatedAt) > 24*time.Hour) {
		reason := "is older than a day"
		if !marked {
			reason = "carries no player reservation mark"
			if time.Since(m.CreatedAt) > 24*time.Hour {
				reason += " and is older than a day"
			}
		}
		opts.Warn("WARNING: this snapshot " + reason + "; restore the box's newest snapshot when possible to keep player IDs and tokens farther from earlier restores.")
	}
	if err := fsutil.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := engine.WriteReservation(dataDir, livePath, want); err != nil {
		return fmt.Errorf("restore player reservation: %w", err)
	}
	return nil
}

// randomSource keeps the production default explicit and testable without a
// probabilistic assertion about two random outputs happening to differ.
func (o RestoreOptions) randomSource() io.Reader {
	if o.Random != nil {
		return o.Random
	}
	return rand.Reader
}

// discardReplacedPlayerQueue removes the destination's queue only when its
// identity changes, and answers whether it removed one: none when the
// identity stays, or when there was no queue to remove (a box whose player
// kept none, or an interrupted attempt that already removed it). The caller
// holds the data-dir lock and has already made the raised reservation
// durable. ProbeDurable actually fsyncs the directory chain: the second call
// commits the unlink, even when a previous interrupted attempt already
// removed the file. A failed barrier must stop before UUID.
func discardReplacedPlayerQueue(target, id string) (discarded bool, err error) {
	old, err := config.ReadPlayerUUIDFile(target)
	if err == nil && old == id {
		return false, nil
	}
	dir := filepath.Dir(target)
	path := filepath.Join(dir, "player-queue.json")
	if err := atomicwrite.ProbeDurable(dir, path); err != nil {
		return false, fmt.Errorf("restore: check player queue discard: %w", err)
	}
	switch err := os.Remove(path); {
	case err == nil:
		discarded = true
	case !errors.Is(err, os.ErrNotExist):
		return false, fmt.Errorf("restore: discard previous player's queue: %w", err)
	}
	if err := atomicwrite.ProbeDurable(dir, path); err != nil {
		return false, fmt.Errorf("restore: make player queue discard durable: %w", err)
	}
	return discarded, nil
}
