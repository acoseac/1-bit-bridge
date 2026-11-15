package updater

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Backlog B898: the updater's renames of the binary, and the probes before
// them, wait out another process's hold only while the binary is in place and
// nothing is armed. These tests run Windows' moves on every host through the
// seams (renameFunc, probeMovable, renameInUse, renameWait): a hold is errHeld,
// which the classification takes for one, and every wait is recorded, never
// slept.

// errHeld stands in, on every host, for what Windows answers a rename of a file
// another process holds without sharing its deletion.
var errHeld = errors.New("The process cannot access the file because it is being used by another process.")

// errDenied is a refusal no wait ends, which the classification never takes
// for a hold.
var errDenied = errors.New("Access is denied.")

// runningBinary is a binary at a path of its own, for an updater whose restart
// asks the recovery latch, as serve's does.
func runningBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bridge")
	if err := os.WriteFile(p, []byte("the running binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// refusals is how many times a call or a file is refused (-1: every time), and
// with what (errHeld where err is nil).
type refusals struct {
	n   int
	err error
}

// heldRig is a binary (N), its backup (P) and a candidate (C) in a directory
// of its own, the moves' seams standing in for Windows' as script says: a key
// is a call ("?bak" probes the backup, "dst>aside" renames the binary aside)
// or a file ("bak": any call that moves or probes it).
type heldRig struct {
	t                   *testing.T
	dir, dst, bak, cand string
	staged              string
	script              map[string]*refusals
	calls               []string
	waits               []time.Duration
	waitedWithoutDst    time.Duration
	armedAt             int
	// onWait, where set, runs at each wait, once it is recorded: a test's
	// hold on an operation there.
	onWait func()
}

// newHeldRig lays the files out as lay says: "" a binary and its backup, the
// candidate beside them as the settings check stages it; "no binary"; "no
// backup"; "elsewhere", the candidate in another directory, which the swap
// stages itself.
func newHeldRig(t *testing.T, lay string, script map[string]*refusals) *heldRig {
	t.Helper()
	dir := t.TempDir()
	r := &heldRig{t: t, dir: dir, dst: filepath.Join(dir, "bridge"), bak: filepath.Join(dir, "bridge.bak"),
		cand: filepath.Join(dir, checkStagePrefix+"1"), script: script, armedAt: -1}
	if lay == "elsewhere" {
		r.cand = filepath.Join(t.TempDir(), "bridge-new")
	}
	write := map[string]string{r.dst: "N", r.bak: "P", r.cand: "C"}
	if lay == "no binary" {
		delete(write, r.dst)
	}
	if lay == "no backup" {
		delete(write, r.bak)
	}
	for p, body := range write {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.seams()
	return r
}

// heldRigAt is a rig over a binary at dst laid out already, its backup
// beside it.
func heldRigAt(t *testing.T, dst string, script map[string]*refusals) *heldRig {
	t.Helper()
	r := &heldRig{t: t, dir: filepath.Dir(dst), dst: dst, bak: dst + ".bak", script: script, armedAt: -1}
	r.seams()
	return r
}

// seams stands the rig in for the moves' seams until the test ends.
func (r *heldRig) seams() {
	t := r.t
	prevRename, prevProbe, prevInUse, prevWait := renameFunc, probeMovable, renameInUse, renameWait
	t.Cleanup(func() { renameFunc, probeMovable, renameInUse, renameWait = prevRename, prevProbe, prevInUse, prevWait })
	renameFunc = func(from, to string) error {
		return r.call(r.name(from)+">"+r.name(to), r.name(from), func() error { return os.Rename(from, to) })
	}
	probeMovable = func(p string) error { return r.call("?"+r.name(p), r.name(p), func() error { return nil }) }
	renameInUse = func(err error) bool { return errors.Is(err, errHeld) }
	renameWait = func(d time.Duration) {
		r.waits = append(r.waits, d)
		if _, err := os.Lstat(r.dst); err != nil {
			r.waitedWithoutDst += d
		}
		if r.onWait != nil {
			r.onWait()
		}
	}
}

// name is a path's part: dst, bak, aside, staged (a candidate beside dst) or
// src (one elsewhere).
func (r *heldRig) name(p string) string {
	base := filepath.Base(p)
	switch {
	case p == r.dst:
		return "dst"
	case p == r.bak:
		return "bak"
	case strings.HasPrefix(base, "bridge.rollback-"):
		return "aside"
	case strings.HasPrefix(base, ".bridge-"):
		r.staged = p
		return "staged"
	}
	return "src"
}

// call records a call, and refuses it where the script says, by its key first
// and then by its file's.
func (r *heldRig) call(key, file string, do func() error) error {
	r.calls = append(r.calls, key)
	for _, k := range []string{key, file} {
		if s := r.script[k]; s != nil && s.n != 0 {
			if s.n > 0 {
				s.n--
			}
			if s.err != nil {
				return s.err
			}
			return errHeld
		}
	}
	return do()
}

func (r *heldRig) rollback() error { return replaceRunningBinary(r.dst, r.bak) }

func (r *heldRig) swap() error {
	return swapByVacate(r.dst, r.cand, ".bak", func() error {
		r.armedAt = len(r.waits)
		r.calls = append(r.calls, "arm")
		return nil
	})
}

// files says what is at the binary's place, the backup's, an aside and the
// candidate's: its bytes, or "-".
func (r *heldRig) files() string {
	read := func(p string) string {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
		return "-"
	}
	aside := "-"
	if m, _ := filepath.Glob(filepath.Join(r.dir, "bridge.rollback-*")); len(m) == 1 {
		aside = read(m[0])
	}
	return fmt.Sprintf("dst=%s bak=%s aside=%s cand=%s", read(r.dst), read(r.bak), aside, read(r.cand))
}

// expand puts the rig's paths and errors in a row's message.
func (r *heldRig) expand(s string) string {
	aside := ""
	if m, _ := filepath.Glob(filepath.Join(r.dir, "bridge.rollback-*")); len(m) == 1 {
		aside = m[0]
	}
	return strings.NewReplacer("{dst}", r.dst, "{bak}", r.bak, "{aside}", aside, "{staged}", r.staged, "{cand}", r.cand,
		"{held}", errHeld.Error(), "{denied}", errDenied.Error()).Replace(s)
}

// heldRow is one step of the rollback or the swap refused: what the rig is
// asked, and what the operation must call, wait, leave and say.
type heldRow struct {
	name, op, lay string
	script        map[string]*refusals
	calls, waits  string // calls by key; waits in ms, both space-separated
	ok            bool
	outcome       Outcome
	inUse         bool
	msg, recover  string
	files         string
}

func held(n int) *refusals { return &refusals{n: n} }

// x repeats key n times, for a row's calls.
func x(key string, n int) string { return strings.TrimSpace(strings.Repeat(key+" ", n)) }

const (
	fwdWaits = "50 100 200 400 800 1600 1600"
	recWaits = "50 100 200 400 800 1600"
	inUse8   = " (in use by another process: 8 attempts over 4.75s)"
	inUse7   = " (in use by another process: 7 attempts over 3.15s)"
	fwd3     = "50 100 200"
)

var heldRows = []heldRow{
	{name: "the binary's probe held once", op: "rollback", script: map[string]*refusals{"?dst": held(1)},
		calls: "?dst ?dst ?bak dst>aside bak>dst", waits: "50", ok: true, files: "dst=P bak=- aside=- cand=C"},
	{name: "the binary's probe held three times", op: "rollback", script: map[string]*refusals{"?dst": held(3)},
		calls: x("?dst", 4) + " ?bak dst>aside bak>dst", waits: fwd3, ok: true, files: "dst=P bak=- aside=- cand=C"},
	{name: "the binary's probe held throughout", op: "rollback", script: map[string]*refusals{"?dst": held(-1)},
		calls: x("?dst", 8), waits: fwdWaits, outcome: NothingMoved, inUse: true,
		msg: "rollback move {dst} aside: {held}" + inUse8 + "; nothing was moved", files: "dst=N bak=P aside=- cand=C"},
	{name: "the backup's probe held three times", op: "rollback", script: map[string]*refusals{"?bak": held(3)},
		calls: "?dst " + x("?bak", 4) + " dst>aside bak>dst", waits: fwd3, ok: true, files: "dst=P bak=- aside=- cand=C"},
	{name: "the backup's probe held throughout", op: "rollback", script: map[string]*refusals{"?bak": held(-1)},
		calls: "?dst " + x("?bak", 8), waits: fwdWaits, outcome: NothingMoved, inUse: true,
		msg: "rollback rename {bak} -> {dst}: {held}" + inUse8 + "; nothing was moved", files: "dst=N bak=P aside=- cand=C"},
	{name: "the vacate held three times", op: "rollback", script: map[string]*refusals{"dst>aside": held(3)},
		calls: "?dst ?bak " + x("dst>aside", 4) + " bak>dst", waits: fwd3, ok: true, files: "dst=P bak=- aside=- cand=C"},
	{name: "the vacate held throughout", op: "rollback", script: map[string]*refusals{"dst>aside": held(-1)},
		calls: "?dst ?bak " + x("dst>aside", 8), waits: fwdWaits, outcome: NothingMoved, inUse: true,
		msg: "rollback move {dst} aside: {held}" + inUse8 + "; nothing was moved", files: "dst=N bak=P aside=- cand=C"},
	{name: "the place held", op: "rollback", script: map[string]*refusals{"bak>dst": held(1)},
		calls: "?dst ?bak dst>aside bak>dst aside>dst", outcome: PutBack, inUse: true,
		msg: "rollback rename {bak} -> {dst}: {held} (restored)", files: "dst=N bak=P aside=- cand=C"},
	{name: "the place held and the put-back three times", op: "rollback",
		script: map[string]*refusals{"bak>dst": held(1), "aside>dst": held(3)},
		calls:  "?dst ?bak dst>aside bak>dst " + x("aside>dst", 4), waits: fwd3, outcome: PutBack, inUse: true,
		msg: "rollback rename {bak} -> {dst}: {held} (restored)", files: "dst=N bak=P aside=- cand=C"},
	{name: "the place held and the put-back throughout", op: "rollback",
		script: map[string]*refusals{"bak>dst": held(1), "aside>dst": held(-1)},
		calls:  "?dst ?bak dst>aside bak>dst " + x("aside>dst", 7), waits: recWaits, outcome: BinaryMissing, inUse: true,
		msg: "rollback rename {bak} -> {dst} failed ({held}); restore also failed ({held}" + inUse7 + "); manual recovery needed: " +
			"rename {aside} to {dst}, then start the bridge", recover: "rename {aside} to {dst}", files: "dst=- bak=P aside=N cand=C"},
	{name: "a refusal no wait ends", op: "rollback", script: map[string]*refusals{"?bak": {n: -1, err: errDenied}},
		calls: "?dst ?bak", outcome: NothingMoved, msg: "rollback rename {bak} -> {dst}: {denied}; nothing was moved",
		files: "dst=N bak=P aside=- cand=C"},
	{name: "no binary, the backup's probe held throughout", op: "rollback", lay: "no binary", script: map[string]*refusals{"?bak": held(-1)},
		calls: x("?bak", 8), waits: fwdWaits, outcome: BinaryMissing, inUse: true,
		msg:     "rollback rename {bak} -> {dst}: {held}" + inUse8 + "; the binary is not at its path: rename {bak} to {dst}",
		recover: "rename {bak} to {dst}", files: "dst=- bak=P aside=- cand=C"},
	{name: "no binary, the backup's rename held three times", op: "rollback", lay: "no binary", script: map[string]*refusals{"bak>dst": held(3)},
		calls: "?bak " + x("bak>dst", 4), waits: fwd3, ok: true, files: "dst=P bak=- aside=- cand=C"},
	{name: "the candidate's probe held three times", op: "swap", script: map[string]*refusals{"?staged": held(3)},
		calls: x("?staged", 4) + " ?dst ?bak arm dst>bak staged>dst", waits: fwd3, ok: true, files: "dst=C bak=N aside=- cand=-"},
	{name: "the candidate's probe held throughout", op: "swap", script: map[string]*refusals{"?staged": held(-1)},
		calls: x("?staged", 8), waits: fwdWaits, outcome: NothingMoved, inUse: true,
		msg: "install {staged} -> {dst}: {held}" + inUse8 + "; nothing was moved", files: "dst=N bak=P aside=- cand=-"},
	{name: "the binary's probe held throughout", op: "swap", script: map[string]*refusals{"?dst": held(-1)},
		calls: "?staged " + x("?dst", 8), waits: fwdWaits, outcome: NothingMoved, inUse: true,
		msg: "rename {dst} -> {bak}: {held}" + inUse8 + "; nothing was moved", files: "dst=N bak=P aside=- cand=-"},
	{name: "an old backup's probe held throughout", op: "swap", script: map[string]*refusals{"?bak": held(-1)},
		calls: "?staged ?dst " + x("?bak", 8), waits: fwdWaits, outcome: NothingMoved, inUse: true,
		msg: "rename {dst} -> {bak}: {held}" + inUse8 + "; nothing was moved", files: "dst=N bak=P aside=- cand=-"},
	{name: "no backup to probe", op: "swap", lay: "no backup", script: map[string]*refusals{},
		calls: "?staged ?dst ?bak arm dst>bak staged>dst", ok: true, files: "dst=C bak=N aside=- cand=-"},
	{name: "the vacate held once armed", op: "swap", script: map[string]*refusals{"dst>bak": held(-1)},
		calls: "?staged ?dst ?bak arm dst>bak", outcome: NothingMoved, inUse: true,
		msg: "rename {dst} -> {bak}: {held}; nothing was moved", files: "dst=N bak=P aside=- cand=-"},
	{name: "the place held", op: "swap", script: map[string]*refusals{"staged>dst": held(1)},
		calls: "?staged ?dst ?bak arm dst>bak staged>dst bak>dst", outcome: PutBack, inUse: true,
		msg: "install {staged} -> {dst}: {held} (rolled back)", files: "dst=N bak=- aside=- cand=-"},
	{name: "the place held and the put-back throughout", op: "swap",
		script: map[string]*refusals{"staged>dst": held(1), "bak>dst": held(-1)},
		calls:  "?staged ?dst ?bak arm dst>bak staged>dst " + x("bak>dst", 7), waits: recWaits, outcome: BinaryMissing, inUse: true,
		msg: "install {staged} -> {dst} failed ({held}); rollback also failed ({held}" + inUse7 + "); manual recovery needed: " +
			"rename {bak} to {dst}, then start the bridge", recover: "rename {bak} to {dst}", files: "dst=- bak=N aside=- cand=-"},
	{name: "its own staging held three times", op: "swap", lay: "elsewhere", script: map[string]*refusals{"src>staged": held(3)},
		calls: x("src>staged", 4) + " ?staged ?dst ?bak arm dst>bak staged>dst", waits: fwd3, ok: true, files: "dst=C bak=N aside=- cand=-"},
	{name: "its own staging held throughout", op: "swap", lay: "elsewhere", script: map[string]*refusals{"src>staged": held(-1)},
		calls: x("src>staged", 8), waits: fwdWaits, outcome: NothingMoved, inUse: true,
		msg: "stage {cand} beside {dst}: {held}" + inUse8, files: "dst=N bak=P aside=- cand=C"},
	{name: "the staging and the probes share one deadline", op: "swap", lay: "elsewhere",
		script: map[string]*refusals{"src>staged": held(7), "?staged": held(1)},
		calls:  x("src>staged", 8) + " ?staged", waits: fwdWaits, outcome: NothingMoved, inUse: true,
		msg:   "install {staged} -> {dst}: {held} (in use by another process: 1 attempt over 0s); nothing was moved",
		files: "dst=N bak=P aside=- cand=-"},
}

// run runs row's operation on a rig of its own.
func (row heldRow) run(t *testing.T) (*heldRig, error) {
	t.Helper()
	script := map[string]*refusals{}
	for k, v := range row.script {
		c := *v
		script[k] = &c
	}
	r := newHeldRig(t, row.lay, script)
	if row.op == "swap" {
		return r, r.swap()
	}
	return r, r.rollback()
}

func msString(ds []time.Duration) string {
	var s []string
	for _, d := range ds {
		s = append(s, fmt.Sprint(d.Milliseconds()))
	}
	return strings.Join(s, " ")
}

// TestEachHeldRenameIsWaitedForWhereTheBinaryIsInPlace is the stage table:
// each step of the rollback and the swap refused once, three times and
// throughout makes exactly its calls in order, waits exactly its schedule's
// waits, never past its bound, and leaves exactly the files and the outcome
// it says; the place and the swap's vacate are each made once.
func TestEachHeldRenameIsWaitedForWhereTheBinaryIsInPlace(t *testing.T) {
	for _, row := range heldRows {
		t.Run(row.op+": "+row.name, func(t *testing.T) {
			r, err := row.run(t)
			if got := strings.Join(r.calls, " "); got != row.calls {
				t.Errorf("calls = %q, want %q", got, row.calls)
			}
			if got := msString(r.waits); got != row.waits {
				t.Errorf("waits = %q ms, want %q", got, row.waits)
			}
			if got := r.files(); got != row.files {
				t.Errorf("files = %s, want %s", got, row.files)
			}
			if row.ok {
				if err != nil {
					t.Fatalf("%s = %v, want it done", row.op, err)
				}
				return
			}
			var swapErr *SwapError
			if !errors.As(err, &swapErr) {
				t.Fatalf("%s = %v (%T), want a *SwapError", row.op, err, err)
			}
			if swapErr.Outcome != row.outcome || swapErr.InUse != row.inUse {
				t.Errorf("outcome %v, in use %v; want %v, %v", swapErr.Outcome, swapErr.InUse, row.outcome, row.inUse)
			}
			if want := r.expand(row.recover); swapErr.Recover != want {
				t.Errorf("Recover = %q, want %q", swapErr.Recover, want)
			}
		})
	}
}

// TestEachSwapOutcomeSaysWhatItLeft: every failure of the stage table says
// exactly what its step of the swap or the rollback left, in the words of its
// row, and matches the sentinels by its outcome, never by its errno: in use
// only where a hold ended it and the binary is in place, missing only where
// it is not; a marker that could not be put back after an in-use failure
// matches both, and after a missing binary that and the missing binary.
func TestEachSwapOutcomeSaysWhatItLeft(t *testing.T) {
	for _, row := range heldRows {
		if row.ok {
			continue
		}
		t.Run(row.op+": "+row.name, func(t *testing.T) {
			r, err := row.run(t)
			if want := r.expand(row.msg); err == nil || err.Error() != want {
				t.Fatalf("%s = %v\nwant %s", row.op, err, want)
			}
			if got, want := errors.Is(err, ErrBinaryInUse), row.inUse && row.outcome != BinaryMissing; got != want {
				t.Errorf("errors.Is(ErrBinaryInUse) = %v, want %v", got, want)
			}
			if got, want := errors.Is(err, ErrBinaryMissing), row.outcome == BinaryMissing; got != want {
				t.Errorf("errors.Is(ErrBinaryMissing) = %v, want %v", got, want)
			}
			if !errors.Is(err, errHeld) && !errors.Is(err, errDenied) {
				t.Errorf("%v does not unwrap to the step's own error", err)
			}
			marker := &MarkerNotRestoredError{Swap: fmt.Errorf("swap: %w", err), Marker: errDenied}
			if !errors.Is(marker, ErrMarkerNotRestored) || errors.Is(marker, ErrBinaryInUse) != errors.Is(err, ErrBinaryInUse) ||
				errors.Is(marker, ErrBinaryMissing) != errors.Is(err, ErrBinaryMissing) {
				t.Errorf("a marker not put back over %v matches its outcome's sentinels and ErrMarkerNotRestored: %v", err, marker)
			}
		})
	}
}

// TestNothingWaitsWithNoBinaryButThePutBack: on every row of the stage table
// that found the binary in place, the waits made while it was not there sum
// to nothing but where a place was refused, and are then the put-back's alone,
// within its schedule: nothing waits between the vacate and the place.
func TestNothingWaitsWithNoBinaryButThePutBack(t *testing.T) {
	for _, row := range heldRows {
		if row.lay == "no binary" {
			continue
		}
		t.Run(row.op+": "+row.name, func(t *testing.T) {
			r, _ := row.run(t)
			putBack := map[string]string{"rollback": "aside>dst", "swap": "bak>dst"}[row.op]
			n := 0
			for _, c := range r.calls {
				if c == putBack {
					n++
				}
			}
			want := waitsTotal(recoveryWaits[:n])
			if r.waitedWithoutDst != want {
				t.Errorf("waited %s with no binary at its path, want %s: the put-back's waits alone", r.waitedWithoutDst, want)
			}
		})
	}
}

// TestNoWaitFollowsTheSwapsArming: every wait of the swap's forward deadline
// comes before markSwapStarted; once it is armed, the vacate and the place
// have one attempt each, and the only waits are the put-back's, after a
// refused place.
func TestNoWaitFollowsTheSwapsArming(t *testing.T) {
	for _, row := range heldRows {
		if row.op != "swap" {
			continue
		}
		t.Run(row.name, func(t *testing.T) {
			r, _ := row.run(t)
			if r.armedAt < 0 {
				return
			}
			after := r.waits[r.armedAt:]
			placeRefused := row.outcome == PutBack || row.outcome == BinaryMissing
			if !placeRefused && len(after) != 0 {
				t.Errorf("waited %v after arming, with the place not refused", after)
			}
			if got := waitsTotal(after); got > RecoveryWait() {
				t.Errorf("waited %s after arming, past the put-back's %s", got, RecoveryWait())
			}
			arm := strings.Index(strings.Join(r.calls, " "), "arm")
			if tail := strings.Join(r.calls, " ")[arm:]; strings.Count(tail, "dst>bak") > 1 || strings.Count(tail, "staged>dst") > 1 {
				t.Errorf("calls after arming %q: the vacate and the place have one attempt each", tail)
			}
		})
	}
}

// TestThePutBackKeepsItsWaitWhateverTheStepsBeforeItUsed: the forward
// deadline spent whole on the binary's probe, the put-back still makes every
// attempt of its recovery schedule.
func TestThePutBackKeepsItsWaitWhateverTheStepsBeforeItUsed(t *testing.T) {
	r := newHeldRig(t, "", map[string]*refusals{"?dst": held(7), "bak>dst": held(1), "aside>dst": held(-1)})
	err := r.rollback()
	if !errors.Is(err, ErrBinaryMissing) {
		t.Fatalf("rollback = %v, want the binary missing", err)
	}
	if got := strings.Count(strings.Join(r.calls, " "), "aside>dst"); got != len(recoveryWaits) {
		t.Errorf("the put-back made %d attempts after the forward deadline was spent, want %d", got, len(recoveryWaits))
	}
	if got := msString(r.waits); got != fwdWaits+" "+recWaits {
		t.Errorf("waits = %q, want the forward schedule's then the recovery's whole", got)
	}
}

// TestTheBackupIsWaitedForBeforeTheBinaryIsVacated: a hold on the backup (the
// file, whatever is asked of it) that ends after three attempts is heard at
// its probe, with the binary in place, and never at the place.
func TestTheBackupIsWaitedForBeforeTheBinaryIsVacated(t *testing.T) {
	r := newHeldRig(t, "", map[string]*refusals{"bak": held(3)})
	var heard []RenameRefusal
	t.Cleanup(SetRenameObserverForTest(func(rr RenameRefusal) {
		if _, err := os.Lstat(r.dst); err != nil {
			t.Errorf("a refusal heard with no binary in place: %+v", rr)
		}
		heard = append(heard, rr)
	}))
	if err := r.rollback(); err != nil {
		t.Fatalf("rollback = %v", err)
	}
	if len(heard) != 3 {
		t.Fatalf("heard %d refusals, want 3: %+v", len(heard), heard)
	}
	for i, h := range heard {
		if h.Stage != StageProbe || h.From != r.bak || h.Attempt != i+1 || h.Wait != forwardWaits[i+1] || !errors.Is(h.Err, errHeld) {
			t.Errorf("refusal %d = %+v, want the backup's probe, attempt %d, then %s", i, h, i+1, forwardWaits[i+1])
		}
	}
}

// TestTheRollbackStopsNoService: Windows' RollbackBinary calls no SCM
// function (backlog B898), read from its source on every host; the swap
// still stops the service.
func TestTheRollbackStopsNoService(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "swap_windows.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string][]string{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "RollbackBinary" && fn.Name.Name != "swapBinary") {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok {
					calls[fn.Name.Name] = append(calls[fn.Name.Name], id.Name)
				}
			}
			return true
		})
	}
	stops := func(name string) bool {
		return strings.Contains(strings.Join(calls[name], " "), "stopServiceIfRunning")
	}
	if stops("RollbackBinary") || !stops("swapBinary") {
		t.Errorf("calls: %v; want the swap to stop the service and the rollback not to", calls)
	}
}
