//go:build !windows

package config

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// B506's fourth round resolved the config's name again in a turn, but before
// the reads its decisions rest on: the turn's own read once it holds the
// lock, a save's comparison before each rename, a writer's own comparison in
// SaveIf (bind's), and a check (FileLock.Check). A link repointed while one
// of those reads ran left the read answering the file the turn had locked,
// unchanged, while the name opened another, and the save renamed onto the
// locked file, the check let its writer's work run, and the turn took its
// read for the file the name opens. Since the fifth round the name is
// resolved after every such read, the last thing asked before the rename or
// the work. Each test repoints the link inside one read, the nth a turn and
// its writer make through ReadFile, to a twin holding the same bytes, so that
// only the name's resolution tells the two apart, and asks which file that
// read was of, so that it cannot pass by repointing somewhere else.

// repointDuringRead makes link lead to moved just before the nth read of the
// config that a turn or a writer's own comparison makes through ReadFile,
// counting from the turn's own read once it holds the lock, and returns the
// path that read was of, "" until it is made.
func repointDuringRead(t *testing.T, link, moved string, n int) (readOf *string) {
	t.Helper()
	reads, of := 0, ""
	t.Cleanup(SetTurnReadForTest(func(p string) ([]byte, error) {
		if reads++; reads == n {
			repoint(t, link, moved)
			of = p
		}
		return fsutil.ReadAsFile(p)
	}))
	return &of
}

// TestASaveRefusesANameMovedDuringItsComparisonRead: a link at the config's
// name repointed while a save's comparison reads the file the turn locked,
// which the read finds unchanged, refuses the save with a *NameMovedError and
// no rename: the name is resolved again once that read has answered. Until
// B506's fifth round it was resolved before the read, and the save renamed
// onto the file the name no longer opened, and answered nil.
func TestASaveRefusesANameMovedDuringItsComparisonRead(t *testing.T) {
	target, link := linkedConfigFile(t)
	moved, raw := twinOf(t, target, "switched.yaml")
	readOf := repointDuringRead(t, link, moved, 2)
	l, err := LockFile(link)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	err = (&Config{LibraryName: "Mine"}).Save(l)
	if *readOf != target {
		t.Fatalf("premise: the second read was of %q, want the save's comparison's, of %s", *readOf, target)
	}
	if !errors.Is(err, ErrNameMoved) {
		t.Errorf("a save whose name moved during its comparison's read = %v, want ErrNameMoved", err)
	}
	requireBytes(t, raw, target, moved)
	requireLink(t, link, moved)
	requireNothingStaged(t, filepath.Dir(target))
}

// TestASaveIfRefusesANameMovedDuringItsOwnComparison: a link repointed while a
// writer's own comparison in SaveIf reads the config through its name, as
// bind's does, refuses the save with a *NameMovedError and no rename: the name
// is resolved again after that comparison, the last thing asked before the
// rename. Until B506's fifth round it was resolved before both comparisons,
// and the save renamed onto the file the name no longer opened.
func TestASaveIfRefusesANameMovedDuringItsOwnComparison(t *testing.T) {
	target, link := linkedConfigFile(t)
	moved, raw := twinOf(t, target, "switched.yaml")
	readOf := repointDuringRead(t, link, moved, 3)
	l, err := LockFile(link)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	read, _ := l.Found()
	err = (&Config{LibraryName: "Mine"}).SaveIf(l, func(ctx context.Context) error {
		now, err := ReadFile(ctx, link)
		switch {
		case err != nil:
			return err
		case !bytes.Equal(now, read):
			return errors.New("the config changed after the writer read it")
		}
		return nil
	})
	if *readOf != link {
		t.Fatalf("premise: the third read was of %q, want the writer's own comparison's, of %s", *readOf, link)
	}
	if !errors.Is(err, ErrNameMoved) {
		t.Errorf("a save whose name moved during its writer's own comparison = %v, want ErrNameMoved", err)
	}
	requireBytes(t, raw, target, moved)
	requireLink(t, link, moved)
	requireNothingStaged(t, filepath.Dir(target))
}

// TestAConsoleCheckRefusesANameMovedDuringItsRead: the console's check, which
// a root change asks just before its library index change, refuses a link
// repointed while it reads the file the turn locked, as a
// *ChangedOutsideError, and the change does not run: the check resolves the
// name once its read has answered. Until B506's fifth round it resolved the
// name before the read, and the index change ran before the save refused.
func TestAConsoleCheckRefusesANameMovedDuringItsRead(t *testing.T) {
	target, link := linkedConfigFile(t)
	cfg, raw, read, err := LoadFileAt(link)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(read, raw)
	moved, _ := twinOf(t, target, "switched.yaml")
	readOf := repointDuringRead(t, link, moved, 2)
	indexChanged := false
	err = rc.UpdateChecked(link, func(c *Config, check func() error) error {
		if err := check(); err != nil {
			return err
		}
		indexChanged = true
		return nil
	})
	if *readOf != target {
		t.Fatalf("premise: the second read was of %q, want the check's, of %s", *readOf, target)
	}
	var outside *ChangedOutsideError
	if !errors.As(err, &outside) || outside.Path != link {
		t.Errorf("a console change whose name moved during its check's read = %v, want a *ChangedOutsideError for %s", err, link)
	}
	if indexChanged {
		t.Error("the check let the change run past a name moved during its read")
	}
	requireBytes(t, raw, target, moved)
}

// TestAnEditRefusesANameMovedDuringItsTurnsRead: a link repointed while a turn
// makes its own read, once it holds the lock, refuses the turn with a
// *NameMovedError before the writer's change runs: the name is resolved again
// once that read has answered. Until B506's fifth round it was resolved before
// the read, so Edit ran its change (library add's wipe of the index is part
// of it) on a turn whose name no longer opened the file it had read, and only
// the save refused.
func TestAnEditRefusesANameMovedDuringItsTurnsRead(t *testing.T) {
	target, link := linkedConfigFile(t)
	moved, raw := twinOf(t, target, "switched.yaml")
	readOf := repointDuringRead(t, link, moved, 1)
	ran := false
	err := Edit(link, func(c *Config) (bool, error) { ran = true; c.LibraryName = "Renamed"; return true, nil })
	if *readOf != target {
		t.Fatalf("premise: the first read was of %q, want the turn's own, of %s", *readOf, target)
	}
	if !errors.Is(err, ErrNameMoved) {
		t.Errorf("an Edit whose name moved during its turn's read = %v, want ErrNameMoved", err)
	}
	if ran {
		t.Error("the Edit ran its change on a turn whose name moved during its read")
	}
	requireBytes(t, raw, target, moved)
	requireLink(t, link, moved)
}
