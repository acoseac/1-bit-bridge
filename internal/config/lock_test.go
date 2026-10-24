package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// holdLock takes the lock beside the config file at path through an open of
// its own, as another writer's process would, and returns what gives it
// back. A test may give it back from a goroutine of its own, beside the
// cleanup, and the kernel lock that orders the two is nothing the race
// detector sees: hence the Once.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := fsutil.OpenLockFile(filepath.Dir(path), LockFileName(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		t.Fatalf("take the lock: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = fsutil.Unlock(f)
			_ = f.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// writeConfigFile writes text as the config file at dir/bridge.yaml and
// returns its path.
func writeConfigFile(t *testing.T, dir, text string) string {
	t.Helper()
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// realTempDir is t.TempDir() as the OS resolves it (on macOS the temp
// directory is reached through the link /var → /private/var): a turn names
// the files it writes that way, and so do a test's expectations of them.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// loadableConfig writes a config that loads (a library root that exists, a
// data dir) with libraryName name to dir/bridge.yaml, and returns its path
// and the text before the name, for a test that writes it again.
func loadableConfig(t *testing.T, dir, name string) (path, base string) {
	t.Helper()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	base = "libraryRoots: [" + yamlStr(lib) + "]\ndataDir: " + yamlStr(filepath.Join(dir, "data")) + "\n"
	return writeConfigFile(t, dir, base+"libraryName: "+name+"\n"), base
}

// writeMeanwhile writes text to path, as a writer that takes no lock (an
// edit, or a bridge older than the lock) does, failing the test on an error.
func writeMeanwhile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Error(err)
	}
}

// TestTheLockIsAFileBesideTheConfig: the lock of dir/bridge.yaml is taken on
// dir/.bridge.yaml.lock, hidden and never the config file itself, which Save
// replaces by a rename; while a turn holds it another open of that file is
// refused, and once the turn gives it back the lock is free.
func TestTheLockIsAFileBesideTheConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	if got := LockFileName(path); got != ".bridge.yaml.lock" {
		t.Fatalf("LockFileName = %q, want .bridge.yaml.lock", got)
	}
	if got := LockFileName(filepath.Join(dir, "other.yaml")); got != ".other.yaml.lock" {
		t.Errorf("LockFileName of another config = %q, want .other.yaml.lock", got)
	}
	l, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Path() != path || l.Unheld() != nil {
		t.Fatalf("the turn is at %q, unheld %v; want %q, held", l.Path(), l.Unheld(), path)
	}
	other, err := fsutil.OpenLockFile(dir, ".bridge.yaml.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := fsutil.TryLock(other); !errors.Is(err, fsutil.ErrLocked) {
		t.Errorf("another open of the lock file while the turn holds it = %v, want ErrLocked", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("taking the lock made the config file itself: %v", err)
	}
	l.Unlock()
	l.Unlock() // a second give-back does nothing
	if err := fsutil.TryLock(other); err != nil {
		t.Errorf("the lock once the turn gave it back = %v, want free", err)
	}
}

// TestALockAnotherHoldsIsWaitedForThenRefused: a turn another writer holds
// is waited for, asked again until the wait is over, and then refused with a
// *BusyError that says another bridge command or the running bridge is
// changing the file, naming the file and nothing it holds; nothing is taken.
func TestALockAnotherHoldsIsWaitedForThenRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfigFile(t, dir, "libraryName: Secret-Name-Not-For-Logs\n")
	holdLock(t, path)
	const wait = 300 * time.Millisecond
	tries := 0
	start := time.Now()
	l, err := lockFile(path, func(f *os.File) error { tries++; return fsutil.TryLock(f) }, wait)
	took := time.Since(start)
	if err == nil {
		l.Unlock()
		t.Fatal("a held lock was taken")
	}
	var busy *BusyError
	if !errors.As(err, &busy) || !errors.Is(err, ErrBusy) || busy.Path != path || busy.Wait != wait {
		t.Fatalf("refusal = %#v (%v), want a *BusyError for %s after %s", err, err, path, wait)
	}
	if took < wait {
		t.Errorf("refused after %s, before the wait of %s was over", took, wait)
	}
	if tries < 3 {
		t.Errorf("asked %d times in %s, want it asked again while it waited", tries, took)
	}
	msg := err.Error()
	if !strings.Contains(msg, "another bridge command or the running bridge is changing "+path) ||
		!strings.Contains(msg, "nothing was written") || strings.Contains(msg, "Secret-Name") {
		t.Errorf("refusal %q: want it to name the file and say nothing was written, and no value of the file", msg)
	}
	if LockWait != 5*time.Second {
		t.Errorf("LockWait = %s, want 5s (the bound the rules and the writers' tests say)", LockWait)
	}
}

// TestALockFreedWhileItWaitsIsTaken: a writer that finds the lock held
// takes it as soon as the other writer gives it back, well inside the wait.
func TestALockFreedWhileItWaitsIsTaken(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, "libraryName: Before\n")
	release := holdLock(t, path)
	const held = 200 * time.Millisecond
	go func() {
		time.Sleep(held)
		release()
	}()
	start := time.Now()
	l, err := LockFile(path)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("a lock given back after %s was refused: %v", held, err)
	}
	defer l.Unlock()
	if took < held || took >= LockWait {
		t.Errorf("took the lock after %s; want after the %s it was held and before the wait of %s", took, held, LockWait)
	}
}

// TestAFilesystemThatKeepsNoLocksIsNoRefusal: where no lock can be taken at
// all (a filesystem that keeps none answers ENOLCK or EOPNOTSUPP), the turn
// is taken without it and a save goes through, said once per file in the
// log, and not again for the next turn.
func TestAFilesystemThatKeepsNoLocksIsNoRefusal(t *testing.T) {
	rec := loggingtest.Record(t)
	dir := t.TempDir()
	path := writeConfigFile(t, dir, "libraryName: Before\n")
	noLocks := &os.PathError{Op: "flock", Path: filepath.Join(dir, LockFileName(path)), Err: errors.ErrUnsupported}
	for range 2 {
		l, err := lockFile(path, func(*os.File) error { return noLocks }, time.Second)
		if err != nil {
			t.Fatalf("a filesystem with no locks refused the turn: %v", err)
		}
		if !errors.Is(l.Unheld(), noLocks) {
			t.Errorf("Unheld = %v, want why no lock is held", l.Unheld())
		}
		if err := (&Config{LibraryName: "After"}).Save(l); err != nil {
			t.Fatalf("a save in a turn with no lock: %v", err)
		}
		l.Unlock()
	}
	const msg = "config: no lock can be taken beside the config file, so its writers do not take turns there"
	if lines := rec.Lines(msg); len(lines) != 1 || !strings.Contains(lines[0], path) {
		t.Errorf("warned %q, want one line naming the file", lines)
	}
}

// TestASaveNeedsATurnItHolds: Save and SaveIf write only in a turn LockFile
// gave and Unlock has not given back.
func TestASaveNeedsATurnItHolds(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, "libraryName: Before\n")
	cfg := &Config{LibraryName: "After"}
	if err := cfg.Save(nil); err == nil {
		t.Error("a save with no turn went through")
	}
	l, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Unlock()
	if err := cfg.Save(l); err == nil || !strings.Contains(err.Error(), "after its lock was given back") {
		t.Errorf("a save after the turn was given back = %v, want a refusal", err)
	}
	if err := cfg.SaveIf(l, func(context.Context) error { return nil }); err == nil {
		t.Error("a SaveIf after the turn was given back went through")
	}
	if got, _ := os.ReadFile(path); string(got) != "libraryName: Before\n" {
		t.Errorf("the refused saves wrote the file: %q", got)
	}
}

// TestEditReadsTheFileInItsTurn: Edit loads the file once its turn is taken,
// so what another writer saved while Edit waited for the lock is in the copy
// fn changes and in what Edit saves; fn's answer not to save, and its error,
// write nothing.
func TestEditReadsTheFileInItsTurn(t *testing.T) {
	dir := t.TempDir()
	path, base := loadableConfig(t, dir, "Before")
	release := holdLock(t, path)
	waited, stop := LockWaitedForTest(path)
	defer stop()
	done := make(chan error, 1)
	var seen string
	go func() {
		done <- Edit(path, func(cfg *Config) (bool, error) {
			seen = cfg.LibraryName
			cfg.ScanIntervalSec = 7200
			return true, nil
		})
	}()
	// The other writer's save, in its own turn, once Edit is waiting for it:
	// observed, never guessed at with a sleep, so an Edit that read the file
	// before its turn has read it by then.
	select {
	case <-waited:
	case err := <-done:
		t.Fatalf("Edit returned while another writer held the lock, never waiting for it: %v", err)
	}
	if err := os.WriteFile(path, []byte(base+"libraryName: Saved Meanwhile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if seen != "Saved Meanwhile" {
		t.Errorf("Edit's fn saw libraryName %q, want the file as it was once Edit held the lock", seen)
	}
	after, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.LibraryName != "Saved Meanwhile" || after.ScanIntervalSec != 7200 {
		t.Errorf("after Edit: libraryName %q, scanIntervalSec %d; want the other writer's name and Edit's interval",
			after.LibraryName, after.ScanIntervalSec)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Edit(path, func(cfg *Config) (bool, error) { cfg.LibraryName = "Not Saved"; return false, nil }); err != nil {
		t.Fatalf("an Edit that saves nothing: %v", err)
	}
	refused := errors.New("fn refused")
	if err := Edit(path, func(cfg *Config) (bool, error) { cfg.LibraryName = "Not Saved"; return true, refused }); err != refused {
		t.Errorf("Edit returned %v, want fn's error as it is", err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, written) {
		t.Errorf("an Edit that saved nothing changed the file:\n%s", again)
	}
}

// TestEditSavesOnlyOverTheFileItRead: Edit's save compares the file with the
// one it read, once its new file is staged and just before the rename, so a
// file an edit (or a bridge older than the lock, which no lock keeps out)
// changed while Edit made its change is left as that writer left it, and
// Edit says so rather than answer success over it.
func TestEditSavesOnlyOverTheFileItRead(t *testing.T) {
	dir := t.TempDir()
	path, base := loadableConfig(t, dir, "Before")
	theirs := base + "libraryName: Edited Meanwhile\n"
	stop := OnSaveStagedForTest(path, func() { writeMeanwhile(t, path, theirs) })
	defer stop()
	err := Edit(path, func(cfg *Config) (bool, error) {
		cfg.ScanIntervalSec = 7200
		return true, nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("Edit over a file another writer changed while it saved = %v, want a refusal that says the file changed", err)
	}
	if got, _ := os.ReadFile(path); string(got) != theirs {
		t.Errorf("Edit wrote over the other writer's file:\n%s", got)
	}
}

// TestARefusedUpdateRunsNoneOfItsChange: Update takes its turn and compares
// the file with what it last read or wrote before it runs fn, so a save it
// refuses runs nothing of the caller's change. The console's root changes
// change the library index in fn (a library going from one root to more
// wipes every filesystem row, and unlinks every rendition and waveform),
// and until B506's second round fn ran first, so a save refused for a file
// another writer had changed had already done that, again on every retry.
func TestARefusedUpdateRunsNoneOfItsChange(t *testing.T) {
	dir := t.TempDir()
	cfg := updateTestCfg(t, dir)
	path := filepath.Join(dir, "bridge.yaml")
	if err := saveAt(cfg, path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(path, raw)
	writeMeanwhile(t, path, string(bytes.Replace(raw, []byte("Home"), []byte("Away"), 1)))
	ran := false
	err = rc.Update(path, func(c *Config) error {
		ran = true
		c.LibraryName = "Renamed"
		return nil
	})
	if !errors.Is(err, ErrChangedOutside) {
		t.Fatalf("Update over a file another writer changed = %v, want ErrChangedOutside", err)
	}
	if ran {
		t.Error("Update ran its change before it compared the file, and refused the save after")
	}
}

// TestUpdateComparesTheFileAgainJustBeforeItsRename: a console save compares
// the file once more when its new file is staged, just before the rename, so
// a file an edit (or a bridge older than the lock) changed while the save
// marshalled, wrote and synced its own is refused and left as that writer
// left it, and nothing is stored; until B506's second round the save went
// through over it and answered success.
func TestUpdateComparesTheFileAgainJustBeforeItsRename(t *testing.T) {
	dir := t.TempDir()
	cfg := updateTestCfg(t, dir)
	path := filepath.Join(dir, "bridge.yaml")
	if err := saveAt(cfg, path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(path, raw)
	theirs := string(bytes.Replace(raw, []byte("Home"), []byte("Away"), 1))
	stop := OnSaveStagedForTest(path, func() { writeMeanwhile(t, path, theirs) })
	defer stop()
	err = rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil })
	var changed *ChangedOutsideError
	if !errors.As(err, &changed) || changed.Path != path {
		t.Fatalf("Update over a file changed while it saved = %v, want a *ChangedOutsideError for %s", err, path)
	}
	if got, _ := os.ReadFile(path); string(got) != theirs {
		t.Errorf("Update wrote over the other writer's file:\n%s", got)
	}
	if rc.Load().LibraryName != "Home" {
		t.Errorf("the refused Update stored libraryName %q", rc.Load().LibraryName)
	}
}

// TestOnlyAFilesystemThatKeepsNoLocksTakesTheTurnUnlocked: the turn is taken
// without the lock only where the lock's own answer says no lock can be
// taken there (noLockAnswers: a filesystem that keeps none, a platform with
// none). Any other failure to take it (an I/O error, a descriptor the
// platform refuses) is returned, naming the config file, and no turn is
// taken: until B506's second round every such failure read as "no locks
// here", and the writer went on beside any other, in silence after the
// first warning.
func TestOnlyAFilesystemThatKeepsNoLocksTakesTheTurnUnlocked(t *testing.T) {
	dir := t.TempDir()
	path := writeConfigFile(t, dir, "libraryName: Before\n")
	answer := func(err error) func(*os.File) error {
		return func(f *os.File) error { return &os.PathError{Op: "lock", Path: f.Name(), Err: err} }
	}
	for _, no := range noLockAnswers() {
		l, err := lockFile(path, answer(no), time.Second)
		if err != nil {
			t.Errorf("%v: refused the turn: %v", no, err)
			continue
		}
		if !errors.Is(l.Unheld(), no) {
			t.Errorf("%v: Unheld = %v, want why no lock is held", no, l.Unheld())
		}
		l.Unlock()
	}
	for _, other := range otherLockFailures() {
		l, err := lockFile(path, answer(other), time.Second)
		if err == nil {
			l.Unlock()
			t.Errorf("%v: took the turn without the lock", other)
			continue
		}
		if !errors.Is(err, other) || !strings.Contains(err.Error(), path) {
			t.Errorf("%v: refusal %v, want that failure, naming %s", other, err, path)
		}
	}
}

// TestAnUnlockedTurnSavesOnlyOverWhatItFound: where no lock can be taken,
// nothing keeps another writer out of the turn, so every save in it compares
// the file with what the turn found, just before its rename: a save over a
// file another writer changed, and one in a turn that found no file (serve's
// seed) over a file that appeared, are refused, and the other writer's file
// is left as it is. Until B506's second round the rule said the writers
// there were back to their own byte comparisons, and Edit, a restore and
// the seed had none.
func TestAnUnlockedTurnSavesOnlyOverWhatItFound(t *testing.T) {
	noLocks := func(f *os.File) error {
		return &os.PathError{Op: "lock", Path: f.Name(), Err: errors.ErrUnsupported}
	}
	for _, tc := range []struct {
		name   string
		before *string
	}{
		{"over a file another writer changed", ptr("libraryName: Before\n")},
		{"in a turn that found no file", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			if tc.before != nil {
				writeConfigFile(t, dir, *tc.before)
			}
			l, err := lockFile(path, noLocks, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Unlock()
			if l.Unheld() == nil {
				t.Fatal("the turn holds a lock, so this test shows nothing about a turn without one")
			}
			const theirs = "libraryName: Theirs\n"
			stop := OnSaveStagedForTest(path, func() { writeMeanwhile(t, path, theirs) })
			defer stop()
			if err := (&Config{LibraryName: "Mine"}).Save(l); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Errorf("a save in a turn without the lock over another writer's file = %v, want a refusal that says the file changed", err)
			}
			if got, _ := os.ReadFile(path); string(got) != theirs {
				t.Errorf("the save wrote over the other writer's file:\n%s", got)
			}
		})
	}
}

// ptr returns a pointer to s.
func ptr(s string) *string { return &s }

// TestUpdateRefusesAFileAnotherWriterChanged: a console save (Update) reads
// the file again in its turn, and saves only over the bytes it last read
// (RecordFile) or wrote: over a file another writer changed or removed
// since, it saves and stores nothing, and says so naming the file and no
// value. Its own saves move what it compares with, so saves in a row go
// through.
func TestUpdateRefusesAFileAnotherWriterChanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		// meanwhile is the other writer, nil for none.
		meanwhile func(t *testing.T, path string)
	}{
		{"nothing written meanwhile", nil},
		{"a change of the same size", func(t *testing.T, path string) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b = bytes.Replace(b, []byte("Home"), []byte("Away"), 1)
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a root added", func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("scanIntervalSec: 7200\n"); err != nil {
				t.Fatal(err)
			}
		}},
		{"the file removed", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "Music"), 0o755); err != nil {
				t.Fatal(err)
			}
			path := writeConfigFile(t, dir, "libraryRoots: ["+yamlStr(filepath.Join(dir, "Music"))+"]\n"+
				"dataDir: "+yamlStr(filepath.Join(dir, "data"))+"\nlibraryName: Home\n")
			cfg, raw, err := LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rc := NewRuntimeConfig(cfg)
			rc.RecordFile(path, raw)
			if tc.meanwhile != nil {
				tc.meanwhile(t, path)
			}
			before, _ := os.ReadFile(path)
			err = rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil })
			if tc.meanwhile == nil {
				if err != nil {
					t.Fatalf("Update over the file it read: %v", err)
				}
				if err := rc.Update(path, func(c *Config) error { c.ScanIntervalSec = 60; return nil }); err != nil {
					t.Fatalf("a second Update over the file the first wrote: %v", err)
				}
				if after, err := Load(path); err != nil || after.LibraryName != "Renamed" || after.ScanIntervalSec != 60 {
					t.Errorf("after two Updates the file loads as %+v, %v", after, err)
				}
				return
			}
			var changed *ChangedOutsideError
			if !errors.As(err, &changed) || !errors.Is(err, ErrChangedOutside) || changed.Path != path {
				t.Fatalf("Update over a file another writer changed = %v, want a *ChangedOutsideError for %s", err, path)
			}
			if msg := err.Error(); !strings.Contains(msg, "restart the bridge") || strings.Contains(msg, "Home") || strings.Contains(msg, "Renamed") {
				t.Errorf("refusal %q: want it to say what to do, and no value", msg)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
				t.Errorf("Update wrote over the other writer's file:\n%s", after)
			}
			if rc.Load().LibraryName != "Home" {
				t.Errorf("the refused Update stored libraryName %q", rc.Load().LibraryName)
			}
		})
	}
}

// TestUpdateWaitsForTheLockThenRefuses: a console save while another writer
// holds the file's lock waits LockWait for it, then refuses with a
// *BusyError, saving and storing nothing, and running none of its change.
func TestUpdateWaitsForTheLockThenRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := updateTestCfg(t, dir)
	path := filepath.Join(dir, "bridge.yaml")
	if err := saveAt(cfg, path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(path, before)
	holdLock(t, path)
	start := time.Now()
	ran := false
	err = rc.Update(path, func(c *Config) error { ran = true; c.LibraryName = "Renamed"; return nil })
	if took := time.Since(start); took < LockWait {
		t.Errorf("Update answered after %s, before the wait of %s was over", took, LockWait)
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Update while another holds the lock = %v, want ErrBusy", err)
	}
	if ran {
		t.Error("Update ran its change before it had its turn, and then refused the save")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) || rc.Load().LibraryName != "Home" {
		t.Errorf("the refused Update changed something: file\n%s\nlive libraryName %q", after, rc.Load().LibraryName)
	}
}
