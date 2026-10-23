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
}

// restoreMargin is the minimum jump and the amount of running room left
// after the largest possible jump. The random part is uniform in [0, margin).
const restoreMargin uint64 = 1 << 24

// playerResetAdvice is the single remedy for giving up an exhausted or
// unreadable live floor. With the service stopped, remove player-uuid,
// player-ids.json and player-queue.json together; the next start makes a new
// identity with no queue (a v1 queue has no identity stamp). The reset gives up
// for good the identity it removes and any the box ran earlier that no other
// box took over, including one an earlier restore replaced here: only the
// dropped floor held their values. No snapshot of a given-up identity taken
// before the reset, here or elsewhere, may restore its player identity again:
// remove player-uuid and player-ids.json from every such snapshot, or delete it.
// An identity another box took over by restore keeps its floor there and its
// snapshots stay valid. Only a restore refusal adds the step to strip the two
// files from a copy of that snapshot and restore that copy. A snapshot's
// player-ids.json is never a repair; never drop a reservation and keep its
// identity. B524 will enforce this operator rule before 5a ships.
func playerResetAdvice(restoring bool) string {
	advice := fmt.Sprintf("with the service stopped, reset the player: remove the box's %s, %s and player-queue.json together", PlayerUUIDFileName, engine.ReservationFileName)
	advice += "; this reset gives up for good the identity it removes and any the box ran earlier that no other box took over; no snapshot of one of them taken before the reset, on this box or another, may restore its player identity again; remove player-uuid and player-ids.json from every such snapshot, or delete those snapshots; an identity another box took over keeps its snapshots"
	if restoring {
		advice += fmt.Sprintf("; remove %s and %s from a copy of the snapshot as well, then restore that copy", PlayerUUIDFileName, engine.ReservationFileName)
	}
	return advice + "; the next start makes a new identity with no queue, a new device to controllers; a snapshot's player-ids.json is never a valid repair; do not delete the reservation while keeping the player identity"
}

// matchingReservation reads the snapshot's already captured and restamped
// mark. Live reservations must not go through this filter: every decodable
// live floor covers the box, whatever UUID or supported version it names.
func matchingReservation(path, id string) (engine.Reservation, bool) {
	r, err := engine.ReadReservation(path)
	if err != nil || r.UUID != id {
		return engine.Reservation{}, false
	}
	return r, true
}

// restorePlayerReservation preflights both counters, draws once, then makes
// the new floor durable before the caller writes any identity file.
func restorePlayerReservation(snapshotDir, target, id string, m Manifest, opts RestoreOptions) error {
	var snap engine.Reservation
	marked := false
	if slices.Contains(m.Files, engine.ReservationFileName) {
		snap, marked = matchingReservation(filepath.Join(snapshotDir, engine.ReservationFileName), id)
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
