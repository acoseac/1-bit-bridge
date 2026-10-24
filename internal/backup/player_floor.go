package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/google/uuid"
)

const playerInitialized = "player-initialized"
const playerFloorDiscarded = "player-floor-discarded.json"

// playerEntry decides presence by the directory entry, including dangling links.
func playerEntry(dir, name string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// readPlayerMarker is the single strict reader of the two local lifecycle markers.
func readPlayerMarker(dir, name string) (bool, error) {
	present, err := playerEntry(dir, name)
	if err != nil || !present {
		return present, err
	}
	f, _, err := fsutil.OpenAsFile(filepath.Join(dir, name))
	if err != nil {
		return true, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil {
		return true, err
	}
	var m struct {
		Version int `json:"version"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if len(b) > 128 || d.Decode(&m) != nil || m.Version != 1 || d.Decode(new(any)) != io.EOF {
		return true, fmt.Errorf("%s is not a supported player marker; restore the original marker or use a newer bridge", name)
	}
	return true, nil
}

// ensurePlayerMarker writes each local marker once and reestablishes its barrier
// on retry. The caller holds the exclusive data-directory lock.
func ensurePlayerMarker(dir, name string) error {
	present, err := readPlayerMarker(dir, name)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if present {
		return atomicwrite.ProbeDurable(dir, path)
	}
	return atomicwrite.WriteBytesDurable(dir, path, []byte("{\"version\":1}\n"), ".player-marker-*.tmp")
}

func readPlayerFloor(dir string) (engine.Reservation, bool, error) {
	present, err := playerEntry(dir, engine.ReservationFileName)
	if err != nil {
		return engine.Reservation{}, present, floorReadError(err)
	}
	if !present {
		return engine.Reservation{}, false, nil
	}
	r, err := engine.ReadReservation(filepath.Join(dir, engine.ReservationFileName))
	if err != nil {
		return r, true, floorReadError(err)
	}
	return r, true, nil
}

func floorReadError(err error) error {
	if kind := fsutil.NotAFileKind(err); kind != "" {
		return fmt.Errorf("player-ids.json is a %s, not a file: move it aside and restore the original floor, or run bridge reset-player --discard-floor: %w", kind, err)
	}
	return fmt.Errorf("read player-ids.json: %w; %s; or run bridge reset-player --discard-floor", err, playerReservationReadAdvice)
}

func playerUUIDReadError(err error) error {
	if kind := fsutil.NotAFileKind(err); kind != "" {
		return fmt.Errorf("player-uuid is a %s, not a file: move it aside: %w", kind, err)
	}
	if errors.Is(err, config.ErrNotOnePlayerUUID) {
		return fmt.Errorf("%w; run bridge reset-player", err)
	}
	return fmt.Errorf("%w; restore read access to player-uuid", err)
}

// checkPlayerFloorLoss distinguishes a new box from lost local allocation state.
// A valid discard seal permits a fresh floor only after UUID and queue are gone.
func checkPlayerFloorLoss(dir string, present bool) error {
	marked, err := readPlayerMarker(dir, playerInitialized)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	id, err := playerEntry(dir, config.PlayerUUIDFile)
	if err != nil {
		return err
	}
	queue, err := playerEntry(dir, "player-queue.json")
	if err != nil {
		return err
	}
	sealed, err := readPlayerMarker(dir, playerFloorDiscarded)
	if err != nil {
		return err
	}
	if id || queue || marked && !sealed {
		return errors.New("player floor is missing; with the service stopped, run bridge reset-player --discard-floor")
	}
	return nil
}

// CheckPlayerStart performs the read-only preflight shared by serve and bind.
func CheckPlayerStart(dir string) error {
	_, present, err := readPlayerFloor(dir)
	if err != nil {
		return err
	}
	idPresent, err := playerEntry(dir, config.PlayerUUIDFile)
	if err != nil {
		return err
	}
	queue, err := playerEntry(dir, "player-queue.json")
	if err != nil {
		return err
	}
	if queue {
		f, _, err := fsutil.OpenAsFile(filepath.Join(dir, "player-queue.json"))
		if err != nil {
			if kind := fsutil.NotAFileKind(err); kind != "" {
				return fmt.Errorf("player-queue.json is a %s, not a file: move it aside: %w", kind, err)
			}
			return fmt.Errorf("player-queue.json cannot be read: %w; restore read access", err)
		}
		_ = f.Close()
	}
	if !idPresent && queue {
		return errors.New("player-uuid is missing beside a queue; run bridge reset-player")
	}
	if idPresent {
		_, err := config.ReadPlayerUUID(dir)
		if err != nil {
			return playerUUIDReadError(err)
		}
	}
	if err := checkPlayerFloorLoss(dir, present); err != nil {
		return err
	}
	return nil
}

// EnsurePlayerUUID is the sole production minter. Floor, marker and UUID commit
// in that order, with the retained floor merged across every UUID the box runs.
func EnsurePlayerUUID(dir string) (string, bool, error) {
	if err := CheckPlayerStart(dir); err != nil {
		return "", false, err
	}
	id, readErr := config.ReadPlayerUUID(dir)
	if readErr == nil {
		if err := ensurePlayerMarker(dir, playerInitialized); err != nil {
			return "", false, err
		}
		return id, false, nil
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		return "", false, playerUUIDReadError(readErr)
	}
	u, err := uuid.NewRandom()
	if err != nil {
		return "", false, err
	}
	if err := fsutil.MkdirAll(dir, 0700); err != nil {
		return "", false, err
	}
	if err := engine.WriteReservation(dir, filepath.Join(dir, engine.ReservationFileName), engine.Reservation{UUID: u.String()}); err != nil {
		return "", false, err
	}
	if err := ensurePlayerMarker(dir, playerInitialized); err != nil {
		return "", false, err
	}
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), u.String()); err != nil {
		return "", false, err
	}
	return u.String(), true, nil
}

// ResetPlayer clears queue and UUID while retaining the box's floor byte-for-byte.
// The caller holds the exclusive data-dir lock and excludes a running service.
func ResetPlayer(dir string) (bool, error) { return resetPlayer(dir, false, nil) }

// DiscardPlayerFloor seals identity restores before clearing the player and
// renaming its unknown or exhausted floor aside. A retry completes every step.
func DiscardPlayerFloor(dir string) (bool, error) { return resetPlayer(dir, true, nil) }

func resetPlayer(dir string, discard bool, after func(string) error) (bool, error) {
	step := func(name string) error {
		if after != nil {
			return after(name)
		}
		return nil
	}
	var floorPresent bool
	if !discard {
		_, present, err := readPlayerFloor(dir)
		if err != nil {
			return false, err
		}
		if err := checkPlayerFloorLoss(dir, present); err != nil {
			return false, err
		}
		floorPresent = present
	}
	// A UUID directory is never removed, even when empty. Reset does not read UUID bytes.
	for _, name := range []string{config.PlayerUUIDFile, "player-queue.json"} {
		st, err := os.Lstat(filepath.Join(dir, name))
		if err == nil && st.IsDir() {
			return false, fmt.Errorf("%s is a directory, not a file: move it aside", name)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if discard {
		if err := fsutil.MkdirAll(dir, 0700); err != nil {
			return false, err
		}
		if err := ensurePlayerMarker(dir, playerFloorDiscarded); err != nil {
			return false, err
		}
		if err := step(playerFloorDiscarded); err != nil {
			return false, err
		}
	} else {
		if !floorPresent {
			return false, nil
		}
		if err := ensurePlayerMarker(dir, playerInitialized); err != nil {
			return false, err
		}
	}
	for _, name := range []string{"player-queue.json", config.PlayerUUIDFile} {
		// Probe a sibling to permit unlinking a dangling link or named pipe itself.
		barrier := filepath.Join(dir, ".player-reset-barrier")
		if err := atomicwrite.ProbeDurable(dir, barrier); err != nil {
			return false, err
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err := atomicwrite.ProbeDurable(dir, barrier); err != nil {
			return false, err
		}
		if err := step(name); err != nil {
			return false, err
		}
	}
	if discard {
		path := filepath.Join(dir, engine.ReservationFileName)
		present, err := playerEntry(dir, engine.ReservationFileName)
		if err != nil {
			return false, err
		}
		if present {
			suffix, err := uuid.NewRandom()
			if err != nil {
				return false, err
			}
			if err := atomicwrite.RenameWithRetry(path, path+".discarded-"+suffix.String()); err != nil {
				return false, err
			}
		}
		if err := atomicwrite.ProbeDurable(dir, filepath.Join(dir, ".player-reset-barrier")); err != nil {
			return false, err
		}
		if err := step(engine.ReservationFileName); err != nil {
			return false, err
		}
	}
	return true, nil
}
