package backup_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// freshSeal is the seal a discard writes.
const freshSeal = "{\"version\":1,\"sealed\":true}\n"

// sealObstructions are the entries that are not a regular file by Lstat that
// obstructSeal makes on every platform; platformSealObstructions adds the
// unix ones.
var sealObstructions = []string{"directory", "directory_with_a_file", "dangling_link", "link_to_a_seal", "link_to_a_directory"}

// obstructSeal replaces dir's seal file with an entry of kind.
func obstructSeal(t *testing.T, dir, kind string) {
	t.Helper()
	path := filepath.Join(dir, sealFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case "directory":
		err = os.Mkdir(path, 0o700)
	case "directory_with_a_file":
		if err = os.Mkdir(path, 0o700); err == nil {
			err = os.WriteFile(filepath.Join(path, "kept"), []byte("kept"), 0o600)
		}
	case "dangling_link":
		err = os.Symlink(sealFile+".nowhere", path)
	case "link_to_a_seal":
		if err = os.WriteFile(filepath.Join(dir, sealFile+".target"), []byte(freshSeal), 0o600); err == nil {
			err = os.Symlink(sealFile+".target", path)
		}
	case "link_to_a_directory":
		if err = os.Mkdir(filepath.Join(dir, sealFile+".elsewhere"), 0o700); err == nil {
			err = os.Symlink(sealFile+".elsewhere", path)
		}
	default:
		obstructSealOnThisPlatform(t, path, kind)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// signature describes the entry at path without following it or opening
// anything but a regular file or a directory: its type and, for a link its
// target, for a directory its names, for a regular file its bytes; "absent"
// for no entry.
func signature(t *testing.T, path string) string {
	t.Helper()
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	kind := st.Mode().Type().String()
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
		return kind + " -> " + target
	case st.IsDir():
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return kind + " [" + strings.Join(names, " ") + "]"
	case st.Mode().IsRegular():
		return kind + " " + string(readBytes(t, path))
	}
	return kind
}

// crashCopy copies dir as a crash at this moment would leave it, into a
// directory of its own: each entry as it stands, a link as a link to the same
// target, a directory with what it holds, a named pipe as a named pipe.
func crashCopy(t *testing.T, dir string) string {
	t.Helper()
	dst := t.TempDir()
	copyTree(t, dir, dst)
	return dst
}

// copyTree copies the entries of src into dst, which exists.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		st, err := os.Lstat(s)
		if err != nil {
			t.Fatal(err)
		}
		switch m := st.Mode(); {
		case m&fs.ModeSymlink != 0:
			target, err := os.Readlink(s)
			if err == nil {
				err = os.Symlink(target, d)
			}
			if err != nil {
				t.Fatal(err)
			}
		case m.IsDir():
			if err := os.Mkdir(d, m.Perm()); err != nil {
				t.Fatal(err)
			}
			copyTree(t, s, d)
		case m.IsRegular():
			if err := os.WriteFile(d, readBytes(t, s), m.Perm()); err != nil {
				t.Fatal(err)
			}
		default:
			copySpecialEntry(t, d, m)
		}
	}
}

// restoreAccepted reports whether an identity restore onto dir was accepted,
// failing the test for a refusal that is not the seal's.
func restoreAccepted(t *testing.T, dir string) bool {
	t.Helper()
	err := backup.RestoreWithOptions(identitySnapshot(t, true), backup.Targets{PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)},
		backup.RestoreOptions{Random: bytes.NewReader(randomDraws(0, 0))})
	if err != nil && !strings.Contains(err.Error(), "sealed") {
		t.Errorf("the identity restore was refused, but not as sealed: %v", err)
	}
	return err == nil
}

// startBox is what the next start of dir's box runs: serve boot's completion,
// adoption and start check, then the player's start through the minter.
func startBox(dir string) error {
	if err := backup.AdoptPlayerState(dir); err != nil {
		return err
	}
	if err := backup.CheckPlayerStart(dir); err != nil {
		return err
	}
	_, _, err := backup.EnsurePlayerUUID(dir)
	return err
}

// requireBoxSealed requires dir's box to refuse an identity restore, after its
// next start too; and, unless the seal's name is empty, after the loss of
// player-initialized and the start after that. Every check runs on a copy of
// the box, so the caller's box stays as it was.
func requireBoxSealed(t *testing.T, dir, at string) {
	t.Helper()
	if restoreAccepted(t, crashCopy(t, dir)) {
		t.Errorf("%s: an identity restore was accepted", at)
	}
	box := crashCopy(t, dir)
	if err := startBox(box); err != nil {
		t.Errorf("%s: the next start refused: %v", at, err)
	}
	if restoreAccepted(t, box) {
		t.Errorf("%s: an identity restore was accepted after the next start", at)
	}
	if signature(t, filepath.Join(dir, sealFile)) == "absent" {
		return
	}
	box = crashCopy(t, dir)
	if err := startBox(box); err != nil {
		t.Errorf("%s: the next start refused: %v", at, err)
	}
	if err := os.Remove(filepath.Join(box, "player-initialized")); err != nil {
		t.Fatal(err)
	}
	if err := startBox(box); err != nil {
		t.Errorf("%s: the start after player-initialized was lost refused: %v", at, err)
	}
	if restoreAccepted(t, box) {
		t.Errorf("%s: an identity restore was accepted once player-initialized was lost", at)
	}
}

// requireDiscardHeals runs the discard again on a copy of dir's box and
// requires a fresh regular seal, with every entry it replaced kept aside, and
// identity restores refused.
func requireDiscardHeals(t *testing.T, dir, at string) {
	t.Helper()
	box := crashCopy(t, dir)
	if _, err := backup.DiscardPlayerFloor(box); err != nil {
		t.Errorf("%s: the discard run again: %v", at, err)
		return
	}
	if got := signature(t, filepath.Join(box, sealFile)); got != "---------- "+freshSeal {
		t.Errorf("%s: the discard run again left the seal %q", at, got)
	}
	if restoreAccepted(t, box) {
		t.Errorf("%s: an identity restore was accepted after the discard ran again", at)
	}
}

// asideEntries is every entry discard kept aside under the seal's name.
func asideEntries(t *testing.T, dir string) []string {
	t.Helper()
	aside, err := filepath.Glob(filepath.Join(dir, sealFile+".discarded-*"))
	if err != nil {
		t.Fatal(err)
	}
	return aside
}

// stagedFiles is every file a strict write staged in dir and left there.
func stagedFiles(t *testing.T, dir string) []string {
	t.Helper()
	staged, err := filepath.Glob(filepath.Join(dir, ".player-marker-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return staged
}

// healSeams watches a discard heal the seal entry through the strict
// writer's seams: the staged file's fsync, which the seal's write reaches
// once player-initialized has landed (discard seals that record first beside
// a damaged seal entry); the rename of the entry aside; the rename of the
// staged seal into its place; and the directory sync after them. Each hook,
// when set, runs at its point; a hook that returns an error makes that step
// fail with it.
type healSeams struct {
	staging, staged, aside, inPlace, synced func() error
	// rollback, when set, answers the rename that puts the entry back.
	rollback func() error
	// syncedFiles is every staged file whose fsync answered.
	syncedFiles                      []string
	markerLanded, sealSynced, placed bool
}

// install sets the seams until the test ends; the returned stop puts them
// back at once.
func (h *healSeams) install(t *testing.T, dir string) (stop func()) {
	t.Helper()
	prevRename := atomicwrite.SetRenameFuncForTest(func(src, dst string) error {
		sb, db := filepath.Base(src), filepath.Base(dst)
		switch {
		case sb == sealFile && strings.HasPrefix(db, sealFile+".discarded-"):
			if h.staged != nil {
				if err := h.staged(); err != nil {
					return err
				}
			}
			if err := os.Rename(src, dst); err != nil {
				return err
			}
			if h.aside != nil {
				return h.aside()
			}
			return nil
		case db == sealFile && strings.HasPrefix(sb, ".player-marker-"):
			if h.inPlace != nil {
				// The hook decides first whether this rename fails.
				if err := h.inPlace(); err != nil {
					return err
				}
			}
			if err := os.Rename(src, dst); err != nil {
				return err
			}
			h.placed = true
			return nil
		case db == sealFile && strings.HasPrefix(sb, sealFile+".discarded-"):
			if h.rollback != nil {
				if err := h.rollback(); err != nil {
					return err
				}
			}
			return os.Rename(src, dst)
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		if db == "player-initialized" {
			h.markerLanded = true
		}
		return nil
	})
	restoreSyncFile := atomicwrite.SetSyncFileForTest(func(f *os.File) error {
		if err := f.Sync(); err != nil {
			return err
		}
		h.syncedFiles = append(h.syncedFiles, filepath.Base(f.Name()))
		if h.markerLanded && !h.sealSynced {
			h.sealSynced = true
			if h.staging != nil {
				return h.staging()
			}
		}
		return nil
	})
	restoreSyncDir := atomicwrite.SetSyncDirForTest(func(string) error {
		if h.placed && h.synced != nil {
			synced := h.synced
			h.synced = nil
			return synced()
		}
		return nil
	})
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		restoreSyncDir()
		restoreSyncFile()
		atomicwrite.SetRenameFuncForTest(prevRename)
	}
	t.Cleanup(stop)
	return stop
}

// lostMarkerBox is the walk's state: a sealed, minted box whose
// player-initialized is gone and whose seal is an entry of kind.
func lostMarkerBox(t *testing.T, kind string) (dir, entry string) {
	t.Helper()
	dir = t.TempDir()
	sealedMint(t, dir)
	if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
		t.Fatal(err)
	}
	obstructSeal(t, dir, kind)
	return dir, signature(t, filepath.Join(dir, sealFile))
}

// TestEveryCrashPointOfTheSealsHealLeavesTheBoxSealed: discard heals a seal
// entry that is not a regular file, on a sealed, minted box whose
// player-initialized is gone, in steps a crash may come between: the fresh
// seal staged and synced beside the entry, the entry renamed aside, the
// fresh seal renamed into its place, the directory synced. The box a crash
// leaves at each point, copied as it stands there, is sealed: an identity
// restore is refused, after the next start too, and the discard run again
// heals it. The fresh seal is durable before the entry moves, so at every
// point but one the seal's name holds the entry or the fresh seal, and a
// later loss of player-initialized leaves the box sealed; at that one,
// between the two renames, player-initialized holds the seal discard wrote
// there first. Discard used to rename the entry aside before it staged the
// fresh seal, so the seal's name stood empty through the whole write.
func TestEveryCrashPointOfTheSealsHealLeavesTheBoxSealed(t *testing.T) {
	reservationStorage(t)
	for _, kind := range append(sealObstructions, platformSealObstructions...) {
		t.Run(kind, func(t *testing.T) {
			dir, entry := lostMarkerBox(t, kind)
			boxes := map[string]string{}
			var order []string
			at := func(point string) func() error {
				return func() error {
					boxes[point] = crashCopy(t, dir)
					order = append(order, point)
					return nil
				}
			}
			h := &healSeams{staging: at("staging"), aside: at("aside"), synced: at("synced")}
			h.staged = func() error {
				var fresh []string
				for _, p := range stagedFiles(t, dir) {
					if string(readBytes(t, p)) == freshSeal && slices.Contains(h.syncedFiles, filepath.Base(p)) {
						fresh = append(fresh, p)
					}
				}
				if len(fresh) != 1 {
					t.Errorf("the entry moves with no fresh seal staged and synced beside it: staged %v, synced %v", stagedFiles(t, dir), h.syncedFiles)
				}
				return at("staged")()
			}
			stop := h.install(t, dir)
			placed := false
			h.inPlace = func() error {
				placed = true
				return nil
			}
			_, err := backup.DiscardPlayerFloor(dir)
			stop()
			if err != nil {
				t.Fatalf("discard: %v", err)
			}
			if !placed {
				t.Fatal("discard never renamed a staged seal into place")
			}
			if want := []string{"staging", "staged", "aside", "synced"}; !slices.Equal(order, want) {
				t.Errorf("the heal's points came in the order %v, want %v", order, want)
			}
			for _, point := range order {
				box := boxes[point]
				seal := signature(t, filepath.Join(box, sealFile))
				m := markerAt(t, box)
				switch point {
				case "staging", "staged":
					if seal != entry {
						t.Errorf("%s: the seal's name holds %q, not the entry %q", point, seal, entry)
					}
				case "aside":
					if seal != "absent" || len(asideEntries(t, box)) != 1 {
						t.Errorf("%s: the seal's name holds %q beside %v", point, seal, asideEntries(t, box))
					}
				case "synced":
					if seal != "---------- "+freshSeal {
						t.Errorf("%s: the seal's name holds %q", point, seal)
					}
					if aside := asideEntries(t, box); len(aside) != 1 || signature(t, aside[0]) != entry {
						t.Errorf("%s: the entry was not kept aside: %v", point, aside)
					}
				}
				if m.Version != 1 || !m.Sealed {
					t.Errorf("%s: player-initialized does not hold the seal: %+v", point, m)
				}
				requireBoxSealed(t, box, point)
				requireDiscardHeals(t, box, point)
			}
		})
	}
}

// TestAFailedStepOfTheSealsHealKeepsTheEntryItFound: each step of discard's
// heal of a seal entry that is not a regular file fails in turn, on the
// walk's state. The fresh seal's staging, the rename of the entry aside and
// the rename of the fresh seal into its place each leave the entry at the
// seal's name, as it was, and no staged file; the last puts it back. Both
// renames into the seal's name failing leaves the entry aside, which the
// error names, and player-initialized holding the seal; the directory's sync
// failing leaves the fresh seal in place and the entry aside. Every failure
// says to run the discard again, leaves the box sealed, and the discard run
// again heals it. Discard used to rename the entry aside first, so a failed
// write left the seal's name empty, and it said nothing but the error.
func TestAFailedStepOfTheSealsHealKeepsTheEntryItFound(t *testing.T) {
	reservationStorage(t)
	broken := errors.New("the step failed")
	fail := func() error { return broken }
	for _, tc := range []struct {
		step  string
		seams func(h *healSeams)
		want  func(t *testing.T, dir, entry string, err error)
	}{
		{"staging", func(h *healSeams) { h.staging = fail }, keptAtItsName},
		{"aside", func(h *healSeams) { h.staged = fail }, keptAtItsName},
		{"into_place", func(h *healSeams) { h.inPlace = fail }, keptAtItsName},
		{"into_place_and_back", func(h *healSeams) { h.inPlace, h.rollback = fail, fail }, func(t *testing.T, dir, entry string, err error) {
			aside := asideEntries(t, dir)
			if len(aside) != 1 || signature(t, aside[0]) != entry {
				t.Fatalf("the entry is not kept aside: %v", aside)
			}
			if seal := signature(t, filepath.Join(dir, sealFile)); seal != "absent" {
				t.Errorf("the seal's name holds %q", seal)
			}
			if !strings.Contains(err.Error(), filepath.Base(aside[0])) {
				t.Errorf("the error does not say where the entry is kept: %v", err)
			}
			if m := markerAt(t, dir); m.Version != 1 || !m.Sealed {
				t.Errorf("player-initialized does not hold the seal: %+v", m)
			}
		}},
		{"synced", func(h *healSeams) { h.synced = fail }, func(t *testing.T, dir, entry string, err error) {
			if seal := signature(t, filepath.Join(dir, sealFile)); seal != "---------- "+freshSeal {
				t.Errorf("the seal's name holds %q", seal)
			}
			if aside := asideEntries(t, dir); len(aside) != 1 || signature(t, aside[0]) != entry {
				t.Errorf("the entry is not kept aside: %v", aside)
			}
		}},
	} {
		for _, kind := range []string{"directory_with_a_file", "dangling_link"} {
			t.Run(tc.step+"/"+kind, func(t *testing.T) {
				dir, entry := lostMarkerBox(t, kind)
				h := &healSeams{}
				tc.seams(h)
				stop := h.install(t, dir)
				_, err := backup.DiscardPlayerFloor(dir)
				stop()
				if !errors.Is(err, broken) {
					t.Fatalf("discard over a failed %s: %v", tc.step, err)
				}
				if !strings.Contains(err.Error(), "run bridge reset-player --discard-floor again") || strings.Contains(err.Error(), "aside:") {
					t.Errorf("the failure does not say to run the discard again: %v", err)
				}
				if staged := stagedFiles(t, dir); len(staged) != 0 {
					t.Errorf("the failure left staged files: %v", staged)
				}
				tc.want(t, dir, entry, err)
				requireBoxSealed(t, dir, tc.step)
				requireDiscardHeals(t, dir, tc.step)
			})
		}
	}
}

// keptAtItsName requires the entry back at the seal's name, as it was, and
// nothing kept aside.
func keptAtItsName(t *testing.T, dir, entry string, _ error) {
	t.Helper()
	if seal := signature(t, filepath.Join(dir, sealFile)); seal != entry {
		t.Errorf("the seal's name holds %q, not the entry %q", seal, entry)
	}
	if aside := asideEntries(t, dir); len(aside) != 0 {
		t.Errorf("an entry is left aside: %v", aside)
	}
}

// TestANonRegularSealIsASealAtEveryCheckUntilDiscardHealsIt: a seal entry
// that is not a regular file by Lstat (a directory, empty or not; a symbolic
// link to nothing, to a regular seal or to a directory; on unix a named pipe)
// is never read as no seal. An identity restore is refused as sealed; a UUID
// beside it is no upgrade to adopt, but a floor lost after a mint; and serve's
// boot, a start, a backup, a refused restore and a reset each keep the entry
// as it is, never opening, following, replacing or removing it, while
// completion records the seal in player-initialized beside it. Only discard
// replaces it, with a fresh regular seal, keeping the entry aside. A reader
// that took the entry for no seal, or followed a link, lets the restore
// through.
func TestANonRegularSealIsASealAtEveryCheckUntilDiscardHealsIt(t *testing.T) {
	reservationStorage(t)
	for _, kind := range append(sealObstructions, platformSealObstructions...) {
		t.Run(kind, func(t *testing.T) {
			dir, entry := lostMarkerBox(t, kind)
			seal := filepath.Join(dir, sealFile)
			if restoreAccepted(t, crashCopy(t, dir)) {
				t.Fatal("an identity restore was accepted beside the entry")
			}
			for _, op := range []struct {
				name string
				run  func() error
			}{
				{"serve boot", func() error {
					if err := backup.AdoptPlayerState(dir); err != nil {
						return err
					}
					return backup.CheckPlayerStart(dir)
				}},
				{"start", func() error { _, _, err := backup.EnsurePlayerUUID(dir); return err }},
				{"backup", func() error {
					_, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
					return err
				}},
				{"refused restore", func() error {
					if restoreAccepted(t, dir) {
						t.Fatal("an identity restore was accepted beside the entry")
					}
					return nil
				}},
				{"reset", func() error { _, err := backup.ResetPlayer(dir); return err }},
				{"start after the reset", func() error { _, _, err := backup.EnsurePlayerUUID(dir); return err }},
			} {
				if err := op.run(); err != nil {
					t.Fatalf("%s beside the entry: %v", op.name, err)
				}
				if got := signature(t, seal); got != entry {
					t.Fatalf("%s changed the entry from %q to %q", op.name, entry, got)
				}
				if m := markerAt(t, dir); m.Version != 1 || !m.Sealed {
					t.Fatalf("after %s player-initialized records no seal: %+v", op.name, m)
				}
			}
			if _, err := backup.DiscardPlayerFloor(dir); err != nil {
				t.Fatal(err)
			}
			if got := signature(t, seal); got != "---------- "+freshSeal {
				t.Errorf("discard left the seal %q", got)
			}
			if aside := asideEntries(t, dir); len(aside) != 1 || signature(t, aside[0]) != entry {
				t.Errorf("discard did not keep the entry aside: %v", aside)
			}
			if restoreAccepted(t, dir) {
				t.Error("an identity restore was accepted after the discard")
			}
		})
		t.Run(kind+"/beside_a_lone_uuid", func(t *testing.T) {
			dir := t.TempDir()
			if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			obstructSeal(t, dir, kind)
			if err := backup.AdoptPlayerState(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(dir, engine.ReservationFileName)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a UUID beside the entry was adopted as an upgrade: %v", err)
			}
			if m := markerAt(t, dir); m.Version != 1 || !m.Sealed || !m.Minted {
				t.Errorf("completion did not record the seal, spent, beside the UUID: %+v", m)
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err == nil || !strings.Contains(err.Error(), "floor is missing") {
				t.Errorf("a start beside the entry and a lone UUID: %v", err)
			}
			if restoreAccepted(t, crashCopy(t, dir)) {
				t.Error("an identity restore was accepted beside the entry")
			}
		})
	}
}
