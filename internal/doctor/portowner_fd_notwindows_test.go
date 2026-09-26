//go:build !windows

package doctor

import (
	"os"
	"path/filepath"
	"reflect"
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

// TestFdDirHoldsSocketMatchesOnlyTheListenersInode walks a fixture fd
// directory the way /proc/<pid>/fd reads: one entry per descriptor, each a
// link to what it is open on. Readlink works on the dangling links, which
// is all the walk asks of them.
func TestFdDirHoldsSocketMatchesOnlyTheListenersInode(t *testing.T) {
	fdDir := t.TempDir()
	for fd, target := range map[string]string{
		"0": "/dev/null",
		"1": "pipe:[1342449]",
		"3": "socket:[11111]", // a socket, but not the listener
		"4": "socket:[24680]",
	} {
		if err := os.Symlink(target, filepath.Join(fdDir, fd)); err != nil {
			t.Fatal(err)
		}
	}
	if !fdDirHoldsSocket(fdDir, map[string]bool{"socket:[24680]": true}) {
		t.Error("the descriptor linked to the listener's inode was not found")
	}
	if fdDirHoldsSocket(fdDir, map[string]bool{"socket:[99999]": true}) {
		t.Error("matched an inode no descriptor links to")
	}
	// A directory that cannot be listed: the process is gone, or its
	// descriptors are denied to this user. No match, not an error.
	if fdDirHoldsSocket(filepath.Join(fdDir, "absent"), map[string]bool{"socket:[24680]": true}) {
		t.Error("an unlistable fd directory matched")
	}
}
