package backup_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

func TestAdoptionBoundsReservedSpaceAcross4097Restarts(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, "player-uuid"), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, dir, boxPlayerUUID, 0, 0)
	var highest uint32
	for i := 0; i < 4097; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			token := startRestoredPlayer(t, dir).Snapshot().Token()
			if token != uint32(i)*8192+4097 {
				t.Fatalf("startup token %d at restart %d", token, i)
			}
			highest = max(highest, token)
		})
	}
	if highest != 33558529 {
		t.Fatalf("highest token %d", highest)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-queue.json")); !os.IsNotExist(err) {
		t.Fatalf("queue exists: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "player-ids.json")); err != nil {
		t.Fatal(err)
	}
	if err := backup.AdoptPlayerState(dir); err != nil {
		t.Fatal(err)
	}
	floor := markAt(t, dir)
	if floor.Exhausted() {
		t.Fatalf("adoption exhausted engine: %+v", floor)
	}
	t.Run("adopted", func(t *testing.T) {
		id, token := firstValues(t, dir, floor)
		if id <= highest || token <= highest {
			t.Fatalf("first values %d/%d repeat pre-adoption space through %d", id, token, highest)
		}
	})
	for _, value := range []uint32{floor.IDs, floor.Tokens} {
		if value < 1<<31 || uint64(value) >= 1<<31+1<<24 {
			t.Fatalf("adoption outside upper-half jump: %+v", floor)
		}
	}
}

func sealedMint(t *testing.T, dir string) {
	t.Helper()
	if _, err := backup.DiscardPlayerFloor(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
}

func TestDamagedSealedMarkerKeepsBackupAndIdentityFreeRestore(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	sealedMint(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "player-initialized"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := backup.AdoptPlayerState(dir); err != nil {
		t.Fatalf("completion rejected damaged marker: %v", err)
	}
	if err := backup.CheckPlayerStart(dir); err != nil {
		t.Fatalf("damaged marker rejected matching floor: %v", err)
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")})
	if err != nil {
		t.Fatalf("backup refused damaged marker: %v", err)
	}
	for _, name := range []string{"player-uuid", "player-ids.json"} {
		if err := os.Remove(filepath.Join(snap, name)); err != nil {
			t.Fatal(err)
		}
	}
	before := readBytes(t, filepath.Join(dir, "player-initialized"))
	if err := backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dir, "player-uuid")}); err != nil {
		t.Fatalf("identity-free restore refused: %v", err)
	}
	if !bytes.Equal(before, readBytes(t, filepath.Join(dir, "player-initialized"))) {
		t.Fatal("damaged marker was rewritten")
	}
	if err := os.Remove(filepath.Join(dir, "player-ids.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err == nil {
		t.Fatal("damaged marker allowed floor loss")
	}
}

func TestSealedExemptionConsumedByEveryEvidenceAndLifecycle(t *testing.T) {
	reservationStorage(t)
	for _, evidence := range []string{"player-ids.json", "player-uuid", "player-queue.json"} {
		for _, op := range []string{"adopt", "backup", "restore"} {
			t.Run(evidence+"/"+op, func(t *testing.T) {
				dir := t.TempDir()
				if _, err := backup.DiscardPlayerFloor(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, evidence), []byte("evidence"), 0600); err != nil {
					t.Fatal(err)
				}
				switch op {
				case "adopt":
					if err := backup.AdoptPlayerState(dir); err != nil {
						t.Fatal(err)
					}
				case "backup":
					_, _ = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")})
				case "restore":
					_ = fixedRestore(t, identitySnapshot(t, true), dir, 0)
				}
				var marker struct{ Sealed, Minted bool }
				if err := json.Unmarshal(readBytes(t, filepath.Join(dir, "player-initialized")), &marker); err != nil {
					t.Fatal(err)
				}
				if !marker.Sealed || !marker.Minted {
					t.Fatalf("exemption not consumed: %+v", marker)
				}
			})
		}
	}
	t.Run("legacy_reset", func(t *testing.T) {
		dir := t.TempDir()
		sealedMint(t, dir)
		t.Run("engine", func(t *testing.T) { firstValues(t, dir, markAt(t, dir)) })
		if err := os.WriteFile(filepath.Join(dir, "player-initialized"), []byte(`{"version":1,"sealed":true}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := backup.ResetPlayer(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "player-ids.json")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backup.EnsurePlayerUUID(dir); err == nil || !strings.Contains(err.Error(), "floor is missing") {
			t.Fatalf("reset reopened exemption: %v", err)
		}
	})
}

func TestCompletionErrorDoesNotBlockBackupOrIdentityFreeRestore(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	sealedMint(t, dir)
	if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
		t.Fatal(err)
	}
	// A seal a newer bridge wrote is one completion refuses to read. (A
	// directory at the seal path made the error until round 11, which keeps
	// a non-file seal and completes beside it.)
	if err := os.WriteFile(filepath.Join(dir, "player-floor-discarded.json"), []byte(`{"version":2,"sealed":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	rec := loggingtest.Record(t)
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")})
	if err != nil {
		t.Errorf("backup blocked by completion: %v", err)
	}
	if failed := rec.Lines("player seal completion failed"); len(failed) != 1 {
		t.Fatalf("no completion error to go on past: %v", failed)
	}
	// A separate ordinary snapshot proves restore even if backup above refuses.
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "tokens.json"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: source, TokensJSON: filepath.Join(source, "tokens.json")})
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dir, "player-uuid"), TokensJSON: filepath.Join(dir, "tokens.json")}); err != nil {
		t.Fatalf("identity-free restore blocked by completion: %v", err)
	}
	if got := string(readBytes(t, filepath.Join(dir, "tokens.json"))); got != "replacement" {
		t.Fatalf("restore did not import ordinary data: %q", got)
	}
}

func TestDiscardExemptionWaitsForFloorRenameBarrier(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	sealedMint(t, dir)
	floorMoved := false
	previous := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		err := os.Rename(a, b)
		if err == nil && filepath.Base(a) == "player-ids.json" {
			floorMoved = true
		}
		return err
	})
	defer atomicwrite.SetRenameFuncForTest(previous)
	stopped := errors.New("floor rename barrier")
	undo := atomicwrite.SetSyncDirForTest(func(string) error {
		if floorMoved {
			return stopped
		}
		return nil
	})
	_, err := backup.DiscardPlayerFloor(dir)
	undo()
	if !errors.Is(err, stopped) {
		t.Fatalf("floor barrier ignored: %v", err)
	}
	var marker struct{ Sealed, Minted bool }
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, "player-initialized")), &marker); err != nil {
		t.Fatal(err)
	}
	if !marker.Sealed || !marker.Minted {
		t.Fatalf("exemption opened before floor rename barrier: %+v", marker)
	}
	if _, err := backup.DiscardPlayerFloor(dir); err != nil {
		t.Fatal(err)
	}
	// Unmarshal into a fresh value: false is omitted on disk.
	var fresh struct{ Sealed, Minted bool }
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, "player-initialized")), &fresh); err != nil {
		t.Fatal(err)
	}
	if !fresh.Sealed || fresh.Minted {
		t.Fatalf("completed discard did not reopen exemption: %+v", fresh)
	}
}
