//go:build unix

package backup_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// restoreWithin runs backup.Restore on a goroutine of its own, bounded by
// fsutiltest.ServeBound with the writer played on pipe past it
// (fsutiltest.AwaitPastFIFOs), so a restore that waits on the pipe fails
// the test rather than hang it.
func restoreWithin(t *testing.T, snap string, dst backup.Targets, pipe string) error {
	t.Helper()
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		err = backup.Restore(snap, dst)
	}()
	fsutiltest.AwaitPastFIFOs(t, "Restore", fsutiltest.ServeBound, done, pipe)
	return err
}

// TestRestoreRefusesANamedPipeForThePlayerUUIDAtOnce: a snapshot's copy of
// the player's UUID is read by the rule the player reads its own with
// (config.ReadPlayerUUIDFile), which opens only a file and never waits: a
// named pipe in its place fails the restore at once, before any file is
// written, naming the copy and what it is, where the read waited for a
// writer and held the restore. It is no identity to restore, and no reason
// to skip one either: the box keeps its own, and nothing else the snapshot
// lists is restored, the database and its -wal and -shm included.
func TestRestoreRefusesANamedPipeForThePlayerUUIDAtOnce(t *testing.T) {
	live := t.TempDir()
	src := primeLiveState(t, live)
	withPlayerUUID(t, &src, snapshotPlayerUUID+"\n", 0o600)
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	inSnap := filepath.Join(snap, backup.PlayerUUIDFileName)
	if err := os.Remove(inSnap); err != nil {
		t.Fatal(err)
	}
	fsutiltest.MakeFIFO(t, inSnap)
	// The install moves on after the snapshot, so a restore that wrote
	// anything shows.
	after := map[string][]byte{
		src.TokensJSON: []byte(`[{"live":true}]`),
		src.BridgeYAML: []byte("libraryRoots:\n  - /live\n"),
		src.PlayerUUID: []byte(boxPlayerUUID + "\n"),
	}
	for p, b := range after {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	database := moveOnLiveDatabase(t, src.ManifestDB)

	err = restoreWithin(t, snap, backup.Targets{
		ManifestDB: src.ManifestDB,
		TokensJSON: src.TokensJSON,
		BridgeYAML: src.BridgeYAML,
		PlayerUUID: src.PlayerUUID,
	}, inSnap)
	if kind := fsutil.NotAFileKind(err); kind != "named pipe" || !strings.Contains(err.Error(), inSnap) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Errorf("Restore with a named pipe for the snapshot's player-uuid: %v (refused as %q); want it refused as a named pipe, naming %s, with nothing restored", err, kind, inSnap)
	}
	for p, want := range after {
		if got := readBytes(t, p); string(got) != string(want) {
			t.Errorf("restore replaced %s before refusing the snapshot's player-uuid: %q", filepath.Base(p), got)
		}
	}
	for p, want := range database {
		if !pathExists(t, p) {
			t.Errorf("restore removed %s before refusing the snapshot's player-uuid", filepath.Base(p))
		} else if got := readBytes(t, p); string(got) != string(want) {
			t.Errorf("restore replaced %s before refusing the snapshot's player-uuid", filepath.Base(p))
		}
	}
}

// TestRestoreOverANamedPipeAtTheBoxsPlayerUUIDDoesNotWait: history preflight
// must read the identity whose floor it would replace. A named pipe is refused
// immediately, before any write, because its identity cannot be remembered.
// The pipe, queue and reservation remain intact for the operator to repair.
func TestRestoreOverANamedPipeAtTheBoxsPlayerUUIDDoesNotWait(t *testing.T) {
	reservationStorage(t)
	live := t.TempDir()
	src := primeLiveState(t, live)
	withPlayerUUID(t, &src, snapshotPlayerUUID+"\n", 0o600)
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := os.Remove(src.PlayerUUID); err != nil {
		t.Fatal(err)
	}
	fsutiltest.MakeFIFO(t, src.PlayerUUID)
	queue := filepath.Join(live, "player-queue.json")
	if err := os.WriteFile(queue, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	putMark(t, live, boxPlayerUUID, 90000, 80000)
	reservation := filepath.Join(live, "player-ids.json")
	before := readBytes(t, reservation)
	err = restoreWithin(t, snap, backup.Targets{PlayerUUID: src.PlayerUUID}, src.PlayerUUID)
	if kind := fsutil.NotAFileKind(err); kind != "named pipe" || !strings.Contains(err.Error(), "read live player identity") {
		t.Errorf("restore must refuse the unreadable live identity at once: %v (kind %q)", err, kind)
	}
	// Lstat only: reading the preserved pipe would wait for a writer.
	if info, err := os.Lstat(src.PlayerUUID); err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("refused restore replaced the identity pipe: %v (%v)", info, err)
	}
	if got := string(readBytes(t, queue)); got != `{"version":1}` {
		t.Errorf("refused restore changed queue: %s", got)
	}
	if got := string(readBytes(t, reservation)); got != string(before) {
		t.Errorf("refused restore changed reservation: %s", got)
	}
	if _, err := os.Stat(filepath.Join(live, backup.PlayerHistoryFileName)); !os.IsNotExist(err) {
		t.Errorf("refused restore wrote history: %v", err)
	}
}
