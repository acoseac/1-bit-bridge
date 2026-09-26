package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusFSUIDReadsTheFourthUidValue: /proc/<pid>/status lists a
// process's real, effective, saved and filesystem uids on its Uid: line, and
// the census compares the FOURTH with the uid that created a listener,
// because a socket is stamped with its creator's fsuid. Measured on dido
// (2026-09-26): a root process that called setfsuid(1234) and then listened
// shows "Uid: 0 0 0 1234", and its row in /proc/net/tcp reads 1234. Anything
// that does not name a uid there is -1, which rules nothing out.
func TestStatusFSUIDReadsTheFourthUidValue(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		want         int
	}{
		{"every uid the same", "Name:\tbridge\nUmask:\t0022\nUid:\t1000\t1000\t1000\t1000\nGid:\t1000\t1000\t1000\t1000\n", 1000},
		{"the fsuid apart from the rest", "Name:\tpython3\nUid:\t0\t0\t0\t1234\nGid:\t0\t0\t0\t0\n", 1234},
		{"the overflow uid, as an unmapped one renders", "Uid:\t65534\t65534\t65534\t65534\n", 65534},
		{"root", "Uid:\t0\t0\t0\t0\n", 0},
		{"no Uid line", "Name:\tbridge\nGid:\t1000\t1000\t1000\t1000\n", -1},
		{"a Uid line of three values", "Uid:\t1000\t1000\t1000\n", -1},
		{"a fourth value that does not parse", "Uid:\t1000\t1000\t1000\tx\n", -1},
		{"a negative fourth value", "Uid:\t1\t1\t1\t-5\n", -1},
		{"an empty file", "", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusFSUID(strings.NewReader(tc.status)); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPIDFSUIDReadsTheStatusUnderProcRoot: pidFSUID reads <procRoot>/<pid>/status,
// and a pid with no status there is -1.
func TestPIDFSUIDReadsTheStatusUnderProcRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "4242"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "4242", "status"), []byte("Uid:\t1000\t1000\t1000\t1001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pidFSUID(root, 4242); got != 1001 {
		t.Errorf("pid 4242: got %d, want its fsuid 1001", got)
	}
	if got := pidFSUID(root, 5000); got != -1 {
		t.Errorf("a pid with no status: got %d, want -1", got)
	}
}

// TestCreatedByAnotherSaysNothingOfAnUnknownOrEqualUID: a listener is shown
// to be another uid's only when both uids are known and differ. The two
// files render a uid through the same user namespace, the reader's, so
// different values name different uids even when one of them is the
// overflow uid an unmapped one renders as (65534); only equal values are
// ambiguous, since two unmapped uids render the same, and equal is never
// another's. -1 is a value /proc did not show.
func TestCreatedByAnotherSaysNothingOfAnUnknownOrEqualUID(t *testing.T) {
	for _, tc := range []struct {
		creator, pidUID int
		want            bool
	}{
		{1001, 1000, true},
		{0, 1000, true},
		{1000, 0, true},
		{1000, 1000, false},
		{0, 0, false},
		{65534, 65534, false},
		{65534, 1000, true},
		{1001, 65534, true},
		{-1, 1000, false},
		{1001, -1, false},
		{-1, -1, false},
	} {
		if got := createdByAnother(tc.creator, tc.pidUID); got != tc.want {
			t.Errorf("createdByAnother(%d, %d) = %v, want %v", tc.creator, tc.pidUID, got, tc.want)
		}
	}
}

// TestOthersListeningAccountNamesHoldersThenCreators pins /proc's account of
// a port it rules the recorded pid out of: the readable processes that hold
// its listeners, which an operator can stop, and the uids that created the
// rest, against the uid the pid runs as. Holders alone keep #1030's words.
func TestOthersListeningAccountNamesHoldersThenCreators(t *testing.T) {
	for _, tc := range []struct {
		name              string
		holders, creators []int
		want              string
	}{
		{"holders only", []int{5000}, nil,
			"/proc shows every socket listening on this port held by pid 5000"},
		{"two holders, one of them twice", []int{6000, 5000, 6000}, nil,
			"/proc shows every socket listening on this port held by pids 5000, 6000"},
		{"a creator only", nil, []int{1001},
			"/proc shows every socket listening on this port created by uid 1001, while pid 4242 runs as uid 1000"},
		{"two creators, one of them twice", nil, []int{1001, 0, 1001},
			"/proc shows every socket listening on this port created by uids 0, 1001, while pid 4242 runs as uid 1000"},
		{"a holder and a creator", []int{5000}, []int{0},
			"/proc shows every socket listening on this port held by pid 5000 or created by uid 0, while pid 4242 runs as uid 1000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := othersListening{holders: tc.holders, creators: tc.creators, pidUID: 1000}
			if got := o.account(4242); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
