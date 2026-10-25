package backup_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

func TestAdoptionJumpsPastAnEngineThatLostOnlyItsFloor(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	// Main could publish a token without ever writing a queue or marker.
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, "player-uuid"), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	// Supply the initial zero reservation that main created at its first start.
	putMark(t, dir, boxPlayerUUID, 0, 0)
	var oldToken uint32
	t.Run("main_engine", func(t *testing.T) { oldToken = startRestoredPlayer(t, dir).Snapshot().Token() })
	if oldToken != 4097 {
		t.Fatalf("main token = %d", oldToken)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-queue.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected queue: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "player-ids.json")); err != nil {
		t.Fatal(err)
	}
	id, made, err := backup.EnsurePlayerUUID(dir)
	if err != nil || made || id != boxPlayerUUID {
		t.Fatalf("adoption: %s %t %v", id, made, err)
	}
	floor := markAt(t, dir)
	t.Run("adopted_engine", func(t *testing.T) {
		token := startRestoredPlayer(t, dir).Snapshot().Token()
		if token <= oldToken {
			t.Fatalf("token reused: %d <= %d", token, oldToken)
		}
	})
	if floor.IDs < 1<<24 || floor.Tokens < 1<<24 {
		t.Fatalf("adopted floor did not jump: %+v", floor)
	}
}

func TestDiscardFloorExemptionIsConsumedByMint(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := backup.DiscardPlayerFloor(dir); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
			t.Fatal(err)
		}
		t.Run("engine", func(t *testing.T) { firstValues(t, dir, markAt(t, dir)) })
		before := readBytes(t, filepath.Join(dir, "player-ids.json"))
		if _, err := backup.ResetPlayer(dir); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, readBytes(t, filepath.Join(dir, "player-ids.json"))) {
			t.Fatal("reset changed floor")
		}
		if err := os.Remove(filepath.Join(dir, "player-ids.json")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backup.EnsurePlayerUUID(dir); err == nil || !strings.Contains(err.Error(), "floor is missing") {
			t.Fatalf("post-discard floor loss accepted: %v", err)
		}
	}
}

func TestLoneDiscardSealIsCompletedByLifecycle(t *testing.T) {
	reservationStorage(t)
	snap := identitySnapshot(t, true)
	for _, op := range []string{"start", "reset", "restore", "backup"} {
		t.Run(op, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"player-initialized", "player-floor-discarded.json"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"version":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch op {
			case "start":
				if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
					t.Fatal(err)
				}
			case "reset":
				if _, err := backup.ResetPlayer(dir); err != nil {
					t.Fatal(err)
				}
			case "restore":
				if err := fixedRestore(t, snap, dir, 0); err == nil {
					t.Fatal("restore accepted seal")
				}
			case "backup":
				if _, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")}); err != nil {
					t.Fatal(err)
				}
			}
			var marker struct {
				Sealed bool
				Minted bool
			}
			if err := json.Unmarshal(readBytes(t, filepath.Join(dir, "player-initialized")), &marker); err != nil {
				t.Fatal(err)
			}
			if !marker.Sealed {
				t.Fatal("lone seal not completed")
			}
			if err := os.Remove(filepath.Join(dir, "player-floor-discarded.json")); err != nil {
				t.Fatal(err)
			}
			if err := fixedRestore(t, snap, dir, 0); err == nil || !strings.Contains(err.Error(), "sealed") {
				t.Fatalf("single seal loss reopened restore: %v", err)
			}
		})
	}
}

func TestPostDiscardMintMarkerIsDurableBeforeUUID(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if _, err := backup.DiscardPlayerFloor(dir); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("minted marker barrier")
	landed := false
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		err := os.Rename(a, b)
		if err == nil && filepath.Base(b) == "player-initialized" {
			landed = true
		}
		return err
	})
	defer atomicwrite.SetRenameFuncForTest(prev)
	undo := atomicwrite.SetSyncDirForTest(func(string) error {
		if landed {
			return stop
		}
		return nil
	})
	_, _, err := backup.EnsurePlayerUUID(dir)
	undo()
	if !errors.Is(err, stop) {
		t.Fatalf("ignored minted barrier: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-ids.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-uuid")); !os.IsNotExist(err) {
		t.Fatalf("UUID published before minted marker: %v", err)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLoneSealCompletionRequiresFirstRecordDurable(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "player-floor-discarded.json"), []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("first seal barrier")
	undo := atomicwrite.SetSyncDirForTest(func(string) error { return stop })
	_, _, err := backup.EnsurePlayerUUID(dir)
	undo()
	if !errors.Is(err, stop) {
		t.Fatalf("ignored first seal barrier: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-initialized")); !os.IsNotExist(err) {
		t.Fatalf("second seal preceded first barrier: %v", err)
	}
}

func TestResetAdoptsUUIDOnlyWithJump(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, "player-uuid"), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.ResetPlayer(dir); err != nil {
		t.Fatal(err)
	}
	r := markAt(t, dir)
	if r.UUID != boxPlayerUUID || r.IDs < 1<<24 || r.Tokens < 1<<24 {
		t.Fatalf("reset adoption floor: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-uuid")); !os.IsNotExist(err) {
		t.Fatalf("reset kept identity: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-floor-discarded.json")); !os.IsNotExist(err) {
		t.Fatal("reset adoption sealed restores")
	}
}

func TestLoneSealPreflightAllowsCompletionBeforeFirstMint(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	for _, name := range []string{"player-floor-discarded.json", "player-initialized"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"version":1}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Bind runs the read-only preflight before saving configuration and minting.
	if err := backup.CheckPlayerStart(dir); err != nil {
		t.Fatalf("preflight prevented seal completion: %v", err)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	var marker struct{ Sealed, Minted bool }
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, "player-initialized")), &marker); err != nil {
		t.Fatal(err)
	}
	if !marker.Sealed || !marker.Minted {
		t.Fatalf("mint did not consume completed seal: %+v", marker)
	}
}
