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
	"github.com/acoseac/1-bit-bridge/internal/logging"
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

// playerMarker treats damaged entries conservatively as present. Only a whole,
// newer version can require a newer bridge. A discard is recorded independently
// in both local markers before any allocation state is removed.
type playerMarker struct {
	Version int  `json:"version"`
	Sealed  bool `json:"sealed,omitempty"`
	Minted  bool `json:"minted,omitempty"`
}

func playerMarkerState(dir, name string) (present, valid bool, m playerMarker, err error) {
	st, err := os.Lstat(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, false, m, nil
	}
	if err != nil {
		return false, false, m, err
	}
	if !st.Mode().IsRegular() {
		warnDamagedPlayerMarker(name)
		return true, false, m, nil
	}
	f, _, err := fsutil.OpenAsFile(filepath.Join(dir, name))
	if err != nil {
		return true, false, m, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil {
		return true, false, m, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if len(b) > 128 || d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF {
		warnDamagedPlayerMarker(name)
		return true, false, playerMarker{}, nil
	}
	if m.Version > 1 {
		return true, false, m, fmt.Errorf("%s has a newer version; use a newer bridge", name)
	}
	if m.Version != 1 {
		warnDamagedPlayerMarker(name)
	}
	return true, m.Version == 1, m, nil
}

// warnDamagedPlayerMarker keeps marker reads independent of UUID bytes (reset
// must be able to remove an unreadable UUID). Floor-loss errors still refuse
// the operation; moving this evidence aside is safe only with a matching floor.
func warnDamagedPlayerMarker(name string) {
	if name != playerInitialized {
		return
	}
	logging.Component("player").Warn("player initialization marker is damaged", "file", name,
		"remedy", "with a readable player-ids.json naming the current player-uuid, move it aside; start re-ensures it, and beside a valid seal completion records sealed and minted")
}

func readPlayerMarker(dir, name string) (bool, error) {
	present, _, _, err := playerMarkerState(dir, name)
	return present, err
}

// playerRestoresSealed reads independent evidence; a damaged seal is still a
// seal. The initialized marker's sealed bit survives loss of the seal file.
func playerRestoresSealed(dir string) (bool, error) {
	sealed, err := readPlayerMarker(dir, playerFloorDiscarded)
	if sealed || err != nil {
		return sealed, err
	}
	_, _, m, err := playerMarkerState(dir, playerInitialized)
	return m.Sealed, err
}

// ensurePlayerMarker preserves existing bytes and reestablishes their barrier.
func ensurePlayerMarker(dir, name string) error {
	present, err := readPlayerMarker(dir, name)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if present {
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			if name == playerInitialized {
				// Preserve non-file evidence without opening or following it.
				return atomicwrite.ProbeDurable(dir, filepath.Join(dir, ".player-marker-barrier"))
			}
			return fmt.Errorf("%s is not a regular file: move it aside", name)
		}
		return atomicwrite.ProbeDurable(dir, path)
	}
	return atomicwrite.WriteBytesDurable(dir, path, []byte("{\"version\":1}\n"), ".player-marker-*.tmp")
}

// sealPlayerMarker repairs damaged markers only in explicit discard. Renaming
// a bad entry preserves it for inspection and never opens a FIFO or symlink.
func sealPlayerMarker(dir, name string) error {
	present, valid, m, err := playerMarkerState(dir, name)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if present && !valid {
		suffix, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		if err := atomicwrite.RenameWithRetry(path, path+".discarded-"+suffix.String()); err != nil {
			return err
		}
	}
	if valid && (name == playerFloorDiscarded || m.Sealed) {
		return ensurePlayerMarker(dir, name)
	}
	return atomicwrite.WriteBytesDurable(dir, path, []byte("{\"version\":1,\"sealed\":true}\n"), ".player-marker-*.tmp")
}

// completePlayerSeal closes an interrupted or older discard's second record.
// The caller holds server.lock. Recheck the first record's durability before
// adding independent evidence; never turn a consumed exemption back on.
func completePlayerSeal(dir string) error {
	sealed, err := readPlayerMarker(dir, playerFloorDiscarded)
	if err != nil {
		return err
	}
	present, valid, m, err := playerMarkerState(dir, playerInitialized)
	if err != nil || !sealed && !m.Sealed {
		return err
	}
	if valid && m.Sealed && m.Minted {
		return nil
	}
	if sealed {
		if err := ensurePlayerMarker(dir, playerFloorDiscarded); err != nil {
			return err
		}
	}
	// A damaged second record still counts as present. Keep it for the
	// floor-loss checks; a matching readable floor still permits start.
	if present && !valid {
		return nil
	}
	evidence, err := playerAllocationEvidence(dir)
	if err != nil {
		return err
	}
	if m.Sealed && !evidence {
		return nil
	}
	m.Minted = m.Minted || evidence
	m.Version, m.Sealed = 1, true
	return writePlayerMarker(dir, m)
}

// playerAllocationEvidence spends a sealed marker's one-shot exemption even
// when an older writer left no minted bit. Entry presence alone is evidence.
func playerAllocationEvidence(dir string) (bool, error) {
	for _, name := range []string{engine.ReservationFileName, config.PlayerUUIDFile, "player-queue.json"} {
		present, err := playerEntry(dir, name)
		if err != nil || present {
			return present, err
		}
	}
	return false, nil
}

func writePlayerMarker(dir string, m playerMarker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicwrite.WriteBytesDurable(dir, filepath.Join(dir, playerInitialized), append(b, '\n'), ".player-marker-*.tmp")
}

// markPlayerMinted consumes a discard's exemption after the floor is durable
// and before the UUID is published. Ordinary initialization preserves bytes.
func markPlayerMinted(dir string) error {
	_, valid, m, err := playerMarkerState(dir, playerInitialized)
	if err != nil {
		return err
	}
	if valid && m.Sealed && !m.Minted {
		m.Minted = true
		return writePlayerMarker(dir, m)
	}
	return ensurePlayerMarker(dir, playerInitialized)
}

// AdoptPlayerState completes local discard evidence and adopts a UUID-only
// upgrade, without minting an identity on a pristine box. The caller holds the
// exclusive server.lock, even when the player is disabled.
func AdoptPlayerState(dir string) error {
	if err := completePlayerSeal(dir); err != nil {
		return err
	}
	adopt, err := unstartedPlayer(dir)
	if err != nil || !adopt {
		return err
	}
	_, _, err = EnsurePlayerUUID(dir)
	return err
}

// completeCapturedPlayerSeal serializes a backup's local marker upgrade with
// every lifecycle writer. A normal live backup needs no exclusive lock.
func completeCapturedPlayerSeal(dir string) error {
	sealed, err := readPlayerMarker(dir, playerFloorDiscarded)
	if err != nil {
		return err
	}
	present, valid, m, err := playerMarkerState(dir, playerInitialized)
	if err != nil || !sealed && !m.Sealed || valid && m.Sealed && m.Minted {
		return err
	}
	if present && !valid {
		if sealed {
			return ensurePlayerMarker(dir, playerFloorDiscarded)
		}
		return nil
	}
	if m.Sealed {
		evidence, err := playerAllocationEvidence(dir)
		if err != nil || !evidence {
			return err
		}
	}

	f, err := fsutil.OpenLockFile(dir, "server.lock")
	if err != nil {
		return err
	}
	defer f.Close()
	if err := fsutil.TryLock(f); err != nil {
		return fmt.Errorf("player initialization is busy; stop the service and run the backup again: %w", err)
	}
	defer fsutil.Unlock(f)
	return completePlayerSeal(dir)
}

// unstartedPlayer recognizes a UUID-only upgrade, which can also be a
// main-era engine whose floor alone was lost. Adoption must jump, never zero.
func unstartedPlayer(dir string) (bool, error) {
	for _, name := range []string{engine.ReservationFileName, playerInitialized, "player-queue.json", playerFloorDiscarded} {
		present, err := playerEntry(dir, name)
		if err != nil || present {
			return false, err
		}
	}
	st, err := os.Lstat(filepath.Join(dir, config.PlayerUUIDFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, nil
	}
	_, err = config.ReadPlayerUUID(dir)
	return err == nil, nil
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
// A valid, unconsumed discard permits one fresh floor after UUID and queue are gone.
func checkPlayerFloorLoss(dir string, present bool) error {
	marked, valid, marker, err := playerMarkerState(dir, playerInitialized)
	if err != nil {
		return err
	}
	sealed, err := playerRestoresSealed(dir)
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
	if id && !queue && !marked && !sealed {
		unstarted, err := unstartedPlayer(dir)
		if err != nil {
			return err
		}
		if unstarted {
			return nil
		}
	}
	if id || queue || marked && !(valid && sealed && !marker.Minted) {
		return errors.New("player floor is missing; with the service stopped, run bridge reset-player --discard-floor")
	}
	return nil
}

// CheckPlayerStart performs the read-only preflight shared by serve and bind.
func CheckPlayerStart(dir string) error {
	floor, present, err := readPlayerFloor(dir)
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
	if idPresent {
		st, err := os.Lstat(filepath.Join(dir, config.PlayerUUIDFile))
		if err != nil {
			return playerUUIDReadError(err)
		}
		if !st.Mode().IsRegular() {
			kind := fsutil.NotAFile(st.Mode())
			if kind == "" {
				kind = "symbolic link or non-regular entry"
			}
			advice := "bridge reset-player"
			if !present {
				advice = "bridge reset-player --discard-floor"
			}
			return fmt.Errorf("player-uuid: %s is not a file: move it aside, then run %s", kind, advice)
		}
	}
	if err := checkPlayerFloorLoss(dir, present); err != nil {
		return err
	}

	if floor.Exhausted() {
		return fmt.Errorf("player counter floor is exhausted; %s", playerResetAdvice(false))
	}
	if !idPresent && queue {
		return errors.New("player-uuid is missing beside a queue; run bridge reset-player")
	}
	if idPresent {
		id, err := config.ReadPlayerUUID(dir)
		if err != nil {
			return playerUUIDReadError(err)
		}
		if present && floor.UUID != id {
			return errors.New("player-ids.json does not name the current player-uuid; run bridge reset-player (the counter floor is kept)")
		}
	}
	return nil
}

// EnsurePlayerUUID is the sole production minter. Floor, marker and UUID commit
// in that order, with the retained floor merged across every UUID the box runs.
func EnsurePlayerUUID(dir string) (string, bool, error) {
	if err := completePlayerSeal(dir); err != nil {
		return "", false, err
	}
	if err := CheckPlayerStart(dir); err != nil {
		return "", false, err
	}
	id, readErr := config.ReadPlayerUUID(dir)
	if readErr == nil {
		if unstarted, err := unstartedPlayer(dir); err != nil {
			return "", false, err
		} else if unstarted {
			// No old floor bounds this identity. Skip the lower half of counter
			// space, including reservations skipped at every restart. Restore's
			// smaller jump needs a known bound, which adoption does not have.
			base := engine.Reservation{IDs: 1<<31 - uint32(restoreMargin), Tokens: 1<<31 - uint32(restoreMargin)}
			want, err := jumpedPlayerReservation(id, base, base, (RestoreOptions{}).randomSource())
			if err != nil {
				return "", false, err
			}
			if err := engine.WriteReservation(dir, filepath.Join(dir, engine.ReservationFileName), want); err != nil {
				return "", false, err
			}
		}
		if err := markPlayerMinted(dir); err != nil {
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
	if err := markPlayerMinted(dir); err != nil {
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
		if err := completePlayerSeal(dir); err != nil {
			if errors.Is(err, os.ErrPermission) {
				return false, fmt.Errorf("%w; restore read access to player state; or run bridge reset-player --discard-floor", err)
			}
			return false, err
		}
		_, present, err := readPlayerFloor(dir)
		if err != nil {
			return false, err
		}
		if err := checkPlayerFloorLoss(dir, present); err != nil {
			return false, err
		}
		if !present {
			if unstarted, err := unstartedPlayer(dir); err != nil {
				return false, err
			} else if unstarted {
				if _, _, err := EnsurePlayerUUID(dir); err != nil {
					return false, err
				}
				present = true
			}
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
		if err := sealPlayerMarker(dir, playerFloorDiscarded); err != nil {
			return false, err
		}
		if err := sealPlayerMarker(dir, playerInitialized); err != nil {
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
		// Only a completed explicit discard opens the next one-shot exemption.
		if err := writePlayerMarker(dir, playerMarker{Version: 1, Sealed: true}); err != nil {
			return false, err
		}
		if err := step(engine.ReservationFileName); err != nil {
			return false, err
		}
	}
	return true, nil
}

// upgradeCapturedPlayer adopts a UUID-only upgrade under the same lock
// serve, bind and reset hold. The caller rereads the floor through its retry.
func upgradeCapturedPlayer(dir, captured string) error {
	unstarted, err := unstartedPlayer(dir)
	if err != nil || !unstarted {
		return err
	}
	f, err := fsutil.OpenLockFile(dir, "server.lock")
	if err != nil {
		return err
	}
	defer f.Close()
	if err := fsutil.TryLock(f); err != nil {
		return fmt.Errorf("player initialization is busy; stop the service and run the backup again: %w", err)
	}
	defer fsutil.Unlock(f)
	id, err := config.ReadPlayerUUID(dir)
	if err != nil || id != captured {
		return errors.New("player identity changed during backup; run the backup again")
	}
	_, _, err = EnsurePlayerUUID(dir)
	return err
}
