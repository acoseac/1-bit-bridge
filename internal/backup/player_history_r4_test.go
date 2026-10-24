package backup_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

func TestInterruptedIncomingRestoreSurvivesRestampAndReset(t *testing.T) {
	reservationStorage(t)
	incoming := t.TempDir()
	path := filepath.Join(incoming, config.PlayerUUIDFile)
	if err := config.WritePlayerUUIDFile(path, snapshotPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, incoming, snapshotPlayerUUID, 10000, 10000)
	old, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: incoming, PlayerUUID: path})
	if err != nil {
		t.Fatal(err)
	}
	putMark(t, incoming, snapshotPlayerUUID, 2000000000, 2000000000)
	latest, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: incoming, PlayerUUID: path})
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, dst, boxPlayerUUID, 10000, 10000)
	stopped := errors.New("interrupt before incoming UUID lands")
	reached := false
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		if b == filepath.Join(dst, config.PlayerUUIDFile) {
			reached = true
			return stopped
		}
		return os.Rename(a, b)
	})
	func() {
		defer atomicwrite.SetRenameFuncForTest(prev)
		if err := fixedRestore(t, latest, dst, 0); !errors.Is(err, stopped) {
			t.Fatalf("interruption: %v", err)
		}
	}()
	if !reached || markAt(t, dst).UUID != snapshotPlayerUUID {
		t.Fatal("did not interrupt after incoming reservation")
	}
	if id, err := config.ReadPlayerUUID(dst); err != nil || id != boxPlayerUUID {
		t.Fatalf("old UUID lost: %s %v", id, err)
	}
	// The actual engine restamps B's imported floor as A. Subtest cleanup stops
	// the engine and joins it before the reset removes any files.
	t.Run("restart_A", func(t *testing.T) { firstValues(t, dst, markAt(t, dst)) })
	if r := markAt(t, dst); r.UUID != boxPlayerUUID || r.IDs < 2000000000 {
		t.Fatalf("restamp: %+v", r)
	}
	if _, err := backup.ResetPlayer(dst); err != nil {
		t.Fatal(err)
	}
	before := historyTree(t, dst)
	if err := fixedRestore(t, old, dst, 0); err == nil || !strings.Contains(err.Error(), "given up") {
		t.Errorf("old B revived below issued values: %v", err)
	}
	if !bytes.Equal(before, historyTree(t, dst)) {
		t.Error("refused restore changed files")
	}
}

func TestHistoryOnlyRestoreRefusesGivenUpLiveOrReservationUUID(t *testing.T) {
	reservationStorage(t)
	for _, which := range []string{"live", "reservation"} {
		t.Run(which, func(t *testing.T) {
			// B took over U, advanced it, then gave it up. A was stopped throughout.
			src, dst := t.TempDir(), t.TempDir()
			if err := config.WritePlayerUUIDFile(filepath.Join(src, config.PlayerUUIDFile), snapshotPlayerUUID); err != nil {
				t.Fatal(err)
			}
			putMark(t, src, snapshotPlayerUUID, 2000000000, 2000000000)
			if _, err := backup.ResetPlayer(src); err != nil {
				t.Fatal(err)
			}
			snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: src})
			if err != nil {
				t.Fatal(err)
			}
			live, reserved := snapshotPlayerUUID, boxPlayerUUID
			if which == "reservation" {
				live, reserved = reserved, live
			}
			if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), live); err != nil {
				t.Fatal(err)
			}
			putMark(t, dst, reserved, 10000, 10000)
			before := historyTree(t, dst)
			err = backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)})
			if err == nil || !strings.Contains(err.Error(), "given up") || !strings.Contains(err.Error(), "bridge reset-player") {
				t.Errorf("restore accepted identity given up by merged history: %v", err)
			}
			if !bytes.Equal(before, historyTree(t, dst)) {
				t.Error("refusal wrote files")
			}
		})
	}
}

func TestPlayerStartRefusesGivenUpLiveOrReservationUUID(t *testing.T) {
	reservationStorage(t)
	for _, which := range []string{"live", "reservation"} {
		t.Run(which, func(t *testing.T) {
			dir := t.TempDir()
			if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			putMark(t, dir, snapshotPlayerUUID, 10000, 10000)
			given := boxPlayerUUID
			if which == "reservation" {
				given = snapshotPlayerUUID
			}
			putHistory(t, dir, nil, []string{given})
			before := historyTree(t, dir)
			if err := backup.CheckPlayerStart(dir); err == nil || !strings.Contains(err.Error(), "bridge reset-player") {
				t.Errorf("start accepted given-up %s: %v", which, err)
			}
			if !bytes.Equal(before, historyTree(t, dir)) {
				t.Error("start check wrote files")
			}
		})
	}
}

func TestDanglingPlayerHistoryRefusesOperations(t *testing.T) {
	reservationStorage(t)
	for _, op := range []string{"backup", "restore", "reset", "start"} {
		t.Run(op, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, backup.PlayerHistoryFileName)
			if err := os.Symlink(filepath.Join(dir, "absent"), path); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			var err error
			switch op {
			case "backup":
				_, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir})
			case "restore":
				src := t.TempDir()
				snap, e := backup.Snapshot(t.Context(), backup.Sources{DataDir: src})
				if e != nil {
					t.Fatal(e)
				}
				err = backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
			case "reset":
				_, err = backup.ResetPlayer(dir)
			case "start":
				err = backup.CheckPlayerStart(dir)
			}
			if err == nil || !strings.Contains(err.Error(), backup.PlayerHistoryFileName) {
				t.Errorf("dangling history treated as absent: %v", err)
			}
			if _, err := os.Readlink(path); err != nil {
				t.Error("history link changed")
			}
		})
	}
}

func TestCoveredHistoryWithoutReservationNeedsReset(t *testing.T) {
	reservationStorage(t)
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_uuid", true: "live_uuid"}[live], func(t *testing.T) {
			dir := t.TempDir()
			putHistory(t, dir, []string{snapshotPlayerUUID}, nil)
			if live {
				if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
					t.Fatal(err)
				}
			}
			if err := backup.CheckPlayerStart(dir); err == nil || !strings.Contains(err.Error(), "bridge reset-player") {
				t.Errorf("missing covered floor allowed start: %v", err)
			}
			if changed, err := backup.ResetPlayer(dir); !changed || err != nil {
				t.Fatalf("covered history reset was a no-op: %t %v", changed, err)
			}
			h := readHistoryMap(t, dir)
			if len(h.Covered) != 0 || !slices.Contains(h.GivenUp, snapshotPlayerUUID) {
				t.Fatalf("covered identity lost: %+v", h)
			}
			if err := backup.CheckPlayerStart(dir); err != nil {
				t.Fatalf("completed reset refused: %v", err)
			}
		})
	}
}

func TestRestoreLiveUUIDReadRefusalsNameRemedy(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"malformed", "unreadable", "non_file"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, config.PlayerUUIDFile)
			want := "bridge reset-player"
			switch kind {
			case "malformed":
				if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("requires POSIX permissions and non-root")
				}
				if err := os.WriteFile(path, []byte(boxPlayerUUID), 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0600) })
				want = "restore read access"
			case "non_file":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				want = "move the non-file"
			}
			putMark(t, dir, boxPlayerUUID, 90000, 80000)
			before := readBytes(t, filepath.Join(dir, engine.ReservationFileName))
			err := fixedRestore(t, identitySnapshot(t, true), dir, 0)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q remedy: %v", want, err)
			}
			if !bytes.Equal(before, readBytes(t, filepath.Join(dir, engine.ReservationFileName))) {
				t.Error("refusal wrote floor")
			}
			if _, err := os.Lstat(filepath.Join(dir, backup.PlayerHistoryFileName)); !errors.Is(err, os.ErrNotExist) {
				t.Error("refusal wrote history")
			}
		})
	}
}

func TestRestoreGivenUpBoxIdentityNamesResetEvenWhenSnapshotMatches(t *testing.T) {
	reservationStorage(t)
	for _, which := range []string{"live", "reservation"} {
		t.Run(which, func(t *testing.T) {
			dir := t.TempDir()
			live, reserved := snapshotPlayerUUID, boxPlayerUUID
			if which == "reservation" {
				live, reserved = reserved, live
			}
			if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), live); err != nil {
				t.Fatal(err)
			}
			putMark(t, dir, reserved, 10000, 10000)
			putHistory(t, dir, nil, []string{snapshotPlayerUUID})
			before := historyTree(t, dir)
			err := fixedRestore(t, identitySnapshot(t, true), dir, 0)
			if err == nil || !strings.Contains(err.Error(), "bridge reset-player") {
				t.Errorf("given-up box identity needs reset, even when incoming identity matches: %v", err)
			}
			if !bytes.Equal(before, historyTree(t, dir)) {
				t.Error("refusal wrote files")
			}
		})
	}
}
