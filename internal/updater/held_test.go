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
// seams (renameFunc, probeMovable, renameInUse, renameWait, renameNow): a hold
// is errHeld, which the classification takes for one, every wait is recorded,
// never slept, and the clock the deadlines read is the rig's, which only a
// wait, or a step a test says takes time, moves on.

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
	// clock is the moves' clock (renameNow): each wait moves it on by its
	// length and by sleep more (a host whose sleeps overrun), the nth wait,
	// from 1, by over[n] more still (one wait the host overran), and each
	// call by step and by took's time for its key (a step that takes that
	// long, landed or refused); nothing else moves it.
	clock       time.Time
	took        map[string]time.Duration
	over        map[int]time.Duration
	step, sleep time.Duration
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
	prevRename, prevProbe, prevInUse, prevWait, prevNow := renameFunc, probeMovable, renameInUse, renameWait, renameNow
	t.Cleanup(func() {
		renameFunc, probeMovable, renameInUse, renameWait, renameNow = prevRename, prevProbe, prevInUse, prevWait, prevNow
	})
	renameFunc = func(from, to string) error {
		return r.call(r.name(from)+">"+r.name(to), r.name(from), func() error { return os.Rename(from, to) })
	}
	probeMovable = func(p string) error { return r.call("?"+r.name(p), r.name(p), func() error { return nil }) }
	renameInUse = func(err error) bool { return errors.Is(err, errHeld) }
	renameNow = func() time.Time { return r.clock }
	renameWait = func(d time.Duration) {
		r.waits = append(r.waits, d)
		r.clock = r.clock.Add(d + r.sleep + r.over[len(r.waits)])
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
	r.clock = r.clock.Add(r.step + r.took[key])
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
		"{held}", errHeld.Error(), "{denied}", errDenied.Error(), "{deadline}", ErrDeadlinePassed.Error()).Replace(s)
}

// heldRow is one step of the rollback or the swap refused, or taking time:
// what the rig is asked, and what the operation must call, wait, leave and
// say.
type heldRow struct {
	name, op, lay string
	script        map[string]*refusals
	took          map[string]time.Duration
	over          map[int]time.Duration
	step, sleep   time.Duration
	calls, waits  string // calls by key; waits in ms, both space-separated
	ok            bool
	outcome       Outcome
	inUse         bool
	expired       bool
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
	r.took, r.over, r.step, r.sleep = row.took, row.over, row.step, row.sleep
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
// deadline spent whole on the binary's probe, whose last attempt is made and
// lands at the deadline itself, the steps after it are tried at that instant,
// which has not passed it, and the put-back still makes every attempt of its
// recovery schedule on its own deadline.
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

// secs is n seconds.
func secs(n int) time.Duration { return time.Duration(n) * time.Second }

// deadlineRows are steps of the rollback and the swap that take time, or a
// wait the host overran: once the forward deadline has passed, no step is
// begun and no wait admitted, but for the place after a vacate that landed
// and the put-back after a refused place; a wait admitted is shortened to
// what remains and followed by its attempt however long it took; and a
// put-back that lands late is still a put-back.
var deadlineRows = []heldRow{
	{name: "a slow probe of the binary leaves the backup's untried", op: "rollback", took: map[string]time.Duration{"?dst": secs(5)},
		calls: "?dst", outcome: NothingMoved, expired: true,
		msg: "rollback rename {bak} -> {dst}: {deadline}; nothing was moved", files: "dst=N bak=P aside=- cand=C"},
	{name: "a slow probe of the backup leaves the vacate untried", op: "rollback", took: map[string]time.Duration{"?bak": secs(5)},
		calls: "?dst ?bak", outcome: NothingMoved, expired: true,
		msg: "rollback move {dst} aside: {deadline}; nothing was moved", files: "dst=N bak=P aside=- cand=C"},
	{name: "a vacate that lands late is followed by its place", op: "rollback", took: map[string]time.Duration{"dst>aside": secs(5)},
		calls: "?dst ?bak dst>aside bak>dst", ok: true, files: "dst=P bak=- aside=- cand=C"},
	{name: "a put-back that lands late is a put-back", op: "rollback", script: map[string]*refusals{"bak>dst": held(1)},
		took: map[string]time.Duration{"aside>dst": secs(5)}, calls: "?dst ?bak dst>aside bak>dst aside>dst", outcome: PutBack, inUse: true,
		msg: "rollback rename {bak} -> {dst}: {held} (restored)", files: "dst=N bak=P aside=- cand=C"},
	{name: "no binary, a slow probe of the backup leaves its install untried", op: "rollback", lay: "no binary",
		took: map[string]time.Duration{"?bak": secs(5)}, calls: "?bak", outcome: BinaryMissing, expired: true,
		msg:     "rollback rename {bak} -> {dst}: {deadline}; the binary is not at its path: rename {bak} to {dst}",
		recover: "rename {bak} to {dst}", files: "dst=- bak=P aside=- cand=C"},
	{name: "a held probe whose every attempt takes a second", op: "rollback", script: map[string]*refusals{"?dst": held(-1)},
		took: map[string]time.Duration{"?dst": secs(1)}, calls: x("?dst", 5), waits: "50 100 200 400", outcome: NothingMoved, inUse: true,
		msg:   "rollback move {dst} aside: {held} (in use by another process: 5 attempts over 750ms); nothing was moved",
		files: "dst=N bak=P aside=- cand=C"},
	{name: "a wait the host overran shortens the waits after it to what remains", op: "rollback",
		script: map[string]*refusals{"?dst": held(-1)}, over: map[int]time.Duration{4: secs(3)}, calls: x("?dst", 7),
		waits: "50 100 200 400 800 200", outcome: NothingMoved, inUse: true,
		msg:   "rollback move {dst} aside: {held} (in use by another process: 7 attempts over 1.75s); nothing was moved",
		files: "dst=N bak=P aside=- cand=C"},
	{name: "a wait the host overran past the deadline is still followed by its attempt", op: "rollback",
		script: map[string]*refusals{"?dst": held(-1)}, over: map[int]time.Duration{7: secs(1)}, calls: x("?dst", 8), waits: fwdWaits,
		outcome: NothingMoved, inUse: true, msg: "rollback move {dst} aside: {held}" + inUse8 + "; nothing was moved",
		files: "dst=N bak=P aside=- cand=C"},
	{name: "a slow probe of the candidate leaves the binary's untried", op: "swap", took: map[string]time.Duration{"?staged": secs(5)},
		calls: "?staged", outcome: NothingMoved, expired: true,
		msg: "rename {dst} -> {bak}: {deadline}; nothing was moved", files: "dst=N bak=P aside=- cand=-"},
	{name: "a final probe that lands late arms nothing", op: "swap", took: map[string]time.Duration{"?bak": secs(5)},
		calls: "?staged ?dst ?bak", outcome: NothingMoved, expired: true,
		msg: "rename {dst} -> {bak}: {deadline}; nothing was moved", files: "dst=N bak=P aside=- cand=-"},
	{name: "a staging that takes long leaves the candidate's probe untried", op: "swap", lay: "elsewhere",
		took: map[string]time.Duration{"src>staged": secs(5)}, calls: "src>staged", outcome: NothingMoved, expired: true,
		msg: "install {staged} -> {dst}: {deadline}; nothing was moved", files: "dst=N bak=P aside=- cand=-"},
	{name: "a put-back that lands late is a put-back", op: "swap", script: map[string]*refusals{"staged>dst": held(1)},
		took: map[string]time.Duration{"bak>dst": secs(5)}, calls: "?staged ?dst ?bak arm dst>bak staged>dst bak>dst", outcome: PutBack,
		inUse: true, msg: "install {staged} -> {dst}: {held} (rolled back)", files: "dst=N bak=- aside=- cand=-"},
}

// TestEveryStepIsTriedByItsOperationsDeadline is the deadline table: each
// operation makes exactly its calls and its waits, and leaves and says
// exactly what its row says; a step its deadline had passed before is
// answered ErrDeadlinePassed, never as a hold, its outcome NothingMoved, or
// BinaryMissing where the rollback found no binary.
func TestEveryStepIsTriedByItsOperationsDeadline(t *testing.T) {
	for _, row := range deadlineRows {
		t.Run(row.op+": "+row.name, func(t *testing.T) { requireRow(t, row) })
	}
}

// tick is what each call takes, and each sleep overruns by, on clockRows'
// clock: a host where nothing is instant.
const tick = time.Millisecond

// clockRows are the schedules on a clock every call and every sleep moves
// on by a tick, as the host's does: each schedule keeps every attempt, the
// last wait shortened to what remained, so that the last attempt comes at
// the deadline, past it by the last sleep's overrun alone; a hold that
// clears on a probe's last attempt leaves the step after it untried and arms
// nothing, one that clears on the vacate's is followed by the place and,
// where that is refused, by the put-back, and one that clears on the
// put-back's last attempt is a put-back, the binary in its place.
var clockRows = []heldRow{
	{name: "a probe held throughout keeps its eight attempts", op: "rollback", step: tick, sleep: tick,
		script: map[string]*refusals{"?dst": held(-1)}, calls: x("?dst", 8), waits: "50 100 200 400 800 1600 1587",
		outcome: NothingMoved, inUse: true,
		msg:   "rollback move {dst} aside: {held} (in use by another process: 8 attempts over 4.737s); nothing was moved",
		files: "dst=N bak=P aside=- cand=C"},
	{name: "a put-back held throughout keeps its seven attempts", op: "rollback", step: tick, sleep: tick,
		script: map[string]*refusals{"bak>dst": held(1), "aside>dst": held(-1)},
		calls:  "?dst ?bak dst>aside bak>dst " + x("aside>dst", 7), waits: "50 100 200 400 800 1589", outcome: BinaryMissing, inUse: true,
		msg: "rollback rename {bak} -> {dst} failed ({held}); restore also failed ({held} (in use by another process: 7 attempts " +
			"over 3.139s)); manual recovery needed: rename {aside} to {dst}, then start the bridge",
		recover: "rename {aside} to {dst}", files: "dst=- bak=P aside=N cand=C"},
	{name: "a put-back held throughout keeps its seven attempts", op: "swap", step: tick, sleep: tick,
		script: map[string]*refusals{"staged>dst": held(1), "bak>dst": held(-1)},
		calls:  "?staged ?dst ?bak arm dst>bak staged>dst " + x("bak>dst", 7), waits: "50 100 200 400 800 1589",
		outcome: BinaryMissing, inUse: true,
		msg: "install {staged} -> {dst} failed ({held}); rollback also failed ({held} (in use by another process: 7 attempts " +
			"over 3.139s)); manual recovery needed: rename {bak} to {dst}, then start the bridge",
		recover: "rename {bak} to {dst}", files: "dst=- bak=N aside=- cand=-"},
	{name: "a probe whose hold clears on its last attempt leaves the next step untried", op: "rollback", step: tick, sleep: tick,
		script: map[string]*refusals{"?dst": held(7)}, calls: x("?dst", 8), waits: "50 100 200 400 800 1600 1587",
		outcome: NothingMoved, expired: true, msg: "rollback rename {bak} -> {dst}: {deadline}; nothing was moved",
		files: "dst=N bak=P aside=- cand=C"},
	{name: "a last probe whose hold clears on its last attempt arms nothing", op: "swap", step: tick, sleep: tick,
		script: map[string]*refusals{"?bak": held(7)}, calls: "?staged ?dst " + x("?bak", 8), waits: "50 100 200 400 800 1600 1585",
		outcome: NothingMoved, expired: true, msg: "rename {dst} -> {bak}: {deadline}; nothing was moved",
		files: "dst=N bak=P aside=- cand=-"},
	{name: "a vacate whose hold clears on its last attempt is followed by its place", op: "rollback", step: tick, sleep: tick,
		script: map[string]*refusals{"dst>aside": held(7)}, calls: "?dst ?bak " + x("dst>aside", 8) + " bak>dst",
		waits: "50 100 200 400 800 1600 1585", ok: true, files: "dst=P bak=- aside=- cand=C"},
	{name: "a vacate whose hold clears on its last attempt is put back where its place is refused", op: "rollback", step: tick,
		sleep: tick, script: map[string]*refusals{"dst>aside": held(7), "bak>dst": held(1)},
		calls: "?dst ?bak " + x("dst>aside", 8) + " bak>dst aside>dst", waits: "50 100 200 400 800 1600 1585", outcome: PutBack,
		inUse: true, msg: "rollback rename {bak} -> {dst}: {held} (restored)", files: "dst=N bak=P aside=- cand=C"},
	{name: "a put-back whose hold clears on its last attempt is a put-back", op: "rollback", step: tick, sleep: tick,
		script: map[string]*refusals{"bak>dst": held(1), "aside>dst": held(6)},
		calls:  "?dst ?bak dst>aside bak>dst " + x("aside>dst", 7), waits: "50 100 200 400 800 1589", outcome: PutBack, inUse: true,
		msg: "rollback rename {bak} -> {dst}: {held} (restored)", files: "dst=N bak=P aside=- cand=C"},
}

// TestEveryAttemptIsMadeOnAClockTheStepsMove is the clock table: each
// operation makes exactly its calls and its waits, the last wait the
// remainder of its deadline, and leaves and says exactly what its row says.
func TestEveryAttemptIsMadeOnAClockTheStepsMove(t *testing.T) {
	for _, row := range clockRows {
		t.Run(row.op+": "+row.name, func(t *testing.T) { requireRow(t, row) })
	}
}

// requireRow runs row and requires of it its calls, its waits, its files,
// and its error's words, outcome, sentinels and recovery.
func requireRow(t *testing.T, row heldRow) {
	t.Helper()
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
	if want := r.expand(row.msg); err.Error() != want {
		t.Errorf("%s = %v\nwant %s", row.op, err, want)
	}
	if swapErr.Outcome != row.outcome || swapErr.InUse != row.inUse {
		t.Errorf("outcome %v, in use %v; want %v, %v", swapErr.Outcome, swapErr.InUse, row.outcome, row.inUse)
	}
	if got := errors.Is(err, ErrDeadlinePassed); got != row.expired {
		t.Errorf("errors.Is(ErrDeadlinePassed) = %v, want %v", got, row.expired)
	}
	if got, want := errors.Is(err, ErrBinaryInUse), row.inUse && row.outcome != BinaryMissing; got != want {
		t.Errorf("errors.Is(ErrBinaryInUse) = %v, want %v", got, want)
	}
	if got, want := errors.Is(err, ErrBinaryMissing), row.outcome == BinaryMissing; got != want {
		t.Errorf("errors.Is(ErrBinaryMissing) = %v, want %v", got, want)
	}
	if want := r.expand(row.recover); swapErr.Recover != want {
		t.Errorf("Recover = %q, want %q", swapErr.Recover, want)
	}
}

// TestThePutBacksFirstAttemptIsMadeHoweverLateItComes: on a clock that moves
// on 4 s at every read after the first, a host that stalls between the
// put-back's deadline and its first attempt, the put-back is still tried:
// it lands; held, that first attempt is its only one, no wait admitted
// past its deadline.
func TestThePutBacksFirstAttemptIsMadeHoweverLateItComes(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("held %v", held), func(t *testing.T) {
			dir := t.TempDir()
			aside, dst := filepath.Join(dir, "bridge.rollback-1"), filepath.Join(dir, "bridge")
			if err := os.WriteFile(aside, []byte("N"), 0o755); err != nil {
				t.Fatal(err)
			}
			var clock time.Time
			var reads int
			var waits []time.Duration
			m := moves{
				rename: func(from, to string) error {
					if held {
						return errHeld
					}
					return os.Rename(from, to)
				},
				inUse: func(err error) bool { return errors.Is(err, errHeld) },
				wait:  func(d time.Duration) { waits = append(waits, d) },
				now: func() time.Time {
					if reads++; reads > 1 {
						clock = clock.Add(4 * time.Second)
					}
					return clock
				},
			}
			r := m.putBack(aside, dst)
			if r.attempts != 1 || len(waits) != 0 {
				t.Errorf("the put-back made %d attempts and waited %v, want one attempt and no wait", r.attempts, waits)
			}
			if !held {
				if b, err := os.ReadFile(dst); r.err != nil || err != nil || string(b) != "N" {
					t.Errorf("the put-back = %v, the binary's place %q (%v); want the aside back in its place", r.err, b, err)
				}
				return
			}
			if !r.inUse || !errors.Is(r.err, errHeld) {
				t.Errorf("the put-back = %v (in use %v), want its hold", r.err, r.inUse)
			}
		})
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
