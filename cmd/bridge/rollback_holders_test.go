package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// The diagnostic of what holds a file the Windows rollback could not rename
// (backlog B872 part 2). On windows-latest the crash rollback's rename of the
// backup over the binary was refused twice in 465 jobs ("being used by
// another process"), on two PR heads and never on main, and a read-only trace
// found no handle of the test process or of production on either file. A
// test that runs the real rollback puts this diagnostic on it
// (watchRollbackHolders), through the observer the rollback's put-back tells
// of each rename that fails (updater.SetRollbackObserverForTest): at the first
// refusal of a stage that says a file is in use, it asks who uses each of the
// rename's two files, in a Restart Manager session of its own
// (candidateUsers, Windows'), and the test ends saying what it found, on a
// green run too. The Restart Manager lists the processes that use a file, not
// the one whose handle refused the rename, so each is a candidate user, never
// the holder. An empty list is no failure.

// candidateUser is a process the Restart Manager listed as using a file.
type candidateUser struct {
	pid int
	// parent is the pid the process was started by, -1 where the process
	// list did not have it (it ended, or the list could not be read).
	parent int
	// image is the process's image, or why it could not be read.
	image string
	// app and service are the Restart Manager's names for it.
	app, service string
	// started is when the process started, which tells a pid used again
	// apart: the zero time where the Restart Manager gave none a process can
	// have started at (filetimeStart), which String leaves out.
	started time.Time
}

// filetimeUnixEpoch is the Unix epoch as a FILETIME: 100 ns intervals since
// 1601-01-01 UTC.
const filetimeUnixEpoch = 116444736000000000

// filetimeStart is the time a FILETIME of high and low names, and false for
// one at or before the Unix epoch, which no process this bridge sees started
// at, zero among them (a FILETIME left as it was made, which names
// 1601-01-01), and for one past what an int64 holds of nanoseconds since that
// epoch (2262). Unchecked, the conversion reads both wrong: the epoch's
// subtraction wraps zero to a time in 2185, and the multiplication by 100
// wraps one past 2262 to a time in 1677.
func filetimeStart(high, low uint32) (time.Time, bool) {
	v := uint64(high)<<32 | uint64(low)
	if v <= filetimeUnixEpoch || v-filetimeUnixEpoch > math.MaxInt64/100 {
		return time.Time{}, false
	}
	return time.Unix(0, int64(v-filetimeUnixEpoch)*100), true
}

func (u candidateUser) String() string {
	parent := "parent pid unknown"
	if u.parent >= 0 {
		parent = fmt.Sprintf("parent pid %d", u.parent)
	}
	s := fmt.Sprintf("pid %d, image %s, %s", u.pid, u.image, parent)
	if u.app != "" {
		s += fmt.Sprintf(", app %q", u.app)
	}
	if u.service != "" {
		s += fmt.Sprintf(", service %q", u.service)
	}
	if !u.started.IsZero() {
		s += ", started " + u.started.UTC().Format(time.RFC3339Nano)
	}
	return s
}

// resourceReport is what the diagnostic found of one of a refused rename's
// files: absent (nothing there to ask about), the users the Restart Manager
// listed, or why it could not be asked.
type resourceReport struct {
	path   string
	absent bool
	users  []candidateUser
	err    error
}

// refusalReport is one refused rename the observer heard: the rename, the
// Windows error code it carries (0 for none), whether that code says a file
// is in use, and what was asked at it (only at a stage's first such refusal).
type refusalReport struct {
	rename    updater.RollbackRename
	code      uint32
	inUse     bool
	resources []resourceReport
}

// rollbackHolders is the diagnostic on the rollback of the binary at dst.
type rollbackHolders struct {
	dst string
	// classify is the Windows error code a refusal carries and whether it
	// says a file is in use (windowsErrorCode), and ask the processes the
	// Restart Manager says use a file (candidateUsers): the platform's, or
	// a unit test's stand-ins.
	classify func(error) (code uint32, inUse bool)
	ask      func(path string) ([]candidateUser, error)

	mu       sync.Mutex
	refusals []refusalReport
	asked    map[updater.RollbackStage]bool
	// ignored counts the refusals of another binary's rollback the observer
	// heard and left alone (another test's).
	ignored int
}

// newRollbackHolders is the diagnostic on the rollback of the binary at dst,
// classifying and asking through the platform's functions.
func newRollbackHolders(dst string) *rollbackHolders {
	return &rollbackHolders{dst: dst, classify: windowsErrorCode, ask: candidateUsers, asked: map[updater.RollbackStage]bool{}}
}

// watchRollbackHolders puts the diagnostic on every rollback of the binary
// at dst until the test ends, and logs what it found then, its summary last
// (rollbackHolders.lines). Call it before whatever starts the rollback, so
// its cleanup, which puts the observer back and logs, runs after the
// cleanups that stop it. Process-global, as the observer is: the test MUST
// NOT call t.Parallel().
func watchRollbackHolders(t *testing.T, dst string) *rollbackHolders {
	t.Helper()
	h := newRollbackHolders(dst)
	restore := updater.SetRollbackObserverForTest(h.observe)
	t.Cleanup(func() {
		restore()
		for _, line := range h.lines() {
			t.Log(line)
		}
	})
	return h
}

// observe is the observer: it records a refusal of a rename of the
// rollback of dst, counting and leaving alone another binary's (another
// test's), and at the first refusal of its stage that says a file is in use
// asks about each of the rename's two files, there and then, while the file
// is still held. Every rename of the put-back has dst at one end. It runs on
// the goroutine that made the rename, and logs nothing: the test's goroutine
// logs it all at its end.
func (h *rollbackHolders) observe(r updater.RollbackRename) {
	if r.From != h.dst && r.To != h.dst {
		h.mu.Lock()
		h.ignored++
		h.mu.Unlock()
		return
	}
	rep := refusalReport{rename: r}
	rep.code, rep.inUse = h.classify(r.Err)
	h.mu.Lock()
	first := rep.inUse && !h.asked[r.Stage]
	if first {
		h.asked[r.Stage] = true
	}
	h.mu.Unlock()
	if first {
		for _, p := range []string{r.From, r.To} {
			rep.resources = append(rep.resources, h.askAbout(p))
		}
	}
	h.mu.Lock()
	h.refusals = append(h.refusals, rep)
	h.mu.Unlock()
}

// askAbout asks who uses the file at path; a path where nothing is (the
// binary's, once it is set aside) is absent, and not asked about.
func (h *rollbackHolders) askAbout(path string) resourceReport {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return resourceReport{path: path, absent: true}
	}
	users, err := h.ask(path)
	return resourceReport{path: path, users: users, err: err}
}

// usersOf is what was found using the file at path at the first in-use
// refusal of stage, and whether it was asked about there.
func (h *rollbackHolders) usersOf(stage updater.RollbackStage, path string) ([]candidateUser, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rep := range h.refusals {
		if rep.rename.Stage != stage {
			continue
		}
		for _, res := range rep.resources {
			if res.path == path && !res.absent && res.err == nil {
				return res.users, true
			}
		}
	}
	return nil, false
}

// lines is what the diagnostic found, a line each: this process's pid; each
// refusal, its stage, its paths and its Windows error code; for each file
// asked about, each candidate user, "no candidate user", "absent" or why it
// could not be asked; and last, "refusals: N; ignored: M; candidate users:
// …", which a run that met no refusal says too, M the refusals of another
// binary's rollback it left alone.
func (h *rollbackHolders) lines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	const lead = "rollback holder diagnostic: "
	out := []string{fmt.Sprintf("%sthis test process is pid %d", lead, os.Getpid())}
	var found []string
	asked := false
	for _, rep := range h.refusals {
		r := rep.rename
		code := "no Windows error code"
		if rep.code != 0 {
			code = fmt.Sprintf("Windows error %d", rep.code)
		}
		inUse := "not one that says a file is in use, so nothing was asked"
		switch {
		case rep.inUse && len(rep.resources) > 0:
			inUse = "a file in use, the stage's first"
		case rep.inUse:
			inUse = "a file in use, asked about at the stage's first already"
		}
		out = append(out, fmt.Sprintf("%sthe %s rename %s -> %s was refused (%s: %v): %s", lead, r.Stage, r.From, r.To, code, r.Err, inUse))
		for _, res := range rep.resources {
			asked = true
			switch {
			case res.absent:
				out = append(out, fmt.Sprintf("%s  %s: absent", lead, res.path))
			case res.err != nil:
				out = append(out, fmt.Sprintf("%s  %s: the Restart Manager could not be asked: %v", lead, res.path, res.err))
			case len(res.users) == 0:
				out = append(out, fmt.Sprintf("%s  %s: no candidate user", lead, res.path))
			}
			for _, u := range res.users {
				out = append(out, fmt.Sprintf("%s  %s: candidate user %s", lead, res.path, u))
				found = append(found, fmt.Sprintf("%s of %s", u, filepath.Base(res.path)))
			}
		}
	}
	users := "none"
	switch {
	case len(found) > 0:
		users = strings.Join(found, "; ")
	case !asked && len(h.refusals) > 0:
		users = "not asked, no refusal said a file was in use"
	}
	return append(out, fmt.Sprintf("%srefusals: %d; ignored: %d; candidate users: %s", lead, len(h.refusals), h.ignored, users))
}

// TestTheRollbackHolderDiagnosticAsksOnceAStageIsRefusedInUse drives the
// diagnostic with refusals as the rollback's observer hands them over, its
// platform's two functions stood in for, so its bookkeeping runs on every
// host: a refusal of another binary's rollback is left alone and counted
// (ignored); the first
// refusal of a stage that says a file is in use asks about both of its
// files there, a file that is not there "absent" and not asked about; a
// second such refusal of that stage, and a refusal that says no file is in
// use, are counted and ask nothing; a file nobody uses says "no candidate
// user", one the Restart Manager could not be asked about says so; and the
// report ends with the summary line, which a run that met no refusal says
// too. What the Restart Manager answers is windows-latest's to show
// (TestTheHolderDiagnosticNamesTheChildThatHoldsTheBackup).
func TestTheRollbackHolderDiagnosticAsksOnceAStageIsRefusedInUse(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bridge.exe")
	bak := dst + ".bak"
	aside := dst + ".rollback-1"
	for _, p := range []string{bak, aside} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	errInUse := errors.New("The process cannot access the file because it is being used by another process.")
	errDenied := errors.New("Access is denied.")
	errNoRM := errors.New("the Restart Manager is not there")
	holder := candidateUser{pid: 4242, parent: 7, image: `C:\holder.exe`}
	var askedAbout []string
	h := &rollbackHolders{dst: dst, asked: map[updater.RollbackStage]bool{},
		classify: func(err error) (uint32, bool) {
			switch err {
			case errInUse:
				return 32, true
			case errDenied:
				return 5, false
			}
			return 0, false
		},
		ask: func(path string) ([]candidateUser, error) {
			askedAbout = append(askedAbout, path)
			switch path {
			case bak:
				return []candidateUser{holder}, nil
			case aside:
				return nil, errNoRM
			}
			return nil, nil
		},
	}
	if got := h.lines(); len(got) != 2 || got[1] != "rollback holder diagnostic: refusals: 0; ignored: 0; candidate users: none" {
		t.Errorf("the report of a run that met no refusal = %q, want this process's pid and the summary", got)
	}
	h.observe(updater.RollbackRename{Stage: updater.RollbackStageBackup, From: filepath.Join(dir, "other.exe.bak"), To: filepath.Join(dir, "other.exe"), Err: errInUse})
	// The binary is set aside: the backup's rename finds nothing at dst.
	h.observe(updater.RollbackRename{Stage: updater.RollbackStageBackup, From: bak, To: dst, Err: errInUse})
	h.observe(updater.RollbackRename{Stage: updater.RollbackStageBackup, From: bak, To: dst, Err: errInUse})
	h.observe(updater.RollbackRename{Stage: updater.RollbackStagePutBack, From: aside, To: dst, Err: errDenied})
	// A later rollback's vacate, the binary there again.
	if err := os.WriteFile(dst, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.observe(updater.RollbackRename{Stage: updater.RollbackStageVacate, From: dst, To: aside, Err: errInUse})

	if want := []string{bak, dst, aside}; strings.Join(askedAbout, "|") != strings.Join(want, "|") {
		t.Errorf("asked about %q, want %q: the backup stage's first in-use refusal's backup (its binary is not there), "+
			"then the vacate's two files, and nothing for a second refusal of a stage or one that says no file is in use", askedAbout, want)
	}
	users, ok := h.usersOf(updater.RollbackStageBackup, bak)
	if !ok || len(users) != 1 || users[0].pid != holder.pid {
		t.Errorf("the backup's users at the backup stage = %v (asked %v), want the holder", users, ok)
	}
	const lead = "rollback holder diagnostic: "
	want := []string{
		fmt.Sprintf("%sthis test process is pid %d", lead, os.Getpid()),
		fmt.Sprintf("%sthe backup rename %s -> %s was refused (Windows error 32: %v): a file in use, the stage's first", lead, bak, dst, errInUse),
		fmt.Sprintf("%s  %s: candidate user pid 4242, image C:\\holder.exe, parent pid 7", lead, bak),
		fmt.Sprintf("%s  %s: absent", lead, dst),
		fmt.Sprintf("%sthe backup rename %s -> %s was refused (Windows error 32: %v): a file in use, asked about at the stage's first already", lead, bak, dst, errInUse),
		fmt.Sprintf("%sthe putback rename %s -> %s was refused (Windows error 5: %v): not one that says a file is in use, so nothing was asked", lead, aside, dst, errDenied),
		fmt.Sprintf("%sthe vacate rename %s -> %s was refused (Windows error 32: %v): a file in use, the stage's first", lead, dst, aside, errInUse),
		fmt.Sprintf("%s  %s: no candidate user", lead, dst),
		fmt.Sprintf("%s  %s: the Restart Manager could not be asked: %v", lead, aside, errNoRM),
		fmt.Sprintf("%srefusals: 4; ignored: 1; candidate users: pid 4242, image C:\\holder.exe, parent pid 7 of bridge.exe.bak", lead),
	}
	if got := h.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the report =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Refusals that never said a file was in use ask nothing, and the
	// summary says so.
	quiet := &rollbackHolders{dst: dst, asked: map[updater.RollbackStage]bool{}, classify: h.classify, ask: h.ask}
	quiet.observe(updater.RollbackRename{Stage: updater.RollbackStageInstall, From: bak, To: dst, Err: errDenied})
	if got := quiet.lines(); got[len(got)-1] != lead+"refusals: 1; ignored: 0; candidate users: not asked, no refusal said a file was in use" {
		t.Errorf("the summary of a run whose one refusal said no file was in use = %q", got[len(got)-1])
	}
}

// TestTheStartTimeIsOnlyOneAProcessCanHave: the Restart Manager's start time
// of a candidate user is read from its FILETIME (filetimeStart) only where it
// names a time a process can have started at, so zero (1601-01-01, which the
// unchecked conversion read as a time in 2185), one at or before the Unix
// epoch, and one past what an int64 of nanoseconds since it holds, leave the
// start time zero, which String leaves out.
func TestTheStartTimeIsOnlyOneAProcessCanHave(t *testing.T) {
	split := func(v uint64) (uint32, uint32) { return uint32(v >> 32), uint32(v) }
	at := time.Date(2026, 11, 14, 3, 4, 5, 600, time.UTC)
	hi, lo := split(filetimeUnixEpoch + uint64(at.UnixNano()/100))
	if got, ok := filetimeStart(hi, lo); !ok || !got.Equal(at.Truncate(100*time.Nanosecond)) {
		t.Errorf("filetimeStart of %s = %s (%v), want it back to 100 ns", at, got, ok)
	}
	for _, tc := range []struct {
		name string
		v    uint64
	}{
		{"zero", 0},
		{"1601 plus one interval", 1},
		{"the Unix epoch itself", filetimeUnixEpoch},
		{"past an int64 of nanoseconds", filetimeUnixEpoch + math.MaxInt64/100 + 1},
		{"the largest FILETIME", math.MaxUint64},
	} {
		hi, lo := split(tc.v)
		if got, ok := filetimeStart(hi, lo); ok || !got.IsZero() {
			t.Errorf("filetimeStart of %s = %s (%v), want the zero time and false", tc.name, got, ok)
		}
	}
	u := candidateUser{pid: 9, parent: -1, image: `C:\x.exe`}
	if s := u.String(); strings.Contains(s, "started") || s != `pid 9, image C:\x.exe, parent pid unknown` {
		t.Errorf("a candidate user with no start time reads %q, want no start time in it", s)
	}
	u.started = at
	if s := u.String(); !strings.Contains(s, "started 2026-11-14T03:04:05.0000006Z") {
		t.Errorf("a candidate user with its start time reads %q, want it", s)
	}
}
