package backup_test

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

func TestMintExhaustionWritesNothing(t *testing.T) {
	reservationStorage(t)
	for _, tokens := range []bool{false, true} {
		t.Run(map[bool]string{false: "ids", true: "tokens"}[tokens], func(t *testing.T) {
			dir := t.TempDir()
			ids, toks := uint32(math.MaxUint32-4096), uint32(10)
			if tokens {
				ids, toks = toks, ids
			}
			putMark(t, dir, boxPlayerUUID, ids, toks)
			before := readBytes(t, filepath.Join(dir, "player-ids.json"))
			if _, _, err := backup.EnsurePlayerUUID(dir); err == nil || !strings.Contains(err.Error(), "--discard-floor") {
				t.Fatalf("exhausted mint: %v", err)
			}
			if !bytes.Equal(before, readBytes(t, filepath.Join(dir, "player-ids.json"))) {
				t.Fatal("refused mint changed floor")
			}
			for _, name := range []string{"player-uuid", "player-initialized"} {
				if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("mint wrote %s", name)
				}
			}
			if _, err := backup.DiscardPlayerFloor(dir); err != nil {
				t.Fatal(err)
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			firstValues(t, dir, markAt(t, dir))
		})
	}
}

func TestStartRequiresTheFloorsIdentity(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"mismatch", "symlink", "v1"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "player-uuid")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				other := filepath.Join(t.TempDir(), "uuid")
				if err := config.WritePlayerUUIDFile(other, markAt(t, dir).UUID); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Skip(err)
				}
			} else if err := config.WritePlayerUUIDFile(path, boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			if kind == "v1" {
				if err := os.WriteFile(filepath.Join(dir, "player-ids.json"), []byte(`{"version":1,"ids":20000,"tokens":30000}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := readBytes(t, filepath.Join(dir, "player-ids.json"))
			if err := backup.CheckPlayerStart(dir); err == nil || !strings.Contains(err.Error(), "bridge reset-player") {
				t.Fatalf("accepted %s: %v", kind, err)
			}
			if _, err := backup.ResetPlayer(dir); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, readBytes(t, filepath.Join(dir, "player-ids.json"))) {
				t.Fatal("reset changed floor")
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			firstValues(t, dir, markAt(t, dir))
		})
	}
}

func TestBackupNeverPairsAcrossDiscard(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	captured := false
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		if err := os.Rename(a, b); err != nil {
			return err
		}
		if filepath.Base(b) == "player-uuid" && filepath.Dir(b) != dir {
			captured = true
			if _, err := backup.DiscardPlayerFloor(dir); err != nil {
				return err
			}
			_, _, err := backup.EnsurePlayerUUID(dir)
			return err
		}
		return nil
	})
	defer atomicwrite.SetRenameFuncForTest(prev)
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")})
	if !captured || err == nil || snap != "" || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("mixed snapshot: %q %v captured=%t", snap, err, captured)
	}
}

func TestDiscardSurvivesAnyOneLocalFileLoss(t *testing.T) {
	reservationStorage(t)
	snap := identitySnapshot(t, true)
	for _, started := range []bool{false, true} {
		for _, name := range []string{"player-floor-discarded.json", "player-initialized", "player-ids.json", "player-uuid", "player-queue.json"} {
			for _, damage := range []string{"delete", "truncate", "replace"} {
				t.Run(name+"/"+damage+map[bool]string{false: "/before_start", true: "/after_start"}[started], func(t *testing.T) {
					dir := t.TempDir()
					if _, err := backup.DiscardPlayerFloor(dir); err != nil {
						t.Fatal(err)
					}
					if started {
						if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
							t.Fatal(err)
						}
						t.Run("engine", func(t *testing.T) { firstValues(t, dir, markAt(t, dir)) })
					}
					path := filepath.Join(dir, name)
					if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					if damage != "delete" {
						b := []byte{}
						if damage == "replace" {
							b = []byte(`{"version":1}`)
						}
						if err := os.WriteFile(path, b, 0600); err != nil {
							t.Fatal(err)
						}
					}
					if err := fixedRestore(t, snap, dir, 0); err == nil {
						t.Fatal("one file loss reopened identity restores")
					}
				})
			}
		}
	}
}

func TestUUIDOnlyUpgradeAndFloorLossBoundary(t *testing.T) {
	reservationStorage(t)
	for _, extra := range []string{"", "player-initialized", "player-floor-discarded.json", "player-queue.json"} {
		for _, op := range []string{"start", "backup"} {
			t.Run(op+"/"+extra, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "player-uuid")
				if err := config.WritePlayerUUIDFile(path, boxPlayerUUID); err != nil {
					t.Fatal(err)
				}
				if extra != "" {
					if err := os.WriteFile(filepath.Join(dir, extra), []byte(`{"version":1}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if op == "start" {
					_, _, err = backup.EnsurePlayerUUID(dir)
				} else {
					_, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: path})
				}
				if extra != "" {
					if err == nil || !strings.Contains(err.Error(), "--discard-floor") || strings.Contains(err.Error(), "again") {
						t.Fatalf("floor loss remedy: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("bind-only upgrade: %v", err)
				}
				if id, err := config.ReadPlayerUUID(dir); err != nil || id != boxPlayerUUID {
					t.Fatalf("upgrade rotated UUID: %s %v", id, err)
				}
				r := markAt(t, dir)
				if r.UUID != boxPlayerUUID || r.IDs != 0 || r.Tokens != 0 {
					t.Fatalf("upgrade floor: %+v", r)
				}
				if _, err := os.Stat(filepath.Join(dir, "player-initialized")); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(dir, "player-floor-discarded.json")); !os.IsNotExist(err) {
					t.Fatal("upgrade sealed box")
				}
				firstValues(t, dir, r)
			})
		}
	}
}

func TestFloorLossRemediesCanBeFollowed(t *testing.T) {
	reservationStorage(t)
	snap := identitySnapshot(t, true)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "player-queue.json"), []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backup.CheckPlayerStart(dir); err == nil || !strings.Contains(err.Error(), "--discard-floor") {
		t.Fatalf("orphan remedy: %v", err)
	}
	if err := fixedRestore(t, snap, dir, 0); err == nil || !strings.Contains(err.Error(), "seals all") || !strings.Contains(err.Error(), "copy of the snapshot") {
		t.Fatalf("restore loss remedy: %v", err)
	}
	if _, err := backup.DiscardPlayerFloor(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	firstValues(t, dir, markAt(t, dir))
}

func TestBackupRetryOnlyWhenUUIDVanished(t *testing.T) {
	reservationStorage(t)
	for _, vanished := range []bool{false, true} {
		t.Run(map[bool]string{false: "uuid_remains", true: "uuid_vanished"}[vanished], func(t *testing.T) {
			dir := t.TempDir()
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
				if err := os.Rename(a, b); err != nil {
					return err
				}
				if filepath.Base(b) == "player-uuid" && filepath.Dir(b) != dir {
					if err := os.Remove(filepath.Join(dir, "player-ids.json")); err != nil {
						return err
					}
					if vanished {
						return os.Remove(filepath.Join(dir, "player-uuid"))
					}
				}
				return nil
			})
			_, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, "player-uuid")})
			atomicwrite.SetRenameFuncForTest(prev)
			if err == nil || strings.Contains(err.Error(), "run the backup again") != vanished {
				t.Fatalf("retry advice: %v", err)
			}
			if !vanished && (!strings.Contains(err.Error(), "--discard-floor") || !strings.Contains(err.Error(), "seals all")) {
				t.Fatalf("loss advice: %v", err)
			}
		})
	}
}

func TestDiscardSecondSealIsDurableBeforeRemoval(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	before := readBytes(t, filepath.Join(dir, "player-ids.json"))
	landed := false
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		err := os.Rename(a, b)
		if err == nil && filepath.Base(b) == "player-initialized" {
			landed = true
		}
		return err
	})
	undo := atomicwrite.SetSyncDirForTest(func(string) error {
		if landed {
			return os.ErrPermission
		}
		return nil
	})
	_, err := backup.DiscardPlayerFloor(dir)
	undo()
	atomicwrite.SetRenameFuncForTest(prev)
	if err == nil {
		t.Fatal("discard ignored second seal barrier")
	}
	if !bytes.Equal(before, readBytes(t, filepath.Join(dir, "player-ids.json"))) {
		t.Fatal("floor removed before both seals")
	}
	if _, err := config.ReadPlayerUUID(dir); err != nil {
		t.Fatal("UUID removed before both seals")
	}
	if _, err := backup.DiscardPlayerFloor(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	firstValues(t, dir, markAt(t, dir))
}

func TestBothLocalMarkersRefuseOnlyWholeNewerVersions(t *testing.T) {
	reservationStorage(t)
	for _, name := range []string{"player-initialized", "player-floor-discarded.json"} {
		for _, body := range []string{`{"version":9}`, `{"version":9`, "", `{"version":1,"future":true}`} {
			t.Run(name+"/"+body, func(t *testing.T) {
				dir := t.TempDir()
				if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				err := backup.CheckPlayerStart(dir)
				if body == `{"version":9}` {
					if err == nil || !strings.Contains(err.Error(), "newer bridge") {
						t.Fatalf("newer marker: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("damaged marker was not conservative: %v", err)
				}
			})
		}
	}
}
