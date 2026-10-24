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

func TestRestoreAdoptsUUIDOnlyAfter8193Starts(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "player-uuid")
	if err := config.WritePlayerUUIDFile(path, boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, dir, boxPlayerUUID, 0, 0)
	var highest uint32
	var snap string
	for i := 0; i < 8193; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			token := startRestoredPlayer(t, dir).Snapshot().Token()
			if token != uint32(i)*8192+4097 {
				t.Fatalf("startup token %d at start %d", token, i)
			}
			highest = max(highest, token)
		})
		if i == 0 {
			var err error
			snap, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: path})
			if err != nil {
				t.Fatal(err)
			}
			if floor := markAt(t, snap); floor.IDs != 8192 || floor.Tokens != 8192 {
				t.Fatalf("first reservation: %+v", floor)
			}
		}
	}
	for _, name := range []string{"player-queue.json", "player-initialized", "player-floor-discarded.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected %s: %v", name, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "player-ids.json")); err != nil {
		t.Fatal(err)
	}
	if err := fixedRestore(t, snap, dir, 0); err != nil {
		t.Fatal(err)
	}
	floor := markAt(t, dir)
	t.Run("restored", func(t *testing.T) {
		id, token := firstValues(t, dir, floor)
		if id <= highest || token <= highest {
			t.Fatalf("restored values %d/%d repeat pre-upgrade space through %d", id, token, highest)
		}
	})
	for _, v := range []uint32{floor.IDs, floor.Tokens} {
		if uint64(v) < 1<<31+1<<24 || uint64(v) >= 1<<31+2<<24 {
			t.Fatalf("restore skipped adoption before its jump: %+v", floor)
		}
	}
}

func TestDamagedMarkerWithMatchingFloorKeepsEveryLifecycle(t *testing.T) {
	reservationStorage(t)
	for _, sealed := range []bool{false, true} {
		for _, op := range []string{"start", "adopt", "backup", "restore", "reset"} {
			t.Run(fmt.Sprintf("sealed_%t/%s", sealed, op), func(t *testing.T) {
				dir := t.TempDir()
				if sealed {
					sealedMint(t, dir)
				} else if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
					t.Fatal(err)
				}
				var identitySnap string
				if op == "restore" && !sealed {
					var err error
					identitySnap, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")})
					if err != nil {
						t.Fatal(err)
					}
				}
				path := filepath.Join(dir, "player-initialized")
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := backup.CheckPlayerStart(dir); err != nil {
					t.Fatalf("damaged marker rejected matching floor: %v", err)
				}
				rec := loggingtest.Record(t)
				var err error
				switch op {
				case "start":
					_, _, err = backup.EnsurePlayerUUID(dir)
					if err == nil {
						t.Run("engine", func(t *testing.T) { firstValues(t, dir, markAt(t, dir)) })
					}
				case "adopt":
					err = backup.AdoptPlayerState(dir)
				case "backup":
					_, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")})
				case "restore":
					source := t.TempDir()
					var snap string
					snap, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: source})
					if err == nil {
						if identitySnap != "" {
							snap = identitySnap
						}
						err = backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dir, "player-uuid")})
					}
				case "reset":
					_, err = backup.ResetPlayer(dir)
				}
				if err != nil {
					t.Fatalf("%s refused damaged marker: %v", op, err)
				}
				if !bytes.Equal(readBytes(t, path), nil) {
					t.Fatal("damaged marker rewritten")
				}
				lines := rec.Lines("player initialization marker is damaged")
				if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "move it aside") || strings.Contains(strings.Join(lines, "\n"), "--discard-floor") {
					t.Fatalf("missing proportionate warning: %v", lines)
				}
				// Remove all allocation evidence: the damaged marker alone must keep the exemption spent.
				for _, name := range []string{"player-ids.json", "player-uuid", "player-queue.json"} {
					if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
				}
				if _, _, err := backup.EnsurePlayerUUID(dir); err == nil || !strings.Contains(err.Error(), "floor is missing") {
					t.Fatalf("damaged marker reopened exemption: %v", err)
				}
			})
		}
	}
}

func TestMovingDamagedMarkerAsideReensuresItsSpentState(t *testing.T) {
	reservationStorage(t)
	for _, sealed := range []bool{false, true} {
		t.Run(fmt.Sprint(sealed), func(t *testing.T) {
			dir := t.TempDir()
			if sealed {
				sealedMint(t, dir)
			} else if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "player-initialized")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path, path+".damaged"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			var marker struct {
				Version        int
				Sealed, Minted bool
			}
			if err := json.Unmarshal(readBytes(t, path), &marker); err != nil {
				t.Fatal(err)
			}
			if marker.Version != 1 || marker.Sealed != sealed || marker.Minted != sealed {
				t.Fatalf("reensured marker: %+v", marker)
			}
			if err := os.Remove(filepath.Join(dir, "player-ids.json")); err != nil {
				t.Fatal(err)
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err == nil {
				t.Fatal("reensured marker allowed floor loss")
			}
		})
	}
}

func TestInvalidMarkerEntriesPreserveEvidence(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"directory", "invalid_version"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			sealedMint(t, dir)
			path := filepath.Join(dir, "player-initialized")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if kind == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte(`{"version":0,"sealed":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatalf("invalid marker stopped player: %v", err)
			}
			if kind == "directory" {
				if st, err := os.Lstat(path); err != nil || !st.IsDir() {
					t.Fatalf("marker replaced: %v", err)
				}
			} else {
				if string(readBytes(t, path)) != `{"version":0,"sealed":true}` {
					t.Fatal("invalid marker rewritten")
				}
			}
			if _, err := backup.ResetPlayer(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "player-ids.json")); err != nil {
				t.Fatal(err)
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err == nil {
				t.Fatal("invalid marker reopened exemption")
			}
		})
	}
}

func TestRestoreAdoptionBarrierPrecedesSnapshotWrites(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, "player-uuid"), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	snap := identitySnapshot(t, true)
	markerFailure := errors.New("adoption marker barrier")
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		if filepath.Base(b) == "player-initialized" {
			return markerFailure
		}
		return os.Rename(a, b)
	})
	err := fixedRestore(t, snap, dir, 0)
	atomicwrite.SetRenameFuncForTest(prev)
	if !errors.Is(err, markerFailure) {
		t.Fatalf("restore bypassed adoption barrier: %v", err)
	}
	if id, err := config.ReadPlayerUUID(dir); err != nil || id != boxPlayerUUID {
		t.Fatalf("snapshot identity published before adoption: %s %v", id, err)
	}
	floor := markAt(t, dir)
	if floor.UUID != boxPlayerUUID || floor.IDs < 1<<31 || floor.Tokens < 1<<31 {
		t.Fatalf("failed adoption lost its floor: %+v", floor)
	}
	if err := fixedRestore(t, snap, dir, 0); err != nil {
		t.Fatal(err)
	}
	restored := markAt(t, dir)
	if uint64(restored.IDs) != uint64(floor.IDs)+1<<24 || uint64(restored.Tokens) != uint64(floor.Tokens)+1<<24 {
		t.Fatalf("retry lost adopted bound: %+v -> %+v", floor, restored)
	}
}
