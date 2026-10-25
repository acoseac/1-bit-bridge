//go:build unix

package backup_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestAMintRefusedForVolatileStorageLeavesNoIdentity: the minter commits the
// floor and the initialization marker through the strict writer before it
// writes the UUID, and that writer refuses storage that does not outlive a
// reboot (here the medium check answering as tmpfs does) before it creates
// anything. A refused mint therefore leaves the data directory as it was, so
// a second one in the same boot finds no identity to take up and is refused
// the same way. An identity the directory held already is kept as it was, and
// its call is refused too: the marker's probe runs for an identity that is
// there, before anything starts on it.
func TestAMintRefusedForVolatileStorageLeavesNoIdentity(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "fresh"
		if held {
			name = "held"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if held {
				disk := atomicwrite.SetMediumForTest(func(string) error { return nil })
				_, _, err := backup.EnsurePlayerUUID(dir)
				disk()
				if err != nil {
					t.Fatal(err)
				}
			}
			before := dirState(t, dir)
			t.Cleanup(atomicwrite.SetMediumForTest(func(root string) error {
				return fmt.Errorf("%s is on tmpfs: %w", root, atomicwrite.ErrVolatile)
			}))
			for call := 1; call <= 2; call++ {
				id, made, err := backup.EnsurePlayerUUID(dir)
				if !errors.Is(err, atomicwrite.ErrVolatile) || id != "" || made {
					t.Fatalf("call %d on volatile storage: %q, made %t, %v", call, id, made, err)
				}
				if after := dirState(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
					t.Fatalf("call %d left the data directory %v, want %v", call, after, before)
				}
			}
		})
	}
}
