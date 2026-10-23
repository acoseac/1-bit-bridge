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

// matchingReservation takes a snapshot mark only when it names its identity.
// A missing, unreadable, v1 or foreign reservation is no snapshot mark.
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
		return fmt.Errorf("restore: read the box's player reservation: %w; nothing was restored", err)
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
			exhausted = append(exhausted, fmt.Errorf("box's player %s mark leaves no room for a restore: remove the box's player-uuid and %s, then restore again; controllers see a new device when the box makes its own identity", c.name, engine.ReservationFileName))
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
