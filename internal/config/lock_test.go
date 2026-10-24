package config

import (
	"bytes"
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
	noLocks := errors.New("no locks on this filesystem")
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
	if err := cfg.SaveIf(l, func() error { return nil }); err == nil {
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
	lib := filepath.Join(dir, "Music")
	if err := os.Mkdir(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	base := "libraryRoots: [" + yamlStr(lib) + "]\ndataDir: " + yamlStr(filepath.Join(dir, "data")) + "\n"
	path := writeConfigFile(t, dir, base+"libraryName: Before\n")
	release := holdLock(t, path)
	done := make(chan error, 1)
	var seen string
	go func() {
		done <- Edit(path, func(cfg *Config) (bool, error) {
			seen = cfg.LibraryName
			cfg.ScanIntervalSec = 7200
			return true, nil
		})
	}()
	// The other writer's save, in its own turn, while Edit waits for it.
	time.Sleep(100 * time.Millisecond)
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
			rc.RecordFile(raw)
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
// *BusyError, saving and storing nothing.
func TestUpdateWaitsForTheLockThenRefuses(t *testing.T) {
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
	rc.RecordFile(before)
	holdLock(t, path)
	start := time.Now()
	err = rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil })
	if took := time.Since(start); took < LockWait {
		t.Errorf("Update answered after %s, before the wait of %s was over", took, LockWait)
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Update while another holds the lock = %v, want ErrBusy", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) || rc.Load().LibraryName != "Home" {
		t.Errorf("the refused Update changed something: file\n%s\nlive libraryName %q", after, rc.Load().LibraryName)
	}
}
