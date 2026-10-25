//go:build unix

package backup_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestALockFileLinkNamesTheEntryAndWhatItLinksTo: server.lock as a link. A
// chain to a file this user may write but not read (0200), and a link to a
// missing file in a subdirectory it may not write (0500), both refuse the
// open the link leads to. The advice is the lock file's union, which names the
// entry's access and what the link leads to, whatever refused.
func TestALockFileLinkNamesTheEntryAndWhatItLinksTo(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	for _, link := range []string{"chain_to_0200", "dangling_into_0500"} {
		t.Run(link, func(t *testing.T) {
			dir := loneSealBox(t)
			lock := filepath.Join(dir, "server.lock")
			switch link {
			case "chain_to_0200":
				if err := os.WriteFile(filepath.Join(dir, "actual.lock"), nil, 0o200); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "actual.lock"), 0o600) })
				if err := os.Symlink("actual.lock", filepath.Join(dir, "middle.lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("middle.lock", lock); err != nil {
					t.Fatal(err)
				}
			case "dangling_into_0500":
				locks := filepath.Join(dir, "locks")
				if err := os.Mkdir(locks, 0o700); err != nil {
					t.Fatal(err)
				}
				chmodFor(t, locks, 0o500, 0o700)
				if err := os.Symlink(filepath.Join("locks", "missing"), lock); err != nil {
					t.Fatal(err)
				}
			}
			want := entryAdvice(dir, "server.lock", "read and write")
			if line := completionWarning(t, dir, true); !strings.Contains(line, want) {
				t.Fatalf("advice for a server.lock link:\n%s\nwant it to end:\n%s", line, want)
			}
		})
	}
}

// TestALockFileLinkLeadingOutIsNeverFollowed: a server.lock that is a link
// leading out of the data directory, by an absolute or a relative target, is
// refused by the data directory's root and never followed, so the file it
// names is never made. The refusal ("path escapes from parent") is no
// permission failure and takes no remedy: a seal completion logs it with its
// error alone and the backup goes on, and an adoption refuses the backup with
// it, never busy, the player's state as it was.
func TestALockFileLinkLeadingOutIsNeverFollowed(t *testing.T) {
	reservationStorage(t)
	const escape = "path escapes from parent"
	for _, target := range []string{"absolute", "relative"} {
		for _, step := range []string{"completion", "adoption"} {
			t.Run(target+"_"+step, func(t *testing.T) {
				dir := loneSealBox(t)
				if step == "adoption" {
					dir = uuidOnlyBox(t)
				}
				link := filepath.Join(t.TempDir(), "named.lock")
				if target == "relative" {
					link = filepath.Join("..", "named.lock")
				}
				named := link
				if !filepath.IsAbs(named) {
					named = filepath.Join(dir, named)
				}
				if err := os.Symlink(link, filepath.Join(dir, "server.lock")); err != nil {
					t.Fatal(err)
				}
				if step == "completion" {
					if line := completionWarning(t, dir, true); !strings.HasSuffix(line, "player seal completion failed: openat server.lock: "+escape) {
						t.Fatalf("a completion over a lock link leading out logged:\n%s\nwant the refusal alone", line)
					}
				} else {
					before := playerFiles(t, dir)
					snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
					if err == nil || snap != "" || !strings.HasSuffix(err.Error(), "player initialization failed: openat server.lock: "+escape) ||
						strings.Contains(err.Error(), "busy") {
						t.Fatalf("an adoption over a lock link leading out: %q %v", snap, err)
					}
					if after := playerFiles(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
						t.Fatalf("the refused adoption changed the player's state: %v -> %v", before, after)
					}
				}
				if _, err := os.Lstat(named); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the file the lock link names, %s, was made: %v", named, err)
				}
			})
		}
	}
}
