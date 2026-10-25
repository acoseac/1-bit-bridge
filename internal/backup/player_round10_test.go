package backup_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

const (
	// floorlessCeiling is the bound restore gives an identity whose snapshot,
	// from a writer before reservation capture, lists no floor: counter space
	// below 2^31, plus an adoption's random part below 2^24 when its source
	// upgraded after the snapshot (B524 round 10).
	floorlessCeiling = 1<<31 + 1<<24
	// legacyHighest is the largest startup token 8,193 starts of a main-era
	// engine publish with no queue edit: 8,192 * 8,192 + 4,097.
	legacyHighest = 8192*8192 + 4097
	// damagedMarkerWarning is the warning a start owes a damaged
	// player-initialized beside a floor that names the current UUID.
	damagedMarkerWarning = "player initialization marker is damaged"
	// soleSealWarning is the warning for one whose sealed bit is the only
	// intact seal record left.
	soleSealWarning = "player initialization marker is damaged and holds the only intact discard seal"
	// keepMarkerPhrase is what every floor-loss refusal beside a marker says.
	keepMarkerPhrase = "do not move player-initialized aside"
)

// randomDraws is the random input of restore's draws, three bytes each, in
// the order restore reads them.
func randomDraws(values ...uint32) []byte {
	var b []byte
	for _, v := range values {
		b = append(b, byte(v>>16), byte(v>>8), byte(v))
	}
	return b
}

// editManifest rewrites a snapshot's manifest.json through a map, so a test
// can drop a key whatever this build's Manifest declares.
func editManifest(t *testing.T, snap string, edit func(map[string]any)) {
	t.Helper()
	p := filepath.Join(snap, backup.ManifestFile)
	var m map[string]any
	if err := json.Unmarshal(readBytes(t, p), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}

// stripSnapshotFloor removes player-ids.json from a snapshot and from its
// file list. legacy also drops the manifest's record that its writer lists
// the floor beside every player UUID: a writer before reservation capture
// recorded nothing of the kind.
func stripSnapshotFloor(t *testing.T, snap string, legacy bool) {
	t.Helper()
	if err := os.Remove(filepath.Join(snap, engine.ReservationFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	editManifest(t, snap, func(m map[string]any) {
		files, _ := m["files"].([]any)
		m["files"] = slices.DeleteFunc(files, func(v any) bool { return v == engine.ReservationFileName })
		if legacy {
			delete(m, "playerFloorRequired")
		}
	})
}

// legacyIdentitySnapshot is the snapshot a writer before reservation
// capture made of a box whose player was id: player-uuid and no floor.
func legacyIdentitySnapshot(t *testing.T, id string) string {
	t.Helper()
	snap := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(snap, backup.PlayerUUIDFileName), id); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"schemaVersion": 2, "bridgeVersion": "0.2.1", "protocolVersion": 1,
		"createdAt": time.Now().UTC(), "files": []string{backup.PlayerUUIDFileName}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, backup.ManifestFile), b, 0600); err != nil {
		t.Fatal(err)
	}
	return snap
}

// floorSnapshot is this bridge's snapshot of a player at the given floor,
// with a token store when tokens is not empty.
func floorSnapshot(t *testing.T, ids, marks uint32, tokens string) string {
	t.Helper()
	dir := t.TempDir()
	src := backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}
	if err := config.WritePlayerUUIDFile(src.PlayerUUID, snapshotPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, dir, snapshotPlayerUUID, ids, marks)
	if tokens != "" {
		src.TokensJSON = filepath.Join(dir, "tokens.json")
		if err := os.WriteFile(src.TokensJSON, []byte(tokens), 0600); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// uuidOnlyBox is the upgrade state: a valid UUID and no floor, queue or
// local marker.
func uuidOnlyBox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	return dir
}

// dirState is every entry of dir with its bytes ("<dir>" for a directory).
func dirState(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			state[e.Name()] = "<dir>"
			continue
		}
		state[e.Name()] = string(readBytes(t, filepath.Join(dir, e.Name())))
	}
	return state
}

// holdServerLock holds dir's server.lock as a running serve does, until
// release or the end of the test.
func holdServerLock(t *testing.T, dir string) (release func()) {
	t.Helper()
	f, err := fsutil.OpenLockFile(dir, "server.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	done := false
	release = func() {
		if done {
			return
		}
		done = true
		_ = fsutil.Unlock(f)
		_ = f.Close()
	}
	t.Cleanup(release)
	return release
}

// loneSealBox is a sealed, minted box whose initialized marker was deleted
// while it ran: the seal file is the one record, and completion is owed.
func loneSealBox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sealedMint(t, dir)
	if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// noMarkerWarning fails the test for any warning about player-initialized.
func noMarkerWarning(t *testing.T, rec *loggingtest.Recorder) {
	t.Helper()
	for _, line := range rec.All() {
		if strings.Contains(line, "player initialization marker") || strings.Contains(line, "re-ensures") {
			t.Errorf("marker warning beside a lost floor: %s", line)
		}
	}
}

// TestThisBridgeNeverCapturesAFloorlessIdentity is the creation side of a
// floorless snapshot. A box that adopted, took a restore jump and ran on,
// then lost only player-ids.json, has a UUID and a marker and no floor:
// backup refuses with the floor-loss remedy and leaves no snapshot. The
// UUID-only state is the one floorless live state a backup goes on from,
// and it adopts that state first, so its snapshot lists a floor too.
func TestThisBridgeNeverCapturesAFloorlessIdentity(t *testing.T) {
	reservationStorage(t)
	dir := uuidOnlyBox(t)
	path := filepath.Join(dir, config.PlayerUUIDFile)
	upgrade, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: path})
	if err != nil {
		t.Fatal(err)
	}
	adopted := markAt(t, dir)
	if adopted.IDs < 1<<31 || adopted.Tokens < 1<<31 || !slices.Contains(snapshotFiles(t, upgrade), engine.ReservationFileName) {
		t.Fatalf("backup of the upgrade state did not adopt first: %+v %v", adopted, snapshotFiles(t, upgrade))
	}
	if err := fixedRestore(t, upgrade, dir, 0); err != nil {
		t.Fatal(err)
	}
	jumped := markAt(t, dir)
	if uint64(jumped.IDs) != uint64(adopted.IDs)+1<<24 || uint64(jumped.Tokens) != uint64(adopted.Tokens)+1<<24 {
		t.Fatalf("restore jump %+v after %+v", jumped, adopted)
	}
	t.Run("run_on", func(t *testing.T) { firstValues(t, dir, jumped) })
	if err := os.Remove(filepath.Join(dir, engine.ReservationFileName)); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(dir, backup.BackupsDirName)
	before := dirState(t, backups)
	_, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: path})
	if err == nil || !strings.Contains(err.Error(), "player floor is missing") || !strings.Contains(err.Error(), "--discard-floor") {
		t.Fatalf("backup captured an identity it cannot bound: %v", err)
	}
	if after := dirState(t, backups); !maps.Equal(before, after) {
		t.Fatalf("refused backup left a snapshot: %v -> %v", before, after)
	}
	if _, err := os.Lstat(filepath.Join(dir, engine.ReservationFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused backup wrote a floor: %v", err)
	}
}

// TestThisBridgesSnapshotWithoutItsFloorIsRefused: this bridge records in
// every manifest that it lists the floor beside a player UUID, so a snapshot
// of its making that carries the UUID alone was stripped. Restore refuses it
// with the snapshot-copy remedy before anything is written, a UUID-only
// destination's adoption included.
func TestThisBridgesSnapshotWithoutItsFloorIsRefused(t *testing.T) {
	reservationStorage(t)
	for _, dest := range []string{"pristine", "uuid_only"} {
		t.Run(dest, func(t *testing.T) {
			snap := identitySnapshot(t, true)
			var m map[string]any
			if err := json.Unmarshal(readBytes(t, filepath.Join(snap, backup.ManifestFile)), &m); err != nil {
				t.Fatal(err)
			}
			if m["playerFloorRequired"] != true {
				t.Errorf("manifest does not record that its writer lists the floor beside the UUID: %v", m)
			}
			stripSnapshotFloor(t, snap, false)
			dst := t.TempDir()
			if dest == "uuid_only" {
				dst = uuidOnlyBox(t)
			}
			before := dirState(t, dst)
			err := backup.RestoreWithOptions(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)},
				backup.RestoreOptions{Random: bytes.NewReader(randomDraws(0, 0))})
			if err == nil || !strings.Contains(err.Error(), "copy of the snapshot") || !strings.Contains(err.Error(), "nothing was restored") {
				t.Fatalf("restore accepted this bridge's UUID without its floor: %v", err)
			}
			if after := dirState(t, dst); !maps.Equal(before, after) {
				t.Fatalf("refused restore changed the box: %v -> %v", before, after)
			}
		})
	}
}

// TestLegacyFloorlessSnapshotRestoresAboveTheAdoptionCeiling is Codex's
// round-9 sequence: a main-era player runs 8,193 starts with no queue edit,
// and its latest snapshot, from a writer before reservation capture, holds
// the UUID alone. Restored onto a pristine box, its floor is the adoption
// ceiling plus the jump, exactly, and the missing-mark warning still fires.
func TestLegacyFloorlessSnapshotRestoresAboveTheAdoptionCeiling(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	path := filepath.Join(dir, config.PlayerUUIDFile)
	if err := config.WritePlayerUUIDFile(path, boxPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, dir, boxPlayerUUID, 0, 0)
	var highest uint32
	for i := 0; i < 8193; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			token := startRestoredPlayer(t, dir).Snapshot().Token()
			if token != uint32(i)*8192+4097 {
				t.Fatalf("startup token %d at start %d", token, i)
			}
			highest = max(highest, token)
		})
	}
	if highest != legacyHighest {
		t.Fatalf("highest startup token %d", highest)
	}
	for _, name := range []string{"player-queue.json", "player-initialized", "player-floor-discarded.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("not a main-era box: %s %v", name, err)
		}
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: path})
	if err != nil {
		t.Fatal(err)
	}
	stripSnapshotFloor(t, snap, true)
	for _, tc := range []struct {
		draw uint32
		want uint64
	}{
		{0, 1<<31 + 1<<25},
		{1<<24 - 1, 1<<31 + 1<<25 + 1<<24 - 1},
	} {
		t.Run(fmt.Sprintf("draw_%d", tc.draw), func(t *testing.T) {
			dst := t.TempDir()
			r := bytes.NewReader(randomDraws(tc.draw))
			var warnings []string
			err := backup.RestoreWithOptions(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)},
				backup.RestoreOptions{Random: r, Warn: func(s string) { warnings = append(warnings, s) }})
			if err != nil {
				t.Fatal(err)
			}
			got := markAt(t, dst)
			if uint64(got.IDs) != tc.want || uint64(got.Tokens) != tc.want || got.UUID != boxPlayerUUID {
				t.Fatalf("restored floor %+v, want %d on both counters", got, tc.want)
			}
			if r.Len() != 0 {
				t.Fatalf("restore left %d random bytes unread", r.Len())
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "no player reservation mark") {
				t.Fatalf("missing-mark warning: %v", warnings)
			}
			id, token := firstValues(t, dst, got)
			if id <= highest || token <= highest {
				t.Fatalf("restored values %d/%d repeat the source's space through %d", id, token, highest)
			}
		})
	}
}

// TestLegacyCeilingCoversASourceThatAdoptedAfterItsSnapshot: a UUID-only
// main-era box is snapshotted by a writer before reservation capture, then
// upgrades and adopts at its largest draw, and starts once. The old snapshot
// restored elsewhere at draw zero still lands above that source's durable
// floor: what the 2^24 term of the ceiling is for.
func TestLegacyCeilingCoversASourceThatAdoptedAfterItsSnapshot(t *testing.T) {
	reservationStorage(t)
	src := uuidOnlyBox(t)
	snap := legacyIdentitySnapshot(t, boxPlayerUUID)
	if err := backup.AdoptPlayerStateWith(src, bytes.NewReader(randomDraws(1<<24-1))); err != nil {
		t.Fatal(err)
	}
	adopted := markAt(t, src)
	if uint64(adopted.IDs) != 1<<31+1<<24-1 || uint64(adopted.Tokens) != 1<<31+1<<24-1 {
		t.Fatalf("adoption at the largest draw: %+v", adopted)
	}
	t.Run("one_start", func(t *testing.T) { firstValues(t, src, adopted) })
	source := markAt(t, src)
	if source.IDs <= adopted.IDs || source.Tokens <= adopted.Tokens {
		t.Fatalf("the start reserved nothing: %+v after %+v", source, adopted)
	}
	dst := t.TempDir()
	if err := fixedRestore(t, snap, dst, 0); err != nil {
		t.Fatal(err)
	}
	got := markAt(t, dst)
	if got.IDs <= source.IDs || got.Tokens <= source.Tokens {
		t.Fatalf("restored floor %+v does not pass the source's durable floor %+v", got, source)
	}
	if uint64(got.IDs) != 1<<31+1<<25 || uint64(got.Tokens) != 1<<31+1<<25 {
		t.Fatalf("restored floor %+v, want the ceiling plus the jump", got)
	}
	t.Run("restored", func(t *testing.T) { firstValues(t, dst, got) })
}

// TestLegacyCeilingYieldsToEachHigherLiveBound: the ceiling is the snapshot's
// bound, and restore still takes the larger of it and the destination's
// live bound counter by counter.
func TestLegacyCeilingYieldsToEachHigherLiveBound(t *testing.T) {
	reservationStorage(t)
	snap := legacyIdentitySnapshot(t, snapshotPlayerUUID)
	for _, tc := range []struct {
		name        string
		ids, tokens uint32
	}{
		{"ids", 3_000_000_000, 1_000_000_000},
		{"tokens", 1_000_000_000, 3_000_000_000},
		{"both", 3_000_000_000, 3_100_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			if err := config.WritePlayerUUIDFile(filepath.Join(dst, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			putMark(t, dst, boxPlayerUUID, tc.ids, tc.tokens)
			if err := fixedRestore(t, snap, dst, 5); err != nil {
				t.Fatal(err)
			}
			got := markAt(t, dst)
			wantI := max(uint64(floorlessCeiling), uint64(tc.ids)) + 1<<24 + 5
			wantT := max(uint64(floorlessCeiling), uint64(tc.tokens)) + 1<<24 + 5
			if uint64(got.IDs) != wantI || uint64(got.Tokens) != wantT || got.UUID != snapshotPlayerUUID {
				t.Fatalf("restored floor %+v, want %d/%d", got, wantI, wantT)
			}
			firstValues(t, dst, got)
		})
	}
}

// TestLegacyFloorlessSnapshotOntoAUUIDOnlyBox: the destination adopts first,
// below the ceiling at any draw, so the ceiling bounds the jump.
func TestLegacyFloorlessSnapshotOntoAUUIDOnlyBox(t *testing.T) {
	reservationStorage(t)
	snap := legacyIdentitySnapshot(t, snapshotPlayerUUID)
	for _, adoption := range []uint32{0, 1<<24 - 1} {
		t.Run(fmt.Sprint(adoption), func(t *testing.T) {
			dst := uuidOnlyBox(t)
			if err := fixedRestore(t, snap, dst, adoption, 0); err != nil {
				t.Fatal(err)
			}
			got := markAt(t, dst)
			if uint64(got.IDs) != 1<<31+1<<25 || uint64(got.Tokens) != 1<<31+1<<25 || got.UUID != snapshotPlayerUUID {
				t.Fatalf("restored floor %+v, want the ceiling plus the jump", got)
			}
			firstValues(t, dst, got)
		})
	}
}

// TestALegacySnapshotsListedFloorStillMustRead: a manifest that LISTS the
// floor never takes the ceiling, whatever wrote it: a listed floor that is
// missing or corrupt refuses, as it always has.
func TestALegacySnapshotsListedFloorStillMustRead(t *testing.T) {
	reservationStorage(t)
	for _, damage := range []string{"deleted", "corrupt"} {
		t.Run(damage, func(t *testing.T) {
			snap := identitySnapshot(t, true)
			editManifest(t, snap, func(m map[string]any) { delete(m, "playerFloorRequired") })
			p := filepath.Join(snap, engine.ReservationFileName)
			var err error
			if damage == "deleted" {
				err = os.Remove(p)
			} else {
				err = os.WriteFile(p, []byte("}{"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			dst := t.TempDir()
			err = fixedRestore(t, snap, dst, 0)
			if err == nil || !strings.Contains(err.Error(), "lists player-ids.json but it cannot be read") {
				t.Fatalf("listed floor took another path: %v", err)
			}
			if state := dirState(t, dst); len(state) != 0 {
				t.Fatalf("refused restore wrote %v", state)
			}
		})
	}
}

// TestRestoreAdoptsWithItsOwnRandomnessFirst: restore's adoption draws from
// restore's random source, before the jump's draw. With a listed snapshot
// floor below 2^31, the adopted live floor decides the result, which is
// exactly 2^31 + r_a + 2^24 + r_j, the same for the same bytes.
func TestRestoreAdoptsWithItsOwnRandomnessFirst(t *testing.T) {
	reservationStorage(t)
	snap := floorSnapshot(t, 10_000_000, 10_000_000, "")
	const ra, rj = 0x123456, 0x0789ab
	want := uint64(1<<31 + ra + 1<<24 + rj)
	var floors []engine.Reservation
	for range 2 {
		dst := uuidOnlyBox(t)
		r := bytes.NewReader(randomDraws(ra, rj))
		if err := backup.RestoreWithOptions(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)}, backup.RestoreOptions{Random: r}); err != nil {
			t.Fatal(err)
		}
		got := markAt(t, dst)
		if uint64(got.IDs) != want || uint64(got.Tokens) != want || got.UUID != snapshotPlayerUUID {
			t.Fatalf("restored floor %+v, want %d on both counters", got, want)
		}
		if r.Len() != 0 {
			t.Fatalf("restore left %d of its six random bytes unread", r.Len())
		}
		floors = append(floors, got)
	}
	if floors[0] != floors[1] {
		t.Fatalf("the same bytes gave two floors: %+v %+v", floors[0], floors[1])
	}
}

// TestRestoreAdoptionDrawFailureWritesNothing: a random source that fails
// on its first read stops restore before adoption writes anything, so the
// UUID-only state is left as it was, and a start still adopts it.
func TestRestoreAdoptionDrawFailureWritesNothing(t *testing.T) {
	reservationStorage(t)
	snap := floorSnapshot(t, 10_000_000, 10_000_000, "")
	dst := uuidOnlyBox(t)
	before := dirState(t, dst)
	failing := errors.New("no randomness")
	err := backup.RestoreWithOptions(snap, backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile)},
		backup.RestoreOptions{Random: iotest.ErrReader(failing)})
	if !errors.Is(err, failing) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("restore over a failed draw: %v", err)
	}
	for _, name := range []string{engine.ReservationFileName, "player-initialized"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed draw left %s: %v", name, err)
		}
	}
	if after := dirState(t, dst); !maps.Equal(before, after) {
		t.Fatalf("failed draw changed the box: %v -> %v", before, after)
	}
	if _, _, err := backup.EnsurePlayerUUID(dst); err != nil {
		t.Fatal(err)
	}
	if r := markAt(t, dst); r.IDs < 1<<31 || r.Tokens < 1<<31 || r.UUID != boxPlayerUUID {
		t.Fatalf("the box left its upgrade state: %+v", r)
	}
}

// TestRestoreAdoptionDrawThenFailureKeepsTheAdoptedFloor: a source that
// supplies the adoption's draw and then fails leaves the adoption durable
// and restores nothing of the snapshot; a retry jumps from that floor.
func TestRestoreAdoptionDrawThenFailureKeepsTheAdoptedFloor(t *testing.T) {
	reservationStorage(t)
	snap := floorSnapshot(t, 10_000_000, 10_000_000, "snapshot tokens")
	dst := uuidOnlyBox(t)
	targets := backup.Targets{PlayerUUID: filepath.Join(dst, config.PlayerUUIDFile), TokensJSON: filepath.Join(dst, "tokens.json")}
	failing := errors.New("randomness failed after the adoption's draw")
	const ra, rj = 0x2468ac, 0x13579b
	err := backup.RestoreWithOptions(snap, targets, backup.RestoreOptions{Random: io.MultiReader(bytes.NewReader(randomDraws(ra)), iotest.ErrReader(failing))})
	if !errors.Is(err, failing) {
		t.Fatalf("restore over a failed jump draw: %v", err)
	}
	adopted := markAt(t, dst)
	if uint64(adopted.IDs) != 1<<31+ra || uint64(adopted.Tokens) != 1<<31+ra || adopted.UUID != boxPlayerUUID {
		t.Fatalf("adopted floor %+v, want %d", adopted, uint64(1<<31+ra))
	}
	if _, err := os.Stat(filepath.Join(dst, "player-initialized")); err != nil {
		t.Fatalf("adoption's marker: %v", err)
	}
	if id, err := config.ReadPlayerUUID(dst); err != nil || id != boxPlayerUUID {
		t.Fatalf("snapshot identity published: %q %v", id, err)
	}
	if _, err := os.Stat(targets.TokensJSON); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot contents restored: %v", err)
	}
	r := bytes.NewReader(randomDraws(rj))
	if err := backup.RestoreWithOptions(snap, targets, backup.RestoreOptions{Random: r}); err != nil {
		t.Fatal(err)
	}
	got := markAt(t, dst)
	want := uint64(adopted.IDs) + 1<<24 + rj
	if uint64(got.IDs) != want || uint64(got.Tokens) != want || got.UUID != snapshotPlayerUUID || r.Len() != 0 {
		t.Fatalf("retry floor %+v (%d bytes unread), want %d from the adopted bound", got, r.Len(), want)
	}
	if string(readBytes(t, targets.TokensJSON)) != "snapshot tokens" {
		t.Fatal("retry did not restore the snapshot")
	}
}

// TestBusySealCompletionIsLeftToTheNextLifecycleOperation: a backup that
// finds a seal to complete while serve holds server.lock logs that the next
// lifecycle operation completes it, never that the service must stop, and
// that operation does.
func TestBusySealCompletionIsLeftToTheNextLifecycleOperation(t *testing.T) {
	reservationStorage(t)
	dir := loneSealBox(t)
	release := holdServerLock(t, dir)
	rec := loggingtest.Record(t)
	if _, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}); err != nil {
		t.Fatalf("busy completion stopped the backup: %v", err)
	}
	lines := rec.Lines("player seal completion deferred")
	if len(lines) != 1 || !strings.Contains(lines[0], "the next lifecycle operation") || strings.Contains(lines[0], "stop the service") ||
		strings.Contains(lines[0], "run the backup again") || strings.Contains(lines[0], "write access") {
		t.Fatalf("busy completion's text: %v", lines)
	}
	if failed := rec.Lines("player seal completion failed"); len(failed) != 0 {
		t.Fatalf("busy completion logged as failed: %v", failed)
	}
	if _, err := os.Lstat(filepath.Join(dir, "player-initialized")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy completion wrote the marker: %v", err)
	}
	release()
	if err := backup.AdoptPlayerState(dir); err != nil {
		t.Fatal(err)
	}
	var marker struct{ Sealed, Minted bool }
	if err := json.Unmarshal(readBytes(t, filepath.Join(dir, "player-initialized")), &marker); err != nil {
		t.Fatal(err)
	}
	if !marker.Sealed || !marker.Minted {
		t.Fatalf("the next lifecycle operation did not complete the seal: %+v", marker)
	}
}

// TestBusyAdoptionStillAsksToStopTheService: busy ADOPTION refuses the
// backup, and its remedy is unchanged.
func TestBusyAdoptionStillAsksToStopTheService(t *testing.T) {
	reservationStorage(t)
	dir := uuidOnlyBox(t)
	holdServerLock(t, dir)
	rec := loggingtest.Record(t)
	_, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
	if err == nil || !strings.Contains(err.Error(), "player initialization is busy; stop the service and run the backup again") {
		t.Fatalf("busy adoption's text: %v", err)
	}
	if lines := rec.Lines("player seal completion deferred"); len(lines) != 0 {
		t.Fatalf("busy adoption logged as a deferred completion: %v", lines)
	}
}

// TestFloorLossRefusalsNeverAdviseMovingTheMarker: a UUID beside a damaged
// marker and no floor is floor loss. Start, its preflight, backup and
// restore refuse with the discard remedy and the phrase that keeps the
// marker, and none of them logs the move-aside warning.
func TestFloorLossRefusalsNeverAdviseMovingTheMarker(t *testing.T) {
	reservationStorage(t)
	for _, sealed := range []bool{false, true} {
		for _, op := range []string{"preflight", "start", "backup", "restore"} {
			if sealed && op == "restore" {
				continue // a sealed box refuses identity restores before it reads its floor
			}
			t.Run(fmt.Sprintf("sealed_%t/%s", sealed, op), func(t *testing.T) {
				dir := t.TempDir()
				if sealed {
					sealedMint(t, dir)
				} else if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
					t.Fatal(err)
				}
				snap := ""
				if op == "restore" {
					snap = identitySnapshot(t, true)
				}
				if err := os.WriteFile(filepath.Join(dir, "player-initialized"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(dir, engine.ReservationFileName)); err != nil {
					t.Fatal(err)
				}
				rec := loggingtest.Record(t)
				var err error
				switch op {
				case "preflight":
					err = backup.CheckPlayerStart(dir)
				case "start":
					_, _, err = backup.EnsurePlayerUUID(dir)
				case "backup":
					_, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
				case "restore":
					err = fixedRestore(t, snap, dir, 0)
				}
				if err == nil || !strings.Contains(err.Error(), keepMarkerPhrase) || !strings.Contains(err.Error(), "--discard-floor") {
					t.Errorf("floor-loss refusal: %v", err)
				}
				noMarkerWarning(t, rec)
			})
		}
	}
}

// TestMoveAsideWarningOncePerStart: beside a floor that names the UUID, a
// start's public preflight logs the move-aside warning once, the minter
// after it logs nothing, and so does every other lifecycle operation.
func TestMoveAsideWarningOncePerStart(t *testing.T) {
	reservationStorage(t)
	for _, sealed := range []bool{false, true} {
		t.Run(fmt.Sprint(sealed), func(t *testing.T) {
			dir := t.TempDir()
			if sealed {
				sealedMint(t, dir)
			} else if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "player-initialized"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			snap := identitySnapshot(t, true)
			if err := os.Remove(filepath.Join(snap, config.PlayerUUIDFile)); err != nil {
				t.Fatal(err)
			}
			rec := loggingtest.Record(t)
			for start := 1; start <= 2; start++ {
				if err := backup.CheckPlayerStart(dir); err != nil {
					t.Fatal(err)
				}
				if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
					t.Fatal(err)
				}
				lines := rec.Lines(damagedMarkerWarning)
				if len(lines) != start {
					t.Fatalf("start %d logged %d warnings: %v", start, len(lines), lines)
				}
				line := lines[start-1]
				if !strings.Contains(line, "player-initialized") || !strings.Contains(line, "move it aside") || !strings.Contains(line, "re-ensures") || strings.Contains(line, "--discard-floor") {
					t.Fatalf("warning's text: %s", line)
				}
			}
			if err := backup.AdoptPlayerState(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}); err != nil {
				t.Fatal(err)
			}
			if err := backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}); err != nil {
				t.Fatal(err)
			}
			if _, err := backup.ResetPlayer(dir); err != nil {
				t.Fatal(err)
			}
			if lines := rec.Lines(damagedMarkerWarning); len(lines) != 2 {
				t.Fatalf("lifecycle operations other than a start's preflight warned: %v", lines)
			}
		})
	}
}

// TestMoveAsideAdviceNeedsTheFloorToNameTheUUID: without a UUID, beside a
// floor naming another UUID, or beside an exhausted floor, a damaged marker
// gets no move-aside warning.
func TestMoveAsideAdviceNeedsTheFloorToNameTheUUID(t *testing.T) {
	reservationStorage(t)
	for _, state := range []string{"no_uuid", "other_uuid", "exhausted"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			id, _, err := backup.EnsurePlayerUUID(dir)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "no_uuid":
				if _, err := backup.ResetPlayer(dir); err != nil {
					t.Fatal(err)
				}
			case "other_uuid":
				if err := os.WriteFile(filepath.Join(dir, engine.ReservationFileName), fmt.Appendf(nil, `{"version":2,"uuid":%q,"ids":9,"tokens":9}`, boxPlayerUUID), 0600); err != nil {
					t.Fatal(err)
				}
			case "exhausted":
				putMark(t, dir, id, math.MaxUint32-1, 9)
			}
			if err := os.WriteFile(filepath.Join(dir, "player-initialized"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			rec := loggingtest.Record(t)
			err = backup.CheckPlayerStart(dir)
			if (state == "no_uuid") != (err == nil) {
				t.Fatalf("preflight: %v", err)
			}
			if lines := rec.All(); len(lines) != 0 {
				t.Fatalf("warned without a floor naming the UUID: %v", lines)
			}
		})
	}
}

// TestADamagedMarkerHoldingTheOnlySealIsKept: an invalid-version marker that
// reads sealed:true with no valid seal file holds the box's only intact
// seal. Moving it aside would let start write an unsealed marker, so the
// warning names the seal and says to keep it. Following that advice, and
// the discard repair it names, identity restores stay refused as sealed.
func TestADamagedMarkerHoldingTheOnlySealIsKept(t *testing.T) {
	reservationStorage(t)
	for _, seal := range []string{"missing", "damaged"} {
		t.Run(seal, func(t *testing.T) {
			dir := t.TempDir()
			sealedMint(t, dir)
			sealPath := filepath.Join(dir, "player-floor-discarded.json")
			if seal == "missing" {
				if err := os.Remove(sealPath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(sealPath, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "player-initialized")
			damaged := []byte(`{"version":0,"sealed":true}`)
			if err := os.WriteFile(marker, damaged, 0600); err != nil {
				t.Fatal(err)
			}
			refusedAsSealed := func(t *testing.T) {
				t.Helper()
				err := fixedRestore(t, identitySnapshot(t, true), dir, 0)
				if err == nil || !strings.Contains(err.Error(), "sealed") {
					t.Fatalf("identity restore over the sole seal: %v", err)
				}
			}
			rec := loggingtest.Record(t)
			if err := backup.CheckPlayerStart(dir); err != nil {
				t.Fatalf("the sole seal turned the player off: %v", err)
			}
			if all := strings.Join(rec.All(), "\n"); strings.Contains(all, "move it aside") || len(rec.Lines(damagedMarkerWarning)) != 0 {
				t.Fatalf("move-aside advice beside the sole seal: %s", all)
			}
			lines := rec.Lines(soleSealWarning)
			if len(lines) != 1 || !strings.Contains(lines[0], "player-initialized") || !strings.Contains(lines[0], "player-floor-discarded.json") || !strings.Contains(lines[0], "do not move") {
				t.Fatalf("sole-seal warning: %v", lines)
			}
			refusedAsSealed(t)
			// The advice: keep it, and the box runs as it is.
			steps := []struct {
				name string
				run  func() error
			}{
				{"start", func() error { _, _, err := backup.EnsurePlayerUUID(dir); return err }},
				{"boot", func() error { return backup.AdoptPlayerState(dir) }},
				{"backup", func() error {
					_, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
					return err
				}},
				{"reset", func() error { _, err := backup.ResetPlayer(dir); return err }},
				{"start_after_reset", func() error { _, _, err := backup.EnsurePlayerUUID(dir); return err }},
			}
			for _, step := range steps {
				if err := step.run(); err != nil {
					t.Fatalf("%s: %v", step.name, err)
				}
				if !bytes.Equal(readBytes(t, marker), damaged) {
					t.Fatalf("%s rewrote the sole seal", step.name)
				}
				refusedAsSealed(t)
			}
			t.Run("engine", func(t *testing.T) { firstValues(t, dir, markAt(t, dir)) })
			// The repair the warning names rewrites both records.
			if _, err := backup.DiscardPlayerFloor(dir); err != nil {
				t.Fatal(err)
			}
			if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"player-floor-discarded.json", "player-initialized"} {
				var m struct {
					Version int
					Sealed  bool
				}
				if err := json.Unmarshal(readBytes(t, filepath.Join(dir, name)), &m); err != nil || m.Version != 1 || !m.Sealed {
					t.Fatalf("discard left %s %+v %v", name, m, err)
				}
			}
			refusedAsSealed(t)
		})
	}
}
