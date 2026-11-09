package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The console's save that turns update checks off (backlog B761's round 3):
// the install marker's lock it takes before anything else (LockForChecksOff),
// and its decision under that lock whether the off may be saved while an
// install is on probation (ChecksOffRefused).

// requireNoPathIn requires err's words to name none of paths.
func requireNoPathIn(t *testing.T, err error, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if strings.Contains(err.Error(), p) {
			t.Errorf("the refusal names a path, %q: %v", p, err)
		}
	}
}

// TestTheOffSaveHoldsTheInstallMarkersLock: LockForChecksOff answers a hold
// of the install marker's lock, which keeps every other taker out until it is
// let go; a holder past the wait, every try answering ENOLCK, and any other
// failure of the lock are a *ChecksOffLockError (ErrChecksOffLocked) whose
// words say nothing was saved and name no path; a filesystem that keeps no
// locks answers a hold that is not held and no error, so the save goes on; a
// stop in the wait is the context's error, never a refusal of the lock's; and
// a context that ended before an uncontended take answers its error with the
// lock let go.
func TestTheOffSaveHoldsTheInstallMarkersLock(t *testing.T) {
	shortStateLockWait(t, 200*time.Millisecond)
	t.Run("a free lock is held until it is let go", func(t *testing.T) {
		dir := t.TempDir()
		lock, err := LockForChecksOff(context.Background(), dir)
		if err != nil || !lock.Held() {
			t.Fatalf("LockForChecksOff = %+v, %v; want the lock held", lock, err)
		}
		if other, err := LockState(context.Background(), dir); !errors.Is(err, ErrStateLockHeld) {
			other.Release()
			t.Errorf("another taker while the off's save holds the lock = %v, want ErrStateLockHeld", err)
		}
		lock.Release()
		other, err := LockState(context.Background(), dir)
		if err != nil || !other.Held() {
			t.Errorf("another taker once the off's save let the lock go = %+v, %v; want the lock", other, err)
		}
		other.Release()
	})
	t.Run("another holder past the wait", func(t *testing.T) {
		dir := t.TempDir()
		holder, err := LockState(context.Background(), dir)
		if err != nil || !holder.Held() {
			t.Fatalf("hold the lock: %+v, %v", holder, err)
		}
		defer holder.Release()
		lock, err := LockForChecksOff(context.Background(), dir)
		if lock != nil || !errors.Is(err, ErrChecksOffLocked) || !errors.Is(err, ErrStateLockHeld) {
			t.Fatalf("LockForChecksOff behind a holder past the wait = %+v, %v; want ErrChecksOffLocked, held", lock, err)
		}
		for _, says := range []string{"update checks were not turned off", "another process held the install marker's lock",
			"an install cannot read the checks on and then install", "nothing was saved"} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("the refusal does not say %q: %v", says, err)
			}
		}
		requireNoPathIn(t, err, dir)
	})
	for _, tc := range []struct {
		name    string
		answer  error
		refused bool
	}{
		{"no lock to give", syscall.ENOLCK, true},
		{"the lock fails", syscall.EIO, true},
		{"a filesystem that keeps none", syscall.EOPNOTSUPP, false},
		{"a platform with none", errors.ErrUnsupported, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubStateTryLock(t, tc.answer)
			dir := t.TempDir()
			lock, err := LockForChecksOff(context.Background(), dir)
			if !tc.refused {
				if err != nil || lock == nil || lock.Held() || lock.Unheld() == nil {
					t.Fatalf("LockForChecksOff = %+v, %v; want a hold that is not held, saying why, and no error", lock, err)
				}
				lock.Release()
				return
			}
			if lock != nil || !errors.Is(err, ErrChecksOffLocked) || errors.Is(err, ErrStateLockHeld) {
				t.Fatalf("LockForChecksOff = %+v, %v; want ErrChecksOffLocked, not held by another", lock, err)
			}
			if !strings.Contains(err.Error(), "update checks were not turned off") || !strings.Contains(err.Error(), "nothing was saved") {
				t.Errorf("the refusal does not say the off was not saved: %v", err)
			}
			requireNoPathIn(t, err, dir)
		})
	}
	t.Run("a stop in the wait", func(t *testing.T) {
		dir := t.TempDir()
		shortStateLockWait(t, testWait)
		holder, err := LockState(context.Background(), dir)
		if err != nil || !holder.Held() {
			t.Fatalf("hold the lock: %+v, %v", holder, err)
		}
		defer holder.Release()
		waiting := make(chan struct{})
		var once sync.Once
		t.Cleanup(SetStateLockWaitingForTest(func(string) { once.Do(func() { close(waiting) }) }))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := make(chan error, 1)
		go func() {
			lock, err := LockForChecksOff(ctx, dir)
			lock.Release()
			got <- err
		}()
		select {
		case <-waiting:
		case err := <-got:
			t.Fatalf("the off's save answered %v while another held the lock, without waiting for it", err)
		case <-time.After(testWait):
			t.Fatal("the off's save never waited for the held lock")
		}
		cancel()
		select {
		case err := <-got:
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrChecksOffLocked) {
				t.Errorf("LockForChecksOff stopped in its wait = %v, want the context's error and no refusal of the lock's", err)
			}
		case <-time.After(testWait):
			t.Fatal("the off's save stopped in its wait never answered")
		}
	})
	t.Run("a context that ended before an uncontended take", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		lock, err := LockForChecksOff(ctx, dir)
		if lock != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("LockForChecksOff on a context that had ended = %+v, %v; want the context's error", lock, err)
		}
		other, err := LockState(context.Background(), dir)
		if err != nil || !other.Held() {
			t.Errorf("the lock after the refused take = %+v, %v; want it let go", other, err)
		}
		other.Release()
	})
}

// offCase is an install of 0.2.4 over 0.2.3 as the off's decision finds it
// (probationMarker's): its install marker in dir unless none is given, the
// binary serve runs as and its backup beside it, and the settings, in a
// directory of their own.
type offCase struct {
	dir, bin, cfg string
}

// offSettings are the settings an offCase's file holds.
const offSettings = "libraryName: Hi-Fi\nupdate:\n  autoInstall: true\n"

func newOffCase(t *testing.T, st *State) offCase {
	t.Helper()
	dir := t.TempDir()
	c := offCase{dir: dir, bin: filepath.Join(dir, "bridge"), cfg: filepath.Join(dir, "conf", "bridge.yaml")}
	if err := os.MkdirAll(filepath.Dir(c.cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{c.cfg: offSettings, c.bin: "bridge-binary-0.2.4", c.bin + ".bak": "bridge-binary-0.2.3"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if st != nil {
		if err := SaveState(dir, *st); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// decide is the off's decision over c, as a start of 0.2.4 makes it, the
// backup's checks answered by b.
func (c offCase) decide(ctx context.Context, b *backupAnswers, within time.Duration) error {
	return ChecksOffRefused(ctx, ChecksOffSave{DataDir: c.dir, BinaryPath: c.bin, ConfigPath: c.cfg, Current: "0.2.4",
		Within: within, check: b.check, probe: b.probe})
}

// backupAnswers stands in for the backup's two checks the off's decision asks
// (ChecksOffSave's check and probe): its settings check answers asIs for the
// settings file itself and withKey for any other (the copy with the key
// written off), and its probe version, or probeErr. It records what each was
// asked, in order, and where the copy it read stood and what it held.
type backupAnswers struct {
	cfg           string
	withKey, asIs ConfigCheck
	version       string
	probeErr      error
	// onCheck, when set, runs at the check of the copy, with its context,
	// before it answers.
	onCheck func(ctx context.Context)

	mu       sync.Mutex
	asked    []string
	copyPath string
	copyText string
}

func (b *backupAnswers) check(ctx context.Context, _, configPath string) ConfigCheck {
	if configPath == b.cfg {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.asked = append(b.asked, "the settings as they are")
		return b.asIs
	}
	raw, _ := os.ReadFile(configPath)
	b.mu.Lock()
	b.asked = append(b.asked, "the settings with the key")
	b.copyPath, b.copyText = configPath, string(raw)
	b.mu.Unlock()
	if b.onCheck != nil {
		b.onCheck(ctx)
	}
	return b.withKey
}

func (b *backupAnswers) probe(context.Context, string, time.Duration) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, "its version")
	return b.version, b.probeErr
}

func (b *backupAnswers) askedOf() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.asked, ", ")
}

// refusesTheKey is a backup of 0.2.3 from before the switch, as it answers
// c's checks: it refuses the settings with the key and takes them as they
// are.
func refusesTheKey(c offCase) *backupAnswers {
	return &backupAnswers{cfg: c.cfg, withKey: ConfigCheck{Verdict: ConfigRefused,
		Reasons: []string{"line 4: field checkEnabled not found in type config.UpdateConfig"}},
		asIs: ConfigCheck{Verdict: ConfigValid}, version: "0.2.3"}
}

// requireAsIs requires the settings file and the install marker in c as they
// were before the decision, settings and marker.
func requireAsIs(t *testing.T, c offCase, marker []byte) {
	t.Helper()
	if got, err := os.ReadFile(c.cfg); err != nil || string(got) != offSettings {
		t.Errorf("the settings after the decision = %q (%v), want them as they were", got, err)
	}
	got, err := os.ReadFile(StatePath(c.dir))
	if marker == nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("an install marker after the decision (%v), want none written", err)
		}
		return
	}
	if err != nil || !bytes.Equal(got, marker) {
		t.Errorf("the install marker after the decision = %s (%v), want it as it was: %s", got, err, marker)
	}
}

// markerBytes is c's install marker as it is now, nil where there is none.
func markerBytes(t *testing.T, c offCase) []byte {
	t.Helper()
	raw, err := os.ReadFile(StatePath(c.dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAnOffIsRefusedWhileACrashRollbackWouldPutBackABackupThatRefusesTheKey:
// an install of 0.2.4 over 0.2.3 is on probation, whatever its count, or its
// crash rollback's record stands with the rename not landed; the backup, of
// 0.2.3, refuses settings that write update.checkEnabled and takes them as
// they are. An off saved now would keep that rollback from putting it back
// (its settings check is the crash rollback's third), so the decision refuses
// it (*ChecksOffRefusal), having asked the backup as the crash rollback does:
// its settings check of a copy of the settings with the key written false,
// beside the settings and removed after, then its version, then the settings
// as they are. The words say when the off can be saved and how to turn the
// checks off without the key, and name no path; nothing is written.
func TestAnOffIsRefusedWhileACrashRollbackWouldPutBackABackupThatRefusesTheKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   State
	}{
		{"no start of it has failed", probationMarker(0)},
		{"a start of it did not end cleanly", probationMarker(1)},
		{"its failed starts at the limit", probationMarker(ProbationFailedStarts)},
		{"the crash rollback's record, its rename not landed", CrashRollbackRecord(probationMarker(ProbationFailedStarts), "0.2.4")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newOffCase(t, &tc.st)
			marker := markerBytes(t, c)
			b := refusesTheKey(c)
			err := c.decide(context.Background(), b, 0)
			var refusal *ChecksOffRefusal
			if !errors.As(err, &refusal) || !errors.Is(err, ErrChecksOffKeepsTheBackup) || refusal.Target != "0.2.4" || refusal.Previous != "0.2.3" {
				t.Fatalf("the off's decision = %v, want a *ChecksOffRefusal of 0.2.4 over 0.2.3", err)
			}
			if got, want := b.askedOf(), "the settings with the key, its version, the settings as they are"; got != want {
				t.Errorf("the backup was asked %q, want %q", got, want)
			}
			if filepath.Dir(b.copyPath) != filepath.Dir(c.cfg) || b.copyPath == c.cfg {
				t.Errorf("the settings with the key were read at %s, want a copy beside %s", b.copyPath, c.cfg)
			}
			for _, holds := range []string{"libraryName: Hi-Fi", "autoInstall: true", "checkEnabled: false"} {
				if !strings.Contains(b.copyText, holds) {
					t.Errorf("the copy the backup's check read does not hold %q:\n%s", holds, b.copyText)
				}
			}
			if _, err := os.Lstat(b.copyPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the copy is still beside the settings once the decision is made (lstat: %v)", err)
			}
			for _, says := range []string{"update checks were not turned off", "0.2.4 is on probation", "its backup, 0.2.3",
				"keep a crash rollback from putting 0.2.3 back", "once 0.2.4 is confirmed", "BRIDGE_UPDATE_CHECK_ENABLED=false",
				"restart the service", "0.2.3 does not read that variable", "nothing was saved"} {
				if !strings.Contains(err.Error(), says) {
					t.Errorf("the refusal does not say %q: %v", says, err)
				}
			}
			requireNoPathIn(t, err, c.dir)
			requireAsIs(t, c, marker)
		})
	}
}

// TestAnOffIsSavedWhereNoCrashRollbackWouldPutBackABackupThatRefusesTheKey:
// where the boot's own checks rule the crash rollback out (no marker, one
// that does not read, the install confirmed or another version's, a migration
// of the store begun, no swap, another version over the data dir, nothing to
// go back to, or no regular file at the backup's place), the off takes
// nothing from it and the backup is asked nothing; and where the backup
// takes the key, predates the settings check, is of another version, or
// refuses the settings as they are already, the crash rollback refuses or
// acts whatever the off, which is then saved. Nothing is written either way.
func TestAnOffIsSavedWhereNoCrashRollbackWouldPutBackABackupThatRefusesTheKey(t *testing.T) {
	with := func(f func(*State)) *State {
		st := probationMarker(1)
		f(&st)
		return &st
	}
	for _, tc := range []struct {
		name  string
		st    *State
		setup func(t *testing.T, c offCase)
	}{
		{name: "no install marker"},
		{name: "a marker that does not read", setup: func(t *testing.T, c offCase) {
			if err := os.WriteFile(StatePath(c.dir), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the install confirmed", st: with(func(st *State) { st.Status, st.InstalledAt = "installed", time.Now().UTC() })},
		{name: "an install of another version", st: with(func(st *State) { st.TargetVersion = "0.2.5" })},
		{name: "a migration of the store begun", st: with(func(st *State) { st.StoreMigratedBy = "0.2.4" })},
		{name: "the install never reached its swap", st: with(func(st *State) { st.SwapStarted = false })},
		{name: "another version opened the data dir", st: with(func(st *State) { st.DataDirOpenedBy = "0.2.3" })},
		{name: "no version to go back to", st: with(func(st *State) { st.PreviousVersion = "" })},
		{name: "no backup", st: with(func(*State) {}), setup: func(t *testing.T, c offCase) {
			if err := os.Remove(c.bin + ".bak"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a directory at the backup's place", st: with(func(*State) {}), setup: func(t *testing.T, c offCase) {
			if err := os.Remove(c.bin + ".bak"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(c.bin+".bak", 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newOffCase(t, tc.st)
			if tc.setup != nil {
				tc.setup(t, c)
			}
			marker := markerBytes(t, c)
			b := refusesTheKey(c)
			if err := c.decide(context.Background(), b, 0); err != nil {
				t.Errorf("the off's decision = %v, want the off saved", err)
			}
			if got := b.askedOf(); got != "" {
				t.Errorf("the backup was asked %q, want nothing", got)
			}
			requireAsIs(t, c, marker)
		})
	}
	for _, tc := range []struct {
		name  string
		edit  func(b *backupAnswers)
		asked string
	}{
		{"the backup takes the key", func(b *backupAnswers) { b.withKey = ConfigCheck{Verdict: ConfigValid} },
			"the settings with the key"},
		{"the backup predates the settings check", func(b *backupAnswers) { b.withKey = ConfigCheck{Verdict: ConfigCheckUnsupported} },
			"the settings with the key"},
		{"the backup is of another version", func(b *backupAnswers) { b.version = "0.2.2" },
			"the settings with the key, its version"},
		{"the backup refuses the settings as they are", func(b *backupAnswers) {
			b.asIs = ConfigCheck{Verdict: ConfigRefused, Reasons: []string{"line 1: a setting this version does not take"}}
		}, "the settings with the key, its version, the settings as they are"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := probationMarker(1)
			c := newOffCase(t, &st)
			marker := markerBytes(t, c)
			b := refusesTheKey(c)
			tc.edit(b)
			if err := c.decide(context.Background(), b, 0); err != nil {
				t.Errorf("the off's decision = %v, want the off saved", err)
			}
			if got := b.askedOf(); got != tc.asked {
				t.Errorf("the backup was asked %q, want %q", got, tc.asked)
			}
			requireAsIs(t, c, marker)
		})
	}
}

// TestAnOffWhoseBackupDoesNotAnswerIsRefused: the decision fails closed. A
// backup whose check of either settings does not answer, or whose version
// does not, and settings that cannot be read, cannot take the key or whose
// copy cannot be made, refuse the off as one that could not be told
// (*ChecksOffUncheckedError), its Detail for the log saying what failed and
// its words naming no path and saying how to turn the checks off without the
// key; nothing is written.
func TestAnOffWhoseBackupDoesNotAnswerIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, c offCase, b *backupAnswers)
		detail string
	}{
		{"its check of the off does not answer", func(_ *testing.T, _ offCase, b *backupAnswers) {
			b.withKey = ConfigCheck{Verdict: ConfigCheckFailed, Detail: "it could not be run: exec format error"}
		}, "the backup's settings check of the off did not answer: it could not be run"},
		{"it does not say its version", func(_ *testing.T, _ offCase, b *backupAnswers) {
			b.probeErr = errors.New("it did not answer within 10s")
		}, "the backup did not say its version: it did not answer within 10s"},
		{"its check of the settings as they are does not answer", func(_ *testing.T, _ offCase, b *backupAnswers) {
			b.asIs = ConfigCheck{Verdict: ConfigCheckFailed, Detail: "it did not answer within 30s"}
		}, "the backup's settings check of the settings as they are did not answer: it did not answer within 30s"},
		{"the settings do not read", func(t *testing.T, c offCase, _ *backupAnswers) {
			if err := os.Remove(c.cfg); err != nil {
				t.Fatal(err)
			}
		}, "the settings could not be read"},
		{"the settings cannot take the key", func(t *testing.T, c offCase, _ *backupAnswers) {
			if err := os.WriteFile(c.cfg, []byte("- a list\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "the settings could not be written with the key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := probationMarker(1)
			c := newOffCase(t, &st)
			b := refusesTheKey(c)
			tc.setup(t, c, b)
			err := c.decide(context.Background(), b, 0)
			requireUnchecked(t, err, c, tc.detail)
		})
	}
	t.Run("the copy cannot be made", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("a directory this user cannot write needs POSIX modes and a user that is not root")
		}
		st := probationMarker(1)
		c := newOffCase(t, &st)
		conf := filepath.Dir(c.cfg)
		if err := os.Chmod(conf, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(conf, 0o755) })
		b := refusesTheKey(c)
		err := c.decide(context.Background(), b, 0)
		requireUnchecked(t, err, c, "the copy of the settings could not be made")
		if got := b.askedOf(); got != "" {
			t.Errorf("the backup was asked %q with no copy to read, want nothing", got)
		}
	})
}

// requireUnchecked requires err to be the decision's refusal of an off it
// could not tell, its Detail holding detail.
func requireUnchecked(t *testing.T, err error, c offCase, detail string) {
	t.Helper()
	var unchecked *ChecksOffUncheckedError
	if !errors.As(err, &unchecked) || !errors.Is(err, ErrChecksOffUnchecked) || errors.Is(err, ErrChecksOffKeepsTheBackup) {
		t.Fatalf("the off's decision = %v, want a *ChecksOffUncheckedError", err)
	}
	if unchecked.Target != "0.2.4" || unchecked.Previous != "0.2.3" || !strings.Contains(unchecked.Detail, detail) {
		t.Errorf("the refusal = %+v, want 0.2.4 over 0.2.3, its detail holding %q", unchecked, detail)
	}
	for _, says := range []string{"update checks were not turned off", "did not answer whether it takes settings that write update.checkEnabled",
		"nothing was saved", "BRIDGE_UPDATE_CHECK_ENABLED=false", "restart the service"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	if strings.Contains(err.Error(), unchecked.Detail) {
		t.Errorf("the refusal's words carry its detail, which is the log's: %v", err)
	}
	requireNoPathIn(t, err, c.dir)
}

// TestTheOffsDecisionKeepsItsBound: the backup's checks run within the
// decision's bound together. A check that answers nothing is stopped at the
// bound and the off refused as one that could not be told, within the bound
// and its slack; one that answers only as the bound ends leaves its version
// unasked, refused the same; and the request's stop ends the decision with
// its error, never a refusal of the backup's.
func TestTheOffsDecisionKeepsItsBound(t *testing.T) {
	const within = 300 * time.Millisecond
	t.Run("a check that answers nothing", func(t *testing.T) {
		st := probationMarker(1)
		c := newOffCase(t, &st)
		b := refusesTheKey(c)
		b.onCheck = func(ctx context.Context) {
			<-ctx.Done()
			b.mu.Lock()
			b.withKey = ConfigCheck{Verdict: ConfigCheckFailed, Detail: "the check was stopped", Err: ctx.Err()}
			b.mu.Unlock()
		}
		start := time.Now()
		err := c.decide(context.Background(), b, within)
		if took := time.Since(start); took > within+10*time.Second {
			t.Errorf("the decision took %s, want it within its bound of %s", took, within)
		}
		requireUnchecked(t, err, c, "the check was stopped")
	})
	t.Run("a check that answers as the bound ends", func(t *testing.T) {
		st := probationMarker(1)
		c := newOffCase(t, &st)
		b := refusesTheKey(c)
		b.onCheck = func(ctx context.Context) { <-ctx.Done() }
		err := c.decide(context.Background(), b, within)
		requireUnchecked(t, err, c, "the backup's checks ran past their bound")
		if got := b.askedOf(); got != "the settings with the key" {
			t.Errorf("the backup was asked %q, want its check of the off alone", got)
		}
	})
	t.Run("a stop of the request", func(t *testing.T) {
		st := probationMarker(1)
		c := newOffCase(t, &st)
		b := refusesTheKey(c)
		ctx, cancel := context.WithCancel(context.Background())
		b.onCheck = func(cctx context.Context) {
			cancel()
			<-cctx.Done()
			b.mu.Lock()
			b.withKey = ConfigCheck{Verdict: ConfigCheckFailed, Detail: "the check was stopped", Err: cctx.Err()}
			b.mu.Unlock()
		}
		err := c.decide(ctx, b, time.Minute)
		var unchecked *ChecksOffUncheckedError
		if !errors.Is(err, context.Canceled) || errors.As(err, &unchecked) {
			t.Errorf("the decision the request's stop ended = %v, want the context's error", err)
		}
	})
}

// TestTheOffsDecisionAsksTheBackupItself: the backup is a release (the test
// binary's stand-in) whose strict decoder refuses settings that write
// update.checkEnabled, asked as the crash rollback asks it, through
// CheckConfig and ProbeVersionWithin: of 0.2.3, the version the install went
// over, the off is refused; of 0.2.2 it is saved; and a backup that takes the
// key lets it be saved. Its checks of the off read a copy beside the
// settings, gone once the decision is made.
func TestTheOffsDecisionAsksTheBackupItself(t *testing.T) {
	for _, tc := range []struct {
		name, check, version string
		refused              bool
	}{
		{"a backup of the version it went over refuses the key", "refuse-key", "0.2.3", true},
		{"a backup of another version", "refuse-key", "0.2.2", false},
		{"a backup that takes the key", "valid", "0.2.3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := probationMarker(1)
			c := newOffCase(t, &st)
			if err := os.Remove(c.bin + ".bak"); err != nil {
				t.Fatal(err)
			}
			installCandidateCopy(t, c.bin+".bak")
			script := candidateStandIn(t, map[string]string{"check": tc.check, "version": tc.version})
			err := ChecksOffRefused(context.Background(), ChecksOffSave{DataDir: c.dir, BinaryPath: c.bin, ConfigPath: c.cfg,
				Current: "0.2.4"})
			if tc.refused != errors.Is(err, ErrChecksOffKeepsTheBackup) || (!tc.refused && err != nil) {
				t.Fatalf("the off's decision = %v, want refused %v", err, tc.refused)
			}
			calls := candidateCalls(t, script)
			if len(calls) == 0 || !strings.HasPrefix(calls[0], "serve --"+ConfigCheckFlag+" --config ") {
				t.Fatalf("the backup's calls = %q, want its settings check of the off first", calls)
			}
			copyPath := strings.TrimPrefix(calls[0], "serve --"+ConfigCheckFlag+" --config ")
			if filepath.Dir(copyPath) != filepath.Dir(c.cfg) || copyPath == c.cfg {
				t.Errorf("the backup's check of the off read %s, want a copy beside %s", copyPath, c.cfg)
			}
			if _, err := os.Lstat(copyPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the copy is still beside the settings once the decision is made (lstat: %v)", err)
			}
			if tc.refused && (len(calls) != 3 || calls[1] != "version" || calls[2] != "serve --"+ConfigCheckFlag+" --config "+c.cfg) {
				t.Errorf("the backup's calls = %q, want the off's check, its version and the settings' check", calls)
			}
		})
	}
}
