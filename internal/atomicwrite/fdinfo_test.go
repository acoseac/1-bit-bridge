package atomicwrite

import (
	"errors"
	"slices"
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
