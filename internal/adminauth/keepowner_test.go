package adminauth

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestAWriteAsRootKeepsTheAdminStoreOwners pins that the files this
// package writes, adminauth.json and each login ticket's file, give their
// staged file to the owner of the file it replaces (fsutil.KeepOwner), or
// for a ticket, which is always a new file, to the owner of its directory.
// A `sudo bridge admin reset-password` (or sign-out-everywhere, or
// login-link) beside a service install otherwise left a root-owned 0600
// file, and the running bridge answered 503 to every console request, or
// could not read the ticket it was asked to redeem. Driven through
// fsutil.SimulateRootForTest, since the chown itself needs root; fsutil's
// TestKeepOwnerAsRoot pins the real one.
func TestAWriteAsRootKeepsTheAdminStoreOwners(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; KeepOwner does nothing there")
	}
	path := filepath.Join(t.TempDir(), "adminauth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()

	if _, err := s.MintInitial("admin"); err != nil {
		t.Fatal(err)
	}
	store := fsutil.OwnerChange{Dst: path, UID: 4242, GID: 4243}
	if got := changes(); len(got) != 1 || got[0] != store {
		t.Fatalf("after MintInitial, owner changes = %+v, want exactly %+v", got, store)
	}

	raw, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	ticket := fsutil.OwnerChange{Dst: s.ticketFilePath(hashTicket(raw)), UID: 4242, GID: 4243}
	if got := changes(); len(got) != 2 || got[1] != ticket {
		t.Fatalf("after MintLoginTicket, owner changes = %+v, want %+v last", got, ticket)
	}
}
