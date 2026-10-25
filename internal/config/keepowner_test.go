package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestSaveAsRootKeepsTheConfigOwner pins that Save gives its staged file
// to the owner of the bridge.yaml it replaces (fsutil.KeepOwner), so a
// `sudo bridge library add` beside a service install leaves a config the
// service can still read at its next start; and that the lock beside it,
// which the first writer creates (LockFile), takes the directory's owner
// too (fsutil.OpenLockFile's Precreate), or the service could not open it
// for its next save. Driven through fsutil.SimulateRootForTest, since the
// chown itself needs root; fsutil's TestKeepOwnerAsRoot pins the real one.
func TestSaveAsRootKeepsTheConfigOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; KeepOwner does nothing there")
	}
	path := filepath.Join(realTempDir(t), "bridge.yaml")
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	var c Config
	if err := saveAt(&c, path); err != nil {
		t.Fatal(err)
	}
	want := []fsutil.OwnerChange{
		{Dst: filepath.Join(filepath.Dir(path), LockFileName(path)), UID: 4242, GID: 4243},
		{Dst: path, UID: 4242, GID: 4243},
	}
	if got := changes(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
}

// TestALockFileMadeAsRootTakesTheConfigsOwner pins whose the lock file is
// when a writer run as root creates it: the config file's owner, the owner
// Save keeps for the file it replaces, and only while there is no config
// file yet the directory's. Until B506's second round it took the
// directory's: a user's config in a directory root owns (/tmp, a mount
// point) had its lock made root's 0600 by the first `sudo bridge library
// add`, and every later save of the config's own user failed to open it,
// though Save had kept the config theirs. The two owners differ here, which
// TestSaveAsRootKeepsTheConfigOwner's one owner cannot show.
func TestALockFileMadeAsRootTakesTheConfigsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; no owner is given there")
	}
	// The directory's owner stands for root's, as /tmp's is. It is not 0:0,
	// so that a test run as root, whose new file is already 0:0, still
	// sees the change to it.
	const dirUID, dirGID = 4300, 4301
	const cfgUID, cfgGID = 4242, 4243 // the config's: the service user's
	for _, tc := range []struct {
		name       string
		withConfig bool
		uid, gid   int
	}{
		{"beside a config file", true, cfgUID, cfgGID},
		{"before there is one", false, dirUID, dirGID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := realTempDir(t)
			path := filepath.Join(dir, "bridge.yaml")
			if tc.withConfig {
				if err := os.WriteFile(path, []byte("libraryName: Theirs\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			changes, restore := fsutil.SimulateRootOwnersForTest(func(p string) (int, int) {
				if p == path {
					return cfgUID, cfgGID
				}
				return dirUID, dirGID
			})
			defer restore()
			l, err := LockFile(path)
			if err != nil {
				t.Fatal(err)
			}
			l.Unlock()
			lockPath := filepath.Join(dir, LockFileName(path))
			want := fsutil.OwnerChange{Dst: lockPath, UID: tc.uid, GID: tc.gid}
			if got := changes(); len(got) != 1 || got[0] != want {
				t.Errorf("owner changes = %+v, want exactly %+v", got, want)
			}
			info, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0o600 != 0o600 {
				t.Errorf("the lock file's mode %v does not let its owner open it to read and write", info.Mode().Perm())
			}
		})
	}
}
