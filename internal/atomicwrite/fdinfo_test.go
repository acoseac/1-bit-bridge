package atomicwrite

import (
	"errors"
	"io/fs"
	"slices"
	"syscall"
	"testing"
)

// TestParseFdinfoMountID pins the reading of a descriptor's fdinfo listing
// (proc(5)), where the kernel lists the mount a descriptor lies on when statx
// gives no mount ID: the number on its mnt_id line; and none for a listing
// without that line, or with one that does not read as a number.
func TestParseFdinfoMountID(t *testing.T) {
	const listing = "pos:\t0\nflags:\t012100000\nmnt_id:\t29\nino:\t1048577\n"
	if id, ok := parseFdinfoMountID([]byte(listing)); !ok || id != 29 {
		t.Fatalf("parseFdinfoMountID = %d, %v, want 29", id, ok)
	}
	for _, bad := range []string{"pos:\t0\nflags:\t02\n", "mnt_id:\tx\n", ""} {
		if id, ok := parseFdinfoMountID([]byte(bad)); ok {
			t.Errorf("parseFdinfoMountID(%q) = %d, true, want none", bad, id)
		}
	}
}

// TestFdinfoMountIDSaysWhatFailedToRead pins the Claude API's round 9
// finding: of a descriptor's fdinfo listing that cannot be read, only one
// that does not exist, with no /proc mounted, gives no mount identity; any
// other failure is the read's error, returned as it is (an *fs.PathError),
// never "the platform gives no mount identity". A process one descriptor
// short of its limit, the O_PATH open taking the last, reads the listing
// with EMFILE, which round 9 turned into ErrMountsUnknown, and New then told
// the operator so of what was descriptor exhaustion.
func TestFdinfoMountIDSaysWhatFailedToRead(t *testing.T) {
	const name = "/proc/self/fdinfo/7"
	refused := func(errno syscall.Errno) func(string) ([]byte, error) {
		return func(n string) ([]byte, error) { return nil, &fs.PathError{Op: "open", Path: n, Err: errno} }
	}
	for _, tc := range []struct {
		name string
		read func(string) ([]byte, error)
		id   uint64
		ok   bool
		err  error
	}{
		{"a listing", func(string) ([]byte, error) { return []byte("pos:\t0\nflags:\t012100000\nmnt_id:\t29\n"), nil }, 29, true, nil},
		{"no /proc", refused(syscall.ENOENT), 0, false, nil},
		{"no descriptor left", refused(syscall.EMFILE), 0, false, syscall.EMFILE},
		{"a read that fails", refused(syscall.EIO), 0, false, syscall.EIO},
	} {
		id, ok, err := fdinfoMountIDFrom(tc.read, name)
		var pe *fs.PathError
		switch {
		case id != tc.id || ok != tc.ok:
			t.Errorf("%s: fdinfoMountIDFrom = %d, %v, %v, want %d, %v", tc.name, id, ok, err, tc.id, tc.ok)
		case tc.err == nil && err != nil:
			t.Errorf("%s: fdinfoMountIDFrom = %v, want no error", tc.name, err)
		case tc.err != nil && (!errors.Is(err, tc.err) || !errors.As(err, &pe) || pe.Path != name):
			t.Errorf("%s: fdinfoMountIDFrom = %v, want the read's error, an *fs.PathError naming %s", tc.name, err, name)
		}
	}
}

// TestMountIDFromFallsBackToFdinfoOnlyWhereStatxGivesNone pins the order in
// which Linux asks for a mount ID (mountIDFrom): statx's where it gives one,
// fdinfo never asked; fdinfo's where statx gives none, a kernel older than
// 5.8 or a statx a policy refuses, which leaves a Raspberry Pi on Linux 5.4
// with an identity; statx's error, fdinfo never asked, since an error is the
// answer; and none where neither gives one, which the writer refuses
// (ErrMountsUnknown).
func TestMountIDFromFallsBackToFdinfoOnlyWhereStatxGivesNone(t *testing.T) {
	errStat := errors.New("statx: no such file or directory")
	gives := func(id uint64, ok bool, err error) func(string) (uint64, bool, error) {
		return func(string) (uint64, bool, error) { return id, ok, err }
	}
	for _, tc := range []struct {
		name          string
		statx, fdinfo func(string) (uint64, bool, error)
		id            uint64
		ok            bool
		err           error
		fdinfoAsked   bool
	}{
		{"statx gives one", gives(7, true, nil), gives(9, true, nil), 7, true, nil, false},
		{"statx gives none", gives(0, false, nil), gives(9, true, nil), 9, true, nil, true},
		{"statx fails", gives(0, false, errStat), gives(9, true, nil), 0, false, errStat, false},
		{"neither gives one", gives(0, false, nil), gives(0, false, nil), 0, false, nil, true},
	} {
		var asked []string
		fdinfo := func(path string) (uint64, bool, error) {
			asked = append(asked, path)
			return tc.fdinfo(path)
		}
		id, ok, err := mountIDFrom("/data", tc.statx, fdinfo)
		if id != tc.id || ok != tc.ok || err != tc.err {
			t.Errorf("%s: mountIDFrom = %d, %v, %v, want %d, %v, %v", tc.name, id, ok, err, tc.id, tc.ok, tc.err)
		}
		if got := slices.Equal(asked, []string{"/data"}); got != tc.fdinfoAsked {
			t.Errorf("%s: fdinfo was asked for %v, want it asked: %v", tc.name, asked, tc.fdinfoAsked)
		}
	}
}
