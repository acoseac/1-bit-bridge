package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

const round11PlayerUUID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

// fastPlayerStorage stands in for the directory and medium barriers, as
// resetInstall does, so thousands of engine starts stay quick.
func fastPlayerStorage(t *testing.T) {
	t.Helper()
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
}

// setRestoreRandom fixes bridge restore's random source until the test ends.
func setRestoreRandom(t *testing.T, r io.Reader) {
	t.Helper()
	prev := restoreRandom
	restoreRandom = r
	t.Cleanup(func() { restoreRandom = prev })
}

// adoptedAtTwoToThe31 writes what adoption writes at a zero draw: the UUID,
// a floor of 2^31 on both counters naming it, and the initialization marker.
func adoptedAtTwoToThe31(t *testing.T, dir, id string) {
	t.Helper()
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), id); err != nil {
		t.Fatal(err)
	}
	if err := engine.WriteReservation(dir, filepath.Join(dir, playerReservationFile), engine.Reservation{UUID: id, IDs: 1 << 31, Tokens: 1 << 31}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "player-initialized"), []byte("{\"version\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// engineStartToken starts the player's engine on dir once, as serve does,
// stops it, and returns the token it published at startup.
func engineStartToken(t *testing.T, dir string) uint32 {
	t.Helper()
	id, err := config.ReadPlayerUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.Config{PlayerUUID: id, Backend: newPlayerStandIn(), DataDir: dir,
		QueuePath: filepath.Join(dir, playerQueueFile), ReservePath: filepath.Join(dir, playerReservationFile)})
	if err != nil {
		t.Fatal(err)
	}
	token := e.Snapshot().Token()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Run(ctx) }()
	drainLoopOnCleanup(t, stop, done, "the player's engine")
	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the engine did not stop")
	}
	return token
}

// historicalManifest rewrites a snapshot's manifest as every writer before
// this one's record wrote it (main since 5a0, and e396b625): the floor listed
// and intact beside the UUID, and no playerFloorRequired. edit then changes
// its file list, as a damaged byte would.
func historicalManifest(t *testing.T, snap string, edit func([]any) []any) {
	t.Helper()
	rewriteManifest(t, snap, true, edit)
}

// rewriteManifest edits a snapshot's file list through a map, dropping the
// playerFloorRequired record when dropRecord is set.
func rewriteManifest(t *testing.T, snap string, dropRecord bool, edit func([]any) []any) {
	t.Helper()
	p := filepath.Join(snap, backup.ManifestFile)
	var m map[string]any
	if err := json.Unmarshal(readFileT(t, p), &m); err != nil {
		t.Fatal(err)
	}
	if dropRecord {
		delete(m, "playerFloorRequired")
	}
	files, _ := m["files"].([]any)
	m["files"] = edit(files)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}

// renameEntry renames one entry of a manifest's file list.
func renameEntry(t *testing.T, from, to string) func([]any) []any {
	return func(files []any) []any {
		t.Helper()
		found := false
		for i, f := range files {
			if f == from {
				files[i], found = to, true
			}
		}
		if !found {
			t.Fatalf("the manifest lists no %s: %v", from, files)
		}
		return files
	}
}

// dropEntry removes one entry from a manifest's file list.
func dropEntry(t *testing.T, name string) func([]any) []any {
	return func(files []any) []any {
		t.Helper()
		kept := files[:0]
		for _, f := range files {
			if f != name {
				kept = append(kept, f)
			}
		}
		if len(kept) == len(files) {
			t.Fatalf("the manifest lists no %s: %v", name, files)
		}
		return kept
	}
}

// topLevelEntries is dir's entries by name: a directory as "<dir>", anything
// else by its bytes.
func topLevelEntries(t *testing.T, dir string) map[string]string {
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
		state[e.Name()] = string(readFileT(t, filepath.Join(dir, e.Name())))
	}
	return state
}

// playerFilesIn is which of the player's files dir holds.
func playerFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	for _, name := range []string{config.PlayerUUIDFile, playerReservationFile, "player-initialized", playerQueueFile} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			found = append(found, name)
		}
	}
	return found
}

// requireDamagedManifestRefusal requires bridge restore to have refused snap
// as a damaged manifest, naming the snapshot remedy and nothing older, with
// none of the snapshot's player state or library written.
func requireDamagedManifestRefusal(t *testing.T, code int, stderr, dstDir string, dbBefore []byte) {
	t.Helper()
	if code == 0 {
		t.Fatal("restore went on past a damaged manifest")
	}
	for _, want := range []string{"does not list player-ids.json", "copy of the snapshot", "newer snapshot of this box", "nothing was restored"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal lacks %q: %s", want, stderr)
		}
	}
	if strings.Contains(stderr, "another snapshot") {
		t.Errorf("refusal steers to an older snapshot: %s", stderr)
	}
	if files := playerFilesIn(t, dstDir); len(files) != 0 {
		t.Errorf("refused restore wrote player state: %v", files)
	}
	if !bytes.Equal(dbBefore, readFileT(t, filepath.Join(dstDir, "bridge.db"))) {
		t.Error("refused restore replaced the library")
	}
}

// TestRestoreRefusesAFloorItsManifestDoesNotList is Codex's round-10
// sequence through bridge restore. A box adopted at 2^31 starts 4,097 times
// and is captured by a writer that lists the floor but records no
// playerFloorRequired. One damaged entry, player-ids.json read as
// player-ids.jsoo, sent the intact floor down the legacy path: the 2^31 + 2^24
// ceiling and a zero draw gave the restored player the source's last token,
// 2,181,042,177. A floor the snapshot holds and its manifest does not list is
// a damaged manifest, and restore refuses it before any write.
func TestRestoreRefusesAFloorItsManifestDoesNotList(t *testing.T) {
	needsDirectorySync(t)
	fastPlayerStorage(t)
	srcCfg, srcDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	adoptedAtTwoToThe31(t, srcDir, round11PlayerUUID)
	var last uint32
	for i := 0; i < 4097; i++ {
		token := engineStartToken(t, srcDir)
		if want := uint32(1<<31) + uint32(i)*8192 + 4097; token != want {
			t.Fatalf("startup token %d at start %d, want %d", token, i, want)
		}
		last = token
	}
	if last != 2_181_042_177 {
		t.Fatalf("the source's last startup token %d", last)
	}
	snap := runBackup(t, srcCfg)
	if floor, err := engine.ReadReservation(filepath.Join(snap, playerReservationFile)); err != nil || floor.IDs != 1<<31+4097*8192 || floor.Tokens != 1<<31+4097*8192 {
		t.Fatalf("captured floor %+v %v", floor, err)
	}
	historicalManifest(t, snap, renameEntry(t, playerReservationFile, "player-ids.jsoo"))
	dstCfg, dstDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	dbBefore := readFileT(t, filepath.Join(dstDir, "bridge.db"))
	setRestoreRandom(t, bytes.NewReader(make([]byte, 6)))
	code, _, stderr := runRestoreYes(t, dstCfg, snap)
	if code == 0 {
		t.Fatalf("restore went on past a damaged manifest: the restored player's first startup token is %d, and the source's last was %d", engineStartToken(t, dstDir), last)
	}
	requireDamagedManifestRefusal(t, code, stderr, dstDir, dbBefore)
}

// TestRestoreRefusesAnUnlistedFloorEntryWithoutFollowingIt: an unlisted
// player-ids.json of any type marks a damaged manifest. A directory, and a
// symbolic link to nothing, which a reader that followed it would take for
// no floor, are refused and never followed.
func TestRestoreRefusesAnUnlistedFloorEntryWithoutFollowingIt(t *testing.T) {
	needsDirectorySync(t)
	fastPlayerStorage(t)
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			srcCfg, srcDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
			if _, _, err := backup.EnsurePlayerUUID(srcDir); err != nil {
				t.Fatal(err)
			}
			snap := runBackup(t, srcCfg)
			historicalManifest(t, snap, renameEntry(t, playerReservationFile, "player-ids.jsoo"))
			floor := filepath.Join(snap, playerReservationFile)
			if err := os.Remove(floor); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "directory" {
				err = os.Mkdir(floor, 0700)
			} else {
				err = os.Symlink(playerReservationFile+".nowhere", floor)
			}
			if err != nil {
				t.Fatal(err)
			}
			dstCfg, dstDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
			dbBefore := readFileT(t, filepath.Join(dstDir, "bridge.db"))
			setRestoreRandom(t, bytes.NewReader(make([]byte, 6)))
			code, _, stderr := runRestoreYes(t, dstCfg, snap)
			requireDamagedManifestRefusal(t, code, stderr, dstDir, dbBefore)
		})
	}
}

// TestAnUnlistedFloorTheSnapshotDoesNotHoldIsStillAbsent guards the other
// side of that refusal through the same command: with no player-ids.json in
// the snapshot either, the floor is genuinely absent. A snapshot from a
// writer that records no playerFloorRequired takes the round-10 ceiling, its
// floor 2^31 + 2^24 + 2^24 at a zero draw, with the missing-mark warning;
// one this bridge wrote, which carries the record, was stripped and is
// refused.
func TestAnUnlistedFloorTheSnapshotDoesNotHoldIsStillAbsent(t *testing.T) {
	needsDirectorySync(t)
	fastPlayerStorage(t)
	for _, writer := range []string{"older_writer", "this_bridge"} {
		t.Run(writer, func(t *testing.T) {
			srcCfg, srcDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
			if _, _, err := backup.EnsurePlayerUUID(srcDir); err != nil {
				t.Fatal(err)
			}
			snap := runBackup(t, srcCfg)
			rewriteManifest(t, snap, writer == "older_writer", dropEntry(t, playerReservationFile))
			if err := os.Remove(filepath.Join(snap, playerReservationFile)); err != nil {
				t.Fatal(err)
			}
			dstCfg, dstDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
			dbBefore := readFileT(t, filepath.Join(dstDir, "bridge.db"))
			setRestoreRandom(t, bytes.NewReader(make([]byte, 6)))
			code, out, stderr := runRestoreYes(t, dstCfg, snap)
			if writer == "this_bridge" {
				if code == 0 || !strings.Contains(stderr, "lists beside every player UUID") || !strings.Contains(stderr, "nothing was restored") {
					t.Fatalf("this bridge's stripped snapshot: %d %s %s", code, out, stderr)
				}
				if files := playerFilesIn(t, dstDir); len(files) != 0 || !bytes.Equal(dbBefore, readFileT(t, filepath.Join(dstDir, "bridge.db"))) {
					t.Fatalf("the refused restore wrote %v or the library", files)
				}
				return
			}
			if code != 0 || !strings.Contains(out, "no player reservation mark") {
				t.Fatalf("an older writer's floorless snapshot: %d %s %s", code, out, stderr)
			}
			floor, err := engine.ReadReservation(filepath.Join(dstDir, playerReservationFile))
			const want = 1<<31 + 1<<24 + 1<<24
			if err != nil || floor.IDs != want || floor.Tokens != want {
				t.Fatalf("restored floor %+v %v, want %d on both counters", floor, err, want)
			}
		})
	}
}

// TestADamagedManifestRefusesBeforeAdoptingTheDestination: onto a box whose
// player holds only a UUID, the upgrade state restore adopts before its jump,
// the damaged manifest is refused first. The box keeps its UUID-only state
// byte for byte, with no floor or marker written and no random byte drawn,
// and its library as it was.
func TestADamagedManifestRefusesBeforeAdoptingTheDestination(t *testing.T) {
	needsDirectorySync(t)
	fastPlayerStorage(t)
	srcCfg, srcDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	if _, _, err := backup.EnsurePlayerUUID(srcDir); err != nil {
		t.Fatal(err)
	}
	snap := runBackup(t, srcCfg)
	historicalManifest(t, snap, renameEntry(t, playerReservationFile, "player-ids.jsoo"))
	dstCfg, dstDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	if err := config.WritePlayerUUIDFile(filepath.Join(dstDir, config.PlayerUUIDFile), round11PlayerUUID); err != nil {
		t.Fatal(err)
	}
	before := topLevelEntries(t, dstDir)
	draws := bytes.NewReader(make([]byte, 6))
	setRestoreRandom(t, draws)
	code, out, stderr := runRestoreYes(t, dstCfg, snap)
	if code == 0 || !strings.Contains(stderr, "does not list player-ids.json") || !strings.Contains(stderr, "nothing was restored") {
		t.Fatalf("restore of a damaged manifest onto a UUID-only box: %d %s %s", code, out, stderr)
	}
	if draws.Len() != 6 {
		t.Errorf("the refused restore drew %d random bytes", 6-draws.Len())
	}
	after := topLevelEntries(t, dstDir)
	if lock, ok := after["server.lock"]; ok && before["server.lock"] == "" && lock == "" {
		delete(after, "server.lock")
		delete(before, "server.lock")
	}
	for name, was := range before {
		if after[name] != was {
			t.Errorf("the refused restore changed %s", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("the refused restore added %s", name)
		}
	}
}

// TestARefusedDamagedManifestOnlyCompletesTheLocalSeal: restore completes the
// destination's own seal before it reads the snapshot, which only adds seal
// evidence the box owes. Onto a box whose discard left its seal file and lost
// player-initialized, the refused restore of a damaged manifest may seal that
// marker; nothing else changes: no floor, adoption or identity from the
// snapshot, and the library as it was.
func TestARefusedDamagedManifestOnlyCompletesTheLocalSeal(t *testing.T) {
	needsDirectorySync(t)
	fastPlayerStorage(t)
	srcCfg, srcDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	if _, _, err := backup.EnsurePlayerUUID(srcDir); err != nil {
		t.Fatal(err)
	}
	snap := runBackup(t, srcCfg)
	historicalManifest(t, snap, renameEntry(t, playerReservationFile, "player-ids.jsoo"))
	dstCfg, dstDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	if _, err := backup.DiscardPlayerFloor(dstDir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backup.EnsurePlayerUUID(dstDir); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dstDir, "player-initialized")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	before := topLevelEntries(t, dstDir)
	setRestoreRandom(t, bytes.NewReader(make([]byte, 6)))
	code, out, stderr := runRestoreYes(t, dstCfg, snap)
	if code == 0 || !strings.Contains(stderr, "does not list player-ids.json") || !strings.Contains(stderr, "nothing was restored") {
		t.Fatalf("restore of a damaged manifest onto a box owing its seal: %d %s %s", code, out, stderr)
	}
	after := topLevelEntries(t, dstDir)
	if sealed, ok := after["player-initialized"]; ok {
		var m struct {
			Version        int
			Sealed, Minted bool
		}
		if err := json.Unmarshal([]byte(sealed), &m); err != nil || m.Version != 1 || !m.Sealed || !m.Minted {
			t.Fatalf("the completion wrote %q %v", sealed, err)
		}
		delete(after, "player-initialized")
	}
	if lock, ok := after["server.lock"]; ok && before["server.lock"] == "" && lock == "" {
		delete(after, "server.lock")
		delete(before, "server.lock")
	}
	for name, was := range before {
		if after[name] != was {
			t.Errorf("the refused restore changed %s", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("the refused restore added %s", name)
		}
	}
}

// TestRestoreKeepsTheBoxIdentityWhenItsUUIDEntryIsDamaged: the same damage to
// the player-uuid entry of such a snapshot makes the restore identity-free.
// The box keeps its own identity and floor byte for byte, so nothing the
// snapshot's identity handed out can come back.
func TestRestoreKeepsTheBoxIdentityWhenItsUUIDEntryIsDamaged(t *testing.T) {
	needsDirectorySync(t)
	fastPlayerStorage(t)
	srcCfg, srcDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	if _, _, err := backup.EnsurePlayerUUID(srcDir); err != nil {
		t.Fatal(err)
	}
	snap := runBackup(t, srcCfg)
	historicalManifest(t, snap, renameEntry(t, config.PlayerUUIDFile, "player-uuie"))
	dstCfg, dstDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	if _, _, err := backup.EnsurePlayerUUID(dstDir); err != nil {
		t.Fatal(err)
	}
	floorBefore := readFileT(t, filepath.Join(dstDir, playerReservationFile))
	idBefore := readFileT(t, filepath.Join(dstDir, config.PlayerUUIDFile))
	code, out, stderr := runRestoreYes(t, dstCfg, snap)
	if code != 0 {
		if !bytes.Equal(floorBefore, readFileT(t, filepath.Join(dstDir, playerReservationFile))) {
			t.Fatalf("a refused restore changed the floor: %s", stderr)
		}
		return
	}
	if !strings.Contains(out, "Restore complete") {
		t.Fatalf("restore %d: %s %s", code, out, stderr)
	}
	if !bytes.Equal(floorBefore, readFileT(t, filepath.Join(dstDir, playerReservationFile))) || !bytes.Equal(idBefore, readFileT(t, filepath.Join(dstDir, config.PlayerUUIDFile))) {
		t.Fatal("an identity-free restore changed the box's identity or floor")
	}
}

// TestDiscardFloorRepairsANonFileSealFromTheStateItself: bridge reset-player
// --discard-floor on the round-10 hazard's state, a seal that is a directory
// beside no initialized marker and before any start sealed it: the directory
// is kept aside, a fresh regular seal replaces it, and identity restores stay
// refused.
func TestDiscardFloorRepairsANonFileSealFromTheStateItself(t *testing.T) {
	cfg, dir := resetInstall(t, "127.0.0.1:0")
	if _, err := backup.DiscardPlayerFloor(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	seal := filepath.Join(dir, "player-floor-discarded.json")
	if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(seal); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(seal, 0700); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := run(t.Context(), []string{"reset-player", "--discard-floor", "--config", cfg, "--yes"}, &out, &errs); code != 0 {
		t.Fatalf("discard over a directory seal: %d %s", code, &errs)
	}
	st, err := os.Lstat(seal)
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("no regular seal after discard: %v", err)
	}
	var fresh struct {
		Version int
		Sealed  bool
	}
	if err := json.Unmarshal(readFileT(t, seal), &fresh); err != nil || fresh.Version != 1 || !fresh.Sealed {
		t.Fatalf("fresh seal %+v %v", fresh, err)
	}
	aside, err := filepath.Glob(seal + ".discarded-*")
	if err != nil || len(aside) != 1 {
		t.Fatalf("the directory was not kept aside: %v %v", aside, err)
	}
	if st, err := os.Lstat(aside[0]); err != nil || !st.IsDir() {
		t.Fatalf("aside entry: %v", err)
	}
	identityDir := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(identityDir, config.PlayerUUIDFile), round11PlayerUUID); err != nil {
		t.Fatal(err)
	}
	if err := engine.WriteReservation(identityDir, filepath.Join(identityDir, playerReservationFile), engine.Reservation{UUID: round11PlayerUUID, IDs: 9, Tokens: 9}); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: identityDir, PlayerUUID: filepath.Join(identityDir, config.PlayerUUIDFile)})
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runRestoreYes(t, cfg, snap)
	if code == 0 || !strings.Contains(stderr, "sealed") {
		t.Fatalf("identity restore after the repair: %d %s", code, stderr)
	}
}
