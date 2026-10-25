package backup_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

const sealFile = "player-floor-discarded.json"

// replaceSealWith turns dir's seal file into kind: a directory, or a symbolic
// link to nothing, which a reader that follows it would take for no seal.
func replaceSealWith(t *testing.T, dir, kind string) string {
	t.Helper()
	path := filepath.Join(dir, sealFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case "directory":
		err = os.Mkdir(path, 0700)
	case "symlink":
		err = os.Symlink(sealFile+".nowhere", path)
	default:
		t.Fatalf("no seal kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// entryMode is the type bits of the entry at path, never following it.
func entryMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Type()
}

// markerAt decodes dir's player-initialized.
func markerAt(t *testing.T, dir string) (m struct {
	Version        int
	Sealed, Minted bool
}) {
	t.Helper()
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, "player-initialized")), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// identityRestoreRefused requires an identity restore onto dir to refuse as sealed.
func identityRestoreRefused(t *testing.T, dir string) {
	t.Helper()
	err := backup.RestoreWithOptions(identitySnapshot(t, true), backup.Targets{PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)},
		backup.RestoreOptions{Random: bytes.NewReader(randomDraws(0, 0))})
	if err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("identity restore onto the sealed box: %v", err)
	}
}

// noMoveAsideAdvice fails the test for any advice to move a seal aside.
func noMoveAsideAdvice(t *testing.T, rec *loggingtest.Recorder, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "aside") {
		t.Errorf("advice to move the seal aside: %v", err)
	}
	for _, line := range rec.All() {
		if strings.Contains(line, sealFile) && strings.Contains(line, "aside") {
			t.Errorf("advice to move the seal aside: %s", line)
		}
	}
}

// TestAnUnlistedFloorRefusesUnlessItIsGenuinelyAbsent pins restore's verdict
// on the Lstat of a floor its manifest does not list: an entry of any kind is
// a damaged manifest, refused with the snapshot remedy and pointing to a
// newer snapshot, never just another one; only ErrNotExist is a genuine
// absence; any other error refuses with itself.
func TestAnUnlistedFloorRefusesUnlessItIsGenuinelyAbsent(t *testing.T) {
	err := backup.UnlistedFloorError(nil)
	if err == nil {
		t.Fatal("an unlisted entry passed as no floor")
	}
	for _, want := range []string{"does not list player-ids.json", "copy of the snapshot", "or restore a newer snapshot of this box", "nothing was restored"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("an unlisted entry's refusal lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "another snapshot") {
		t.Errorf("an unlisted entry's refusal steers to an older snapshot: %v", err)
	}
	for _, absent := range []error{os.ErrNotExist, &os.PathError{Op: "lstat", Path: "player-ids.json", Err: syscall.ENOENT}} {
		if err := backup.UnlistedFloorError(absent); err != nil {
			t.Errorf("a genuine absence (%v) refused: %v", absent, err)
		}
	}
	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.EIO, neitherAbsentNorPresent} {
		if errors.Is(errno, os.ErrNotExist) {
			t.Fatalf("%v reads as no entry here, so it cannot stand for an error that says neither", errno)
		}
		lstatErr := &os.PathError{Op: "lstat", Path: "player-ids.json", Err: errno}
		if err := backup.UnlistedFloorError(lstatErr); !errors.Is(err, errno) || !strings.Contains(err.Error(), "nothing was restored") {
			t.Errorf("an Lstat error %v: %v", errno, err)
		}
	}
}

// TestANonFileSealIsKeptAndRecordedTwice is the round-10 hazard (B539): a
// sealed, minted box loses player-initialized, and its seal file becomes a
// directory or a symbolic link. A non-file seal is still a seal: start keeps
// it, never opening or following it, writes the sealed initialized marker
// beside it, and identity restores stay refused. Start used to refuse with
// "move it aside", and following that dropped the only seal.
func TestANonFileSealIsKeptAndRecordedTwice(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			sealedMint(t, dir)
			if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
				t.Fatal(err)
			}
			seal := replaceSealWith(t, dir, kind)
			want := entryMode(t, seal)
			rec := loggingtest.Record(t)
			_, _, err := backup.EnsurePlayerUUID(dir)
			noMoveAsideAdvice(t, rec, err)
			if err != nil {
				t.Fatalf("start refused a non-file seal: %v", err)
			}
			if m := markerAt(t, dir); m.Version != 1 || !m.Sealed || !m.Minted {
				t.Fatalf("start did not record the seal again: %+v", m)
			}
			if got := entryMode(t, seal); got != want {
				t.Fatalf("seal entry changed from %v to %v", want, got)
			}
			identityRestoreRefused(t, dir)
			t.Run("engine", func(t *testing.T) { firstValues(t, dir, markAt(t, dir)) })
		})
	}
}

// TestANonFileSealIsCompletedByABackup: the same state reached through a
// backup's completion, which seals the initialized marker under server.lock
// and logs no failure, so no log line advises moving the seal aside.
func TestANonFileSealIsCompletedByABackup(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			sealedMint(t, dir)
			if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
				t.Fatal(err)
			}
			seal := replaceSealWith(t, dir, kind)
			want := entryMode(t, seal)
			rec := loggingtest.Record(t)
			_, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
			noMoveAsideAdvice(t, rec, err)
			if err != nil {
				t.Fatal(err)
			}
			if failed := rec.Lines("player seal completion failed"); len(failed) != 0 {
				t.Fatalf("completion failed on a non-file seal: %v", failed)
			}
			if m := markerAt(t, dir); m.Version != 1 || !m.Sealed || !m.Minted {
				t.Fatalf("backup's completion did not record the seal: %+v", m)
			}
			if got := entryMode(t, seal); got != want {
				t.Fatalf("seal entry changed from %v to %v", want, got)
			}
			identityRestoreRefused(t, dir)
		})
	}
}

// TestANonFileLoneSealIsCompletedAroundIt replaces round 7's expectation that
// a non-regular lone seal refuses startup. Completion keeps it and writes the
// second record: spent on a box minted before, open as the one-shot
// missing-floor exemption on a box with no allocation evidence, which the
// first mint then spends.
func TestANonFileLoneSealIsCompletedAroundIt(t *testing.T) {
	reservationStorage(t)
	for _, box := range []string{"minted_before", "no_evidence"} {
		t.Run(box, func(t *testing.T) {
			dir := t.TempDir()
			if box == "minted_before" {
				sealedMint(t, dir)
				if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
					t.Fatal(err)
				}
			}
			seal := replaceSealWith(t, dir, "directory")
			if err := backup.AdoptPlayerState(dir); err != nil {
				t.Fatalf("completion refused a non-file seal: %v", err)
			}
			m := markerAt(t, dir)
			if m.Version != 1 || !m.Sealed || m.Minted != (box == "minted_before") {
				t.Fatalf("completed marker %+v on %s", m, box)
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			if m := markerAt(t, dir); !m.Sealed || !m.Minted {
				t.Fatalf("the mint did not spend the exemption: %+v", m)
			}
			if entryMode(t, seal) != os.ModeDir {
				t.Fatal("seal directory replaced")
			}
			if err := os.Remove(filepath.Join(dir, engine.ReservationFileName)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err == nil || !strings.Contains(err.Error(), "floor is missing") {
				t.Fatalf("the spent exemption opened again: %v", err)
			}
		})
	}
}

// TestDiscardRecordsASecondSealBeforeRenamingADamagedOne: discard repairs a
// damaged seal by renaming it aside and writing a fresh one. With the seal a
// directory and player-initialized gone, that rename leaves no seal until the
// replacement lands, so discard records the seal in player-initialized
// first. An injected failure between the rename and the replacement leaves
// identity restores refused, and a rerun completes the repair.
func TestDiscardRecordsASecondSealBeforeRenamingADamagedOne(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	sealedMint(t, dir)
	if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
		t.Fatal(err)
	}
	seal := replaceSealWith(t, dir, "directory")
	broken := errors.New("the seal's replacement refused")
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		if filepath.Base(b) == sealFile {
			return broken
		}
		return os.Rename(a, b)
	})
	_, err := backup.DiscardPlayerFloor(dir)
	atomicwrite.SetRenameFuncForTest(prev)
	if !errors.Is(err, broken) {
		t.Fatalf("discard over a refused replacement: %v", err)
	}
	if _, err := os.Lstat(seal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the damaged seal was not renamed before its replacement: %v", err)
	}
	aside, err := filepath.Glob(seal + ".discarded-*")
	if err != nil || len(aside) != 1 || entryMode(t, aside[0]) != os.ModeDir {
		t.Fatalf("the damaged seal was not kept aside: %v %v", aside, err)
	}
	identityRestoreRefused(t, dir)
	if _, err := backup.DiscardPlayerFloor(dir); err != nil {
		t.Fatal(err)
	}
	if entryMode(t, seal) != 0 {
		t.Fatalf("discard left no regular seal: %v", entryMode(t, seal))
	}
	var fresh struct {
		Version int
		Sealed  bool
	}
	if err := json.Unmarshal(readBytes(t, seal), &fresh); err != nil || fresh.Version != 1 {
		t.Fatalf("fresh seal %+v %v", fresh, err)
	}
	if m := markerAt(t, dir); m.Version != 1 || !m.Sealed || m.Minted {
		t.Fatalf("completed discard's marker: %+v", m)
	}
	identityRestoreRefused(t, dir)
}

// TestMoveAsideAdviceCountsANonFileSealAsASeal: a damaged initialized marker
// that reads sealed:true beside a directory or symbolic link at the seal path
// is not the only seal, since completion keeps that entry and seals the
// marker again. The preflight gives the move-aside advice, and following it
// keeps identity restores refused.
func TestMoveAsideAdviceCountsANonFileSealAsASeal(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			sealedMint(t, dir)
			seal := replaceSealWith(t, dir, kind)
			marker := filepath.Join(dir, "player-initialized")
			if err := os.WriteFile(marker, []byte(`{"version":0,"sealed":true}`), 0600); err != nil {
				t.Fatal(err)
			}
			rec := loggingtest.Record(t)
			if err := backup.CheckPlayerStart(dir); err != nil {
				t.Fatal(err)
			}
			if lines := rec.Lines(damagedMarkerWarning); len(lines) != 1 || len(rec.Lines(soleSealWarning)) != 0 {
				t.Fatalf("advice beside a non-file seal: move-aside %v, sole-seal %v", lines, rec.Lines(soleSealWarning))
			}
			if err := os.Rename(marker, marker+".damaged"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatalf("start after the advised move: %v", err)
			}
			if m := markerAt(t, dir); !m.Sealed || !m.Minted {
				t.Fatalf("start after the move did not seal the marker: %+v", m)
			}
			if entryMode(t, seal) == 0 {
				t.Fatal("seal entry replaced")
			}
			identityRestoreRefused(t, dir)
		})
	}
}

// TestSealCompletionAdviceClaimsNothingForATransientError: a lock or I/O
// error that need not recur keeps its error alone, with no claim about serve
// boot and no access advice; a read-only data directory says so, and that it
// persists.
func TestSealCompletionAdviceClaimsNothingForATransientError(t *testing.T) {
	reservationStorage(t)
	for _, tc := range []struct {
		name   string
		inject func(t *testing.T)
		want   string // in the reason; empty: only the error
	}{
		{"eintr", func(t *testing.T) {
			t.Cleanup(backup.SetTryLockForTest(func(f *os.File) error { return &os.PathError{Op: "flock", Path: f.Name(), Err: syscall.EINTR} }))
		}, ""},
		{"enolck", func(t *testing.T) {
			t.Cleanup(backup.SetTryLockForTest(func(f *os.File) error { return &os.PathError{Op: "flock", Path: f.Name(), Err: syscall.ENOLCK} }))
		}, ""},
		{"eio", func(t *testing.T) {
			prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
				if filepath.Base(b) == "player-initialized" {
					return &os.LinkError{Op: "rename", Old: a, New: b, Err: syscall.EIO}
				}
				return os.Rename(a, b)
			})
			t.Cleanup(func() { atomicwrite.SetRenameFuncForTest(prev) })
		}, ""},
		{"erofs", func(t *testing.T) {
			prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
				if filepath.Base(b) == "player-initialized" {
					return &os.LinkError{Op: "rename", Old: a, New: b, Err: syscall.EROFS}
				}
				return os.Rename(a, b)
			})
			t.Cleanup(func() { atomicwrite.SetRenameFuncForTest(prev) })
		}, "the data directory is read-only: serve boot and start fail on it too"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := loneSealBox(t)
			tc.inject(t)
			rec := loggingtest.Record(t)
			if _, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}); err != nil {
				t.Fatalf("a failed completion stopped the backup: %v", err)
			}
			lines := rec.Lines("player seal completion failed")
			if len(lines) != 1 {
				t.Fatalf("failure records: %v", lines)
			}
			line := lines[0]
			if tc.want == "" {
				for _, claim := range []string{"serve boot", "fail on it too", "access", "read-only"} {
					if strings.Contains(line, claim) {
						t.Fatalf("a transient %s claims %q: %s", tc.name, claim, line)
					}
				}
				return
			}
			if !strings.Contains(line, tc.want) || strings.Contains(line, "write access") {
				t.Fatalf("read-only advice: %s", line)
			}
		})
	}
}
