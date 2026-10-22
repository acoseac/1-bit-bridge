package backup_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The UUIDs these tests give the player: the one a snapshot holds, and the
// one the box has by the time it is restored.
const (
	snapshotPlayerUUID = "0f8fad5b-d9cb-469f-a165-70867728950e"
	boxPlayerUUID      = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
)

// withPlayerUUID gives src a player-uuid in its data dir holding body,
// written at perm.
func withPlayerUUID(t *testing.T, src *backup.Sources, body string, perm os.FileMode) {
	t.Helper()
	src.PlayerUUID = filepath.Join(src.DataDir, config.PlayerUUIDFile)
	if err := os.WriteFile(src.PlayerUUID, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src.PlayerUUID, perm); err != nil {
		t.Fatal(err)
	}
}

// snapshotFiles is the file list a snapshot's manifest.json holds.
func snapshotFiles(t *testing.T, snap string) []string {
	t.Helper()
	var m backup.Manifest
	if err := json.Unmarshal(readBytes(t, filepath.Join(snap, backup.ManifestFile)), &m); err != nil {
		t.Fatal(err)
	}
	return m.Files
}

// TestSnapshotCarriesThePlayerUUIDAndRestoreWritesItAsTheIdentityFile: a
// snapshot copies the player's UUID as it is, 0600 whatever the live file's
// mode, and a restore writes it the way the identity file is always written
// (config.WritePlayerUUIDFile): the UUID and a newline, 0600, whatever white
// space followed it in the snapshot and whatever mode the snapshot's copy
// has, over the identity the box has now.
func TestSnapshotCarriesThePlayerUUIDAndRestoreWritesItAsTheIdentityFile(t *testing.T) {
	if backup.PlayerUUIDFileName != config.PlayerUUIDFile {
		t.Fatalf("the snapshot names %q, the player names %q", backup.PlayerUUIDFileName, config.PlayerUUIDFile)
	}
	live := t.TempDir()
	src := primeLiveState(t, live)
	withPlayerUUID(t, &src, snapshotPlayerUUID+"\r\n", 0o644)

	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(snapshotFiles(t, snap), backup.PlayerUUIDFileName) {
		t.Fatalf("the snapshot lists %v, without %s", snapshotFiles(t, snap), backup.PlayerUUIDFileName)
	}
	inSnap := filepath.Join(snap, backup.PlayerUUIDFileName)
	if got := string(readBytes(t, inSnap)); got != snapshotPlayerUUID+"\r\n" {
		t.Fatalf("the snapshot holds %q, not the live file as it is", got)
	}
	if runtime.GOOS != "windows" {
		if m := mode(t, inSnap); m&0o777 != 0o600 {
			t.Errorf("the snapshot's player-uuid mode = %o, want 0600", m&0o777)
		}
		if err := os.Chmod(inSnap, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The box's player has another identity by the time it is restored.
	if err := os.WriteFile(src.PlayerUUID, []byte(boxPlayerUUID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(snap, backup.Targets{PlayerUUID: src.PlayerUUID}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := string(readBytes(t, src.PlayerUUID)); got != snapshotPlayerUUID+"\n" {
		t.Fatalf("the restored player-uuid holds %q, want the snapshot's UUID and a newline", got)
	}
	if got, err := config.ReadPlayerUUID(live); err != nil || got != snapshotPlayerUUID {
		t.Fatalf("the player reads %q, %v after the restore", got, err)
	}
	if runtime.GOOS != "windows" {
		if m := mode(t, src.PlayerUUID); m&0o777 != 0o600 {
			t.Errorf("the restored player-uuid mode = %o, want 0600", m&0o777)
		}
	}

	// A box with no identity gets the snapshot's.
	fresh := t.TempDir()
	target := filepath.Join(fresh, config.PlayerUUIDFile)
	if err := backup.Restore(snap, backup.Targets{PlayerUUID: target}); err != nil {
		t.Fatalf("Restore onto a box with no identity: %v", err)
	}
	if got, err := config.ReadPlayerUUID(fresh); err != nil || got != snapshotPlayerUUID {
		t.Fatalf("the player reads %q, %v after a restore onto a box with no identity", got, err)
	}
}

// TestSnapshotSkipsAPlayerUUIDTheBoxDoesNotHave: a box whose player was
// never enabled has no player-uuid, and its snapshot lists none.
func TestSnapshotSkipsAPlayerUUIDTheBoxDoesNotHave(t *testing.T) {
	live := t.TempDir()
	src := primeLiveState(t, live)
	src.PlayerUUID = filepath.Join(live, config.PlayerUUIDFile)
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(snapshotFiles(t, snap), backup.PlayerUUIDFileName) || pathExists(t, filepath.Join(snap, backup.PlayerUUIDFileName)) {
		t.Fatal("the snapshot captured a player-uuid that is not on disk")
	}
}

// TestRestoreOfASnapshotWithoutThePlayerUUIDLeavesTheIdentityAlone: a
// snapshot taken before the player's UUID was part of the bundle (or of a
// box with none) restores the files it lists, leaves the box's identity as
// it is, and makes none where there is none. So does a snapshot that lists
// the file and no longer holds it, which is how an operator restores the
// rest of a snapshot whose copy does not hold one UUID.
func TestRestoreOfASnapshotWithoutThePlayerUUIDLeavesTheIdentityAlone(t *testing.T) {
	live := t.TempDir()
	src := primeLiveState(t, live)
	old, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	withPlayerUUID(t, &src, boxPlayerUUID+"\n", 0o600)
	listed, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(listed, backup.PlayerUUIDFileName)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(snapshotFiles(t, listed), backup.PlayerUUIDFileName) {
		t.Fatal("the second snapshot does not list the player-uuid it removed")
	}
	for _, tc := range []struct{ name, snap string }{{"a snapshot from before", old}, {"a snapshot whose copy was removed", listed}} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(src.PlayerUUID, []byte(boxPlayerUUID+"\r\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := backup.Restore(tc.snap, backup.Targets{TokensJSON: src.TokensJSON, PlayerUUID: src.PlayerUUID}); err != nil {
				t.Fatal(err)
			}
			if got := string(readBytes(t, src.PlayerUUID)); got != boxPlayerUUID+"\r\n" {
				t.Fatalf("restore rewrote the box's player-uuid from a snapshot that holds none: %q", got)
			}
			none := filepath.Join(t.TempDir(), config.PlayerUUIDFile)
			if err := backup.Restore(tc.snap, backup.Targets{PlayerUUID: none}); err != nil {
				t.Fatal(err)
			}
			if pathExists(t, none) {
				t.Fatal("restore made a player-uuid the snapshot does not hold")
			}
		})
	}
}

// moveOnLiveDatabase changes the live database at dbPath after a snapshot, as
// a running install does, and lays a -wal and a -shm beside it, as an
// install that stopped without a checkpoint leaves them. It returns the bytes
// of the three, which a restore that writes nothing leaves as they are: a
// restore of bridge.db removes the -wal and the -shm before it renames the
// snapshot's copy over the database.
func moveOnLiveDatabase(t *testing.T, dbPath string) map[string][]byte {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(dbPath, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE after_snapshot (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		dbPath + "-wal": []byte("the live write-ahead log"),
		dbPath + "-shm": []byte("the live shared memory"),
	}
	for p, b := range files {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files[dbPath] = readBytes(t, dbPath)
	return files
}

// TestRestoreRefusesAPlayerUUIDThatIsNotOneUUIDBeforeWritingAnyFile: a
// snapshot's player-uuid is read by the rule the player reads its own with
// (config.ReadPlayerUUIDFile), and one that does not hold one UUID fails the
// restore before any file is written: the box's identity is not replaced
// with it, none is made from it, and nothing the snapshot lists before it is
// restored either, so the install is never half restored. The database is
// the first file a restore writes, and its -wal and -shm go before it, so a
// check made after it would leave a new database beside the old install:
// the live database, which moved on after the snapshot, and its -wal and
// -shm must be as they were (Codex on B505's round 1: the test restored no
// database). The refusal names the snapshot's file and says how to restore
// the rest.
func TestRestoreRefusesAPlayerUUIDThatIsNotOneUUIDBeforeWritingAnyFile(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"a newline", "\n"},
		{"not a UUID", "not-a-uuid\n"},
		{"upper case", strings.ToUpper(snapshotPlayerUUID) + "\n"},
		{"two UUIDs", snapshotPlayerUUID + "\n" + boxPlayerUUID + "\n"},
		{"a byte order mark", "\xef\xbb\xbf" + snapshotPlayerUUID + "\n"},
		{"white space before", " " + snapshotPlayerUUID + "\n"},
		{"text after", snapshotPlayerUUID + " x\n"},
		{"cut short", snapshotPlayerUUID[:35] + "\n"},
		{"too long", snapshotPlayerUUID + strings.Repeat(" ", 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := t.TempDir()
			src := primeLiveState(t, live)
			withPlayerUUID(t, &src, tc.body, 0o600)
			snap, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if got := string(readBytes(t, filepath.Join(snap, backup.PlayerUUIDFileName))); got != tc.body {
				t.Fatalf("the snapshot holds %q, not the live file as it is", got)
			}
			// The install moves on after the snapshot, so a restore that
			// wrote anything shows.
			after := map[string][]byte{
				src.TokensJSON: []byte(`[{"live":true}]`),
				src.ServerCert: []byte("live cert"),
				src.ServerKey:  []byte("live key"),
				src.BridgeYAML: []byte("libraryRoots:\n  - /live\n"),
				src.PlayerUUID: []byte(boxPlayerUUID + "\n"),
			}
			for p, b := range after {
				if err := os.WriteFile(p, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			database := moveOnLiveDatabase(t, src.ManifestDB)
			err = backup.Restore(snap, backup.Targets{
				ManifestDB: src.ManifestDB,
				TokensJSON: src.TokensJSON,
				ServerCert: src.ServerCert,
				ServerKey:  src.ServerKey,
				BridgeYAML: src.BridgeYAML,
				PlayerUUID: src.PlayerUUID,
			})
			inSnap := filepath.Join(snap, backup.PlayerUUIDFileName)
			if err == nil {
				t.Fatal("restore took a player-uuid that does not hold one UUID")
			}
			if msg := err.Error(); !strings.Contains(msg, inSnap) || !strings.Contains(msg, "remove it from the snapshot") {
				t.Errorf("the refusal does not name %s and the way through: %v", inSnap, err)
			}
			for p, want := range after {
				if got := readBytes(t, p); string(got) != string(want) {
					t.Errorf("restore replaced %s before refusing the player-uuid: %q", filepath.Base(p), got)
				}
			}
			for p, want := range database {
				if !pathExists(t, p) {
					t.Errorf("restore removed %s before refusing the player-uuid", filepath.Base(p))
				} else if got := readBytes(t, p); string(got) != string(want) {
					t.Errorf("restore replaced %s before refusing the player-uuid", filepath.Base(p))
				}
			}
			none := filepath.Join(t.TempDir(), config.PlayerUUIDFile)
			if err := backup.Restore(snap, backup.Targets{PlayerUUID: none}); err == nil {
				t.Fatal("restore took a player-uuid that does not hold one UUID")
			}
			if pathExists(t, none) {
				t.Fatal("restore made a player-uuid from one that does not hold one UUID")
			}
		})
	}
}

// TestSnapshotAndRestoreAsRootKeepThePlayerUUID: run as root over an
// install another user owns, the snapshot's copy of the player's UUID and
// the identity file a restore writes each take the install's owner, once.
func TestSnapshotAndRestoreAsRootKeepThePlayerUUID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; these helpers change no owner there")
	}
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	withPlayerUUID(t, &src, snapshotPlayerUUID+"\n", 0o600)
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()

	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	assertGivenAway(t, changes(), []string{
		filepath.Join(dataDir, backup.BackupsDirName),
		snap,
		filepath.Join(snap, backup.ManifestDBFileName),
		filepath.Join(snap, "tokens.json"),
		filepath.Join(snap, "server.crt"),
		filepath.Join(snap, "server.key"),
		filepath.Join(snap, "bridge.yaml"),
		filepath.Join(snap, backup.PlayerUUIDFileName),
		filepath.Join(snap, backup.ManifestFile),
	})

	before := len(changes())
	if err := backup.Restore(snap, backup.Targets{PlayerUUID: src.PlayerUUID}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	assertGivenAway(t, changes()[before:], []string{src.PlayerUUID})
}
