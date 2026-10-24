//go:build unix

package backup_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// TestSnapshotRefusesALivePlayerUUIDPipeAtOnce exercises the capture of
// the live identity, before Snapshot reads its captured copy as a UUID.
func TestSnapshotRefusesALivePlayerUUIDPipeAtOnce(t *testing.T) {
	snapshotRefusesLivePipe(t, backup.PlayerUUIDFileName)
}

// TestSnapshotRefusesALiveTokensPipeAtOnce requires the same nonblocking
// refusal for a source copied without the player's identity reader.
func TestSnapshotRefusesALiveTokensPipeAtOnce(t *testing.T) {
	snapshotRefusesLivePipe(t, "tokens.json")
}

// snapshotRefusesLivePipe checks the source open, the value-free refusal
// naming that source, and removal of the partially populated snapshot.
func snapshotRefusesLivePipe(t *testing.T, name string) {
	t.Helper()
	live := t.TempDir()
	src := primeLiveState(t, live)
	withPlayerUUID(t, &src, snapshotPlayerUUID+"\n", 0o600)
	pipe := filepath.Join(live, name)
	if err := os.Remove(pipe); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	var snap string
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		snap, err = backup.Snapshot(t.Context(), src)
	}()
	fsutiltest.AwaitPastFIFOs(t, "Snapshot", fsutiltest.ServeBound, done, pipe)
	var pathErr *fs.PathError
	var notFile *fsutil.NotAFileError
	if !errors.As(err, &pathErr) || pathErr.Path != pipe ||
		!errors.As(pathErr.Err, &notFile) || notFile.Kind != "named pipe" {
		t.Errorf("Snapshot error = %v; want a PathError naming %s around a named-pipe NotAFileError", err, pipe)
	}
	want := "copy " + name + ": open " + pipe + ": named pipe is not a file"
	if err == nil || err.Error() != want {
		t.Errorf("Snapshot error = %v; want %s (file name and kind only)", err, want)
	}
	if snap != "" {
		t.Errorf("Snapshot returned a directory on refusal: %s", snap)
	}
	entries, readErr := os.ReadDir(filepath.Join(live, backup.BackupsDirName))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("failed Snapshot left %d entries in backups; want no partial snapshot directory", len(entries))
	}
	if info, statErr := os.Lstat(pipe); statErr != nil || info.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("Snapshot changed its source pipe: %v (%v)", info, statErr)
	}
}
