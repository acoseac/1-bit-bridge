package backup_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// seedDestinationQueue is the old, UUID-less development format as well as
// the stamped format. Restore must discard either before replacing identity.
func seedDestinationQueue(t *testing.T, dir, id string, version int) []byte {
	t.Helper()
	stamp := ""
	if version == 2 {
		stamp = `,"uuid":"` + id + `"`
	}
	data := []byte(fmt.Sprintf(`{"version":%d%s,"revision":1,"ids":{"next":4098,"reserved":8192},"tokens":{"current":4098,"reserved":8192},"repeat":true,"shuffle":false,"selected":4097,"entries":[{"id":4097,"ref":"destination-track","meta":null}]}`, version, stamp))
	if err := os.WriteFile(filepath.Join(dir, "player-queue.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRestoreDiscardsOnlyAReplacedIdentitysQueue(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"foreign", "same", "missing_identity", "no_snapshot_identity"} {
		for _, version := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/v%d", kind, version), func(t *testing.T) {
				snap, dst := identitySnapshot(t, true), t.TempDir()
				id := boxPlayerUUID
				if kind == "same" {
					id = snapshotPlayerUUID
				}
				if kind != "missing_identity" {
					if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), id); err != nil {
						t.Fatal(err)
					}
				}
				before := seedDestinationQueue(t, dst, id, version)
				if kind == "no_snapshot_identity" {
					if err := os.Remove(filepath.Join(snap, config.PlayerUUIDFile)); err != nil {
						t.Fatal(err)
					}
				}
				if err := backup.RestoreWithOptions(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)}, backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 7})}); err != nil {
					t.Fatal(err)
				}
				keep := kind == "same" || kind == "no_snapshot_identity"
				data, err := os.ReadFile(filepath.Join(dst, "player-queue.json"))
				if keep {
					if err != nil || !bytes.Equal(data, before) {
						t.Fatalf("own queue changed: %v", err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("destination queue survived identity replacement: %v", err)
				}
				e := startRestoredPlayer(t, dst)
				if keep && e.Snapshot().Len() != 1 || !keep && e.Snapshot().Len() != 0 {
					t.Fatalf("queue entries under restored identity: %v", e.Snapshot().IDs())
				}
			})
		}
	}
}

func TestRestoreQueueDiscardIsDurableBeforeIdentity(t *testing.T) {
	reservationStorage(t)
	snap := identitySnapshot(t, true)
	prepare := func(t *testing.T) (string, backup.Targets) {
		dir := t.TempDir()
		if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
			t.Fatal(err)
		}
		putMark(t, dir, boxPlayerUUID, 90000, 80000)
		seedDestinationQueue(t, dir, boxPlayerUUID, 1)
		return dir, backup.Targets{PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}
	}
	_, target := prepare(t)
	events, _ := crashRestore(t, snap, target, 0)
	for i, event := range events {
		t.Run(fmt.Sprintf("%02d_%s", i+1, event), func(t *testing.T) {
			dir, target := prepare(t)
			_, crashed := crashRestore(t, snap, target, i+1)
			if !crashed {
				t.Fatal("seam not reached")
			}
			id, err := config.ReadPlayerUUID(dir)
			if err != nil {
				t.Fatal(err)
			}
			mark := markAt(t, dir)
			t.Run("start_after_crash", func(t *testing.T) {
				e := startRestoredPlayer(t, dir)
				if id == snapshotPlayerUUID && e.Snapshot().Len() != 0 {
					t.Fatal("restored identity served the destination's entry")
				}
				if e.Snapshot().Token() <= mark.Tokens {
					t.Fatal("start did not pass crash floor")
				}
			})
			if err := fixedRestore(t, snap, dir, 7); err != nil {
				t.Fatal(err)
			}
			if e := startRestoredPlayer(t, dir); e.Snapshot().Len() != 0 {
				t.Fatal("retry served another identity's entry")
			}
		})
	}
	for _, failure := range []string{"remove", "barrier", "missing_queue_barrier"} {
		t.Run(failure, func(t *testing.T) {
			dir, target := prepare(t)
			qp := filepath.Join(dir, "player-queue.json")
			if failure == "remove" {
				if err := os.Remove(qp); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(qp, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(qp, "keep"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "missing_queue_barrier" {
				if err := os.Remove(qp); err != nil {
					t.Fatal(err)
				}
			}
			broken := errors.New("queue removal barrier failed")
			reservationDone := false
			prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
				err := os.Rename(a, b)
				if filepath.Base(b) == engine.ReservationFileName {
					reservationDone = true
				}
				return err
			})
			// First directory sync after reservation rename commits that reservation;
			// later syncs must make the queue's absence durable, even on a retry.
			syncs := 0
			undo := atomicwrite.SetSyncDirForTest(func(string) error {
				if reservationDone {
					syncs++
					if failure == "missing_queue_barrier" && syncs > 2 {
						return broken
					}
					if failure == "barrier" {
						if _, err := os.Stat(qp); errors.Is(err, os.ErrNotExist) {
							return broken
						}
					}
				}
				return nil
			})
			err := backup.RestoreWithOptions(snap, target, backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 7})})
			undo()
			atomicwrite.SetRenameFuncForTest(prev)
			if err == nil {
				t.Fatal("restore ignored queue discard failure")
			}
			if failure != "remove" && !errors.Is(err, broken) {
				t.Fatalf("wrong refusal: %v", err)
			}
			if id, _ := config.ReadPlayerUUID(dir); id != boxPlayerUUID {
				t.Fatal("identity replaced before durable queue discard")
			}
		})
	}
}

func TestInterruptedRestoreBackupWithoutStartKeepsTheRaisedFloor(t *testing.T) {
	reservationStorage(t)
	for _, counter := range []string{"IDs", "tokens"} {
		t.Run(counter, func(t *testing.T) {
			snap, dst := identitySnapshot(t, true), t.TempDir()
			if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			ids, tokens := uint32(90000), uint32(80000)
			if counter == "IDs" {
				ids = 40000000
			} else {
				tokens = 40000000
			}
			putMark(t, dst, boxPlayerUUID, ids, tokens)
			prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
				if filepath.Base(b) == config.PlayerUUIDFile {
					return errors.New("interrupted before UUID")
				}
				return os.Rename(a, b)
			})
			err := fixedRestore(t, snap, dst, 7)
			atomicwrite.SetRenameFuncForTest(prev)
			if err == nil {
				t.Fatal("restore did not stop")
			}
			raised := markAt(t, dst)
			newSnap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dst, PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)})
			if err != nil {
				t.Fatal(err)
			}
			carried := markAt(t, newSnap)
			if carried.UUID != boxPlayerUUID || carried.IDs != raised.IDs || carried.Tokens != raised.Tokens {
				t.Fatalf("snapshot dropped the interrupted restore floor: %+v versus %+v", carried, raised)
			}
			elsewhere := t.TempDir()
			if err := fixedRestore(t, newSnap, elsewhere, 0); err != nil {
				t.Fatal(err)
			}
			got := markAt(t, elsewhere)
			if got.IDs <= raised.IDs || got.Tokens <= raised.Tokens {
				t.Fatal("restore dropped the floor")
			}
			firstValues(t, elsewhere, got)
		})
	}
}

func TestExhaustedLiveRemedyNeverRevivesAnOlderSameIdentity(t *testing.T) {
	reservationStorage(t)
	for _, counter := range []string{"IDs", "tokens"} {
		for _, version := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/v%d", counter, version), func(t *testing.T) {
				snap, dst := identitySnapshot(t, true), t.TempDir()
				if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), snapshotPlayerUUID); err != nil {
					t.Fatal(err)
				}
				putMark(t, snap, snapshotPlayerUUID, 3000000000, 3000000000)
				ids, tokens := uint32(3000000000), uint32(3000000000)
				if counter == "IDs" {
					ids = 4250000000
				} else {
					tokens = 4250000000
				}
				putMark(t, dst, snapshotPlayerUUID, ids, tokens)
				seedDestinationQueue(t, dst, snapshotPlayerUUID, version)
				err := fixedRestore(t, snap, dst, 0)
				if err == nil {
					t.Fatal("exhaustion accepted")
				}
				advice := err.Error()
				// Follow the actual error's advice. A legacy queue has no UUID, so
				// leaving it behind exposes its entries under the new identity.
				for _, name := range []string{config.PlayerUUIDFile, engine.ReservationFileName} {
					if err := os.Remove(filepath.Join(dst, name)); err != nil {
						t.Fatal(err)
					}
				}
				if strings.Contains(err.Error(), "player-queue.json") {
					if err := os.Remove(filepath.Join(dst, "player-queue.json")); err != nil {
						t.Fatal(err)
					}
				}
				if strings.Contains(err.Error(), "from a copy of the snapshot as well") {
					for _, name := range []string{config.PlayerUUIDFile, engine.ReservationFileName} {
						if err := os.Remove(filepath.Join(snap, name)); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)}); err != nil {
					t.Fatal(err)
				}
				id, _, err := config.EnsurePlayerUUID(dst)
				if err != nil {
					t.Fatal(err)
				}
				if id == snapshotPlayerUUID {
					got := markAt(t, dst)
					if got.IDs < ids || got.Tokens < tokens {
						t.Fatalf("stated remedy revived the same UUID below its live floor: %+v, was %d/%d", got, ids, tokens)
					}
					t.Fatal("live exhaustion remedy did not make a new identity")
				}
				e := startRestoredPlayer(t, dst)
				if e.Snapshot().Len() != 0 || e.Snapshot().Token() == 0 {
					t.Fatalf("new identity did not start empty: visible IDs %v", e.Snapshot().IDs())
				}
				assertPlayerResetAdvice(t, advice, "restore")
			})
		}
	}
}

// assertPlayerResetAdvice pins the complete shared remedy independently of the
// production builder: a snapshot may contain an older, correctly stamped floor.
func assertPlayerResetAdvice(t *testing.T, advice, operation string) {
	t.Helper()
	const restoreStep = "remove player-uuid and player-ids.json from a copy of the snapshot as well, then restore that copy"
	if got, want := strings.Contains(advice, restoreStep), operation == "restore"; got != want {
		t.Errorf("%s refusal restore step present = %t, want %t: %s", operation, got, want, advice)
	}
	for _, required := range []string{
		"with the service stopped, reset the player: remove the box's player-uuid, player-ids.json and player-queue.json together",
		"this reset gives up for good the identity it removes and any the box ran earlier, unless, after this box last ran it, another box took it over from this box, by restoring a snapshot this box made, and has not reset it since, running or powered off",
		"no snapshot of one of them taken before the reset, on this box or another, may restore its player identity again",
		"remove player-uuid and player-ids.json from every such snapshot, or delete those snapshots",
		"reset any other box that still holds one of them before it runs again",
		"an identity another box took over from this box, after this box last ran it, and has not reset since, keeps its snapshots",
		"the next start makes a new identity with no queue, a new device to controllers",
		"a snapshot's player-ids.json is never a valid repair",
		"do not delete the reservation while keeping the player identity",
	} {
		if !strings.Contains(advice, required) {
			t.Errorf("refusal lacks %q: %s", required, advice)
		}
	}
	for _, unsafe := range []string{"known valid copy", "repair the reservation from", "repair it from"} {
		if strings.Contains(advice, unsafe) {
			t.Errorf("unsafe repair advice %q: %s", unsafe, advice)
		}
	}
}

// assertNoPlayerResetAdvice rejects a destructive remedy for a failed read
// that established nothing about the reservation's contents.
func assertNoPlayerResetAdvice(t *testing.T, message string) {
	t.Helper()
	for _, advice := range []string{"reset the player", "remove the box's", "gives up for good", "new device to controllers"} {
		if strings.Contains(message, advice) {
			t.Errorf("read failure carries reset advice %q: %s", advice, message)
		}
	}
}

func TestUnreadableReservationAdviceResetsIdentityAndQueue(t *testing.T) {
	reservationStorage(t)
	for _, operation := range []string{"backup", "restore"} {
		for _, kind := range []string{"unreadable", "invalid", "unsupported_version"} {
			t.Run(operation+"/"+kind, func(t *testing.T) {
				snap, dst := identitySnapshot(t, true), t.TempDir()
				uuidPath := filepath.Join(dst, config.PlayerUUIDFile)
				if err := config.WritePlayerUUIDFile(uuidPath, snapshotPlayerUUID); err != nil {
					t.Fatal(err)
				}
				beforeQueue := seedDestinationQueue(t, dst, snapshotPlayerUUID, 1)
				rp := filepath.Join(dst, engine.ReservationFileName)
				if kind == "unreadable" {
					if err := os.Mkdir(rp, 0700); err != nil {
						t.Fatal(err)
					}
				} else {
					data := []byte("broken")
					if kind == "unsupported_version" {
						data = []byte(`{"version":99,"ids":1,"tokens":2}`)
					}
					if err := os.WriteFile(rp, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if operation == "backup" {
					_, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dst, PlayerUUID: uuidPath})
				} else {
					err = fixedRestore(t, snap, dst, 0)
				}
				if err == nil {
					t.Fatal("unreadable reservation accepted")
				}
				if operation == "backup" && kind == "unreadable" {
					assertNoPlayerResetAdvice(t, err.Error())
					var pathErr *os.PathError
					if !errors.As(err, &pathErr) || filepath.Base(pathErr.Path) != engine.ReservationFileName {
						t.Errorf("backup lost its file-access error: %v", err)
					}
				} else {
					assertPlayerResetAdvice(t, err.Error(), operation)
				}
				if id, err := config.ReadPlayerUUID(dst); err != nil || id != snapshotPlayerUUID {
					t.Fatalf("refusal changed identity: %s, %v", id, err)
				}
				if !bytes.Equal(beforeQueue, readBytes(t, filepath.Join(dst, "player-queue.json"))) {
					t.Fatal("refusal changed queue")
				}
				if kind == "invalid" && string(readBytes(t, rp)) != "broken" {
					t.Fatal("refusal replaced unreadable reservation")
				}
			})
		}
	}
}
