package backup_test

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

func TestResetRetainsTheBoxFloor(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, dir, boxPlayerUUID, 2000000000, 2000000001)
	path := filepath.Join(dir, "player-ids.json")
	before := readBytes(t, path)
	if _, err := backup.ResetPlayer(dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("reset changed the floor: %s %v", after, err)
	}
	id, made, err := backup.EnsurePlayerUUID(dir)
	if err != nil || !made || id == boxPlayerUUID {
		t.Fatalf("mint after reset: %s %t %v", id, made, err)
	}
	r := markAt(t, dir)
	if r.UUID != id || r.IDs != 2000000000 || r.Tokens != 2000000001 {
		t.Fatalf("restamp lost floor: %+v", r)
	}
}

func TestInitializedWithoutFloorRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "player-initialized"), []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"start", "reset"} {
		t.Run(op, func(t *testing.T) {
			var err error
			if op == "start" {
				_, _, err = backup.EnsurePlayerUUID(dir)
			} else {
				_, err = backup.ResetPlayer(dir)
			}
			if err == nil || !strings.Contains(err.Error(), "--discard-floor") {
				t.Fatalf("lost floor accepted: %v", err)
			}
		})
	}
}

func TestBackupUUIDWithoutFloorRefuses(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	path := filepath.Join(dir, config.PlayerUUIDFile)
	if err := config.WritePlayerUUIDFile(path, boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "player-initialized"), []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: path}); err == nil || !strings.Contains(err.Error(), "--discard-floor") {
		t.Fatalf("missing floor accepted: %v", err)
	}
}

func TestEveryOldIdentityRestoresAboveTheBoxFloor(t *testing.T) {
	reservationStorage(t)
	oldY := identitySnapshot(t, true)
	for _, interrupted := range []bool{false, true} {
		for _, incoming := range []string{"X", "Y"} {
			t.Run(fmt.Sprintf("interrupted_%t_restore_%s", interrupted, incoming), func(t *testing.T) {
				dir := t.TempDir()
				if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
					t.Fatal(err)
				}
				putMark(t, dir, boxPlayerUUID, 10000, 10000)
				oldX, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
				if err != nil {
					t.Fatal(err)
				}
				putMark(t, dir, boxPlayerUUID, 2000000000, 2000000001)
				if interrupted {
					stop := errors.New("stop UUID commit")
					prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
						if b == filepath.Join(dir, config.PlayerUUIDFile) {
							return stop
						}
						return os.Rename(a, b)
					})
					err = fixedRestore(t, oldY, dir, 0)
					atomicwrite.SetRenameFuncForTest(prev)
					if !errors.Is(err, stop) {
						t.Fatalf("interruption: %v", err)
					}
					if err := backup.CheckPlayerStart(dir); err == nil {
						t.Fatal("start accepted an interrupted identity replacement")
					}
					if _, err := backup.ResetPlayer(dir); err != nil {
						t.Fatal(err)
					}
					if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
						t.Fatal(err)
					}
					t.Run("restart", func(t *testing.T) { firstValues(t, dir, markAt(t, dir)) })
				} else if err := fixedRestore(t, oldY, dir, 0); err != nil {
					t.Fatal(err)
				}
				if _, err := backup.ResetPlayer(dir); err != nil {
					t.Fatal(err)
				}
				snap := oldX
				if incoming == "Y" {
					snap = oldY
				}
				if err := fixedRestore(t, snap, dir, 0); err != nil {
					t.Fatal(err)
				}
				r := markAt(t, dir)
				if r.IDs <= 2000000000 || r.Tokens <= 2000000001 {
					t.Fatalf("old identity lost floor: %+v", r)
				}
			})
		}
	}
}

func TestDiscardFloorResumesEveryStepAndSealsRestores(t *testing.T) {
	reservationStorage(t)
	snap := identitySnapshot(t, true)
	for _, at := range []string{"player-floor-discarded.json", "player-queue.json", "player-uuid", "player-ids.json"} {
		t.Run(at, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "player-queue.json"), []byte("old queue"), 0600); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("crash")
			if _, err := backup.DiscardPlayerFloorAfterStep(dir, func(step string) error {
				if step == at {
					return stop
				}
				return nil
			}); !errors.Is(err, stop) {
				t.Fatalf("did not stop: %v", err)
			}
			if err := fixedRestore(t, snap, dir, 0); err == nil || !strings.Contains(err.Error(), "sealed") {
				t.Fatalf("seal bypass: %v", err)
			}
			if _, err := backup.DiscardPlayerFloor(dir); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"player-uuid", "player-queue.json", "player-ids.json"} {
				if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("left %s: %v", name, err)
				}
			}
			if _, made, err := backup.EnsurePlayerUUID(dir); err != nil || !made {
				t.Fatalf("fresh start after discard: %t %v", made, err)
			}
			if err := fixedRestore(t, snap, dir, 0); err == nil {
				t.Fatal("new start cleared seal")
			}
		})
	}
}

func TestPlayerMarkersCommitBeforeDependentWrites(t *testing.T) {
	reservationStorage(t)
	for _, marker := range []string{"player-initialized", "player-floor-discarded.json"} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			if marker == "player-floor-discarded.json" {
				if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
					t.Fatal(err)
				}
			}
			landed := false
			stop := errors.New("marker directory fsync failed")
			prevRename := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
				err := os.Rename(a, b)
				if err == nil && filepath.Base(b) == marker {
					landed = true
				}
				return err
			})
			defer atomicwrite.SetRenameFuncForTest(prevRename)
			failed := false
			restoreSync := atomicwrite.SetSyncDirForTest(func(string) error {
				if landed && !failed {
					failed = true
					return stop
				}
				return nil
			})
			var err error
			if marker == "player-initialized" {
				_, _, err = backup.EnsurePlayerUUID(dir)
			} else {
				_, err = backup.DiscardPlayerFloor(dir)
			}
			restoreSync()
			if !errors.Is(err, stop) {
				t.Fatalf("marker barrier failure: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "player-ids.json")); err != nil {
				t.Fatalf("floor lost: %v", err)
			}
			_, uuidErr := os.Stat(filepath.Join(dir, "player-uuid"))
			if marker == "player-initialized" && !os.IsNotExist(uuidErr) {
				t.Fatal("UUID committed before marker")
			}
			if marker == "player-floor-discarded.json" && uuidErr != nil {
				t.Fatal("discard continued past marker failure")
			}
		})
	}
}

func TestPristinePlayerCommitsFloorMarkerUUIDInOrder(t *testing.T) {
	if backup.SchemaVersion != 2 {
		t.Fatalf("snapshot format changed: schema %d", backup.SchemaVersion)
	}
	reservationStorage(t)
	dir := t.TempDir()
	var order []string
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error { order = append(order, filepath.Base(b)); return os.Rename(a, b) })
	defer atomicwrite.SetRenameFuncForTest(prev)
	id, made, err := backup.EnsurePlayerUUID(dir)
	if err != nil || !made {
		t.Fatalf("initialize: %v", err)
	}
	if strings.Join(order, ",") != "player-ids.json,player-initialized,player-uuid" {
		t.Fatalf("commit order: %v", order)
	}
	r := markAt(t, dir)
	if r.UUID != id || r.IDs != 0 || r.Tokens != 0 {
		t.Fatalf("initial floor: %+v", r)
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range snapshotFiles(t, snap) {
		if name == "player-initialized" || name == "player-floor-discarded.json" {
			t.Fatalf("captured local marker: %s", name)
		}
	}
}

func TestRestoreFloorPreflightWritesNothing(t *testing.T) {
	reservationStorage(t)
	snap := identitySnapshot(t, true)
	for _, kind := range []string{"seal", "dangling_seal", "marker_without_floor", "bad_floor", "dangling_floor"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			switch kind {
			case "seal":
				if err := os.WriteFile(filepath.Join(dir, "player-floor-discarded.json"), []byte(`{"version":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "dangling_seal":
				if err := os.Symlink("absent", filepath.Join(dir, "player-floor-discarded.json")); err != nil {
					t.Skip(err)
				}
			case "marker_without_floor":
				if err := os.WriteFile(filepath.Join(dir, "player-initialized"), []byte(`{"version":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "bad_floor":
				if err := os.WriteFile(filepath.Join(dir, "player-ids.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "dangling_floor":
				if err := os.Symlink("absent", filepath.Join(dir, "player-ids.json")); err != nil {
					t.Skip(err)
				}
			}
			writes := 0
			prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error { writes++; return os.Rename(a, b) })
			err := fixedRestore(t, snap, dir, 0)
			atomicwrite.SetRenameFuncForTest(prev)
			if err == nil || writes != 0 {
				t.Fatalf("preflight: %v writes=%d", err, writes)
			}
		})
	}
}

func TestOrdinaryResetCommitsQueueBeforeUUIDAndNeverChangesFloor(t *testing.T) {
	reservationStorage(t)
	for _, at := range []string{"player-queue.json", "player-uuid", "queue_barrier"} {
		t.Run(at, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			queue := filepath.Join(dir, "player-queue.json")
			if err := os.WriteFile(queue, []byte("old queue"), 0600); err != nil {
				t.Fatal(err)
			}
			before := readBytes(t, filepath.Join(dir, "player-ids.json"))
			stop := errors.New("interrupt reset")
			var restoreSync func()
			if at == "queue_barrier" {
				restoreSync = atomicwrite.SetSyncDirForTest(func(string) error {
					if _, err := os.Lstat(queue); os.IsNotExist(err) {
						return stop
					}
					return nil
				})
			} else {
				restoreSync = func() {}
			}
			_, err := backup.ResetPlayerAfterStep(dir, func(step string) error {
				if step == at {
					return stop
				}
				return nil
			})
			restoreSync()
			if !errors.Is(err, stop) {
				t.Fatalf("interruption missed: %v", err)
			}
			if !bytes.Equal(before, readBytes(t, filepath.Join(dir, "player-ids.json"))) {
				t.Fatal("reset touched floor")
			}
			_, idErr := os.Stat(filepath.Join(dir, "player-uuid"))
			if at != "player-uuid" && idErr != nil {
				t.Fatal("UUID removed before queue barrier")
			}
			if _, err := backup.ResetPlayer(dir); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, readBytes(t, filepath.Join(dir, "player-ids.json"))) {
				t.Fatal("retry touched floor")
			}
		})
	}
}

func TestLocalPlayerMarkersAreStrictAndNeverRewritten(t *testing.T) {
	reservationStorage(t)
	for _, body := range []string{`{"version":2}`} {
		dir := t.TempDir()
		putMark(t, dir, boxPlayerUUID, 2000000000, 2000000001)
		if err := config.WritePlayerUUIDFile(filepath.Join(dir, "player-uuid"), boxPlayerUUID); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "player-initialized"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backup.EnsurePlayerUUID(dir); err == nil {
			t.Fatalf("accepted marker %q", body)
		}
	}
	dir := t.TempDir()
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "player-initialized")
	body := []byte("  {\"version\":1}\n\n")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.ResetPlayer(dir); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, readBytes(t, path)) {
		t.Fatal("reset rewrote immutable marker")
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, readBytes(t, path)) {
		t.Fatal("mint rewrote immutable marker")
	}
}

func TestDiscardSealAllowsOnlyNonPlayerRestore(t *testing.T) {
	reservationStorage(t)
	src := primeLiveState(t, t.TempDir())
	withPlayerUUID(t, &src, snapshotPlayerUUID, 0600)
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, err := backup.DiscardPlayerFloor(dst); err != nil {
		t.Fatal(err)
	}
	id, _, err := backup.EnsurePlayerUUID(dst)
	if err != nil {
		t.Fatal(err)
	}
	target := backup.Targets{PlayerUUID: filepath.Join(dst, "player-uuid"), ManifestDB: filepath.Join(dst, "bridge.db"), TokensJSON: filepath.Join(dst, "tokens.json"), BridgeYAML: filepath.Join(dst, "bridge.yaml")}
	if err := backup.Restore(snap, target); err == nil {
		t.Fatal("seal allowed incoming identity")
	}
	if _, err := os.Stat(target.ManifestDB); !os.IsNotExist(err) {
		t.Fatalf("refusal wrote database: %v", err)
	}
	for _, name := range []string{"player-uuid", "player-ids.json"} {
		if err := os.Remove(filepath.Join(snap, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := backup.Restore(snap, target); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tokens.json", "bridge.yaml"} {
		if !bytes.Equal(readBytes(t, filepath.Join(snap, name)), readBytes(t, filepath.Join(dst, name))) {
			t.Fatalf("did not restore %s", name)
		}
	}
	// Restore advances backupEpoch, so inspect database contents rather than bytes.
	db, err := sql.Open("sqlite", target.ManifestDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM sentinel").Scan(&rows); err != nil {
		t.Fatalf("database not restored: %v", err)
	}
	if got, err := config.ReadPlayerUUID(dst); err != nil || got != id {
		t.Fatalf("identity changed: %s %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "player-floor-discarded.json")); err != nil {
		t.Fatal(err)
	}
}

func TestBackupReadsFloorAfterUUIDCapture(t *testing.T) {
	reservationStorage(t)
	for _, change := range []string{"raised", "discarded"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			id, _, err := backup.EnsurePlayerUUID(dir)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "player-ids.json")
			captured := false
			prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
				if err := os.Rename(a, b); err != nil {
					return err
				}
				if filepath.Base(b) == "player-uuid" && filepath.Dir(b) != dir {
					captured = true
					if change == "discarded" {
						return os.Remove(path)
					}
					putMark(t, dir, id, 2000000000, 2000000001)
				}
				return nil
			})
			defer atomicwrite.SetRenameFuncForTest(prev)
			snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")})
			if !captured {
				t.Fatal("did not observe UUID capture")
			}
			if change == "discarded" {
				if err == nil || !strings.Contains(err.Error(), "--discard-floor") {
					t.Fatalf("lost floor at capture: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r := markAt(t, snap); r.IDs != 2000000000 || r.Tokens != 2000000001 {
				t.Fatalf("captured floor before UUID: %+v", r)
			}
		})
	}
}
