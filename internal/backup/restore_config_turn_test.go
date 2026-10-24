package backup_test

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// restorableBox is an install with a player identity, snapshotted, that has
// moved on since: every file a restore writes holds other bytes than the
// snapshot's, the player's identity, reservation and queue included, so a
// restore that wrote any of them shows.
type restorableBox struct {
	src     backup.Sources
	snap    string
	targets backup.Targets
	// queue is the player's queue file, and live every file a restore can
	// write, with the bytes it holds now.
	queue string
	live  map[string][]byte
}

// newRestorableBox lays out restorableBox in a directory of its own.
func newRestorableBox(t *testing.T) *restorableBox {
	t.Helper()
	reservationStorage(t)
	dir := t.TempDir()
	src := primeLiveState(t, dir)
	withPlayerUUID(t, &src, snapshotPlayerUUID+"\n", 0o600)
	putMark(t, dir, snapshotPlayerUUID, 10000, 30000)
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	b := &restorableBox{src: src, snap: snap, queue: filepath.Join(dir, "player-queue.json")}
	for p, data := range map[string][]byte{
		src.TokensJSON: []byte(`[{"live":true}]`),
		src.ServerCert: []byte("live cert"),
		src.ServerKey:  []byte("live key"),
		src.BridgeYAML: []byte("libraryRoots:\n  - /live\n"),
		src.PlayerUUID: []byte(boxPlayerUUID + "\n"),
	} {
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	putMark(t, dir, boxPlayerUUID, 20000, 40000)
	seedDestinationQueue(t, dir, boxPlayerUUID, 2)
	b.live = moveOnLiveDatabase(t, src.ManifestDB)
	for _, p := range []string{src.TokensJSON, src.ServerCert, src.ServerKey, src.BridgeYAML, src.PlayerUUID,
		filepath.Join(dir, engine.ReservationFileName), b.queue} {
		b.live[p] = readBytes(t, p)
	}
	b.targets = backup.Targets{
		ManifestDB: src.ManifestDB,
		TokensJSON: src.TokensJSON,
		ServerCert: src.ServerCert,
		ServerKey:  src.ServerKey,
		BridgeYAML: src.BridgeYAML,
		PlayerUUID: src.PlayerUUID,
	}
	return b
}

// requireUntouched fails the test for each file a restore can write that no
// longer holds what it held before the restore, but those in except.
func (b *restorableBox) requireUntouched(t *testing.T, except ...string) {
	t.Helper()
	for p, want := range b.live {
		if slicesContains(except, p) {
			continue
		}
		if !pathExists(t, p) {
			t.Errorf("the refused restore removed %s", filepath.Base(p))
		} else if got := readBytes(t, p); !bytes.Equal(got, want) {
			t.Errorf("the refused restore wrote %s", filepath.Base(p))
		}
	}
}

// slicesContains reports whether list holds s.
func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// hasTable reports whether the database at path holds a table named name.
func hasTable(t *testing.T, path, name string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// edit changes the config at path as an edit does, or a bridge older than the
// config's lock, which takes no turns, and returns what it wrote.
func edit(t *testing.T, path string) []byte {
	t.Helper()
	edited := []byte("libraryRoots:\n  - /edited/while/the/restore/ran\n")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Error(err)
	}
	return edited
}

// TestARestoreRefusedBeforeItsFirstWriteRestoresNothing: a restore compares
// the config file with the one its turn found once it has read the snapshot,
// before its first write, so a config an edit (or a bridge older than the
// lock) changed meanwhile refuses the restore with nothing restored: the
// player's reservation, identity and queue, the database and every file are
// as they were. Until B506's third round the comparison came only at
// bridge.yaml's own write, after every file before it was restored.
func TestARestoreRefusedBeforeItsFirstWriteRestoresNothing(t *testing.T) {
	b := newRestorableBox(t)
	var edited []byte
	opts := backup.WithBeforeWritesForTest(backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 7})}, func() {
		edited = edit(t, b.src.BridgeYAML)
	})
	err := backup.RestoreWithOptions(b.snap, b.targets, opts)
	if err == nil || !strings.Contains(err.Error(), "changed") || !strings.Contains(err.Error(), "nothing was restored") {
		t.Errorf("a restore over a config another writer changed before its first write = %v, want a refusal that says the file changed and nothing was restored", err)
	}
	b.requireUntouched(t, b.src.BridgeYAML)
	if got := readBytes(t, b.src.BridgeYAML); !bytes.Equal(got, edited) {
		t.Errorf("the restore wrote over the other writer's bridge.yaml:\n%s", got)
	}
}

// TestARestoreRefusesAConfigItsTargetsDidNotComeFrom: a restore given the
// config as its caller read it, when the caller worked out the targets from
// it (bridge restore reads it before the data dir's lock and the prompt),
// refuses with nothing restored a config that no longer holds those bytes
// once the restore has its turn: its data dir or its TLS pair may be
// elsewhere now. It asks whether or not the snapshot carries bridge.yaml.
func TestARestoreRefusesAConfigItsTargetsDidNotComeFrom(t *testing.T) {
	for _, tc := range []struct {
		name string
		// withoutConfig takes bridge.yaml out of the snapshot.
		withoutConfig bool
	}{
		{"a snapshot that carries bridge.yaml", false},
		{"a snapshot without it", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newRestorableBox(t)
			if tc.withoutConfig {
				writeSnapshotManifest(t, b.snap, "bridge.db", "tokens.json", "server.crt", "server.key", backup.PlayerUUIDFileName, engine.ReservationFileName)
			}
			asRead := b.live[b.src.BridgeYAML]
			edited := edit(t, b.src.BridgeYAML)
			err := backup.RestoreWithOptions(b.snap, b.targets, backup.RestoreOptions{
				Random: bytes.NewReader([]byte{0, 0, 7}), ConfigAsRead: asRead,
			})
			if err == nil || !strings.Contains(err.Error(), "changed") || !strings.Contains(err.Error(), "nothing was restored") {
				t.Errorf("a restore whose config changed since its caller read it = %v, want a refusal that says so and that nothing was restored", err)
			}
			b.requireUntouched(t, b.src.BridgeYAML)
			if got := readBytes(t, b.src.BridgeYAML); !bytes.Equal(got, edited) {
				t.Errorf("the restore wrote over the changed bridge.yaml:\n%s", got)
			}
		})
	}
}

// TestARestoreLeavesAConfigChangedInItsTurn: a restore that writes
// bridge.yaml compares the file, just before the rename that would replace
// it, with what the restore found once it held the file's lock, so a config
// an edit (or a bridge older than the lock) changed meanwhile, as the copies
// before it ran, is left as that writer left it. The restore says which files
// it restored before it and that running the restore again finishes it (which
// it does: the second restore puts every file back as the snapshot has it),
// never that nothing was written. Where no lock can be taken that comparison
// is all that keeps another writer's save; until B506's second round a
// restore had none, and until its third the refusal said nothing was written.
func TestARestoreLeavesAConfigChangedInItsTurn(t *testing.T) {
	b := newRestorableBox(t)
	var edited []byte
	opts := backup.WithBeforeConfigForTest(backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 7})}, func() {
		edited = edit(t, b.src.BridgeYAML)
	})
	err := backup.RestoreWithOptions(b.snap, b.targets, opts)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a restore over a config another writer changed in its turn = %v, want a refusal that says the file changed", err)
	}
	if got := readBytes(t, b.src.BridgeYAML); !bytes.Equal(got, edited) {
		t.Errorf("the restore wrote the snapshot's bridge.yaml over the other writer's:\n%s", got)
	}
	msg := err.Error()
	for _, want := range []string{"run the restore again", "bridge.db", "tokens.json", "server.crt", "server.key", backup.PlayerUUIDFileName} {
		if !strings.Contains(msg, want) {
			t.Errorf("the late refusal %q does not say %q", msg, want)
		}
	}
	if strings.Contains(msg, "nothing was written") || strings.Contains(msg, "nothing was restored") {
		t.Errorf("the late refusal %q says nothing was written, after the files before bridge.yaml were restored", msg)
	}

	if err := backup.RestoreWithOptions(b.snap, b.targets, backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 9})}); err != nil {
		t.Fatalf("running the restore again: %v", err)
	}
	for _, name := range []string{"tokens.json", "server.crt", "server.key", "bridge.yaml"} {
		target := map[string]string{"tokens.json": b.src.TokensJSON, "server.crt": b.src.ServerCert,
			"server.key": b.src.ServerKey, "bridge.yaml": b.src.BridgeYAML}[name]
		if got, want := readBytes(t, target), readBytes(t, filepath.Join(b.snap, name)); !bytes.Equal(got, want) {
			t.Errorf("after the second restore %s holds %q, want the snapshot's %q", name, got, want)
		}
	}
	if hasTable(t, b.src.ManifestDB, "after_snapshot") {
		t.Error("after the second restore the database is still the live one")
	}
	if id, err := config.ReadPlayerUUID(b.src.DataDir); err != nil || id != snapshotPlayerUUID {
		t.Errorf("after the second restore the player is %q, %v; want the snapshot's identity", id, err)
	}
	if mark := markAt(t, b.src.DataDir); mark.UUID != snapshotPlayerUUID || mark.IDs <= 20000 || mark.Tokens <= 40000 {
		t.Errorf("after the second restore the reservation is %+v, want the snapshot's identity past both floors", mark)
	}
	if pathExists(t, b.queue) {
		t.Error("after the second restore the box's queue for its replaced identity is still there")
	}
}
