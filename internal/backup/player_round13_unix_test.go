//go:build unix

package backup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
