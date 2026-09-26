//go:build !windows

package doctor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// These drive the /proc attribution's two halves against fixture files, so
// they run wherever the helpers compile rather than only on Linux. Not on
// Windows, because the fd fixture is a directory of symlinks, which
// Windows creates only with a privilege a CI account may lack.

// writeTable writes a socket-table fixture and returns its path.
func writeTable(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestListenerSocketsReadsBothFamilies: a dual-stack Go listener appears
// only in tcp6 and an IPv4-only one only in tcp, so both tables are read,
// and a table that is missing (a kernel built without IPv6) is not an
// error while the other answers. With neither readable there is no answer,
// and that is an error rather than "nothing listens".
func TestListenerSocketsReadsBothFamilies(t *testing.T) {
	dir := t.TempDir()
	v4 := writeTable(t, dir, "tcp", procNetTCPFixture)
	v6 := writeTable(t, dir, "tcp6", procNetTCP6Fixture)
	missing := filepath.Join(dir, "absent")

	for _, tc := range []struct {
		name    string
		paths   []string
		port    int
		want    map[string]bool
		wantErr bool
	}{
		{"ipv4 listener", []string{v4, v6}, 7789, map[string]bool{"socket:[24680]": true}, false},
		{"ipv6 listener", []string{v4, v6}, 443, map[string]bool{"socket:[6213098]": true}, false},
		{"tcp6 missing", []string{v4, missing}, 7789, map[string]bool{"socket:[24680]": true}, false},
		{"nothing listens", []string{v4, v6}, 8080, map[string]bool{}, false},
		{"no table readable", []string{missing, missing}, 7789, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := listenerSockets(tc.paths, tc.port)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// writeFdDir builds a fixture fd directory the way /proc/<pid>/fd reads:
// one entry per descriptor, each a link to what it is open on. Readlink
// works on the dangling links, which is all the walk asks of them.
func writeFdDir(t *testing.T, links map[string]string) string {
	t.Helper()
	fdDir := t.TempDir()
	for fd, target := range links {
		if err := os.Symlink(target, filepath.Join(fdDir, fd)); err != nil {
			t.Fatal(err)
		}
	}
	return fdDir
}

// fdFixture is a process's descriptors: a socket that is not the
// listener, and the listener on 7789 in procNetTCPFixture (inode 24680).
var fdFixture = map[string]string{
	"0": "/dev/null",
	"1": "pipe:[1342449]",
	"3": "socket:[11111]", // a socket, but not the listener
	"4": "socket:[24680]",
}

// TestFdDirHoldsSocketMatchesOnlyTheListenersInode walks a fixture fd
// directory and matches the listener's inode and nothing else. A directory
// that cannot be listed returns its error and no match; the caller must
// not read that as a broken probe.
func TestFdDirHoldsSocketMatchesOnlyTheListenersInode(t *testing.T) {
	fdDir := writeFdDir(t, fdFixture)
	if held, _, err := fdDirHoldsSocket(fdDir, map[string]bool{"socket:[24680]": true}); !held || err != nil {
		t.Errorf("the descriptor linked to the listener's inode was not found (err %v)", err)
	}
	if held, readAll, err := fdDirHoldsSocket(fdDir, map[string]bool{"socket:[99999]": true}); held || !readAll || err != nil {
		t.Errorf("an inode no descriptor links to: held %v, readAll %v, err %v; want no match, every link read", held, readAll, err)
	}
	// A directory that cannot be listed: the process is gone, or its
	// descriptors are denied to this user. No match, and the error says which.
	if held, _, err := fdDirHoldsSocket(filepath.Join(fdDir, "absent"), map[string]bool{"socket:[24680]": true}); held || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an absent fd directory: held %v, err %v; want no match and ErrNotExist", held, err)
	}
}

// TestProcSightingAccountsForEachMiss gives the /proc attribution every
// shape of miss and pins what it says of each: the account that checkPort's
// liveness arm and checkChosenPort print when the recorded bridge is alive
// and was not seen on the port.
//
// Three rule the pid out, and say so without a blind spot: no socket
// listens on the port, and the pid's descriptors all read without one of
// them. Three leave it possible: the fd directory is not there, cannot be
// listed, or lists and will not let its links be read (a process of this
// user in another group: listing takes the uid, a readlink ptrace's full
// read check). The last two are what a process of another user, or one
// with dumpable=0, looks like, and carry the blind spot the caller passed.
func TestProcSightingAccountsForEachMiss(t *testing.T) {
	dir := t.TempDir()
	tables := []string{writeTable(t, dir, "tcp", procNetTCPFixture), writeTable(t, dir, "tcp6", procNetTCP6Fixture)}
	const blind = "the blind spot"
	unlistable := func(t *testing.T) string {
		fdDir := writeFdDir(t, fdFixture)
		chmodForTest(t, fdDir, 0o000)
		return fdDir
	}
	linksUnreadable := func(t *testing.T) string {
		fdDir := writeFdDir(t, fdFixture)
		chmodForTest(t, fdDir, 0o400) // read without search: lists, and no readlink
		return fdDir
	}
	for _, tc := range []struct {
		name      string
		port      int
		fdDir     func(t *testing.T) string
		needsUser bool // permission bits bind only a user other than root
		want      ownerSighting
		wantFound bool
	}{
		{"the pid holds the listener", 7789, func(t *testing.T) string { return writeFdDir(t, fdFixture) }, false, ownerSighting{}, true},
		{"no socket listens on the port", 8080, func(t *testing.T) string { return writeFdDir(t, fdFixture) }, false,
			ownerSighting{saw: "/proc lists no socket listening on this port", ruledOut: true}, false},
		{"every descriptor read, none the listener", 443, func(t *testing.T) string { return writeFdDir(t, fdFixture) }, false,
			ownerSighting{saw: "/proc shows no descriptor of pid 4242 listening on this port", ruledOut: true}, false},
		{"no such process here", 7789, func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, false,
			ownerSighting{saw: "/proc has no pid 4242"}, false},
		{"the fd directory cannot be listed", 7789, unlistable, true,
			ownerSighting{saw: "/proc does not let this user read pid 4242's descriptors", blind: blind}, false},
		{"the fd directory lists and its links do not read", 7789, linksUnreadable, true,
			ownerSighting{saw: "/proc does not let this user read pid 4242's descriptors", blind: blind}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsUser && os.Geteuid() == 0 {
				t.Skip("root reads a directory whatever its mode")
			}
			found, seen, err := procSighting(tables, tc.fdDir(t), tc.port, 4242, blind)
			if err != nil || found != tc.wantFound || seen != tc.want {
				t.Errorf("got %v, %+v, %v; want %v, %+v, no error", found, seen, err, tc.wantFound, tc.want)
			}
		})
	}

	t.Run("the fd path is not a directory", func(t *testing.T) {
		notDir := filepath.Join(t.TempDir(), "fd")
		if err := os.WriteFile(notDir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		found, seen, err := procSighting(tables, notDir, 7789, 4242, blind)
		if err != nil || found || seen.ruledOut || !strings.HasPrefix(seen.saw, "/proc could not list pid 4242's descriptors (") {
			t.Errorf("got %v, %+v, %v; want a miss naming the listing error", found, seen, err)
		}
	})
	t.Run("no socket table readable", func(t *testing.T) {
		absent := filepath.Join(dir, "absent")
		if _, _, err := procSighting([]string{absent, absent}, writeFdDir(t, fdFixture), 7789, 4242, blind); err == nil {
			t.Error("with neither table readable there is no answer, and that is an error")
		}
	})
}

// chmodForTest sets a fixture directory's mode and restores a mode the
// test's own cleanup can remove.
func chmodForTest(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}
