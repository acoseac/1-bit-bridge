//go:build unix

package backup_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// TestPlayerUUIDMinterRefusesNonFilesAtOnce pins the minter's nonblocking
// refusal of non-file kinds, leaving the original node in place. A plain
// open of a named pipe would wait for a writer before checking its kind.
func TestPlayerUUIDMinterRefusesNonFilesAtOnce(t *testing.T) {
	for _, tc := range fsutiltest.NonFileCases() {
		t.Run(tc.Name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, config.PlayerUUIDFile)
			pipes := tc.Plant(t, path)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			var id string
			var made bool
			done := make(chan struct{})
			go func() {
				defer close(done)
				id, made, err = backup.EnsurePlayerUUID(dir)
			}()
			// The helper fails a slow refusal, then releases any blocked FIFO
			// read and joins it before the fixture is removed.
			fsutiltest.AwaitPastFIFOs(t, "EnsurePlayerUUID", fsutiltest.ServeBound, done, pipes...)
			if err == nil || errors.Is(err, fs.ErrNotExist) || id != "" || made {
				t.Errorf("EnsurePlayerUUID = %q, %t, %v; want %s refused without an identity", id, made, err, tc.Kind)
			}
			after, statErr := os.Lstat(path)
			if statErr != nil {
				t.Fatalf("original node is no longer present: %v", statErr)
			}
			if !os.SameFile(before, after) || after.Mode() != before.Mode() {
				t.Errorf("original %s node was replaced or changed: before %v, after %v", tc.Kind, before, after)
			}
		})
	}
}
