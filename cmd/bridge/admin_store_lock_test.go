package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestTheAdminCommandsSayWhenAnotherProcessHoldsTheStoresLock: every writer
// of the console's credential store holds the lock beside it from its check
// through its rename (backlog B881's second round), and a command that waited
// out its bound for a lock another process holds writes nothing and says so,
// in words that say what to do. The lock is held here through an open of the
// test's own, which the store's open is refused beside as another process's
// would be.
func TestTheAdminCommandsSayWhenAnotherProcessHoldsTheStoresLock(t *testing.T) {
	cfgDir := t.TempDir()
	lib := filepath.Join(cfgDir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := publicInit(t, cfgDir, pickPublicInitPorts(t), "Busy", "--library", lib); code != 0 {
		t.Fatalf("bridge init --public = %d:\n%s", code, out)
	}
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	dataDir := filepath.Join(cfgDir, "data")
	storePath := filepath.Join(dataDir, adminauth.FileName)
	before, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := fsutil.OpenLockFile(dataDir, adminauth.LockFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(lock); err != nil {
		lock.Close()
		t.Fatalf("take the store's lock for the test: %v", err)
	}
	t.Cleanup(func() {
		_ = fsutil.Unlock(lock)
		_ = lock.Close()
	})

	const said = "another bridge process is writing the console's credentials; try again"
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
	}{
		{"reset-password", []string{"reset-password", "--config", cfgPath, "--from-stdin"}, "a password nobody gets\n"},
		{"sign-out-everywhere", []string{"sign-out-everywhere", "--config", cfgPath}, ""},
	} {
		var out, errOut bytes.Buffer
		if code := adminCmd(tc.args, strings.NewReader(tc.stdin), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), said) {
			t.Errorf("bridge admin %s beside a held lock = %d, stderr %q; want 1 saying %q", tc.name, code, errOut.String(), said)
		}
	}
	if got, _ := os.ReadFile(storePath); !bytes.Equal(got, before) {
		t.Errorf("a command that never had the lock changed the store:\n%s", got)
	}
}
