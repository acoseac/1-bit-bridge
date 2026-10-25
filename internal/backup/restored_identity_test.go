package backup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// reservationStorage supplies barriers on platforms without directory fsync.
// The real durable writer still stages and renames; seam tests fail its barriers.
func reservationStorage(t *testing.T) {
	t.Helper()
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
}

func putMark(t *testing.T, dir, id string, ids, tokens uint32) {
	t.Helper()
	b, err := json.Marshal(engine.Reservation{Version: 2, UUID: id, IDs: ids, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, engine.ReservationFileName), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func markAt(t *testing.T, dir string) engine.Reservation {
	t.Helper()
	r, err := engine.ReadReservation(filepath.Join(dir, engine.ReservationFileName))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// identitySnapshot is this bridge's snapshot of snapshotPlayerUUID at a
// 10000/30000 floor. Unmarked, it is the snapshot a writer before
// reservation capture left: the UUID alone (stripSnapshotFloor).
func identitySnapshot(t *testing.T, marked bool) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, config.PlayerUUIDFile)
	if err := config.WritePlayerUUIDFile(p, snapshotPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, dir, snapshotPlayerUUID, 10000, 30000)
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: p})
	if err != nil {
		t.Fatal(err)
	}
	if !marked {
		stripSnapshotFloor(t, snap, true)
	}
	return snap
}

// fixedRestore restores snap's identity into dst with one injected draw per
// value, in the order restore reads them: a UUID-only destination's
// adoption first, then the jump. It fails the test when a restore that
// succeeds leaves any of them unread.
func fixedRestore(t *testing.T, snap, dst string, draws ...uint32) error {
	t.Helper()
	r := bytes.NewReader(randomDraws(draws...))
	err := backup.RestoreWithOptions(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)}, backup.RestoreOptions{Random: r})
	if err == nil && r.Len() != 0 {
		t.Fatalf("restore left %d of its %d injected random bytes unread", r.Len(), 3*len(draws))
	}
	return err
}

// idlePlayer supplies no hardware; these tests only insert and read queue IDs.
type idlePlayer struct{}

func (idlePlayer) Start(context.Context, engine.StartRequest) error {
	return errors.New("no playback in this test")
}
func (idlePlayer) Prepare(context.Context, engine.SessionID, engine.OccurrenceID, *engine.Occurrence) error {
	return nil
}
func (idlePlayer) Cut(context.Context, engine.SessionID, engine.OccurrenceID) (bool, error) {
	return true, nil
}
func (idlePlayer) Pause(context.Context, engine.SessionID) error { return nil }
func (idlePlayer) Resume(context.Context, engine.SessionID) (engine.ResumeResult, error) {
	return engine.ResumeContinued, nil
}
func (idlePlayer) Release(context.Context, engine.SessionID) error { return nil }
func (idlePlayer) Discard(context.Context) error                   { return nil }

func startRestoredPlayer(t *testing.T, dir string) *engine.Engine {
	t.Helper()
	id, err := config.ReadPlayerUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.Config{PlayerUUID: id, Backend: idlePlayer{}, DataDir: dir, QueuePath: filepath.Join(dir, "player-queue.json"), ReservePath: filepath.Join(dir, engine.ReservationFileName)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Run(ctx) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("player did not stop")
		}
	})
	return e
}

func firstValues(t *testing.T, dir string, written engine.Reservation) (uint32, uint32) {
	t.Helper()
	e := startRestoredPlayer(t, dir)
	token := e.Snapshot().Token()
	r, err := engine.NewReference("restored-entry")
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.InsertAfter(t.Context(), 0, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != written.IDs+4097 || token != written.Tokens+4097 {
		t.Fatalf("first ID/token = %d/%d after marks %+v", id, token, written)
	}
	return id, e.Snapshot().Token()
}

func TestRestoreReservationCases(t *testing.T) {
	reservationStorage(t)
	for _, tc := range []struct {
		name        string
		marked      bool
		liveID      string
		ids, tokens uint32
		noIdentity  bool
	}{
		{"empty", true, "", 0, 0, false},
		{"own", true, snapshotPlayerUUID, 50000, 70000, false},
		{"foreign", true, boxPlayerUUID, 90000, 2000, false},
		{"foreign_inverse", true, boxPlayerUUID, 2000, 90000, false},
		{"live_v1", true, boxPlayerUUID, 90000, 2000, false},
		{"no_mark_empty", false, "", 0, 0, false},
		{"no_mark_live", false, boxPlayerUUID, 90000, 2000, false},
		{"no_identity", true, boxPlayerUUID, 90000, 2000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := identitySnapshot(t, tc.marked)
			dst := t.TempDir()
			if tc.liveID != "" {
				if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), tc.liveID); err != nil {
					t.Fatal(err)
				}
				putMark(t, dst, tc.liveID, tc.ids, tc.tokens)
				if tc.name == "live_v1" {
					b := []byte(fmt.Sprintf(`{"version":1,"ids":%d,"tokens":%d}`, tc.ids, tc.tokens))
					if err := os.WriteFile(filepath.Join(dst, engine.ReservationFileName), b, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.noIdentity {
				if err := os.Remove(filepath.Join(snap, config.PlayerUUIDFile)); err != nil {
					t.Fatal(err)
				}
				before := readBytes(t, filepath.Join(dst, engine.ReservationFileName))
				if err := backup.RestoreWithOptions(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)}, backup.RestoreOptions{Random: bytes.NewReader(nil)}); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, readBytes(t, filepath.Join(dst, engine.ReservationFileName))) {
					t.Fatal("restore without identity changed the reservation")
				}
				if id, err := config.ReadPlayerUUID(dst); err != nil || id != tc.liveID {
					t.Fatalf("identity changed: %q %v", id, err)
				}
				firstValues(t, dst, markAt(t, dst))
				return
			}
			if err := fixedRestore(t, snap, dst, 123); err != nil {
				t.Fatal(err)
			}
			got := markAt(t, dst)
			// An unmarked snapshot's bound is the adoption ceiling (B524 round 10).
			si, st := uint32(floorlessCeiling), uint32(floorlessCeiling)
			if tc.marked {
				si, st = 10000, 30000
			}
			wantI, wantT := max(si, tc.ids)+(1<<24)+123, max(st, tc.tokens)+(1<<24)+123
			if got.IDs != wantI || got.Tokens != wantT || got.UUID != snapshotPlayerUUID || got.Version != 2 {
				t.Fatalf("got %+v, want %d/%d for snapshot identity", got, wantI, wantT)
			}
			firstValues(t, dst, got)
		})
	}
}

func TestRepeatedRestoresOnlyRaiseEachMark(t *testing.T) {
	reservationStorage(t)
	snap := identitySnapshot(t, true)
	dst := t.TempDir()
	last := engine.Reservation{IDs: 10000, Tokens: 30000}
	for n := 0; n < 4; n++ {
		if err := fixedRestore(t, snap, dst, 17); err != nil {
			t.Fatal(err)
		}
		next := markAt(t, dst)
		if next.IDs != last.IDs+(1<<24)+17 || next.Tokens != last.Tokens+(1<<24)+17 {
			t.Fatalf("restore %d: %+v after %+v", n, next, last)
		}
		last = next
	}
	firstValues(t, dst, last)
}

func TestRestoreRequiresTheReservationBarriersBeforeIdentity(t *testing.T) {
	for _, barrier := range []string{"file", "directory"} {
		t.Run(barrier, func(t *testing.T) {
			reservationStorage(t)
			snap, dst := identitySnapshot(t, true), t.TempDir()
			if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			putMark(t, dst, boxPlayerUUID, 90000, 80000)
			broken := errors.New("reservation barrier refused")
			var undo func()
			if barrier == "file" {
				undo = atomicwrite.SetSyncFileForTest(func(*os.File) error { return broken })
			} else {
				// Fail the reservation's barrier only. A later queue-discard
				// barrier must not mask a restore that ignored this failure.
				failed := false
				undo = atomicwrite.SetSyncDirForTest(func(string) error {
					if !failed {
						failed = true
						return broken
					}
					return nil
				})
			}
			err := fixedRestore(t, snap, dst, 1)
			undo()
			if !errors.Is(err, broken) {
				t.Fatalf("restore did not refuse its barrier: %v", err)
			}
			if id, err := config.ReadPlayerUUID(dst); err != nil || id != boxPlayerUUID {
				t.Fatalf("identity written without durable reservation: %q %v", id, err)
			}
			got := markAt(t, dst)
			if got.IDs < 90000 || got.Tokens < 80000 {
				t.Fatalf("barrier failure lowered the floor: %+v", got)
			}
		})
	}
}

func TestTwoRestoresWithSeparatedDrawsDoNotReuseValues(t *testing.T) {
	reservationStorage(t)
	snap := identitySnapshot(t, true)
	b, c := t.TempDir(), t.TempDir()
	if err := fixedRestore(t, snap, b, 0); err != nil {
		t.Fatal(err)
	}
	var bi, bt uint32
	t.Run("first_box", func(t *testing.T) {
		bi, bt = firstValues(t, b, markAt(t, b))
	}) // Its engine stops before the second box restores the same identity.
	if err := fixedRestore(t, snap, c, 100000); err != nil {
		t.Fatal(err)
	}
	ci, ct := firstValues(t, c, markAt(t, c))
	if ci <= bi || ct <= bt {
		t.Fatalf("second restore %d/%d, first handed out %d/%d", ci, ct, bi, bt)
	}
}

func TestSnapshotCarriesEveryDecodableLiveFloor(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"missing", "unreadable", "invalid", "foreign", "v1", "no_identity"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			idPath := filepath.Join(dir, config.PlayerUUIDFile)
			if kind != "no_identity" {
				if err := config.WritePlayerUUIDFile(idPath, snapshotPlayerUUID); err != nil {
					t.Fatal(err)
				}
			}
			rp := filepath.Join(dir, engine.ReservationFileName)
			switch kind {
			case "unreadable":
				if err := os.Mkdir(rp, 0700); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(rp, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				putMark(t, dir, boxPlayerUUID, 100, 200)
			case "v1":
				if err := os.WriteFile(rp, []byte(`{"version":1,"ids":100,"tokens":200}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "no_identity":
				putMark(t, dir, snapshotPlayerUUID, 100, 200)
			}
			snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: idPath})
			if kind == "missing" {
				if err != nil {
					t.Fatal(err)
				}
				if r := markAt(t, snap); r.UUID != snapshotPlayerUUID || r.IDs < 1<<24 || r.Tokens < 1<<24 {
					t.Fatalf("bind-only upgrade: %+v", r)
				}
				return
			}
			if kind == "unreadable" || kind == "invalid" {
				if err == nil {
					t.Fatal("backup accepted an unreadable floor")
				}
				if kind == "unreadable" {
					assertNoPlayerResetAdvice(t, err.Error())
				} else {
					assertPlayerResetAdvice(t, err.Error(), "backup")
				}
				return
			}
			if kind == "foreign" || kind == "v1" {
				if err == nil || !strings.Contains(err.Error(), "bridge reset-player") {
					t.Fatalf("unpaired floor captured: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			if slices.Contains(snapshotFiles(t, snap), engine.ReservationFileName) {
				t.Fatal("snapshot carried a mark without its identity")
			}
			if _, err := os.Stat(filepath.Join(snap, engine.ReservationFileName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("snapshot mark exists: %v", err)
			}
		})
	}
}

func TestExhaustingRestoreRefusesBeforeAnyWrite(t *testing.T) {
	reservationStorage(t)
	const limit = math.MaxUint32 - 3*(1<<24)
	for _, cause := range []string{"snapshot", "box"} {
		for _, counter := range []string{"IDs", "tokens"} {
			for _, draw := range []uint32{0, (1 << 24) - 1} {
				t.Run(fmt.Sprintf("%s/%s/%d", cause, counter, draw), func(t *testing.T) {
					snap := identitySnapshot(t, true)
					dst := t.TempDir()
					if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
						t.Fatal(err)
					}
					putMark(t, dst, boxPlayerUUID, 200, 300)
					ids, tokens := uint32(100), uint32(200)
					if counter == "IDs" {
						ids = limit + 1
					} else {
						tokens = limit + 1
					}
					if cause == "snapshot" {
						putMark(t, snap, snapshotPlayerUUID, ids, tokens)
					} else {
						putMark(t, dst, boxPlayerUUID, ids, tokens)
					}
					beforeID := readBytes(t, filepath.Join(dst, config.PlayerUUIDFile))
					beforeMark := readBytes(t, filepath.Join(dst, engine.ReservationFileName))
					writes := 0
					prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error { writes++; return os.Rename(a, b) })
					err := fixedRestore(t, snap, dst, draw)
					atomicwrite.SetRenameFuncForTest(prev)
					if err == nil || !strings.Contains(err.Error(), cause+"'s player "+counter+" mark") || !strings.Contains(err.Error(), "remove") || (!strings.Contains(err.Error(), "player-uuid") || !strings.Contains(err.Error(), "player-ids.json")) {
						t.Fatalf("refusal lacks cause/remedy: %v", err)
					}
					if writes != 0 || !bytes.Equal(beforeID, readBytes(t, filepath.Join(dst, config.PlayerUUIDFile))) || !bytes.Equal(beforeMark, readBytes(t, filepath.Join(dst, engine.ReservationFileName))) {
						t.Fatal("exhausting restore wrote state")
					}
				})
			}
		}
	}
	// Equality is allowed for either counter, including the largest draw.
	for _, c := range []string{"ids", "tokens"} {
		t.Run("boundary/"+c, func(t *testing.T) {
			snap := identitySnapshot(t, true)
			dst := t.TempDir()
			i, k := uint32(10000), uint32(30000)
			if c == "ids" {
				i = limit
			} else {
				k = limit
			}
			putMark(t, snap, snapshotPlayerUUID, i, k)
			if err := fixedRestore(t, snap, dst, (1<<24)-1); err != nil {
				t.Fatal(err)
			}
			got := markAt(t, dst)
			if uint64(got.IDs)+1<<24 > math.MaxUint32 || uint64(got.Tokens)+1<<24 > math.MaxUint32 {
				t.Fatal("restore left no running room")
			}
			firstValues(t, dst, got)
		})
	}
}

func TestRestoredIdentityReservesBeforeReturning(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	idPath := filepath.Join(dir, config.PlayerUUIDFile)
	if err := config.WritePlayerUUIDFile(idPath, snapshotPlayerUUID); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"version":2,"uuid":"` + snapshotPlayerUUID + `","ids":10000,"tokens":20000}`)
	if err := os.WriteFile(filepath.Join(dir, "player-ids.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: idPath})
	if err != nil {
		t.Fatal(err)
	}
	var manifest backup.Manifest
	if err := json.Unmarshal(readBytes(t, filepath.Join(snap, backup.ManifestFile)), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion == 1 {
		t.Error("old restore would accept the identity without a jump")
	}
	got, err := os.ReadFile(filepath.Join(snap, "player-ids.json"))
	if err != nil || string(got) != string(raw) {
		t.Errorf("snapshot reservation = %s, %v", got, err)
	}
	dst := t.TempDir()
	if err := backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dst, "player-ids.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marks struct{ IDs, Tokens uint32 }
	if err := json.Unmarshal(got, &marks); err != nil {
		t.Fatal(err)
	}
	if marks.IDs < 10000+1<<24 || marks.Tokens < 20000+1<<24 {
		t.Fatalf("restore did not jump: %s", got)
	}
}

func TestRestoreWarnsForOldOrUnmarkedSnapshots(t *testing.T) {
	reservationStorage(t)
	for _, marked := range []bool{true, false} {
		for _, age := range []time.Duration{-time.Hour, time.Hour, 25 * time.Hour} {
			t.Run(fmt.Sprintf("marked_%v/age_%s", marked, age), func(t *testing.T) {
				snap := identitySnapshot(t, marked)
				var m backup.Manifest
				if err := json.Unmarshal(readBytes(t, filepath.Join(snap, backup.ManifestFile)), &m); err != nil {
					t.Fatal(err)
				}
				m.CreatedAt = time.Now().Add(-age)
				b, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(snap, backup.ManifestFile), b, 0600); err != nil {
					t.Fatal(err)
				}
				var warnings []string
				err = backup.RestoreWithOptions(snap, backup.Targets{PlayerUUID: filepath.Join(t.TempDir(), config.PlayerUUIDFile)}, backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 0}), Warn: func(s string) { warnings = append(warnings, s) }})
				if err != nil {
					t.Fatal(err)
				}
				want := !marked || age > 24*time.Hour
				if len(warnings) != 0 && !want || want && len(warnings) != 1 {
					t.Fatalf("warnings %v, want warning %v", warnings, want)
				}
				if want {
					line := warnings[0]
					if !strings.Contains(line, "newest snapshot") || strings.Contains(line, "\n") {
						t.Fatalf("warning is not one actionable line: %q", line)
					}
					if !marked && !strings.Contains(line, "no player reservation mark") || age > 24*time.Hour && !strings.Contains(line, "older than a day") {
						t.Fatalf("warning lacks cause: %q", line)
					}
				}
			})
		}
	}
}

// crashRestore simulates a crash at a numbered atomicwrite seam. Panicking
// bypasses restore's continuation; the OS is never asked to kill a process.
func crashRestore(t *testing.T, snap string, dst backup.Targets, at int) (events []string, crashed bool) {
	t.Helper()
	stop := new(int)
	hit := func(s string) {
		events = append(events, s)
		if at == len(events) {
			panic(stop)
		}
	}
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		hit("before rename " + filepath.Base(b))
		err := os.Rename(a, b)
		hit("after rename " + filepath.Base(b))
		return err
	})
	defer atomicwrite.SetRenameFuncForTest(prev)
	defer atomicwrite.SetSyncFileForTest(func(f *os.File) error {
		hit("before file sync")
		err := f.Sync()
		hit("after file sync")
		return err
	})()
	defer atomicwrite.SetSyncDirForTest(func(string) error { hit("directory sync"); return nil })()
	defer func() {
		if p := recover(); p != nil {
			if p != stop {
				panic(p)
			}
			crashed = true
		}
	}()
	err := backup.RestoreWithOptions(snap, dst, backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 7})})
	if err != nil {
		t.Fatal(err)
	}
	return events, false
}

func TestRestoreCrashesNeverLowerTheLiveReservation(t *testing.T) {
	reservationStorage(t)
	source := t.TempDir()
	src := primeLiveState(t, source)
	withPlayerUUID(t, &src, snapshotPlayerUUID+"\n", 0600)
	putMark(t, source, snapshotPlayerUUID, 10000, 30000)
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(t *testing.T) (string, backup.Targets) {
		dir := t.TempDir()
		if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
			t.Fatal(err)
		}
		putMark(t, dir, boxPlayerUUID, 90000, 80000)
		for _, name := range []string{"tokens.json", "server.crt", "server.key", "bridge.yaml", "bridge.db", "bridge.db-wal", "bridge.db-shm"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("old "+name), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return dir, backup.Targets{PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile), TokensJSON: filepath.Join(dir, "tokens.json"), ServerCert: filepath.Join(dir, "server.crt"), ServerKey: filepath.Join(dir, "server.key"), BridgeYAML: filepath.Join(dir, "bridge.yaml"), ManifestDB: filepath.Join(dir, "bridge.db")}
	}
	_, dst := prepare(t)
	events, _ := crashRestore(t, snap, dst, 0)
	if len(events) < 15 {
		t.Fatalf("restore did not visit its write seams: %v", events)
	}
	for i, event := range events {
		t.Run(fmt.Sprintf("%02d_%s", i+1, event), func(t *testing.T) {
			dir, dst := prepare(t)
			_, crashed := crashRestore(t, snap, dst, i+1)
			if !crashed {
				t.Fatal("crash seam was not reached")
			}
			mark := markAt(t, dir)
			if mark.IDs < 90000 || mark.Tokens < 80000 {
				t.Fatalf("crash lowered marks: %+v", mark)
			}
			id, err := config.ReadPlayerUUID(dir)
			if err != nil {
				t.Fatal(err)
			}
			if id == snapshotPlayerUUID && (mark.IDs != 90000+(1<<24)+7 || mark.Tokens != 80000+(1<<24)+7) {
				t.Fatalf("identity landed without raised marks: %+v", mark)
			}
			for _, name := range []string{"tokens.json", "server.crt", "server.key", "bridge.yaml"} {
				if string(readBytes(t, filepath.Join(dir, name))) != "old "+name && id != snapshotPlayerUUID {
					t.Fatalf("%s restored before player reservation and identity", name)
				}
			}
			// A rerun adds a second jump if the first reservation landed.
			if err := fixedRestore(t, snap, dir, 7); err != nil {
				t.Fatal(err)
			}
			next := markAt(t, dir)
			if next.IDs != mark.IDs+(1<<24)+7 || next.Tokens != mark.Tokens+(1<<24)+7 {
				t.Fatalf("rerun %+v after %+v", next, mark)
			}
			firstValues(t, dir, next)
		})
	}
}

func TestInterruptedRestoreThenStartBackupAndRestoreKeepsTheRaisedFloor(t *testing.T) {
	reservationStorage(t)
	snap := identitySnapshot(t, true)
	dst := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, dst, boxPlayerUUID, 90000, 80000)
	// Fail exactly between the durable reservation and player-uuid.
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		if filepath.Base(b) == config.PlayerUUIDFile {
			return errors.New("interrupted before identity")
		}
		return os.Rename(a, b)
	})
	err := fixedRestore(t, snap, dst, 7)
	atomicwrite.SetRenameFuncForTest(prev)
	if err == nil {
		t.Fatal("restore did not stop before identity")
	}
	raised := markAt(t, dst)
	if raised.UUID != snapshotPlayerUUID {
		t.Fatal("reservation does not name the restore's identity")
	}
	if id, err := config.ReadPlayerUUID(dst); err != nil || id != boxPlayerUUID {
		t.Fatalf("old identity changed: %q %v", id, err)
	}
	lastID, lastToken := firstValues(t, dst, raised)
	// The old identity's first write restamps the raised floor before its
	// token is visible, so a backup taken while it runs carries that floor.
	newSnap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dst, PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)})
	if err != nil {
		t.Fatal(err)
	}
	mark := markAt(t, newSnap)
	if mark.UUID != boxPlayerUUID || mark.IDs < lastID || mark.Tokens < lastToken {
		t.Fatalf("backup lost old identity's raised floor: %+v after %d/%d", mark, lastID, lastToken)
	}
	elsewhere := t.TempDir()
	if err := fixedRestore(t, newSnap, elsewhere, 0); err != nil {
		t.Fatal(err)
	}
	got := markAt(t, elsewhere)
	if got.IDs <= lastID || got.Tokens <= lastToken {
		t.Fatalf("restore reused old values: %+v", got)
	}
	firstValues(t, elsewhere, got)
}
