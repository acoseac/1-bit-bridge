package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/uuid"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// PlayerHistoryFileName records identities whose floors the box has kept or
// given up. It must survive a player reset and travel with every backup.
const PlayerHistoryFileName = "player-history.json"

const maxPlayerHistoryBytes = 1 << 20

// playerHistory is versioned independently of the snapshot manifest. Covered
// identities share the live floor; GivenUp identities must never be restored.
// ResetPending makes the removal sequence resumable even after its UUID is gone.
type playerHistory struct {
	Version      int      `json:"version"`
	Covered      []string `json:"covered"`
	GivenUp      []string `json:"givenUp"`
	ResetPending bool     `json:"resetPending,omitempty"`
}

func readPlayerHistory(path string) (playerHistory, error) {
	h := playerHistory{Version: 1}
	f, _, err := fsutil.OpenAsFile(path)
	if err != nil {
		return h, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPlayerHistoryBytes+1))
	if err != nil {
		return h, err
	}
	if len(b) > maxPlayerHistoryBytes {
		return h, errors.New("player history exceeds its size limit")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	h.Version = 0
	if err := d.Decode(&h); err != nil {
		return h, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return h, errors.New("player history has trailing data")
	}
	if err := h.validate(); err != nil {
		return h, err
	}
	return h, nil
}

func (h playerHistory) validate() error {
	if h.Version != 1 {
		return errors.New("unsupported player history version")
	}
	for _, ids := range [][]string{h.Covered, h.GivenUp} {
		for _, id := range ids {
			u, err := uuid.Parse(id)
			if err != nil || u.String() != id {
				return errors.New("player history does not name lowercase UUIDs")
			}
		}
	}
	return nil
}

func optionalPlayerHistory(path string) (playerHistory, bool, error) {
	h, err := readPlayerHistory(path)
	if errors.Is(err, os.ErrNotExist) {
		return playerHistory{Version: 1}, false, nil
	}
	if err != nil {
		return h, false, fmt.Errorf("read %s: %w", PlayerHistoryFileName, err)
	}
	return h, true, nil
}

// writePlayerHistory is the only writer, including snapshot copies. The strict
// writer keeps the owner and makes the rename durable before an identity or
// its floor may be removed or replaced.
func writePlayerHistory(dir string, h playerHistory) error {
	if err := h.validate(); err != nil {
		return err
	}
	h.Covered = unionIDs(nil, h.Covered...)
	h.GivenUp = unionIDs(nil, h.GivenUp...)
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(b) > maxPlayerHistoryBytes {
		return errors.New("player history exceeds its size limit")
	}
	return atomicwrite.WriteBytesDurable(dir, filepath.Join(dir, PlayerHistoryFileName), b, ".player-history-*.tmp")
}

func unionIDs(ids []string, more ...string) []string {
	ids = slices.Clone(ids)
	for _, id := range more {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// preflightPlayerHistory reads both lists and refuses a given-up identity
// before restore writes anything, including a raised reservation. The old
// reservation's UUID is remembered too: an interrupted restore can leave it
// naming an identity other than the current UUID.
func preflightPlayerHistory(snapshotDir, target, id string, m Manifest) (playerHistory, bool, error) {
	dir := filepath.Dir(target)
	h, present, err := optionalPlayerHistory(filepath.Join(dir, PlayerHistoryFileName))
	if err != nil {
		return h, false, err
	}
	if slices.Contains(m.Files, PlayerHistoryFileName) {
		sh, err := readPlayerHistory(filepath.Join(snapshotDir, PlayerHistoryFileName))
		if err != nil {
			return h, false, fmt.Errorf("restore: read snapshot %s: %w", PlayerHistoryFileName, err)
		}
		h.Covered = unionIDs(h.Covered, sh.Covered...)
		h.GivenUp = unionIDs(h.GivenUp, sh.GivenUp...)
		// A snapshot of an interrupted reset does not instruct a different box
		// to remove its files. Its given-up list is sufficient to refuse revival.
		present = true
	}
	if id != "" && slices.Contains(h.GivenUp, id) {
		return h, false, fmt.Errorf("restore: the snapshot's player identity was given up; nothing was restored; %s", snapshotIdentityRemedy())
	}
	if h.ResetPending {
		return h, false, errors.New("restore: player reset is incomplete; run bridge output player reset before restoring; nothing was restored")
	}
	if id != "" {
		old, err := config.ReadPlayerUUIDFile(target)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return h, false, fmt.Errorf("restore: read live player identity: %w", err)
		}
		if old != "" && old != id {
			h.Covered = unionIDs(h.Covered, old)
			present = true
		}
		r, err := engine.ReadReservation(filepath.Join(dir, engine.ReservationFileName))
		if err == nil && r.UUID != "" && r.UUID != id {
			h.Covered = unionIDs(h.Covered, r.UUID)
			present = true
		}
		h.Covered = slices.DeleteFunc(h.Covered, func(covered string) bool { return covered == id })
		// Reservation failures retain restorePlayerReservation's existing remedy.
	}
	return h, present, nil
}

// ResetPlayer gives up the live identity and every identity its floor covers,
// then durably removes UUID, queue, reservation in that order. The caller must
// hold the data-dir lock and refuse a running serve, as restore does. It returns
// false without writing when no identity or interrupted reset exists.
func ResetPlayer(dataDir string) (bool, error) { return resetPlayer(dataDir, nil) }

// resetPlayer's instance seam stops after a durable step, never on a timer.
func resetPlayer(dir string, after func(string) error) (bool, error) {
	h, _, err := optionalPlayerHistory(filepath.Join(dir, PlayerHistoryFileName))
	if err != nil {
		return false, err
	}
	id, err := config.ReadPlayerUUID(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if id == "" && !h.ResetPending {
		return false, nil
	}
	r, err := engine.ReadReservation(filepath.Join(dir, engine.ReservationFileName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A malformed floor is one reason to reset. Access errors are different:
		// we cannot know which identity an unreadable file names.
		var pe *os.PathError
		if errors.As(err, &pe) {
			return false, fmt.Errorf("read player reservation before reset: %w", err)
		}
	}
	// A refused reservation can still name a valid UUID (for example a
	// newer version). Keep that name; malformed UUID text is no identity.
	if u, err := uuid.Parse(r.UUID); err != nil || u.String() != r.UUID {
		r.UUID = ""
	}
	h.GivenUp = unionIDs(h.GivenUp, h.Covered...)
	h.GivenUp = unionIDs(h.GivenUp, id, r.UUID)
	h.Covered = nil
	h.ResetPending = true
	if err := writePlayerHistory(dir, h); err != nil {
		return false, err
	}
	if after != nil {
		if err := after(PlayerHistoryFileName); err != nil {
			return false, err
		}
	}
	for _, name := range []string{config.PlayerUUIDFile, "player-queue.json", engine.ReservationFileName} {
		path := filepath.Join(dir, name)
		if err := atomicwrite.ProbeDurable(dir, path); err != nil {
			return false, err
		}
		var removeErr error
		if name == config.PlayerUUIDFile {
			removeErr = config.ForgetPlayerUUID(dir, id)
		} else {
			removeErr = os.Remove(path)
		}
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return false, removeErr
		}
		if err := atomicwrite.ProbeDurable(dir, path); err != nil {
			return false, err
		}
		if after != nil {
			if err := after(name); err != nil {
				return false, err
			}
		}
	}
	h.ResetPending = false
	if err := writePlayerHistory(dir, h); err != nil {
		return false, err
	}
	return true, nil
}
