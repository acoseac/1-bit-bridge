package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestServeOnVolatileStorageRefusesEveryStartAndKeepsNoIdentity: on a data
// directory whose storage does not outlive a reboot (the medium check
// answering as tmpfs does), serve's player is refused at its identity, and a
// refused start leaves none behind: the minter writes the floor and the
// marker through the strict writer, which refuses that storage before it
// creates anything, and the UUID after them. So serve's next start in the
// same boot finds no identity to start the player under, and is refused the
// same way. An identity the data directory held already is kept as it was,
// and each start on it is refused at its marker's probe, before the engine
// is built.
func TestServeOnVolatileStorageRefusesEveryStartAndKeepsNoIdentity(t *testing.T) {
	needsDirectorySync(t)
	for _, held := range []bool{false, true} {
		name := "fresh"
		if held {
			name = "held"
		}
		t.Run(name, func(t *testing.T) {
			t.Cleanup(atomicwrite.SetMediumForTest(func(root string) error {
				return fmt.Errorf("%s is on tmpfs: %w", root, atomicwrite.ErrVolatile)
			}))
			var before map[string]string
			c := playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(), before: func(dataDir string) {
				if held {
					disk := atomicwrite.SetMediumForTest(func(string) error { return nil })
					_, _, err := backup.EnsurePlayerUUID(dataDir)
					disk()
					if err != nil {
						t.Fatal(err)
					}
				}
				before = playerFileBytes(t, dataDir)
			}}
			ps := bootPlayerServe(t, c)
			for start := 1; start <= 2; start++ {
				if eng := ps.engine(t); eng != nil || !strings.HasPrefix(ps.player.failed, "the player's identity: ") ||
					!strings.Contains(ps.player.failed, "is on tmpfs") {
					t.Fatalf("start %d on volatile storage: engine %t, refused %q", start, eng != nil, ps.player.failed)
				}
				if after := playerFileBytes(t, ps.dataDir); fmt.Sprint(after) != fmt.Sprint(before) {
					t.Fatalf("start %d left the player's files %v, want %v", start, after, before)
				}
				if code := ps.stopServe(t); code != 0 {
					t.Fatalf("start %d: serve exit %d; stderr=%s", start, code, ps.stderr.String())
				}
				if start == 1 {
					again := c
					again.standIn, again.before = newPlayerStandIn(), nil
					ps = bootPlayerServeAt(t, again, filepath.Dir(ps.configPath), ps.music, ps.dataDir)
				}
			}
		})
	}
}

// playerFileBytes is each of the player's files dataDir holds, with its
// bytes.
func playerFileBytes(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, name := range playerFilesIn(t, dataDir) {
		b, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(b)
	}
	return files
}
