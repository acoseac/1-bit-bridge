package backup_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

const historyFile = "player-history.json"

func putHistory(t *testing.T, dir string, covered, givenUp []string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "covered": covered, "givenUp": givenUp})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, historyFile), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRefusesGivenUpIdentity(t *testing.T) {
	reservationStorage(t)
	for _, where := range []string{"live", "snapshot"} {
		t.Run(where, func(t *testing.T) {
			snap, dst := identitySnapshot(t, true), t.TempDir()
			id, _, err := config.EnsurePlayerUUID(dst)
			if err != nil {
				t.Fatal(err)
			}
			var manifest backup.Manifest
			mp := filepath.Join(snap, backup.ManifestFile)
			if err := json.Unmarshal(readBytes(t, mp), &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.Files = append([]string{"bridge.db", "tokens.json"}, manifest.Files...)
			mb, _ := json.Marshal(manifest)
			if err := os.WriteFile(mp, mb, 0600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"bridge.db", "tokens.json"} {
				if err := os.WriteFile(filepath.Join(snap, name), []byte("snapshot "+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			histDir := dst
			if where == "snapshot" {
				histDir = snap
				var m backup.Manifest
				if err := json.Unmarshal(readBytes(t, filepath.Join(snap, backup.ManifestFile)), &m); err != nil {
					t.Fatal(err)
				}
				m.Files = append(m.Files, historyFile)
				b, _ := json.Marshal(m)
				if err := os.WriteFile(filepath.Join(snap, backup.ManifestFile), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			putHistory(t, histDir, nil, []string{snapshotPlayerUUID})
			for _, name := range []string{"tokens.json", "bridge.db", "bridge.db-wal", "bridge.db-shm"} {
				if err := os.WriteFile(filepath.Join(dst, name), []byte("keep live "+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := historyTree(t, dst)
			err = backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile), TokensJSON: filepath.Join(dst, "tokens.json"), ManifestDB: filepath.Join(dst, "bridge.db")})
			if err == nil || !strings.Contains(err.Error(), "given up") || !strings.Contains(err.Error(), "remove player-uuid and player-ids.json from a copy") {
				t.Errorf("restore must refuse with snapshot remedy, got %v", err)
			}
			if got, _ := config.ReadPlayerUUID(dst); got != id {
				t.Errorf("identity changed to %s", got)
			}
			if after := historyTree(t, dst); !bytes.Equal(before, after) {
				t.Error("refused restore wrote files")
			}
		})
	}
}

func historyTree(t *testing.T, dir string) []byte {
	t.Helper()
	all := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			all[entry.Name()] = string(readBytes(t, filepath.Join(dir, entry.Name())))
		}
	}
	b, _ := json.Marshal(all)
	return b
}

func TestSnapshotCarriesPlayerHistory(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	putHistory(t, dir, []string{boxPlayerUUID}, []string{snapshotPlayerUUID})
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(snapshotFiles(t, snap), historyFile) {
		t.Fatal("snapshot manifest omits player history")
	}
	var want, got any
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, historyFile)), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(readBytes(t, filepath.Join(snap, historyFile)), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"covered", "givenUp"} {
		a, _ := json.Marshal(want.(map[string]any)[key])
		b, _ := json.Marshal(got.(map[string]any)[key])
		if !bytes.Equal(a, b) {
			t.Errorf("history %s lost", key)
		}
	}
}

func TestPlayerResetRefusesEveryIdentityItsFloorCovered(t *testing.T) {
	reservationStorage(t)
	for _, previousRestore := range []bool{false, true} {
		t.Run(fmt.Sprint(previousRestore), func(t *testing.T) {
			dir := t.TempDir()
			if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			putMark(t, dir, boxPlayerUUID, 10000, 30000)
			old, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
			if err != nil {
				t.Fatal(err)
			}
			if previousRestore {
				if err := fixedRestore(t, identitySnapshot(t, true), dir, 0); err != nil {
					t.Fatal(err)
				}
			}
			// A snapshot can predate millions of values, beyond restore's random jump.
			liveID, err := config.ReadPlayerUUID(dir)
			if err != nil {
				t.Fatal(err)
			}
			putMark(t, dir, liveID, 2000000000, 2100000000)
			if changed, err := backup.ResetPlayer(dir); err != nil || !changed {
				t.Fatalf("reset: %t %v", changed, err)
			}
			newID, created, err := config.EnsurePlayerUUID(dir)
			if err != nil || !created {
				t.Fatalf("start: %t %v", created, err)
			}
			before := historyTree(t, dir)
			if err := fixedRestore(t, old, dir, 0); err == nil || !strings.Contains(err.Error(), "given up") {
				t.Fatalf("old identity admitted: %v", err)
			}
			if got, _ := config.ReadPlayerUUID(dir); got != newID {
				t.Fatal("new identity replaced")
			}
			if !bytes.Equal(before, historyTree(t, dir)) {
				t.Fatal("refusal wrote files")
			}
		})
	}
}

func TestPlayerResetResumesEveryDurableStep(t *testing.T) {
	reservationStorage(t)
	for _, stop := range []string{historyFile, config.PlayerUUIDFile, "player-queue.json", engine.ReservationFileName} {
		t.Run(stop, func(t *testing.T) {
			dir := t.TempDir()
			snap := identitySnapshot(t, true)
			if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), snapshotPlayerUUID); err != nil {
				t.Fatal(err)
			}
			putMark(t, dir, boxPlayerUUID, 90000, 80000)
			third := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			putHistory(t, dir, []string{third}, nil)
			seedDestinationQueue(t, dir, snapshotPlayerUUID, 1)
			interrupted := errors.New("interrupted reset")
			var steps []string
			_, err := backup.ResetPlayerAfterStep(dir, func(step string) error {
				steps = append(steps, step)
				if step == stop {
					return interrupted
				}
				return nil
			})
			if !errors.Is(err, interrupted) {
				t.Fatalf("seam: %v", err)
			}
			order := []string{historyFile, config.PlayerUUIDFile, "player-queue.json", engine.ReservationFileName}
			if !slices.Equal(steps, order[:slices.Index(order, stop)+1]) {
				t.Fatalf("removal order: %v", steps)
			}
			h := readHistoryMap(t, dir)
			for _, id := range []string{snapshotPlayerUUID, boxPlayerUUID, third} {
				if !slices.Contains(h.GivenUp, id) {
					t.Errorf("history lost %s", id)
				}
			}
			before := historyTree(t, dir)
			if err := fixedRestore(t, snap, dir, 0); err == nil || !strings.Contains(err.Error(), "given up") {
				t.Fatalf("interrupted restore: %v", err)
			}
			if !bytes.Equal(before, historyTree(t, dir)) {
				t.Fatal("refusal wrote files")
			}
			if changed, err := backup.ResetPlayer(dir); err != nil || !changed {
				t.Fatalf("resume: %t %v", changed, err)
			}
			for _, name := range order[1:] {
				if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("reset kept %s: %v", name, err)
				}
			}
			before = historyTree(t, dir)
			if changed, err := backup.ResetPlayer(dir); err != nil || changed {
				t.Fatalf("completed reset repeated: %t %v", changed, err)
			}
			if !bytes.Equal(before, historyTree(t, dir)) {
				t.Fatal("no identity changed files")
			}
			if _, _, err := config.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			if err := fixedRestore(t, snap, dir, 0); err == nil || !strings.Contains(err.Error(), "given up") {
				t.Fatalf("resume lost refusal: %v", err)
			}
		})
	}
}

type recordedHistory struct {
	Version          int
	Covered, GivenUp []string
	ResetPending     bool
}

func readHistoryMap(t *testing.T, dir string) recordedHistory {
	t.Helper()
	var h recordedHistory
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, historyFile)), &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPlayerHistoryTravelsAndMerges(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	old := identitySnapshot(t, true)
	if err := fixedRestore(t, old, dir, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.ResetPlayer(dir); err != nil {
		t.Fatal(err)
	}
	current, _, err := config.EnsurePlayerUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	carried, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
	if err != nil {
		t.Fatal(err)
	}
	for _, fresh := range []bool{true, false} {
		t.Run(fmt.Sprint(fresh), func(t *testing.T) {
			dst := t.TempDir()
			if !fresh {
				putHistory(t, dst, []string{boxPlayerUUID}, []string{"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"})
			}
			if err := fixedRestore(t, carried, dst, 0); err != nil {
				t.Fatal(err)
			}
			h := readHistoryMap(t, dst)
			if !slices.Contains(h.GivenUp, snapshotPlayerUUID) {
				t.Fatal("snapshot given-up identity lost")
			}
			if !fresh && (!slices.Contains(h.Covered, boxPlayerUUID) || !slices.Contains(h.GivenUp, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")) {
				t.Fatal("live history overwritten")
			}
			before := historyTree(t, dst)
			if err := fixedRestore(t, old, dst, 0); err == nil || !strings.Contains(err.Error(), "given up") {
				t.Fatalf("imported history did not refuse: %v", err)
			}
			if !bytes.Equal(before, historyTree(t, dst)) {
				t.Fatal("refusal wrote")
			}
			if got, _ := config.ReadPlayerUUID(dst); got != current {
				t.Fatal("identity changed")
			}
		})
	}
}

func TestPlayerHistoryReadFailuresRefuseOperations(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"corrupt", "version", "invalid_uuid", "trailing", "oversize", "directory", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "unreadable" && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("requires Unix permissions and non-root")
			}
			for _, op := range []string{"reset", "backup", "restore_live", "restore_snapshot"} {
				t.Run(op, func(t *testing.T) {
					dir, snap := t.TempDir(), identitySnapshot(t, true)
					if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
						t.Fatal(err)
					}
					hp := filepath.Join(dir, historyFile)
					if op == "restore_snapshot" {
						hp = filepath.Join(snap, historyFile)
						listSnapshotHistory(t, snap)
					}
					body := map[string]string{"corrupt": "{", "version": `{"version":2}`, "invalid_uuid": `{"version":1,"givenUp":["invalid"]}`, "trailing": `{"version":1} {}`, "oversize": strings.Repeat(" ", (1<<20)+1), "unreadable": `{"version":1}`}[kind]
					if kind == "directory" {
						if err := os.Mkdir(hp, 0700); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.WriteFile(hp, []byte(body), 0600); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "unreadable" {
						if err := os.Chmod(hp, 0); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = os.Chmod(hp, 0600) })
					}
					var err error
					switch op {
					case "reset":
						_, err = backup.ResetPlayer(dir)
					case "backup":
						_, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir})
					default:
						err = fixedRestore(t, snap, dir, 0)
					}
					if err == nil || !strings.Contains(err.Error(), historyFile) {
						t.Fatalf("%s accepted %s history: %v", op, kind, err)
					}
					if id, _ := config.ReadPlayerUUID(dir); id != boxPlayerUUID {
						t.Fatal("refusal replaced identity")
					}
					if _, err := os.Stat(filepath.Join(dir, engine.ReservationFileName)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("refusal wrote reservation")
					}
				})
			}
		})
	}
}

func listSnapshotHistory(t *testing.T, snap string) {
	t.Helper()
	var m backup.Manifest
	p := filepath.Join(snap, backup.ManifestFile)
	if err := json.Unmarshal(readBytes(t, p), &m); err != nil {
		t.Fatal(err)
	}
	m.Files = append(m.Files, historyFile)
	b, _ := json.Marshal(m)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPlayerHistoryDurabilityPrecedesIdentityChanges(t *testing.T) {
	reservationStorage(t)
	for _, op := range []string{"reset", "restore"} {
		for _, failure := range []string{"file_sync", "rename", "directory_sync"} {
			t.Run(op+"/"+failure, func(t *testing.T) {
				dir, snap := t.TempDir(), identitySnapshot(t, true)
				if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
					t.Fatal(err)
				}
				putMark(t, dir, boxPlayerUUID, 90000, 80000)
				q := seedDestinationQueue(t, dir, boxPlayerUUID, 1)
				broken := errors.New("history barrier failed")
				landed := false
				prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
					if filepath.Base(b) == historyFile {
						if failure == "rename" {
							return broken
						}
						landed = true
					}
					return os.Rename(a, b)
				})
				defer atomicwrite.SetRenameFuncForTest(prev)
				undoFile := atomicwrite.SetSyncFileForTest(func(f *os.File) error {
					if failure == "file_sync" && strings.Contains(f.Name(), ".player-history-") {
						return broken
					}
					return f.Sync()
				})
				defer undoFile()
				undoDir := atomicwrite.SetSyncDirForTest(func(string) error {
					if failure == "directory_sync" && landed {
						return broken
					}
					return nil
				})
				defer undoDir()
				var err error
				if op == "reset" {
					_, err = backup.ResetPlayer(dir)
				} else {
					err = fixedRestore(t, snap, dir, 0)
				}
				if !errors.Is(err, broken) {
					t.Fatalf("history durability ignored: %v", err)
				}
				if got, _ := config.ReadPlayerUUID(dir); got != boxPlayerUUID {
					t.Fatal("identity changed before history durable")
				}
				if !bytes.Equal(q, readBytes(t, filepath.Join(dir, "player-queue.json"))) {
					t.Fatal("queue removed before history durable")
				}
				if r := markAt(t, dir); r.UUID != boxPlayerUUID || r.IDs != 90000 {
					t.Fatal("floor replaced before history durable")
				}
			})
		}
	}
}

func TestPlayerHistoryWriterKeepsOwnerAndMode(t *testing.T) {
	reservationStorage(t)
	if runtime.GOOS == "windows" {
		t.Skip("Unix owner and modes")
	}
	dir := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putHistory(t, dir, nil, nil)
	if err := os.Chmod(filepath.Join(dir, historyFile), 0644); err != nil {
		t.Fatal(err)
	}
	changes, undo := fsutil.SimulateRootForTest(4242, 4243)
	defer undo()
	if _, err := backup.ResetPlayer(dir); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertGivenAway(t, changes(), []string{filepath.Join(real, historyFile), filepath.Join(real, historyFile)})
	if got := mode(t, filepath.Join(dir, historyFile)); got.Perm() != 0600 {
		t.Fatalf("mode %o", got)
	}
}

func TestPlayerHistorySnapshotRequiresDurability(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	putHistory(t, dir, nil, []string{snapshotPlayerUUID})
	landed := false
	broken := errors.New("history directory sync failed")
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		if filepath.Base(b) == historyFile {
			landed = true
		}
		return os.Rename(a, b)
	})
	defer atomicwrite.SetRenameFuncForTest(prev)
	undo := atomicwrite.SetSyncDirForTest(func(string) error {
		if landed {
			return broken
		}
		return nil
	})
	defer undo()
	if _, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir}); !errors.Is(err, broken) {
		t.Fatalf("snapshot accepted history without durable directory: %v", err)
	}
}

func TestPlayerResetCommitsEachRemovalBeforeDroppingTheFloor(t *testing.T) {
	reservationStorage(t)
	for _, name := range []string{config.PlayerUUIDFile, "player-queue.json", engine.ReservationFileName} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			putMark(t, dir, boxPlayerUUID, 90000, 80000)
			seedDestinationQueue(t, dir, boxPlayerUUID, 1)
			broken := errors.New("unlink barrier failed")
			undo := atomicwrite.SetSyncDirForTest(func(string) error {
				if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
					return broken
				}
				return nil
			})
			_, err := backup.ResetPlayer(dir)
			undo()
			if !errors.Is(err, broken) {
				t.Fatalf("removal durability ignored: %v", err)
			}
			if name != engine.ReservationFileName {
				if r := markAt(t, dir); r.IDs != 90000 {
					t.Fatal("floor removed before earlier unlink durable")
				}
			}
			if !readHistoryMap(t, dir).ResetPending {
				t.Fatal("incomplete reset not recorded")
			}
			if _, err := backup.ResetPlayer(dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPlayerHistorySchemaAndIdentitylessMerge(t *testing.T) {
	reservationStorage(t)
	if backup.SchemaVersion != 3 {
		t.Fatal("old restores would ignore player history")
	}
	src, dst := t.TempDir(), t.TempDir()
	putHistory(t, src, []string{boxPlayerUUID}, []string{snapshotPlayerUUID})
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: src})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := config.EnsurePlayerUUID(dst)
	if err != nil {
		t.Fatal(err)
	}
	putMark(t, dst, id, 12345, 67890)
	before := readBytes(t, filepath.Join(dst, engine.ReservationFileName))
	if err := backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := config.ReadPlayerUUID(dst); got != id {
		t.Fatal("identityless restore changed identity")
	}
	if !bytes.Equal(before, readBytes(t, filepath.Join(dst, engine.ReservationFileName))) {
		t.Fatal("identityless restore changed floor")
	}
	h := readHistoryMap(t, dst)
	if !slices.Contains(h.Covered, boxPlayerUUID) || !slices.Contains(h.GivenUp, snapshotPlayerUUID) {
		t.Fatal("identityless restore omitted history")
	}
	if err := os.Remove(filepath.Join(snap, historyFile)); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)}); err == nil {
		t.Fatal("listed missing history accepted")
	}
}

func TestPlayerHistoryRemembersForeignReservationBeforeRestore(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	foreign := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	putMark(t, dir, foreign, 90000, 80000)
	if err := fixedRestore(t, identitySnapshot(t, true), dir, 0); err != nil {
		t.Fatal(err)
	}
	if h := readHistoryMap(t, dir); !slices.Contains(h.Covered, foreign) || !slices.Contains(h.Covered, boxPlayerUUID) {
		t.Fatal("restore forgot the reservation's displaced UUID")
	}
	if _, err := backup.ResetPlayer(dir); err != nil {
		t.Fatal(err)
	}
	if h := readHistoryMap(t, dir); !slices.Contains(h.GivenUp, foreign) || !slices.Contains(h.GivenUp, boxPlayerUUID) {
		t.Fatal("reset forgot a UUID the replaced reservation covered")
	}
}

func TestPlayerResetOfDamagedReservationRecordsReadableIdentities(t *testing.T) {
	reservationStorage(t)
	for _, body := range []string{`{`, `{"version":2,"uuid":"invalid","ids":1,"tokens":1}`, `{"version":99,"uuid":"0f8fad5b-d9cb-469f-a165-70867728950e","ids":1,"tokens":1}`} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, engine.ReservationFileName), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := backup.ResetPlayer(dir); err != nil {
				t.Fatal(err)
			}
			h := readHistoryMap(t, dir)
			if !slices.Contains(h.GivenUp, boxPlayerUUID) {
				t.Fatal("live identity lost")
			}
			if strings.Contains(body, snapshotPlayerUUID) && !slices.Contains(h.GivenUp, snapshotPlayerUUID) {
				t.Fatal("readable reservation UUID lost")
			}
		})
	}
}
