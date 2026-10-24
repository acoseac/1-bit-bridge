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
// production callers leave it nil. A restored identity reads three bytes of it
// for its jump; onto a UUID-only destination, restore first adopts the box and
// that adoption reads three bytes before the jump's, six in all.
type RestoreOptions struct {
	Random io.Reader
	Warn   func(string)
}

// restoreMargin is the minimum jump and the amount of running room left
// after the largest possible jump. The random part is uniform in [0, margin).
const restoreMargin uint64 = 1 << 24

// playerReservationReadAdvice distinguishes access failures from a lost floor.
const playerReservationReadAdvice = "restore read access to player-ids.json; for an unsupported version use a newer bridge; never replace it with a snapshot's older floor"

// playerResetAdvice names the explicit escape from a lost or exhausted floor.
func playerResetAdvice(restoring bool) string {
	advice := "with the service stopped, run bridge reset-player --discard-floor; this seals all player identity restores on this box and the next start makes a new identity with no queue"
	if restoring {
		advice = snapshotIdentityRemedy() + "; or " + advice
	}
	return advice
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
// snapshot from before reservations, or one an operator stripped), which
// floorlessSnapshotBound bounds or refuses. Live reservations never come here:
// every decodable live floor covers the box, whatever UUID or supported
// version it names.
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

// floorlessSnapshotCeiling bounds an identity whose snapshot, from a writer
// before reservation capture, lists no floor: its counter space below 2^31,
// the documented upgrade boundary, plus an adoption's random part below 2^24
// when its source upgraded after the snapshot. Restore adds its jump on top.
const floorlessSnapshotCeiling = 1<<31 + restoreMargin

// floorlessSnapshotBound is the snapshot bound restorePlayerReservation takes
// after snapshotReservationFloor, and its one treatment of a snapshot whose
// manifest lists no floor (!marked), which cannot bound what its identity
// handed out. This bridge lists the floor beside every UUID it captures and
// says so in the manifest (Manifest.PlayerFloorRequired), so such a snapshot
// of its making was stripped: restore refuses it. One from an older writer
// takes the adoption ceiling. marked stays the caller's, so the missing-mark
// warning still fires; a listed floor that will not read never comes here.
func floorlessSnapshotBound(snap engine.Reservation, marked bool, m Manifest) (engine.Reservation, error) {
	if marked {
		return snap, nil
	}
	if m.PlayerFloorRequired {
		return engine.Reservation{}, fmt.Errorf("restore: the snapshot holds %s without %s, which the bridge that wrote it lists beside every player UUID; nothing was restored; %s", PlayerUUIDFileName, engine.ReservationFileName, snapshotIdentityRemedy())
	}
	return engine.Reservation{IDs: uint32(floorlessSnapshotCeiling), Tokens: uint32(floorlessSnapshotCeiling)}, nil
}

// restorePlayerReservation preflights both counters, draws its jump (after a
// UUID-only destination's adoption has drawn first), then makes the new
// floor durable before the caller writes any identity file.
func restorePlayerReservation(snapshotDir, target, id string, m Manifest, opts RestoreOptions) error {
	snap, marked, err := snapshotReservationFloor(snapshotDir, id, m)
	if err != nil {
		return err
	}
	if snap, err = floorlessSnapshotBound(snap, marked, m); err != nil {
		return err
	}
	dataDir := filepath.Dir(target)
	livePath := filepath.Join(dataDir, engine.ReservationFileName)
	sealed, sealErr := playerRestoresSealed(dataDir)
	if sealed || sealErr != nil {
		return fmt.Errorf("restore: player identity restores are sealed by the local discard records; nothing was restored; %s", snapshotIdentityRemedy())
	}
	// A UUID-only destination has unknown main-era history. Establish the
	// same durable adoption floor as start before computing max(snapshot, live).
	// Restore's caller already holds server.lock. The adoption draws from
	// restore's own source, before the jump does.
	if err := adoptPlayerState(dataDir, opts.randomSource()); err != nil {
		return fmt.Errorf("restore: adopt destination player: %w; nothing was restored", err)
	}
	live, present, err := readPlayerFloor(dataDir)
	if err != nil {
		return fmt.Errorf("restore: %w; %s; nothing was restored", err, playerResetAdvice(true))
	}
	if err := checkPlayerFloorLoss(dataDir, present); err != nil {
		return fmt.Errorf("restore: %w; %s; nothing was restored", err, playerResetAdvice(true))
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
	want, err := jumpedPlayerReservation(id, snap, live, opts.randomSource())
	if err != nil {
		return fmt.Errorf("restore: draw the player reservation jump: %w; nothing was restored", err)
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
	return ensurePlayerMarker(dataDir, playerInitialized)
}

// jumpedPlayerReservation applies restore's migration margin to both counters.
// Callers must first check that their floors leave room for the largest jump.
func jumpedPlayerReservation(id string, snap, live engine.Reservation, random io.Reader) (engine.Reservation, error) {
	var draw [3]byte
	if _, err := io.ReadFull(random, draw[:]); err != nil {
		return engine.Reservation{}, err
	}
	jump := restoreMargin + (uint64(draw[0])<<16 | uint64(draw[1])<<8 | uint64(draw[2]))
	return engine.Reservation{UUID: id,
		IDs:    uint32(uint64(max(snap.IDs, live.IDs)) + jump),
		Tokens: uint32(uint64(max(snap.Tokens, live.Tokens)) + jump),
	}, nil
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
// identity changes. The caller holds the data-dir lock and has already made
// the raised reservation durable. ProbeDurable actually fsyncs the directory
// chain: the second call commits the unlink, even when a previous interrupted
// attempt already removed the file. A failed barrier must stop before UUID.
func discardReplacedPlayerQueue(target, id string) error {
	old, err := config.ReadPlayerUUIDFile(target)
	if err == nil && old == id {
		return nil
	}
	dir := filepath.Dir(target)
	path := filepath.Join(dir, "player-queue.json")
	if err := atomicwrite.ProbeDurable(dir, path); err != nil {
		return fmt.Errorf("restore: check player queue discard: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("restore: discard previous player's queue: %w", err)
	}
	if err := atomicwrite.ProbeDurable(dir, path); err != nil {
		return fmt.Errorf("restore: make player queue discard durable: %w", err)
	}
	return nil
}
