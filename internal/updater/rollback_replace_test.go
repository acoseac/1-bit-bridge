package updater

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// runningPathRename is the rename Windows gives a rollback that
// replaces a mapped image: the destination still exists, so the call
// is refused, and a destination that does not exist is a real rename.
// The first call of a correct rollback moves dst to a new name.
func runningPathRename(calls *[][2]string) func(string, string) error {
	return func(oldpath, newpath string) error {
		*calls = append(*calls, [2]string{oldpath, newpath})
		if _, err := os.Stat(newpath); err == nil {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EACCES}
		}
		return os.Rename(oldpath, newpath)
	}
}

func useRename(t *testing.T, fn func(string, string) error) {
	t.Helper()
	prev := renameFunc
	t.Cleanup(func() { renameFunc = prev })
	renameFunc = fn
}

// recordWaits makes the rollback's waits between its attempts return at
// once, each recorded, until the test ends.
func recordWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	prev := rollbackWait
	t.Cleanup(func() { rollbackWait = prev })
	rollbackWait = func(d time.Duration) { waits = append(waits, d) }
	return &waits
}

// asidesOf lists the rollback's leftovers beside dst, each with what it
// holds.
func asidesOf(t *testing.T, dst string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatal(err)
	}
	var asides []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), filepath.Base(dst)+".rollback-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(filepath.Dir(dst), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		asides = append(asides, string(b))
	}
	return asides
}

func TestRollbackReplaceVacatesARunningPathBeforeRenamingBak(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(dst, []byte("RUNNING"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	useRename(t, runningPathRename(&calls))

	if err := replaceRunningBinary(dst, bak); err != nil {
		t.Fatal(err)
	}
	if len(calls) < 2 {
		t.Fatalf("renames = %v, want the vacate and then bak onto dst", calls)
	}
	if calls[0][0] != dst {
		t.Fatalf("first rename moved %s, want the running path", calls[0][0])
	}
	aside := calls[0][1]
	if aside == bak || !strings.Contains(filepath.Base(aside), ".rollback-") {
		t.Fatalf("aside = %s, want a .rollback- name that is not the backup", aside)
	}
	if calls[1][0] != bak || calls[1][1] != dst {
		t.Fatalf("second rename = %s -> %s, want the backup onto dst", calls[1][0], calls[1][1])
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "KNOWN-GOOD" {
		t.Fatalf("dst = %q, want the backup bytes", got)
	}
	if _, err := os.Stat(bak); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup stat = %v, want consumed", err)
	}
	if _, err := os.Stat(aside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aside stat = %v, want the unmapped leftover removed", err)
	}
}

// TestRollbackReplaceRestoresDstWhenTheSecondRenameFails: the backup's
// rename refused for good, as a mapped image's is (Access is denied, which
// no wait changes), is made once and not again; the running binary is put
// back from the aside, the error says "(restored)", and the backup stays
// beside it, no aside left.
func TestRollbackReplaceRestoresDstWhenTheSecondRenameFails(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(dst, []byte("RUNNING"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	n := 0
	useRename(t, func(oldpath, newpath string) error {
		n++
		calls = append(calls, [2]string{oldpath, newpath})
		if n == 2 {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EACCES}
		}
		if _, err := os.Stat(newpath); err == nil {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EACCES}
		}
		return os.Rename(oldpath, newpath)
	})
	waits := recordWaits(t)

	err := replaceRunningBinary(dst, bak)
	if err == nil || !strings.HasSuffix(err.Error(), "(restored)") || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("replaceRunningBinary = %v, want the backup's refusal, the binary restored", err)
	}
	if len(calls) != 3 || calls[1][0] != bak || calls[1][1] != dst || calls[2][0] != calls[0][1] || calls[2][1] != dst {
		t.Fatalf("renames = %v, want the vacate, the backup's rename once, and the aside renamed back onto dst", calls)
	}
	if len(*waits) != 0 {
		t.Errorf("the rollback waited %v over a refusal that is not a file in use, want no wait", *waits)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "RUNNING" {
		t.Fatalf("dst = %q, want the running bytes restored", got)
	}
	got, err = os.ReadFile(bak)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "KNOWN-GOOD" {
		t.Fatalf("backup = %q, want it left in place", got)
	}
	if asides := asidesOf(t, dst); len(asides) != 0 {
		t.Errorf("asides left beside the binary: %v", asides)
	}
}

func TestRollbackReplaceOfAMissingDestinationIsOneRename(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	useRename(t, runningPathRename(&calls))

	if err := replaceRunningBinary(dst, bak); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0][0] != bak || calls[0][1] != dst {
		t.Fatalf("renames = %v, want one rename of the backup onto dst", calls)
	}
}

func TestRollbackReplaceLeavesALeftoverItCannotRemove(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	if err := os.WriteFile(dst, []byte("RUNNING"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(dir, "bridge.exe.rollback-stuck")
	if err := os.Mkdir(stuck, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "held"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	useRename(t, runningPathRename(&calls))

	if err := replaceRunningBinary(dst, bak); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stuck); err != nil {
		t.Fatalf("stuck leftover: %v", err)
	}
	if calls[0][1] == stuck {
		t.Fatal("vacate replaced the leftover that could not be removed")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "KNOWN-GOOD" {
		t.Fatalf("dst = %q", got)
	}
}

// The rollback's four renames, by the stage each makes.
const (
	stageOntoMissing = "the backup onto a missing binary"
	stageAside       = "the running binary moved aside"
	stageBackup      = "the backup put in place"
	stagePutBack     = "the aside put back"
)

// theSchedule is the waits between one rename's seven attempts that
// rollbackRenameBackoff's doc gives: 50 ms doubling to 1.6 s, 3.15 s.
var theSchedule = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
	400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}

// waitsBefore is what theSchedule waits before a rename's attempts after
// the first, of n.
func waitsBefore(n int) []time.Duration {
	return theSchedule[:max(0, min(n-1, len(theSchedule)))]
}

// rollbackStage is the stage of the rename oldpath to newpath in a rollback
// of dst to bak; missing says dst was not there as the rollback began.
func rollbackStage(dst, bak string, missing bool, oldpath, newpath string) string {
	aside := filepath.Base(dst) + ".rollback-"
	switch {
	case oldpath == bak && newpath == dst && missing:
		return stageOntoMissing
	case oldpath == dst && strings.HasPrefix(filepath.Base(newpath), aside):
		return stageAside
	case oldpath == bak && newpath == dst:
		return stageBackup
	case strings.HasPrefix(filepath.Base(oldpath), aside) && newpath == dst:
		return stagePutBack
	}
	return ""
}

// TestEachRollbackRenameIsMadeAgainWhileItsFileIsInUse: each of the
// rollback's four renames, refused because another process holds its file
// (a sharing violation on Windows: Defender holding the backup the start
// has just run, backlog B872), is made again on its own after each of the
// schedule's waits, every other rename once: never the rollback whole.
// Refused twice, it lands at its third attempt and the rollback goes on
// from where it is. Refused at every attempt, after seven attempts and
// 3.15 s of waits, the rollback ends where that rename leaves the files,
// which the case asserts at the binary's place, the backup's and the
// aside's: the backup kept beside the binary, or the running binary put
// back from the aside, and "(restored)" said only where that put-back
// landed. The put-back's own stage is reached by the backup's rename
// refused once for good, as a mapped image's is.
func TestEachRollbackRenameIsMadeAgainWhileItsFileIsInUse(t *testing.T) {
	inUse := inUseFailure(t)
	every := len(theSchedule) + 1
	for _, tc := range []struct {
		stage string
		// refused is how many attempts at the stage meet the file in use,
		// every one where it is the schedule's length.
		refused int
		// attempts is how many renames each stage makes.
		attempts map[string]int
		// says is what the error says, none where the rollback lands.
		says []string
		// dst, bak and aside are what the binary's place, the backup's and
		// the aside's hold after, "" for nothing.
		dst, bak, aside string
	}{
		{stage: stageOntoMissing, refused: 2, attempts: map[string]int{stageOntoMissing: 3}, dst: "KNOWN-GOOD"},
		{stage: stageOntoMissing, refused: every, attempts: map[string]int{stageOntoMissing: 7},
			says: []string{"rollback rename", "still in use after 7 attempts over 3.15s"}, bak: "KNOWN-GOOD"},
		{stage: stageAside, refused: 2, attempts: map[string]int{stageAside: 3, stageBackup: 1}, dst: "KNOWN-GOOD"},
		{stage: stageAside, refused: every, attempts: map[string]int{stageAside: 7},
			says: []string{"rollback move", "aside", "still in use after 7 attempts over 3.15s"}, dst: "RUNNING", bak: "KNOWN-GOOD"},
		{stage: stageBackup, refused: 2, attempts: map[string]int{stageAside: 1, stageBackup: 3}, dst: "KNOWN-GOOD"},
		{stage: stageBackup, refused: every, attempts: map[string]int{stageAside: 1, stageBackup: 7, stagePutBack: 1},
			says: []string{"still in use after 7 attempts over 3.15s", "(restored)"}, dst: "RUNNING", bak: "KNOWN-GOOD"},
		{stage: stagePutBack, refused: 2, attempts: map[string]int{stageAside: 1, stageBackup: 1, stagePutBack: 3},
			says: []string{"(restored)"}, dst: "RUNNING", bak: "KNOWN-GOOD"},
		{stage: stagePutBack, refused: every, attempts: map[string]int{stageAside: 1, stageBackup: 1, stagePutBack: 7},
			says: []string{"restore also failed", "still in use after 7 attempts over 3.15s", "manual recovery needed"},
			bak:  "KNOWN-GOOD", aside: "RUNNING"},
	} {
		name := fmt.Sprintf("%s, refused %d of %d attempts", tc.stage, min(tc.refused, every), every)
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "bridge.exe")
			bak := dst + ".bak"
			missing := tc.stage == stageOntoMissing
			if !missing {
				if err := os.WriteFile(dst, []byte("RUNNING"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
				t.Fatal(err)
			}
			attempts := map[string]int{}
			useRename(t, func(oldpath, newpath string) error {
				stage := rollbackStage(dst, bak, missing, oldpath, newpath)
				attempts[stage]++
				refuse := func(why error) error { return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: why} }
				switch {
				case stage == tc.stage && attempts[stage] <= tc.refused:
					return refuse(inUse)
				case tc.stage == stagePutBack && stage == stageBackup:
					// Refused for good, as a mapped image's rename is.
					return refuse(syscall.EACCES)
				}
				if _, err := os.Stat(newpath); err == nil {
					return refuse(syscall.EACCES)
				}
				return os.Rename(oldpath, newpath)
			})
			waits := recordWaits(t)

			err := replaceRunningBinary(dst, bak)
			if len(tc.says) == 0 && err != nil {
				t.Fatalf("the rollback = %v, want the backup put back", err)
			}
			if len(tc.says) > 0 {
				if err == nil {
					t.Fatalf("the rollback landed, want it to say %q", tc.says)
				}
				for _, s := range tc.says {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("the rollback = %v, want it to say %q", err, s)
					}
				}
				if restored, want := strings.Contains(err.Error(), "(restored)"), slices.Contains(tc.says, "(restored)"); restored != want {
					t.Errorf("the rollback = %v: it says (restored) %v, want %v: only where the put-back landed", err, restored, want)
				}
				if tc.refused == every && tc.stage != stagePutBack && !errors.Is(err, inUse) {
					t.Errorf("the rollback = %v, want it to carry the refusal of the file in use", err)
				}
			}
			if !maps.Equal(attempts, tc.attempts) {
				t.Errorf("the renames each stage made = %v, want %v: the refused one made again, every other once", attempts, tc.attempts)
			}
			if want := waitsBefore(tc.attempts[tc.stage]); !slices.Equal(*waits, want) {
				t.Errorf("the rollback waited %v, want %v", *waits, want)
			}
			for _, place := range []struct{ name, path, want string }{{"binary", dst, tc.dst}, {"backup", bak, tc.bak}} {
				got, err := os.ReadFile(place.path)
				switch {
				case place.want == "" && !errors.Is(err, os.ErrNotExist):
					t.Errorf("the %s's place holds %q (%v), want nothing there", place.name, got, err)
				case place.want != "" && (err != nil || string(got) != place.want):
					t.Errorf("the %s's place holds %q (%v), want %q", place.name, got, err, place.want)
				}
			}
			asides := asidesOf(t, dst)
			if (tc.aside == "" && len(asides) != 0) || (tc.aside != "" && !slices.Equal(asides, []string{tc.aside})) {
				t.Errorf("the asides beside the binary hold %q, want %q", asides, tc.aside)
			}
		})
	}
}

// TestTheRollbacksRenamesFitInsideTheMarkersLockWait: one of the rollback's
// renames waits 3.15 s at most between its seven attempts, and a rollback
// makes three at most (the vacate, the backup's, the put-back). An
// operator's rollback holds the install marker's lock through the SCM's
// stop, those renames and the marker's two writes, each a rename that
// waits RenameWithRetry's budget at most, and every other taker of the lock
// waits defaultStateLockWait for it.
func TestTheRollbacksRenamesFitInsideTheMarkersLockWait(t *testing.T) {
	if want := append([]time.Duration{0}, theSchedule...); !slices.Equal(rollbackRenameBackoff, want) || rollbackRenameBudget() != 3150*time.Millisecond {
		t.Errorf("one rename's schedule = %v, %s of waits, want %v, 3.15s over 7 attempts, as its doc says",
			rollbackRenameBackoff, rollbackRenameBudget(), want)
	}
	held := scmStopWait + 3*rollbackRenameBudget() + 2*atomicwrite.RenameRetryBudget()
	if held >= defaultStateLockWait {
		t.Errorf("a rollback holds the install marker's lock up to %s, past the %s another taker waits for it", held, defaultStateLockWait)
	}
}
