//go:build !windows

package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A config's name is a link (a dotfiles checkout's, a home-manager's) that
// can be repointed at any moment: a turn resolves the name, waits for the
// lock beside the file it names, reads, compares and saves. Until B506's
// fourth round nothing resolved the name again once the turn had it, so a
// link repointed while a writer waited, or between two attempts at its
// rename, left the writer saving a file the name no longer opened, and
// reporting success. Every test here repoints the link to a file holding the
// same bytes, so that only the name's resolution can tell the two apart.

// repoint makes link lead to to, in one rename, as a dotfiles restow or a
// home-manager switch replaces it.
func repoint(t *testing.T, link, to string) {
	t.Helper()
	tmp := link + ".repoint"
	if err := os.Symlink(to, tmp); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Error(err)
	}
}

// twinOf writes a file holding target's bytes beside it, named name, and
// returns its path and those bytes.
func twinOf(t *testing.T, target, name string) (string, []byte) {
	t.Helper()
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	twin := filepath.Join(filepath.Dir(target), name)
	if err := os.WriteFile(twin, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return twin, raw
}

// requireBytes fails the test for each of paths that no longer holds want.
func requireBytes(t *testing.T, want []byte, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s was written:\n%s (%v)", filepath.Base(p), got, err)
		}
	}
}

// waitThenRepoint holds the lock beside target, starts write, waits until it
// waits for that lock, repoints link to moved, gives the lock back with
// target unchanged, and returns what write answered.
func waitThenRepoint(t *testing.T, target, link, moved string, write func() error) error {
	t.Helper()
	release := holdLock(t, target)
	waited, stop := LockWaitedForTest(link)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- write() }()
	select {
	case <-waited:
	case err := <-done:
		t.Fatalf("the writer went through while another held the lock: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the writer neither waited for the lock nor finished")
	}
	repoint(t, link, moved)
	release()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the writer did not finish once the lock was given back")
	}
	return nil
}

// TestAConsoleSaveRefusesANameRepointedWhileItWaited: serve's console save
// that waits for the lock beside the file serve read, while the link at the
// config's name is repointed to another file and that lock is given back with
// the file unchanged, is refused with a *ChangedOutsideError once it holds
// the lock: its change is not run, and neither file is written.
func TestAConsoleSaveRefusesANameRepointedWhileItWaited(t *testing.T) {
	target, link := linkedConfigFile(t)
	cfg, raw, read, err := LoadFileAt(link)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(read, raw)
	moved, _ := twinOf(t, target, "switched.yaml")
	ran := false
	err = waitThenRepoint(t, target, link, moved, func() error {
		return rc.Update(link, func(c *Config) error { ran = true; c.LibraryName = "Renamed"; return nil })
	})
	if !errors.Is(err, ErrChangedOutside) {
		t.Errorf("a console save whose name was repointed while it waited = %v, want ErrChangedOutside", err)
	}
	if ran {
		t.Error("the refused save ran its change")
	}
	requireBytes(t, raw, target, moved)
	requireLink(t, link, moved)
}

// TestAnEditRefusesANameRepointedWhileItWaited: a CLI writer's turn (Edit,
// library add's and remove's) that waits for the lock while the link at the
// config's name is repointed refuses with a *NameMovedError once it holds the
// lock, saying to run it again: its change, and what it does beside the
// config, is not run, and neither file is written.
func TestAnEditRefusesANameRepointedWhileItWaited(t *testing.T) {
	target, link := linkedConfigFile(t)
	moved, raw := twinOf(t, target, "switched.yaml")
	ran := false
	err := waitThenRepoint(t, target, link, moved, func() error {
		return Edit(link, func(c *Config) (bool, error) { ran = true; c.LibraryName = "Renamed"; return true, nil })
	})
	var movedErr *NameMovedError
	if !errors.As(err, &movedErr) || movedErr.Path != link {
		t.Errorf("an Edit whose name was repointed while it waited = %v, want a *NameMovedError for %s", err, link)
	} else if msg := err.Error(); !strings.Contains(msg, "run it again") || strings.Contains(msg, "Before") {
		t.Errorf("refusal %q: want it to say to run it again, and no value", msg)
	}
	if ran {
		t.Error("the refused Edit ran its change")
	}
	requireBytes(t, raw, target, moved)
	requireLink(t, link, moved)
}

// TestARepointBeforeALaterRenameAttemptEndsTheSave: a link at the config's
// name repointed while a save's first rename fails (a sharing violation on
// Windows) ends the save before its next attempt: the condition each attempt
// asks resolves the name again. No later rename is tried, and neither file is
// written.
func TestARepointBeforeALaterRenameAttemptEndsTheSave(t *testing.T) {
	target, link := linkedConfigFile(t)
	moved, raw := twinOf(t, target, "switched.yaml")
	attempts := failFirstRename(t, target, func() { repoint(t, link, moved) })
	l, err := LockFile(link)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if err := (&Config{LibraryName: "Mine"}).Save(l); !errors.Is(err, ErrNameMoved) {
		t.Errorf("a save whose name was repointed between two rename attempts = %v, want ErrNameMoved", err)
	}
	if *attempts != 1 {
		t.Errorf("%d renames were tried, want none after the name moved", *attempts)
	}
	requireBytes(t, raw, target, moved)
	requireNothingStaged(t, filepath.Dir(target))
}

// TestAConsoleCheckSaysAMovedNameIsAChangeOutside: the console's check, which
// a root change asks just before its library index change, refuses a name
// repointed since the turn resolved it, and says so as the holder says every
// change outside it, a *ChangedOutsideError, before the change runs.
func TestAConsoleCheckSaysAMovedNameIsAChangeOutside(t *testing.T) {
	target, link := linkedConfigFile(t)
	cfg, raw, read, err := LoadFileAt(link)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(read, raw)
	moved, _ := twinOf(t, target, "switched.yaml")
	indexChanged := false
	err = rc.UpdateChecked(link, func(c *Config, check func() error) error {
		repoint(t, link, moved)
		if err := check(); err != nil {
			return err
		}
		indexChanged = true
		return nil
	})
	var outside *ChangedOutsideError
	if !errors.As(err, &outside) || outside.Path != link {
		t.Errorf("a console change whose name moved before its check = %v, want a *ChangedOutsideError for %s", err, link)
	}
	if indexChanged {
		t.Error("the check let the change past a moved name")
	}
	requireBytes(t, raw, target, moved)
}

// TestAConsoleSaveSaysANameMovedDuringItsSaveIsAChangeOutside: a name
// repointed while the console's save stages its file is refused at the
// rename, as a *ChangedOutsideError, with neither file written and nothing
// stored.
func TestAConsoleSaveSaysANameMovedDuringItsSaveIsAChangeOutside(t *testing.T) {
	target, link := linkedConfigFile(t)
	cfg, raw, read, err := LoadFileAt(link)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(read, raw)
	moved, _ := twinOf(t, target, "switched.yaml")
	stop := OnSaveStagedForTest(link, func() { repoint(t, link, moved) })
	defer stop()
	err = rc.Update(link, func(c *Config) error { c.LibraryName = "Renamed"; return nil })
	var outside *ChangedOutsideError
	if !errors.As(err, &outside) || outside.Path != link {
		t.Errorf("a console save whose name moved while it staged = %v, want a *ChangedOutsideError for %s", err, link)
	}
	requireBytes(t, raw, target, moved)
	if rc.Load().LibraryName == "Renamed" {
		t.Error("the refused save stored its change")
	}
}

// TestTheBootReadReadsAgainWhenTheNameMovesAcrossIt: serve's boot read
// resolves the config's name before and after it reads, and reads again when
// the two differ: it answers the bytes and the file of one read, never one
// file's bytes with another's name.
func TestTheBootReadReadsAgainWhenTheNameMovesAcrossIt(t *testing.T) {
	target, link := linkedConfigFile(t)
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(filepath.Dir(target), "switched.yaml")
	other := bytes.Replace(raw, []byte("Before"), []byte("Switched"), 1)
	if err := os.WriteFile(moved, other, 0o600); err != nil {
		t.Fatal(err)
	}
	reads := 0
	cfg, got, read, err := loadFileAt(link, bootHooks{afterRead: func() {
		if reads++; reads == 1 {
			repoint(t, link, moved)
		}
	}})
	if err != nil {
		t.Fatalf("a boot read whose name moved once: %v", err)
	}
	if read.target != moved || !bytes.Equal(got, other) || cfg.LibraryName != "Switched" {
		t.Errorf("the boot read answered %s with %q (libraryName %q): want the file the name opened for that read, and its bytes",
			read.target, got, cfg.LibraryName)
	}
	if reads != 2 {
		t.Errorf("the file was read %d times, want once more after the name moved", reads)
	}
}

// TestTheBootReadRefusesANameThatKeepsMoving: a name that opens another file
// after each of the boot read's three reads than before it is refused, naming
// the config's name and none of its values: serve does not start on a file it
// cannot say the name opens.
func TestTheBootReadRefusesANameThatKeepsMoving(t *testing.T) {
	target, link := linkedConfigFile(t)
	moved, _ := twinOf(t, target, "switched.yaml")
	next := []string{moved, target}
	reads := 0
	_, _, _, err := loadFileAt(link, bootHooks{afterRead: func() {
		repoint(t, link, next[reads%2])
		reads++
	}})
	var moving *NameMovingError
	if !errors.As(err, &moving) || moving.Path != link || !errors.Is(err, ErrNameMoving) {
		t.Fatalf("a boot read whose name moved across every read = %v, want a *NameMovingError for %s", err, link)
	}
	if msg := err.Error(); strings.Contains(msg, "Before") || !strings.Contains(msg, "settled") {
		t.Errorf("refusal %q: want it to say to start again once the link is settled, and no value", msg)
	}
	if reads != 3 {
		t.Errorf("the file was read %d times, want three", reads)
	}
}

// TestTheBootReadResolvesRelativePathsAgainstTheNameAsGiven: the file the
// boot read resolves is beside what it reads, never the base of the config's
// relative paths, which stays the directory config.Load takes from the name
// as given, however far the link leads, and however often the read is made:
// a data dir, a library root and a TLS pair named relatively resolve under
// the link's directory, before the name moves and after.
func TestTheBootReadResolvesRelativePathsAgainstTheNameAsGiven(t *testing.T) {
	base := realTempDir(t)
	aliasDir, targetDir, otherDir := filepath.Join(base, "etc"), filepath.Join(base, "store", "a"), filepath.Join(base, "store", "b")
	for _, d := range []string{filepath.Join(aliasDir, "Music"), targetDir, otherDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const text = "libraryRoots:\n  - Music\ndataDir: data\ntlsCertPath: tls/server.crt\ntlsKeyPath: tls/server.key\nlibraryName: Home\n"
	target, other := filepath.Join(targetDir, "actual.yaml"), filepath.Join(otherDir, "actual.yaml")
	for _, p := range []string{target, other} {
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(aliasDir, "bridge.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		afterRead func()
	}{
		{"one read", nil},
		{"read again after the name moved", func() { repoint(t, link, other) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			moved := false
			cfg, _, read, err := loadFileAt(link, bootHooks{afterRead: func() {
				if tc.afterRead != nil && !moved {
					moved = true
					tc.afterRead()
				}
			}})
			if err != nil {
				t.Fatal(err)
			}
			if read.target == link {
				t.Errorf("the boot read answered the link itself, %s", read.target)
			}
			for _, c := range []struct{ what, got, want string }{
				{"data dir", cfg.DataDir, filepath.Join(aliasDir, "data")},
				{"library root", strings.Join(cfg.LibraryRoots, ","), filepath.Join(aliasDir, "Music")},
				{"TLS cert", cfg.TLSCertPath, filepath.Join(aliasDir, "tls", "server.crt")},
				{"TLS key", cfg.TLSKeyPath, filepath.Join(aliasDir, "tls", "server.key")},
			} {
				if c.got != c.want {
					t.Errorf("the %s resolves to %s, want %s: the base of the config's relative paths moved to the file the name opens", c.what, c.got, c.want)
				}
			}
		})
	}
}
