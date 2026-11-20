package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B898: the rollbacks at a start and the console's run Windows'
// put-back of the backup on every host (updater.ReplaceRunningBinaryForTest),
// its probes and renames refused as each test says: a backup held for a moment
// is waited out with the binary in place, and a put-back that leaves no binary
// keeps the rollback's record and ends the start.

// errBackupHeld stands in for the sharing violation Windows answers a rename
// of a file another process holds open; errBackupDenied for a refusal no wait
// ends.
var (
	errBackupHeld   = errors.New("The process cannot access the file because it is being used by another process.")
	errBackupDenied = errors.New("Access is denied.")
)

// heldRestore is a rollback's restore (serveOpts.restoreBackup, bootSeams):
// Windows' put-back over bin, each probe and rename refused as script says,
// by its call ("?bin" probes the binary, "bak>bin" renames the backup onto
// the binary's path, "aside>bin" puts the binary set aside back) or by its
// file ("bak": a probe or a rename of the backup, as another process holding
// it refuses either; -1 is every time), with errBackupHeld, or errBackupDenied
// for a key in denied. Its waits are recorded, and slept only on Windows once
// the script is spent, so that a scanner of the host's holding a file the
// test has just written is waited out there too. Its clock (RestoreSeams.Now)
// moves on by each wait's length and by sleep more, slept or not, and by step
// and took's time for a call's key as that call is made, and by nothing else;
// answers is each call made, its key and what it answered.
type heldRestore struct {
	bin         string
	denied      map[string]bool
	mu          sync.Mutex
	script      map[string]int
	waits       []time.Duration
	clock       time.Time
	took        map[string]time.Duration
	step, sleep time.Duration
	answers     []string
}

func newHeldRestore(bin string, script map[string]int) *heldRestore {
	return &heldRestore{bin: bin, script: script, denied: map[string]bool{}}
}

func (h *heldRestore) key(p string) string {
	switch {
	case p == h.bin:
		return "bin"
	case p == h.bin+".bak":
		return "bak"
	case strings.Contains(filepath.Base(p), ".rollback-"):
		return "aside"
	}
	return filepath.Base(p)
}

// refuse answers a call by its key, then by its file's, the call taking
// took's time.
func (h *heldRestore) refuse(call, file string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(h.step + h.took[call])
	for _, k := range []string{call, file} {
		n := h.script[k]
		if n == 0 {
			continue
		}
		if n > 0 {
			h.script[k] = n - 1
		}
		if h.denied[k] {
			return errBackupDenied
		}
		return errBackupHeld
	}
	return nil
}

func (h *heldRestore) restore() func(dst, ext string) error {
	return updater.ReplaceRunningBinaryForTest(updater.RestoreSeams{
		Rename: func(from, to string) error {
			call := h.key(from) + ">" + h.key(to)
			err := h.refuse(call, h.key(from))
			if err == nil {
				err = os.Rename(from, to)
			} else {
				err = &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
			}
			h.answer(call, err)
			return err
		},
		Probe: func(p string) error {
			call := "?" + h.key(p)
			err := h.refuse(call, h.key(p))
			if err != nil {
				err = &os.PathError{Op: "open", Path: p, Err: err}
			}
			h.answer(call, err)
			return err
		},
		InUse: func(err error) bool { return errors.Is(err, errBackupHeld) },
		Wait: func(d time.Duration) {
			h.mu.Lock()
			h.waits = append(h.waits, d)
			h.clock = h.clock.Add(d + h.sleep)
			spent := true
			for _, n := range h.script {
				spent = spent && n == 0
			}
			h.mu.Unlock()
			if runtime.GOOS == "windows" && spent {
				time.Sleep(d)
			}
		},
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.clock
		},
	})
}

// answer records what call answered: ok, held, denied, or the error.
func (h *heldRestore) answer(call string, err error) {
	word := "ok"
	switch {
	case errors.Is(err, errBackupHeld):
		word = "held"
	case errors.Is(err, errBackupDenied):
		word = "denied"
	case err != nil:
		word = err.Error()
	}
	h.mu.Lock()
	h.answers = append(h.answers, call+" "+word)
	h.mu.Unlock()
}

// answered is every call h was asked, in order, with its answer; collapsed,
// each run of one call's answers stands as its last, so that a refusal the
// platform's own predicate waits out (a scanner of the host's holding a file
// the test made) leaves the record as the script alone would.
func (h *heldRestore) answered(collapsed bool) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, a := range h.answers {
		call, _, _ := strings.Cut(a, " ")
		if n := len(out); collapsed && n > 0 && strings.HasPrefix(out[n-1], call+" ") {
			out[n-1] = a
			continue
		}
		out = append(out, a)
	}
	return strings.Join(out, ", ")
}

// TestTheHeldRestoresCollapsedRecordKeepsEachCallsLastAnswer: a run of one
// call's answers, as a scanner's refusal of the put-back adds one, stands as
// its last, and nothing else changes.
func TestTheHeldRestoresCollapsedRecordKeepsEachCallsLastAnswer(t *testing.T) {
	h := &heldRestore{answers: []string{"?bin ok", "?bin ok", "bak>bin held",
		"aside>bin The process cannot access the file because it is being used by another process.", "aside>bin ok"}}
	if got, want := h.answered(true), "?bin ok, bak>bin held, aside>bin ok"; got != want {
		t.Errorf("collapsed: %s, want %s", got, want)
	}
	if got := h.answered(false); got != strings.Join(h.answers, ", ") {
		t.Errorf("as made: %s, want every answer", got)
	}
}

func (h *heldRestore) waited() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.waits)
}

// requireWaitedOut requires the backup's two refusals waited out at its probe
// before anything moved, the schedule's first two waits first (exactly those
// off Windows, where nothing else holds the file), and the backup put back.
func (h *heldRestore) requireWaitedOut(t *testing.T, bin string) {
	t.Helper()
	w := h.waited()
	if len(w) < 2 || w[0] != 50*time.Millisecond || w[1] != 100*time.Millisecond || (runtime.GOOS != "windows" && len(w) != 2) {
		t.Errorf("the rollback waited %v, want 50ms and 100ms for the backup's two refusals", w)
	}
	if got := reportedVersion(t, bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q after the rollback, want %s", got, previousVersion)
	}
}

// leftNoBinary is a script whose put-back leaves no binary at its path: the
// place refused once, and the put-back of the binary set aside every time.
func leftNoBinary() map[string]int { return map[string]int{"bak>bin": 1, "aside>bin": -1} }

// asideOf is the binary a rollback set aside beside bin.
func asideOf(t *testing.T, bin string) string {
	t.Helper()
	m, _ := filepath.Glob(bin + ".rollback-*")
	if len(m) != 1 {
		t.Fatalf("asides beside the binary: %v, want one", m)
	}
	return m[0]
}

// markerBytes is what SaveState writes for st.
func markerBytes(t *testing.T, st updater.State) []byte {
	t.Helper()
	dir := t.TempDir()
	if err := updater.SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(updater.StatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// requireMarker requires dataDir's install marker to be st, byte for byte.
func requireMarker(t *testing.T, dataDir string, st updater.State) {
	t.Helper()
	got, err := os.ReadFile(updater.StatePath(dataDir))
	if want := markerBytes(t, st); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the install marker = %s (%v)\nwant %s", got, err, want)
	}
}

// filesBelow is every regular file below dir but the install marker, with its
// bytes.
func filesBelow(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || p == updater.StatePath(dir) {
			return err
		}
		b, err := os.ReadFile(p)
		files[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestTheEarlyRollbackWaitsOutABackupHeldForAMoment: this version refuses its
// settings at its first start, and another process holds the backup the
// rollback has just run for its version: held for a moment, the rollback
// waits it out with the binary in place and exits 75; held throughout,
// nothing moved, the install marker is as it was, and the line says how long
// the hold was waited for before serve refuses its settings as before.
func TestTheEarlyRollbackWaitsOutABackupHeldForAMoment(t *testing.T) {
	t.Run("for a moment", func(t *testing.T) {
		r := newRefusedInstall(t, "scanIntervalSec: 010\n")
		h := newHeldRestore(r.bin, map[string]int{"bak": 2})
		if code, stderr := r.serve(t, serveOpts{restoreBackup: h.restore()}); code != supervision.RestartExitCode {
			t.Fatalf("serve = %d, want %d; stderr=%s", code, supervision.RestartExitCode, stderr)
		}
		h.requireWaitedOut(t, r.bin)
	})
	t.Run("throughout", func(t *testing.T) {
		r := newRefusedInstall(t, "scanIntervalSec: 010\n")
		files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
		code, stderr := r.serve(t, serveOpts{restoreBackup: newHeldRestore(r.bin, map[string]int{"bak": -1}).restore()})
		if code != 2 {
			t.Fatalf("serve = %d, want 2, its settings refused as before; stderr=%s", code, stderr)
		}
		for _, s := range []string{"not rolling back the install", "could not put the backup back",
			"in use by another process: 8 attempts over 4.75s", "nothing was moved"} {
			if !strings.Contains(updaterLines(stderr), s) {
				t.Errorf("serve did not say %q; stderr=%s", s, stderr)
			}
		}
		files.requireUnchanged(t)
	})
}

// TestTheCrashRollbackWaitsOutABackupHeldForAMoment: three starts of this
// version crashed, and the next start's crash rollback finds the backup it
// has just run held: for a moment, it waits it out and exits 75 before
// anything serves; throughout, nothing moved, it says the wait and records
// why, and the start goes on.
func TestTheCrashRollbackWaitsOutABackupHeldForAMoment(t *testing.T) {
	t.Run("for a moment", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		r.crashedThreeTimes(t)
		h := newHeldRestore(r.bin, map[string]int{"bak": 2})
		code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin, restoreBackup: h.restore()})
		if served || code != supervision.RestartExitCode {
			t.Fatalf("the start = %d (served %v), want %d before it served; stderr=%s", code, served, supervision.RestartExitCode, stderr)
		}
		h.requireWaitedOut(t, r.bin)
	})
	t.Run("throughout", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		r.crashedThreeTimes(t)
		binaries := watch(t, r.bin, r.bak)
		code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin,
			restoreBackup: newHeldRestore(r.bin, map[string]int{"bak": -1}).restore()})
		if !served || code != 0 {
			t.Fatalf("the start = %d (served %v), want it to go on and serve; stderr=%s", code, served, stderr)
		}
		binaries.requireUnchanged(t)
		if !strings.Contains(updaterLines(stderr), "in use by another process: 8 attempts over 4.75s") {
			t.Errorf("the start did not say how long the hold was waited for; stderr=%s", stderr)
		}
		if st := markerOf(t, r.dataDir); st.RollbackRefused != "the backup could not be put back over the binary" {
			t.Errorf("the marker = %+v, want it to record why the backup was not put back", st)
		}
	})
}

// TestAStartWhoseCrashRollbackLeavesNoBinaryEndsBeforeItsAdmission: the crash
// rollback's put-back refused past its schedule, no binary is at its path, and
// the start ends there, exit 1, before its admission: the install marker is
// the rollback's record of the marker the starts before left, byte for byte
// (its hold, its veto, its count and its record of the data dir), nothing
// else in the data dir is written, and the line names the rename that puts
// the binary back; after that rename the next start finishes the rollback.
func TestAStartWhoseCrashRollbackLeavesNoBinaryEndsBeforeItsAdmission(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.crashedThreeTimes(t)
	before, files := markerOf(t, r.dataDir), filesBelow(t, r.dataDir)
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin,
		restoreBackup: newHeldRestore(r.bin, leftNoBinary()).restore()})
	if served || code != 1 {
		t.Fatalf("the start = %d (served %v), want 1 before it served; stderr=%s", code, served, stderr)
	}
	requireMarker(t, r.dataDir, updater.CrashRollbackRecord(before, version.ServerVersion))
	if after := filesBelow(t, r.dataDir); !maps.Equal(after, files) {
		t.Errorf("the start wrote the data dir past the rollback's record:\nbefore %v\nafter  %v", slices.Sorted(maps.Keys(files)), slices.Sorted(maps.Keys(after)))
	}
	aside := asideOf(t, r.bin)
	if said := updaterLines(stderr); !strings.Contains(said, "rename "+aside+" to "+r.bin) || !strings.Contains(said, crashRollbackEndsTheStart) {
		t.Errorf("the start did not name the rename that puts the binary back; stderr=%s", stderr)
	}
	if err := os.Rename(aside, r.bin); err != nil {
		t.Fatal(err)
	}
	if code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin}); served || code != supervision.RestartExitCode {
		t.Fatalf("the start after the rename = %d (served %v), want %d; stderr=%s", code, served, supervision.RestartExitCode, stderr)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q, want the backup's %s", got, previousVersion)
	}
}

// TestAnEarlyRollbackThatLeavesNoBinaryKeepsItsRecord: the early rollback's
// put-back refused past its schedule, serve refuses its settings as before
// (exit 2) with the rollback's record kept, never the marker as it was read,
// and after the rename its line names, the next start finishes the rollback.
func TestAnEarlyRollbackThatLeavesNoBinaryKeepsItsRecord(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	code, stderr := r.serve(t, serveOpts{restoreBackup: newHeldRestore(r.bin, leftNoBinary()).restore()})
	if code != 2 {
		t.Fatalf("serve = %d, want 2; stderr=%s", code, stderr)
	}
	st := markerOf(t, r.dataDir)
	if st.RejectedReason == "" || st.RejectedConfigDigest == "" {
		t.Fatalf("the marker = %+v, want the rollback's record", st)
	}
	requireMarker(t, r.dataDir, updater.RollbackRecord(r.marker, version.ServerVersion, st.RejectedReason, st.RejectedConfigDigest))
	aside := asideOf(t, r.bin)
	if !strings.Contains(updaterLines(stderr), "rename "+aside+" to "+r.bin) {
		t.Errorf("serve did not name the rename that puts the binary back; stderr=%s", stderr)
	}
	if err := os.Rename(aside, r.bin); err != nil {
		t.Fatal(err)
	}
	if code, stderr := r.serve(t, serveOpts{}); code != supervision.RestartExitCode {
		t.Fatalf("the start after the rename = %d, want %d; stderr=%s", code, supervision.RestartExitCode, stderr)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q, want the backup's %s", got, previousVersion)
	}
}

// bootRollbackInstall is an install of 9.9.9 over this version whose target
// never came up, beside a backup that says this version: this version's boot
// puts it back (BootInstallFailed).
func bootRollbackInstall(t *testing.T) *refusedInstall {
	t.Helper()
	r := newRefusedInstall(t, "")
	r.marker.TargetVersion, r.marker.PreviousVersion = "9.9.9", version.ServerVersion
	r.saveMarker(t, r.dataDir)
	t.Setenv(runAsBridgeVersionEnv, version.ServerVersion)
	return r
}

// TestABootRollbackThatLeavesNoBinaryEndsTheStart: the boot's rollback, once
// the store has opened, whose put-back leaves no binary: the start ends after
// its housekeeping, exit 1, before anything listens, its vetoed marker kept,
// its line naming the rename that puts the binary back.
func TestABootRollbackThatLeavesNoBinaryEndsTheStart(t *testing.T) {
	persistentMedium(t)
	r := bootRollbackInstall(t)
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin,
		restoreBackup: newHeldRestore(r.bin, leftNoBinary()).restore()})
	if served || code != 1 || !strings.Contains(stderr, "serve: not starting") {
		t.Fatalf("the start = %d (served %v), want 1 before it served; stderr=%s", code, served, stderr)
	}
	if st := markerOf(t, r.dataDir); st.Status != "installing" || st.MigrationVeto != "9.9.9" {
		t.Errorf("the marker = %+v, want the install kept, its version's migrations vetoed", st)
	}
	if !strings.Contains(updaterLines(stderr), "rename "+asideOf(t, r.bin)+" to "+r.bin) {
		t.Errorf("the start did not name the rename that puts the binary back; stderr=%s", stderr)
	}
}

// TestTheBootRollbackKeepsItsMarkerWhereTheBinaryWasInUse: a boot rollback
// refused for a hold, with nothing moved or the binary put back, or whose
// forward deadline passed before its vacate was tried, keeps its vetoed
// marker for the next start to ask again; one whose place was refused
// otherwise, the binary put back, clears the install as before. Each case's
// put-back made exactly its calls, each with its answer, or on Windows, where
// a scanner of the host's may hold a file too and the platform's own
// predicate waits it out, ended with its last ones once each run of one
// call's answers stands as its last.
func TestTheBootRollbackKeepsItsMarkerWhereTheBinaryWasInUse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		script   map[string]int
		denied   string
		took     map[string]time.Duration
		kept     bool
		answered string
		last     string
	}{
		{"nothing moved", map[string]int{"bak": -1}, "", nil, true,
			"?bin ok, " + strings.Repeat("?bak held, ", 7) + "?bak held", "?bak held"},
		{"put back", map[string]int{"bak>bin": 1}, "", nil, true,
			"?bin ok, ?bak ok, bin>aside ok, bak>bin held, aside>bin ok", "bak>bin held, aside>bin ok"},
		{"refused otherwise", map[string]int{"bak>bin": 1}, "bak>bin", nil, false,
			"?bin ok, ?bak ok, bin>aside ok, bak>bin denied, aside>bin ok", "bak>bin denied, aside>bin ok"},
		{"out of time", nil, "", map[string]time.Duration{"?bak": 5 * time.Second}, true,
			"?bin ok, ?bak ok", "?bak ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.marker.TargetVersion = "9.9.9"
			r.saveMarker(t, r.dataDir)
			h := newHeldRestore(r.bin, tc.script)
			if tc.denied != "" {
				h.denied[tc.denied] = true
			}
			h.took = tc.took
			var stderr bytes.Buffer
			if err := bootHousekeeping(t.Context(), &stderr, r.dataDir, r.bin, previousVersion, h.restore(), testBackupProbeTimeout); err != nil {
				t.Fatalf("the housekeeping = %v; stderr=%s", err, stderr.String())
			}
			if got := h.answered(false); (runtime.GOOS != "windows" && got != tc.answered) || !strings.HasSuffix(h.answered(true), tc.last) {
				t.Errorf("the put-back's calls: %s\nwant %s", got, tc.answered)
			}
			st := markerOf(t, r.dataDir)
			if kept := st.Status == "installing" && st.MigrationVeto == "9.9.9"; kept != tc.kept {
				t.Errorf("the marker = %+v: kept %v, want %v; stderr=%s", st, kept, tc.kept, stderr.String())
			}
		})
	}
}

// TestAPutBackOnAClockItsStepsMoveKeepsEveryAttempt: Windows' put-back on
// any host, on a clock every call and every sleep moves on by a millisecond,
// as a host's does: held at the backup throughout, the rollback makes all
// eight of its forward attempts, its last wait shortened to what remained,
// and its put-back held throughout all seven, each said.
func TestAPutBackOnAClockItsStepsMoveKeepsEveryAttempt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a virtual-clock schedule: a scanner's hold of a file this test writes would draw on its deadline; Windows' real holds are held_windows_test.go's")
	}
	ms := func(ns ...int) []time.Duration {
		var ds []time.Duration
		for _, n := range ns {
			ds = append(ds, time.Duration(n)*time.Millisecond)
		}
		return ds
	}
	for _, tc := range []struct {
		name   string
		script map[string]int
		said   string
		waits  []time.Duration
	}{
		{"held at the backup", map[string]int{"bak": -1}, "in use by another process: 8 attempts over 4.736s",
			ms(50, 100, 200, 400, 800, 1600, 1586)},
		{"its put-back held", leftNoBinary(), "in use by another process: 7 attempts over 3.139s",
			ms(50, 100, 200, 400, 800, 1589)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "bridge")
			for _, p := range []string{bin, bin + ".bak"} {
				if err := os.WriteFile(p, []byte(p), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			h := newHeldRestore(bin, tc.script)
			h.step, h.sleep = time.Millisecond, time.Millisecond
			err := h.restore()(bin, ".bak")
			if err == nil || !strings.Contains(err.Error(), tc.said) {
				t.Errorf("the put-back = %v, want it to say %q", err, tc.said)
			}
			if got := h.waited(); !slices.Equal(got, tc.waits) {
				t.Errorf("the put-back waited %v, want %v", got, tc.waits)
			}
		})
	}
}

// TestTheBootRollbackLeavesABackupThatIsNotThePreviousVersion: an install
// marker left armed beside an older backup, as a swap that moved nothing and a
// disarm that could not be written leave it: the boot of the version the
// marker says was replaced asks the backup its version first, leaves it where
// it is and clears the install, where putting it back would go back two
// versions.
func TestTheBootRollbackLeavesABackupThatIsNotThePreviousVersion(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.TargetVersion = "9.9.9"
	r.saveMarker(t, r.dataDir)
	t.Setenv(runAsBridgeVersionEnv, "0.0.0-older")
	files := watch(t, r.bin, r.bak)
	var stderr bytes.Buffer
	if err := bootHousekeeping(t.Context(), &stderr, r.dataDir, r.bin, previousVersion, nil, testBackupProbeTimeout); err != nil {
		t.Fatalf("the housekeeping = %v; stderr=%s", err, stderr.String())
	}
	files.requireUnchanged(t)
	if st := markerOf(t, r.dataDir); updater.InstallArmed(st) {
		t.Errorf("the marker = %+v, want the install cleared", st)
	}
	if !strings.Contains(stderr.String(), "the backup is 0.0.0-older, where the install replaced "+previousVersion) {
		t.Errorf("the housekeeping did not say why it left the backup; stderr=%s", stderr.String())
	}
}

// keptUntil is what a boot rollback that keeps the marker over a backup that
// did not say its version says of the window that bounds the keep (backlog
// B898FU's round 3): an install of target, attempted at attempted, is refused
// as waiting for its restart until the backup says its version or the window
// ends, updater.RecencyWindow() after the attempt, in UTC, and a restart only
// asks the backup again.
func keptUntil(attempted time.Time, target string) string {
	return fmt.Sprintf("until the backup says its version or the window ends at %s, an install of %s is refused as waiting for its "+
		"restart, and a restart only asks the backup again", attempted.Add(updater.RecencyWindow()).UTC().Format(time.RFC3339), target)
}

// TestTheBootRollbackKeepsItsMarkerWhereTheBackupDidNotSayItsVersion: a boot
// rollback whose backup did not say its version within its bound leaves the
// install as it is, as the early and the crash rollbacks do (backlog B898FU):
// the binary, the backup and the marker byte for byte, nothing put back, and
// said, with the window's end (keptUntil, the install attempted an hour
// before, so the end the attempt gives is not the start's clock's); the next
// start asks again, and once the backup answers puts it back. A stop while
// the backup answers leaves the install so too, and is the housekeeping's
// answer. A backup that is not there or is no regular file, which is never
// asked, and one that says another version still clear the install, the
// backup left where it is. Each row is an install of its own, armed, its
// backup in place.
func TestTheBootRollbackKeepsItsMarkerWhereTheBackupDidNotSayItsVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		// noBackup removes the backup before the boot, and dirBackup puts a
		// directory in its place; answer is the backup's to the version's
		// ask, on the boot's context, which stop cancels; window says that the
		// line names the window's end.
		noBackup, dirBackup bool
		answer              func(ctx context.Context, stop context.CancelFunc) (string, error)
		asked               int
		stopped             bool
		kept                bool
		says                []string
		window              bool
	}{
		{name: "it did not answer within its bound",
			answer: func(context.Context, context.CancelFunc) (string, error) {
				return "", errors.New("it did not answer within 1s")
			}, asked: 1, kept: true, window: true,
			says: []string{"the backup did not say its version: it did not answer within 1s", "the install's marker is kept"}},
		{name: "a stop while it answered",
			answer: func(ctx context.Context, stop context.CancelFunc) (string, error) {
				stop()
				return "", ctx.Err()
			}, asked: 1, stopped: true, kept: true,
			says: []string{"stopped while the backup said its version"}},
		{name: "no backup", noBackup: true,
			says: []string{"there is no backup to put back"}},
		{name: "a directory where the backup goes", dirBackup: true,
			says: []string{"there is no backup to put back"}},
		{name: "another version",
			answer: func(context.Context, context.CancelFunc) (string, error) { return "0.0.0-older", nil },
			asked:  1,
			says:   []string{"the backup is 0.0.0-older, where the install replaced " + previousVersion}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.marker.TargetVersion = "9.9.9"
			r.marker.AttemptedAt = time.Now().UTC().Add(-time.Hour)
			r.saveMarker(t, r.dataDir)
			watched := []string{r.bin, r.bak}
			if tc.noBackup || tc.dirBackup {
				if err := os.Remove(r.bak); err != nil {
					t.Fatal(err)
				}
				watched = []string{r.bin}
			}
			if tc.dirBackup {
				if err := os.Mkdir(r.bak, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.kept {
				watched = append(watched, updater.StatePath(r.dataDir))
			}
			files := watch(t, watched...)
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			asked, restored := 0, 0
			var stderr bytes.Buffer
			err := bootHousekeepingWith(ctx, &stderr, r.dataDir, r.bin, previousVersion, bootSeams{
				restore: func(string, string) error { restored++; return nil },
				probeVersion: func(ctx context.Context, _ string, _ time.Duration) (string, error) {
					asked++
					return tc.answer(ctx, stop)
				},
				probeBound: testBackupProbeTimeout})
			if tc.stopped != errors.Is(err, context.Canceled) || (!tc.stopped && err != nil) {
				t.Fatalf("the housekeeping = %v, want the stop's error %v; stderr=%s", err, tc.stopped, stderr.String())
			}
			files.requireUnchanged(t)
			if asked != tc.asked || restored != 0 {
				t.Errorf("the backup was asked its version %d time(s) and put back %d, want %d and never", asked, restored, tc.asked)
			}
			if armed := updater.InstallArmed(markerOf(t, r.dataDir)); armed != tc.kept {
				t.Errorf("the install is armed: %v, want %v; stderr=%s", armed, tc.kept, stderr.String())
			}
			says := tc.says
			if tc.window {
				says = append(slices.Clone(says), keptUntil(r.marker.AttemptedAt, "9.9.9"))
			}
			for _, s := range says {
				if !strings.Contains(stderr.String(), s) {
					t.Errorf("the housekeeping did not say %q; stderr=%s", s, stderr.String())
				}
			}
			if !tc.kept {
				return
			}
			// The next start, the backup answering, puts it back through the
			// production restore.
			stderr.Reset()
			if err := bootHousekeepingWith(t.Context(), &stderr, r.dataDir, r.bin, previousVersion, bootSeams{
				probeVersion: func(context.Context, string, time.Duration) (string, error) { return r.marker.PreviousVersion, nil },
				probeBound:   testBackupProbeTimeout}); err != nil || !strings.Contains(stderr.String(), "rolling back to .bak") {
				t.Fatalf("the next start's housekeeping = %v, want it to put the backup back; stderr=%s", err, stderr.String())
			}
			if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
				t.Errorf("the binary reports %q after the next start, want %s", got, previousVersion)
			}
			if st := markerOf(t, r.dataDir); updater.InstallArmed(st) || st.MigrationVeto != "9.9.9" {
				t.Errorf("the marker after the next start = %+v, want the install cleared, its version's migrations vetoed", st)
			}
		})
	}
}

// inspectionFailure is a way a lookup of the backup beside an install's
// binary fails otherwise than finding nothing there, which shows no backup
// absent (backlog B898FU's round 2): lay makes the lookup of the backup
// beside bin fail with cause, and returns the binary's path a rollback is to
// be given, the backup at that path and .bak, and recovered, which lets the
// filesystem recover, after which that path reaches bin and its backup.
type inspectionFailure struct {
	name  string
	cause error
	lay   func(t *testing.T, bin string) (binaryPath string, recovered func())
}

// laid is f laid over the backup beside bin, its premise required: a lookup
// of the backup at the binary's path it returns fails with f's cause.
func (f inspectionFailure) laid(t *testing.T, bin string) (string, func()) {
	t.Helper()
	binaryPath, recovered := f.lay(t, bin)
	if _, err := os.Lstat(binaryPath + ".bak"); !errors.Is(err, f.cause) {
		recovered()
		t.Fatalf("the premise: a lookup of the backup answered %v, want %v", err, f.cause)
	}
	return binaryPath, recovered
}

// inspectionFailures are the boot, the early and the crash rollbacks' backups
// that cannot be inspected: a directory this user may not search, which needs
// POSIX modes and a user that is not root, whom no mode refuses, and a link
// loop on the way, which needs symbolic links, which Windows gives only with
// a privilege; neither runs on Windows, as the package's other such fixtures.
var inspectionFailures = []inspectionFailure{
	{"a directory this user may not search", fs.ErrPermission, func(t *testing.T, bin string) (string, func()) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("a directory this user may not search needs POSIX modes and a user that is not root")
		}
		dir := filepath.Dir(bin)
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, fi.Mode().Perm()) })
		return bin, func() {
			if err := os.Chmod(dir, fi.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
		}
	}},
	{"a link loop on the way", syscall.ELOOP, func(t *testing.T, bin string) (string, func()) {
		if runtime.GOOS == "windows" {
			t.Skip("a link loop needs symbolic links, which Windows gives only with a privilege")
		}
		loop := filepath.Join(filepath.Dir(filepath.Dir(bin)), "loop")
		if err := os.Symlink("loop", loop); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(loop, filepath.Base(bin)), func() {
			// The link repointed at the binary's directory, through which the
			// path reaches the binary and its backup.
			if err := os.Remove(loop); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Dir(bin), loop); err != nil {
				t.Fatal(err)
			}
		}
	}},
}

// TestTheBootRollbackKeepsItsMarkerWhereTheBackupCannotBeInspected: a boot
// rollback whose lookup of the backup fails otherwise than finding nothing
// there (inspectionFailures) shows no backup absent, and leaves the install
// as one whose backup did not say its version (backlog B898FU's round 2): the
// housekeeping answers nil, the backup is asked nothing and nothing is put
// back, the binary, the backup and the marker are as they were byte for byte
// once the filesystem has recovered, and the line says so, with the window's
// end (keptUntil, the install attempted an hour before); the next start then
// asks the backup and puts it back. Until then the failed lookup was taken
// for no backup, and the install cleared, so no later start could roll it
// back.
func TestTheBootRollbackKeepsItsMarkerWhereTheBackupCannotBeInspected(t *testing.T) {
	for _, f := range inspectionFailures {
		t.Run(f.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.marker.TargetVersion = "9.9.9"
			r.marker.AttemptedAt = time.Now().UTC().Add(-time.Hour)
			r.saveMarker(t, r.dataDir)
			files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
			binaryPath, recovered := f.laid(t, r.bin)
			asked, restored := 0, 0
			var stderr bytes.Buffer
			err := bootHousekeepingWith(t.Context(), &stderr, r.dataDir, binaryPath, previousVersion, bootSeams{
				restore: func(string, string) error { restored++; return nil },
				probeVersion: func(context.Context, string, time.Duration) (string, error) {
					asked++
					return r.marker.PreviousVersion, nil
				},
				probeBound: testBackupProbeTimeout})
			recovered()
			if err != nil {
				t.Fatalf("the housekeeping = %v, want nil; stderr=%s", err, stderr.String())
			}
			files.requireUnchanged(t)
			if asked != 0 || restored != 0 {
				t.Errorf("the backup was asked its version %d time(s) and put back %d, want neither", asked, restored)
			}
			if st := markerOf(t, r.dataDir); !updater.InstallArmed(st) {
				t.Errorf("the marker = %+v, want the install kept; stderr=%s", st, stderr.String())
			}
			for _, s := range []string{"the backup did not say its version: it could not be inspected", "the install's marker is kept",
				keptUntil(r.marker.AttemptedAt, "9.9.9")} {
				if !strings.Contains(stderr.String(), s) {
					t.Errorf("the housekeeping did not say %q; stderr=%s", s, stderr.String())
				}
			}
			// The next start, the filesystem recovered and the backup
			// answering, puts it back through the production restore.
			stderr.Reset()
			if err := bootHousekeepingWith(t.Context(), &stderr, r.dataDir, binaryPath, previousVersion, bootSeams{
				probeVersion: func(context.Context, string, time.Duration) (string, error) { return r.marker.PreviousVersion, nil },
				probeBound:   testBackupProbeTimeout}); err != nil || !strings.Contains(stderr.String(), "rolling back to .bak") {
				t.Fatalf("the next start's housekeeping = %v, want it to put the backup back; stderr=%s", err, stderr.String())
			}
			if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
				t.Errorf("the binary reports %q after the next start, want %s", got, previousVersion)
			}
			if st := markerOf(t, r.dataDir); updater.InstallArmed(st) || st.MigrationVeto != "9.9.9" {
				t.Errorf("the marker after the next start = %+v, want the install cleared, its version's migrations vetoed", st)
			}
		})
	}
}

// TestTheBootRollbackWritesNothingForAStopOnceItsBackupHasAnswered: a stop
// that lands once the backup has said the install's PreviousVersion, after the
// probe's last look at the context (updater.ProbeVersionWithin's, made before
// its answer is read) and before anything is written, leaves the install as it
// is (backlog B951): the housekeeping, its backup asked through the production
// probe and parked where the answer is heard (bootSeams.answered) while the
// case stops it, answers the stop's error and says the stop's line, the
// marker, the binary and the backup as they were byte for byte, the restore
// never called. runServe acts on that answer no further (it acts on
// updater.ErrBinaryMissing alone): its own select sees the stop and ends the
// start. Until then such a stop went on to the veto's write and the restore
// during serve's stop.
func TestTheBootRollbackWritesNothingForAStopOnceItsBackupHasAnswered(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.TargetVersion = "9.9.9"
	r.saveMarker(t, r.dataDir)
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	parked, release, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var letGoOnce sync.Once
	letGo := func() { letGoOnce.Do(func() { close(release) }) }
	var (
		why        string
		unanswered error
		restored   int
		err        error
		stderr     bytes.Buffer
	)
	go func() {
		defer close(ended)
		err = bootHousekeepingWith(ctx, &stderr, r.dataDir, r.bin, previousVersion, bootSeams{
			restore: func(dst, ext string) error {
				restored++
				return updater.RollbackBinary(dst, ext)
			},
			answered: func(w string, u error) {
				why, unanswered = w, u
				close(parked)
				<-release
			},
			probeBound: testBackupProbeTimeout})
	}()
	t.Cleanup(func() {
		letGo()
		select {
		case <-ended:
		case <-time.After(time.Minute):
			t.Error("the housekeeping did not return within a minute of its release")
		}
	})
	select {
	case <-parked:
	case <-ended:
		t.Fatalf("the housekeeping = %v before the backup's answer was heard; stderr=%s", err, stderr.String())
	case <-time.After(testBackupProbeTimeout + time.Minute):
		t.Fatal("the backup's answer was not heard within its probe's bound and a minute")
	}
	if why != "" || unanswered != nil {
		t.Fatalf("the premise: the backup answered %q, %v; want the install's PreviousVersion said", why, unanswered)
	}
	stop()
	letGo()
	select {
	case <-ended:
	case <-time.After(time.Minute):
		t.Fatal("the housekeeping did not return within a minute of the stop")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the housekeeping = %v, want the stop's error; stderr=%s", err, stderr.String())
	}
	files.requireUnchanged(t)
	if restored != 0 {
		t.Errorf("the backup was put back %d time(s), want never", restored)
	}
	if said := stderr.String(); !strings.Contains(said, "updater: stopped as the backup said its version; the install is left as it is, for a later start") ||
		strings.Contains(said, "rolling back to .bak") {
		t.Errorf("the housekeeping did not say the stop's line alone; stderr=%s", said)
	}
}

// TestTheBootRollbackSaysItsWindowEndedWhereTheBackupsAskOutlastedIt: a boot
// rollback that decided within the window (bootSeams.now, the housekeeping's
// clock, a minute before the window's end) and whose backup did not say its
// version until the window had ended (its ask moving that clock past the end)
// keeps the marker at this start all the same, nothing written or renamed, and
// says that the window has ended, when, and that the next start of the running
// version clears the install as abandoned without asking the backup, leaving
// the backup where it is (backlog B951): the line promises no later ask, and
// no refusal of an install until a time that has passed. The next start, its
// clock past the window, does that: the marker cleared, the backup asked
// nothing and left as it was. Until then the line said what it says within the
// window, whatever the clock read once the backup's ask had ended.
func TestTheBootRollbackSaysItsWindowEndedWhereTheBackupsAskOutlastedIt(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.TargetVersion = "9.9.9"
	r.marker.AttemptedAt = time.Date(2026, time.November, 20, 3, 4, 5, 0, time.UTC)
	r.saveMarker(t, r.dataDir)
	end := r.marker.AttemptedAt.Add(updater.RecencyWindow())
	clock := end.Add(-time.Minute)
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	silent := errors.New("it did not answer within 2m0s")
	asked, restored := 0, 0
	seams := bootSeams{
		restore: func(string, string) error { restored++; return nil },
		now:     func() time.Time { return clock },
		probeVersion: func(context.Context, string, time.Duration) (string, error) {
			asked++
			clock = end.Add(time.Second)
			return "", silent
		},
		probeBound: testBackupProbeTimeout}
	var stderr bytes.Buffer
	if err := bootHousekeepingWith(t.Context(), &stderr, r.dataDir, r.bin, previousVersion, seams); err != nil {
		t.Fatalf("the housekeeping = %v, want nil; stderr=%s", err, stderr.String())
	}
	want := fmt.Sprintf("updater: install of 9.9.9 did not reach the expected version (running %s); not rolling back to %s.bak at this start, "+
		"since the backup did not say its version: %v; the install's marker is kept by this start, but its window ended at %s while the "+
		"backup was asked: the next start of %s clears the install as abandoned, without asking the backup, and leaves the backup where it is\n",
		previousVersion, r.bin, silent, end.Format(time.RFC3339), previousVersion)
	if got := stderr.String(); got != want {
		t.Errorf("the housekeeping said\n%q\nwant\n%q", got, want)
	}
	files.requireUnchanged(t)
	if asked != 1 || restored != 0 {
		t.Errorf("the backup was asked its version %d time(s) and put back %d, want once and never", asked, restored)
	}
	// The next start, past the window: the install cleared as abandoned, the
	// backup asked nothing and left where it is.
	binaries := watch(t, r.bin, r.bak)
	clock = end.Add(time.Minute)
	asked = 0
	stderr.Reset()
	if err := bootHousekeepingWith(t.Context(), &stderr, r.dataDir, r.bin, previousVersion, seams); err != nil {
		t.Fatalf("the next start's housekeeping = %v, want nil; stderr=%s", err, stderr.String())
	}
	binaries.requireUnchanged(t)
	if asked != 0 || restored != 0 {
		t.Errorf("the next start asked the backup its version %d time(s) and put it back %d, want neither", asked, restored)
	}
	if st := markerOf(t, r.dataDir); updater.InstallArmed(st) {
		t.Errorf("the marker after the next start = %+v, want the install cleared as abandoned; stderr=%s", st, stderr.String())
	}
}

// TestAServeWithoutItsDataDirsLockRollsBackAtItsBoot: a serve that could not
// take its data dir's lock (a lock that fails, which serve starts without)
// runs its boot's rollback all the same, which stops no service manager's
// service on any host (updater's TestTheRollbackStopsNoService), and serves.
func TestAServeWithoutItsDataDirsLockRollsBackAtItsBoot(t *testing.T) {
	persistentMedium(t)
	r := bootRollbackInstall(t)
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin,
		dataDirTryLock: func(*os.File) error { return syscall.EIO }})
	if !served || code != 0 {
		t.Fatalf("the start = %d (served %v), want it served; stderr=%s", code, served, stderr)
	}
	if !strings.Contains(stderr, "serving without the check that no other bridge serve runs") || !strings.Contains(stderr, "rolling back to .bak") {
		t.Errorf("the start did not say it served unlocked and rolled back; stderr=%s", stderr)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, version.ServerVersion) {
		t.Errorf("the binary reports %q, want the backup's %s", got, version.ServerVersion)
	}
}

// TestTheConsoleAnswersEachUpdaterOutcomeInWords: each of backlog B898's
// classes, for an install and for a rollback, is the console's in fixed words
// that name no path, in its precedence (a binary not on disk, then a marker
// not put back, then a hold), and its error is logged whole at Error: through
// mapUpdaterError for every class, and the console's real handler over a
// booted serve's real adapter for the rollback's own, the latch's beside a
// marker that cannot be read among them.
func TestTheConsoleAnswersEachUpdaterOutcomeInWords(t *testing.T) {
	rec := loggingtest.Record(t)
	dir := filepath.Join(t.TempDir(), "Unique-Held-Dir")
	bin := filepath.Join(dir, "bridge")
	outcome := func(script map[string]int) error {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{bin, bin + ".bak"} {
			if err := os.WriteFile(p, []byte(p), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		err := newHeldRestore(bin, script).restore()(bin, ".bak")
		if err == nil {
			t.Fatal("the stand-in's rollback landed")
		}
		return err
	}
	inUse, putBack, missing := outcome(map[string]int{"bak": -1}), outcome(map[string]int{"bak>bin": 1}), outcome(leftNoBinary())
	latch := updater.New(updater.Options{}).Rollback(t.Context(), updater.InstallOptions{DataDir: t.TempDir(),
		BinaryPath: filepath.Join(dir, "not there"), Force: true})
	marker := &updater.MarkerNotRestoredError{Swap: fmt.Errorf("swap: %w", inUse), Marker: errors.New("rename " + dir + ": no space left on device")}
	for _, tc := range []struct {
		name  string
		err   error
		act   updaterAction
		class error
		words string
	}{
		{"an install held", fmt.Errorf("swap: %w", inUse), updaterInstall, admin.ErrUpdateBinaryInUse,
			"Windows reported a file of the update in use by another program, most likely a virus scanner, for 4.75 seconds, so nothing was installed: try again in a minute."},
		{"an install put back", fmt.Errorf("swap: %w", putBack), updaterInstall, admin.ErrUpdateBinaryInUse,
			"Windows reported a file of the update in use by another program, most likely a virus scanner, so the running version was put back and nothing was installed; the backup of the version before it was replaced in the attempt."},
		{"a rollback held", inUse, updaterRollback, admin.ErrUpdateBinaryInUse,
			"Windows reported a file of the rollback in use by another program, most likely a virus scanner, for 4.75 seconds, so nothing was rolled back: try again in a minute."},
		{"a rollback put back", putBack, updaterRollback, admin.ErrUpdateBinaryInUse,
			"Windows reported a file of the rollback in use by another program, most likely a virus scanner, so the running version was put back and nothing was rolled back."},
		{"an install that left no binary", fmt.Errorf("swap: %w", missing), updaterInstall, admin.ErrUpdateBinaryMissing,
			"The bridge's program file is not on disk: the update moved it aside and could not put it back. The bridge goes on running until it stops, but cannot start again until the file is back, and nothing is installed or rolled back until then. The bridge log names the one rename that puts it back."},
		{"a rollback that left no binary", missing, updaterRollback, admin.ErrUpdateBinaryMissing,
			"The bridge's program file is not on disk: the rollback moved it aside and could not put it back. The bridge goes on running until it stops, but cannot start again until the file is back, and nothing is installed or rolled back until then. The bridge log names the one rename that puts it back."},
		{"the latch", latch, updaterRollback, admin.ErrUpdateBinaryMissing,
			"The bridge's program file is not on disk, so nothing is installed or rolled back until it is back: the bridge log names the rename that puts it back."},
		{"a marker not put back after a hold", marker, updaterInstall, admin.ErrUpdateMarkerNotRestored,
			"Nothing was installed and the bridge's program file is as it was, but the install marker could not be put back as it was read: the bridge log says why. Check the data dir's disk; the next start leaves the backup as it is."},
	} {
		mapped := mapUpdaterError(tc.err, tc.act)
		if !errors.Is(mapped, tc.class) || mapped.Error() != tc.words {
			t.Errorf("%s: %q (%v), want %v saying %q", tc.name, mapped, tc.err, tc.class, tc.words)
		}
		logUpdaterFailure(tc.act, tc.err)
		if len(rec.Carrying(tc.err.Error())) == 0 {
			t.Errorf("%s: the error was not logged whole", tc.name)
		}
	}
	for _, msg := range []string{"the console's install failed", "the console's rollback failed"} {
		lines := rec.Lines(msg)
		if len(lines) != 4 {
			t.Errorf("%q logged %d time(s), want 4: %q", msg, len(lines), lines)
		}
		for _, line := range lines {
			if !strings.HasPrefix(line, "ERROR ") {
				t.Errorf("logged below Error: %s", line)
			}
		}
	}

	// Through the console's handler: the rollback's own classes, then the
	// latch with the marker unreadable.
	h := newHeldRestore("", nil)
	b := startHeldConsole(t, h)
	root := filepath.Dir(b.dataDir)
	for _, tc := range []struct {
		name   string
		script map[string]int
		status int
		code   string
	}{
		{"held", map[string]int{"bak": -1}, http.StatusConflict, "binary-in-use"},
		{"put back", map[string]int{"bak>bin": 1}, http.StatusConflict, "binary-in-use"},
		{"no binary left", leftNoBinary(), http.StatusInternalServerError, "recovery-required"},
		{"the latch, the marker unreadable", nil, http.StatusInternalServerError, "recovery-required"},
	} {
		h.mu.Lock()
		h.script = tc.script
		h.mu.Unlock()
		if tc.script == nil {
			if err := os.Remove(updater.StatePath(b.dataDir)); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			anUnreadableMarker(t, b.dataDir)
		}
		status, code, message := postRollback(t, b)
		if status != tc.status || code != tc.code || message == "" || strings.Contains(strings.ToLower(message), strings.ToLower(root)) {
			t.Errorf("%s: POST /api/updates/rollback = %d %s %q, want %d %s naming no path", tc.name, status, code, message, tc.status, tc.code)
		}
	}
	if panel := readRollbackPanel(t, b); panel.CanRollback || !strings.Contains(panel.RollbackRefusal, "not on disk") {
		t.Errorf("the Updates panel with no binary offers Roll back %v, saying %q; want the latch's reason", panel.CanRollback, panel.RollbackRefusal)
	}
}

// startHeldConsole boots serve with its console over a binary and its backup
// and no install marker, the console's rollback put back through h.
func startHeldConsole(t *testing.T, h *heldRestore) *consoleBridge {
	t.Helper()
	return startConsoleBridge(t, "", nil, func(o *serveOpts) {
		bin, _ := layOutRollback(t, filepath.Dir(o.configPath), true)
		o.binaryPath = bin
		h.bin = bin
		o.restoreBackup = h.restore()
	})
}

// TestTheUpdatesPanelHidesRollBackWhileTheBinaryIsMissing: with the backup
// beside a binary that is not at its path, the panel offers no Roll back and
// says the latch's reason, before the marker's.
func TestTheUpdatesPanelHidesRollBackWhileTheBinaryIsMissing(t *testing.T) {
	bin, dataDir := layOutRollback(t, t.TempDir(), true)
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if err := updater.SaveState(dataDir, updater.State{Status: "installing", TargetVersion: "9.9.9",
		PreviousVersion: "0.0.1", DataDirOpenedBy: "9.9.9", AttemptedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	st := updateInfoAdapter{u: updater.New(updater.Options{}), dataDir: dataDir, binaryPath: bin}.Status()
	if st.CanRollback || st.RollbackRefusal != "the bridge's program file is not on disk: the bridge log names the rename that puts it back" {
		t.Errorf("the panel offers Roll back %v, saying %q; want the latch's reason", st.CanRollback, st.RollbackRefusal)
	}
}

// TestBridgeUpdateSaysTheRenameThatPutsTheBinaryBack: `bridge update` prints
// the error whole, and a line for a binary the swap left missing, naming its
// rename, or the latch's where the error names none (here the rollback's
// latch, which a test reaches with no release), and one for a marker not put
// back.
func TestBridgeUpdateSaysTheRenameThatPutsTheBinaryBack(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bridge")
	for _, p := range []string{bin, bin + ".bak"} {
		if err := os.WriteFile(p, []byte(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	missing := newHeldRestore(bin, leftNoBinary()).restore()(bin, ".bak")
	var swapErr *updater.SwapError
	if !errors.As(missing, &swapErr) || swapErr.Recover == "" {
		t.Fatalf("the stand-in = %v, want a put-back that names its rename", missing)
	}
	latch := updater.New(updater.Options{}).Rollback(t.Context(), updater.InstallOptions{DataDir: t.TempDir(), BinaryPath: bin, Force: true})
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("swap: %w", missing), "The bridge's program file is not on disk: " + swapErr.Recover + ", then start the bridge."},
		{latch, "The bridge's program file is not on disk, so nothing is installed until it is back"},
		{&updater.MarkerNotRestoredError{Swap: errBackupDenied, Marker: errBackupDenied},
			"The install marker could not be put back: check the data dir's disk before the bridge next starts."},
	} {
		var out bytes.Buffer
		reportInstallFailure(&out, tc.err, filepath.Join(dir, "bridge.yaml"))
		if !strings.Contains(out.String(), "Install failed: "+tc.err.Error()) || !strings.Contains(out.String(), tc.want) {
			t.Errorf("bridge update said:\n%s\nwant the error whole and %q", out.String(), tc.want)
		}
	}
}

// TestEveryRollbackAtAStartFitsInsideTheMarkersLockWait: what the early, the
// crash and the boot rollbacks hold the install marker's lock for, built from
// the constants (the backup's version's bound, the backup's settings check's,
// a marker write's retries, the forward and the recovery schedules), and each
// row the terms its function makes, run at its worst (the forward deadline
// spent whole, the place refused, the put-back landing at its last attempt),
// the writes and the waits recorded, and the bound each run of the backup is
// asked within where it finds the lock held as it runs (markersLockHeld).
// Every row fits inside the wait every other taker gives the lock: the crash
// rollback asks its backup before it takes the lock (backlog B894, B898's F1,
// whose 51.4 s its row counted until then), so its row counts no bound.
func TestEveryRollbackAtAStartFitsInsideTheMarkersLockWait(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	t.Cleanup(updater.SetMarkerWriteObserverForTest(func(string) {
		mu.Lock()
		writes++
		mu.Unlock()
	}))
	written := func() int {
		mu.Lock()
		defer mu.Unlock()
		return writes
	}
	type run struct {
		writes int
		waited time.Duration
		bounds []time.Duration
	}
	// worst runs one rollback over r's install at its worst, its restore
	// refused as deny says, and returns what it made.
	worst := func(r *refusedInstall, deny string, rollback func(opts serveOpts)) run {
		t.Helper()
		h := newHeldRestore(r.bin, map[string]int{"?bin": 7, "bak>bin": 1, "aside>bin": 6})
		if deny != "" {
			h.denied[deny] = true
		}
		var bounds []time.Duration
		opts := serveOpts{configPath: r.cfg, binaryPath: r.bin, restoreBackup: h.restore(),
			probeVersion: func(_ context.Context, _ string, within time.Duration) (string, error) {
				if markersLockHeld(t, r.dataDir) {
					bounds = append(bounds, within)
				}
				return r.marker.PreviousVersion, nil
			},
			// The check takes no bound: it is the check's own.
			checkConfig: func(context.Context, string, string) updater.ConfigCheck {
				if markersLockHeld(t, r.dataDir) {
					bounds = append(bounds, updater.ConfigCheckBound())
				}
				return updater.ConfigCheck{Verdict: updater.ConfigValid}
			}}
		before := written()
		rollback(opts)
		return run{written() - before, waitsTotal(h.waited()), bounds}
	}
	runs := map[string]run{}

	r := newRefusedInstall(t, "")
	runs["early"] = worst(r, "", func(opts serveOpts) {
		if code, acted := recoverRefusedInstall(t.Context(), r.cfg, []string{"line 7: refused"}, opts, &bytes.Buffer{}); acted || code != 0 {
			t.Fatalf("the early rollback = %d (acted %v), want it put back at its worst", code, acted)
		}
	})

	r = newRefusedInstall(t, "")
	r.marker.FailedStarts, r.marker.DataDirOpenedBy = updater.ProbationFailedStarts, version.ServerVersion
	r.saveMarker(t, r.dataDir)
	runs["crash"] = worst(r, "", func(opts serveOpts) {
		if code, acted := recoverCrashedInstall(t.Context(), r.dataDir, r.cfg, opts, crashRollbackGoesOn, &bytes.Buffer{}); acted || code != 0 {
			t.Fatalf("the crash rollback = %d (acted %v), want it put back at its worst", code, acted)
		}
	})

	// A place refused for a hold keeps the boot's marker (one write); one
	// refused otherwise clears the install too.
	r = newRefusedInstall(t, "")
	r.marker.TargetVersion = "9.9.9"
	r.saveMarker(t, r.dataDir)
	runs["boot"] = worst(r, "bak>bin", func(opts serveOpts) {
		var stderr bytes.Buffer
		if err := bootHousekeepingWith(t.Context(), &stderr, r.dataDir, r.bin, previousVersion,
			bootSeams{restore: opts.restoreBackup, probeVersion: opts.probeVersion, probeBound: backupProbeBound(opts)}); err != nil {
			t.Fatalf("the boot rollback = %v; stderr=%s", err, stderr.String())
		}
	})

	write, waits := atomicwrite.RenameRetryBudget(), updater.ForwardWait()+updater.RecoveryWait()
	for _, row := range []struct {
		name string
		want run
		past bool
	}{
		{"early", run{2, waits, []time.Duration{updater.VersionProbeTimeout}}, false},
		{"crash", run{2, waits, nil}, false},
		{"boot", run{2, waits, []time.Duration{updater.VersionProbeTimeout}}, false},
	} {
		got := runs[row.name]
		if got.writes != row.want.writes || got.waited != row.want.waited || !slices.Equal(got.bounds, row.want.bounds) {
			t.Errorf("the %s rollback made %d writes, waited %s and asked within %v; its row counts %d, %s and %v",
				row.name, got.writes, got.waited, got.bounds, row.want.writes, row.want.waited, row.want.bounds)
		}
		hold := time.Duration(row.want.writes)*write + row.want.waited
		for _, b := range row.want.bounds {
			hold += b
		}
		if past := hold > updater.StateLockWait; past != row.past {
			t.Errorf("the %s rollback holds the marker's lock up to %s against the %s every other taker waits: past it %v, want %v",
				row.name, hold, updater.StateLockWait, past, row.past)
		}
	}
}

// waitsTotal is what ws waits in all.
func waitsTotal(ws []time.Duration) time.Duration {
	var total time.Duration
	for _, d := range ws {
		total += d
	}
	return total
}
