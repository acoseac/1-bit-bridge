//go:build unix

package backup_test

import (
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscardRepairsEveryBadLocalMarker(t *testing.T) {
	reservationStorage(t)
	for _, name := range []string{"player-initialized", "player-floor-discarded.json"} {
		for _, kind := range []string{"truncated", "garbage", "fifo", "directory", "dangling"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, name)
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				switch kind {
				case "fifo":
					fsutiltest.MakeFIFO(t, path)
				case "directory":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				case "dangling":
					if err := os.Symlink("absent", path); err != nil {
						t.Fatal(err)
					}
				default:
					b := []byte{}
					if kind == "garbage" {
						b = []byte("not JSON")
					}
					if err := os.WriteFile(path, b, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if name == "player-initialized" {
					if err := backup.CheckPlayerStart(dir); err != nil && !strings.Contains(err.Error(), "move it aside") {
						t.Fatalf("unusable remedy: %v", err)
					}
				}
				done := make(chan struct{})
				var err error
				go func() { defer close(done); _, err = backup.DiscardPlayerFloor(dir) }()
				fsutiltest.AwaitPastFIFOs(t, "discard bad marker", fsutiltest.ServeBound, done, path)
				if err != nil {
					t.Fatalf("discard recovery: %v", err)
				}
				aside, err := filepath.Glob(path + ".discarded-*")
				if err != nil || len(aside) != 1 {
					t.Fatalf("bad marker not preserved aside: %v %v", aside, err)
				}
				if string(readBytes(t, path)) != "{\"version\":1,\"sealed\":true}\n" {
					t.Fatalf("marker not repaired: %s", readBytes(t, path))
				}
				if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
					t.Fatalf("start after recovery: %v", err)
				}
				firstValues(t, dir, markAt(t, dir))
			})
		}
	}
}
