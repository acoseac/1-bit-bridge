//go:build unix

package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// TestWhatIsNotAFileAtPlayerUUIDIsRefusedAtOnce: the player's identity is
// read only from a file, opened as the routes that serve a file's bytes open
// one (fsutil.OpenAsFile). A named pipe, a link to one, a link to a
// character device and a directory at player-uuid are each refused at once
// by every reader, named by their kind, with an error that is neither
// fs.ErrNotExist (no identity, which backup.EnsurePlayerUUID would make) nor
// ErrNotOnePlayerUUID, and none of them is replaced or removed. A plain open
// of the pipe waited for a writer, and nothing could cancel the wait; the
// device read as a file that does not hold one UUID.
func TestWhatIsNotAFileAtPlayerUUIDIsRefusedAtOnce(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	for _, tc := range []struct {
		name, kind string
		// plant puts the entry at path and returns the named pipe a read
		// that waits is waiting on, if there is one.
		plant func(t *testing.T, path string) []string
	}{
		{"a named pipe", "named pipe", func(t *testing.T, path string) []string {
			fsutiltest.MakeFIFO(t, path)
			return []string{path}
		}},
		{"a link to a named pipe", "named pipe", func(t *testing.T, path string) []string {
			pipe := filepath.Join(t.TempDir(), "pipe")
			fsutiltest.MakeFIFO(t, pipe)
			if err := os.Symlink(pipe, path); err != nil {
				t.Fatal(err)
			}
			return []string{pipe}
		}},
		{"a link to a character device", "character device", func(t *testing.T, path string) []string {
			if err := os.Symlink(os.DevNull, path); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"a directory", "directory", func(t *testing.T, path string) []string {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, PlayerUUIDFile)
			pipes := tc.plant(t, path)
			planted, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []struct {
				name string
				read func() error
			}{
				{"ReadPlayerUUIDFile", func() error { _, err := ReadPlayerUUIDFile(path); return err }},
				{"ReadPlayerUUID", func() error { _, err := ReadPlayerUUID(dir); return err }},
				{"ForgetPlayerUUID", func() error { return ForgetPlayerUUID(dir, id) }},
			} {
				var err error
				done := make(chan struct{})
				go func() {
					defer close(done)
					err = r.read()
				}()
				fsutiltest.AwaitPastFIFOs(t, r.name, fsutiltest.ServeBound, done, pipes...)
				if kind := fsutil.NotAFileKind(err); kind != tc.kind || errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNotOnePlayerUUID) {
					t.Errorf("%s: %v (refused as %q); want %s refused as one", r.name, err, kind, tc.name)
				}
				if now, err := os.Lstat(path); err != nil || now.Mode() != planted.Mode() {
					t.Errorf("%s: player-uuid is %v (%v), want %v left as it was", r.name, now, err, planted.Mode())
				}
			}
		})
	}
}
