//go:build unix

package backup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

func TestPlayerFloorNonFilesRefusePromptly(t *testing.T) {
	reservationStorage(t)
	for _, name := range []string{"player-ids.json", "player-queue.json"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pipe := filepath.Join(dir, name)
			fsutiltest.MakeFIFO(t, pipe)
			done := make(chan struct{})
			var err error
			go func() { defer close(done); _, _, err = backup.EnsurePlayerUUID(dir) }()
			fsutiltest.AwaitPastFIFOs(t, "player mint", fsutiltest.ServeBound, done, pipe)
			if err == nil || !strings.Contains(err.Error(), name+" is a named pipe, not a file: move it aside") {
				t.Fatalf("non-file remedy: %v", err)
			}
			if name == "player-ids.json" {
				if _, err := backup.ResetPlayer(dir); err == nil {
					t.Fatal("ordinary reset accepted FIFO floor")
				}
				if _, err := backup.DiscardPlayerFloor(dir); err != nil {
					t.Fatal(err)
				}
				if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestResetUnlinksUUIDWithoutReadingIt(t *testing.T) {
	reservationStorage(t)
	for _, kind := range []string{"fifo", "dangling", "unreadable", "malformed", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "player-uuid")
			putMark(t, dir, boxPlayerUUID, 2000000000, 2000000001)
			switch kind {
			case "fifo":
				fsutiltest.MakeFIFO(t, path)
			case "dangling":
				if err := os.Symlink("absent", path); err != nil {
					t.Fatal(err)
				}
			case "unreadable", "malformed":
				mode := os.FileMode(0600)
				if kind == "unreadable" {
					mode = 0000
				}
				if err := os.WriteFile(path, []byte("not an identity"), mode); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan struct{})
			var err error
			go func() { defer close(done); _, err = backup.ResetPlayer(dir) }()
			fsutiltest.AwaitPastFIFOs(t, "reset", fsutiltest.ServeBound, done, path)
			if kind == "directory" {
				if err == nil || !strings.Contains(err.Error(), "directory") {
					t.Fatalf("directory removed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("UUID remains: %v", err)
			}
			if r := markAt(t, dir); r.IDs != 2000000000 || r.Tokens != 2000000001 {
				t.Fatalf("floor: %+v", r)
			}
		})
	}
}

func TestFloorAccessFailureNamesItsRemedy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inaccessible")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	if _, err := os.Lstat(filepath.Join(dir, "player-ids.json")); !os.IsPermission(err) {
		t.Skip("host can traverse a directory without permission")
	}
	for _, op := range []func() error{func() error { return backup.CheckPlayerStart(dir) }, func() error { _, err := backup.ResetPlayer(dir); return err }} {
		if err := op(); err == nil || !strings.Contains(err.Error(), "restore read access") || !strings.Contains(err.Error(), "--discard-floor") {
			t.Fatalf("access remedy: %v", err)
		}
	}
}
