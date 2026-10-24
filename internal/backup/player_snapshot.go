package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// snapshotPlayerCheck brackets the capture without taking serve's exclusive
// lock: scheduled backups already run inside serve. History is read first and
// checked last; the captured UUID and the reservation's original UUID are
// checked before that. Reset and restore write history before changing either.
// A running engine may advance its floor; only the reservation's identity and
// presence must stay fixed. No manifest is published on a failed check.
type snapshotPlayerCheck struct {
	history            playerHistory
	historyPresent     bool
	uuid               string
	uuidInvalid        bool
	reservationUUID    string
	reservationPresent bool
}

func (c snapshotPlayerCheck) verify(ctx context.Context, src Sources) error {
	changed := false
	if src.PlayerUUID != "" {
		if c.uuid != "" && !c.uuidInvalid {
			var r engine.Reservation
			err := atomicwrite.ReadWithRetryCtx(ctx, func() error {
				var err error
				r, err = engine.ReadReservation(filepath.Join(filepath.Dir(src.PlayerUUID), engine.ReservationFileName))
				return err
			})
			present := !errors.Is(err, os.ErrNotExist)
			if err != nil && present {
				return fmt.Errorf("backup: recheck player reservation: %w; run the backup again", err)
			}
			changed = present != c.reservationPresent || r.UUID != c.reservationUUID
		}
		var id string
		err := atomicwrite.ReadWithRetryCtx(ctx, func() error {
			var err error
			id, err = config.ReadPlayerUUIDFile(src.PlayerUUID)
			return err
		})
		invalid := errors.Is(err, config.ErrNotOnePlayerUUID)
		if err != nil && !invalid && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup: recheck player identity: %w; run the backup again", err)
		}
		changed = changed || id != c.uuid || invalid != c.uuidInvalid
	}
	h, present, err := optionalPlayerHistory(filepath.Join(src.DataDir, PlayerHistoryFileName))
	if err != nil {
		return err
	}
	if changed || present != c.historyPresent || h.ResetPending != c.history.ResetPending ||
		!slices.Equal(h.Covered, c.history.Covered) || !slices.Equal(h.GivenUp, c.history.GivenUp) {
		return errors.New("backup: the player's identity changed during the backup; run it again")
	}
	return nil
}
