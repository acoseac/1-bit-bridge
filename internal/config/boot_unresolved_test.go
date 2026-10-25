package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A resolution of the config's name that fails is no file (B506's fifth
// round). Serve's boot read resolves the name before and after each read, and
// until the fifth round took two failed resolutions for an unmoved name: it
// answered "" as the file it had read, the holder skipped "" when it
// compared, and a console save went into whatever file the name opened at the
// save (on Windows, every config under a OneDrive folder in round 4). A read
// across which one side resolved is read again now, a name that never settles
// is refused, and a name that no read could resolve boots a holder that saves
// into no file.

// errUnresolvable is the failure a resolution of the config's name answers
// in these tests.
var errUnresolvable = errors.New("the config's name cannot be resolved (for the test)")

// scriptBootResolve makes the boot read's resolutions answer by script, one
// entry each in turn and again from the start: true resolves the name as a
// turn does, false fails. It returns how many were asked.
func scriptBootResolve(t *testing.T, script ...bool) *int {
	t.Helper()
	n := 0
	t.Cleanup(SetBootResolveForTest(func(path string) (string, error) {
		resolves := script[n%len(script)]
		n++
		if !resolves {
			return "", errUnresolvable
		}
		return ResolveFile(path)
	}))
	return &n
}

// TestTheBootReadRefusesANameResolvedOnOneSideOfARead: a read across which the
// name resolved on one side alone is read again, and a name that did not
// resolve on both sides of any of the three reads is refused as a name that
// would not settle, naming it and none of its values, whether the name
// resolved before each read or after it, or at no side of the first read and
// at one side of the others. Until B506's fifth round a read at neither side
// of which the name resolved was taken for a read of an unmoved name.
func TestTheBootReadRefusesANameResolvedOnOneSideOfARead(t *testing.T) {
	for _, tc := range []struct {
		name string
		// script is the resolutions in turn, before and after each read.
		script []bool
	}{
		{"resolved before each read and not after", []bool{true, false}},
		{"resolved after each read and not before", []bool{false, true}},
		{"resolved at neither side of the first read, then before each read", []bool{false, false, true, false, true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, _ := loadableConfig(t, realTempDir(t), "Before")
			scriptBootResolve(t, tc.script...)
			reads := 0
			_, _, _, err := loadFileAt(path, bootHooks{beforeRead: func() { reads++ }})
			var moving *NameMovingError
			if !errors.As(err, &moving) || moving.Path != path {
				t.Fatalf("a boot read whose name resolved on one side of a read = %v, want a *NameMovingError for %s", err, path)
			}
			if strings.Contains(err.Error(), "Before") {
				t.Errorf("the refusal %q quotes the config", err)
			}
			if reads != bootReads {
				t.Errorf("the file was read %d times, want %d", reads, bootReads)
			}
		})
	}
}

// TestABootReadThatResolvesNothingKeepsItsReadWithNoFile: a name that
// resolved at no side of any of the three reads (a resolution that fails for
// good, as a folder no check could resolve does) boots on the last read, made
// through the name, and answers no file: never "" as a file a holder could
// compare. Until B506's fifth round the first such read was taken at once,
// as the read of the file "".
func TestABootReadThatResolvesNothingKeepsItsReadWithNoFile(t *testing.T) {
	path, _ := loadableConfig(t, realTempDir(t), "Before")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	scriptBootResolve(t, false)
	reads := 0
	cfg, got, file, err := loadFileAt(path, bootHooks{beforeRead: func() { reads++ }})
	if err != nil {
		t.Fatalf("a boot read whose name resolves nowhere: %v", err)
	}
	if !bytes.Equal(got, raw) || cfg.LibraryName != "Before" {
		t.Errorf("the boot read answered %q (libraryName %q), want the file's bytes", got, cfg.LibraryName)
	}
	if file.resolved || file.target != "" {
		t.Errorf("the boot read answered the file %q (resolved %v), want none", file.target, file.resolved)
	}
	if reads != bootReads {
		t.Errorf("the file was read %d times, want %d before the boot keeps a read with no file", reads, bootReads)
	}
}

// TestAHolderBootedWithNoFileRefusesEverySave: a holder told a boot read that
// resolved no file refuses every save, before its turn and before the
// change runs, with a *BootUnresolvedError that names the config and none of
// its values and says to restart once the name resolves; it is still refused
// once the name resolves, until a restart reads the file again. A CLI
// writer's own turn, which resolves the name itself, is not the holder's and
// saves as ever. Until B506's fifth round the holder compared no file, and
// saved into whatever file the name opened at the save.
func TestAHolderBootedWithNoFileRefusesEverySave(t *testing.T) {
	dir := realTempDir(t)
	path, _ := loadableConfig(t, dir, "Before")
	restore := SetBootResolveForTest(func(string) (string, error) { return "", errUnresolvable })
	t.Cleanup(restore)
	cfg, raw, file, err := LoadFileAt(path)
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(file, raw)
	for _, when := range []string{"while the name resolves nowhere", "once the name resolves"} {
		if when == "once the name resolves" {
			restore()
		}
		ran := false
		err := rc.UpdateChecked(path, func(c *Config, _ func() error) error { ran = true; c.LibraryName = "After"; return nil })
		var unresolved *BootUnresolvedError
		if !errors.As(err, &unresolved) || unresolved.Path != path || !errors.Is(err, ErrBootUnresolved) {
			t.Errorf("a console save %s = %v, want a *BootUnresolvedError for %s", when, err, path)
		} else if msg := err.Error(); !strings.Contains(msg, "restart") || strings.Contains(msg, "Before") {
			t.Errorf("the refusal %q: want it to say to restart, and no value", msg)
		}
		if ran {
			t.Errorf("the refused save %s ran its change", when)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
			t.Errorf("the refused save %s wrote the config:\n%s", when, got)
		}
		if _, err := os.Stat(filepath.Join(dir, LockFileName(path))); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused save %s took a turn of the config's lock (%v): it refuses before its turn", when, err)
		}
	}
	if err := Edit(path, func(c *Config) (bool, error) { c.LibraryName = "By the CLI"; return true, nil }); err != nil {
		t.Fatalf("a CLI writer's turn beside the holder: %v", err)
	}
	if after, err := Load(path); err != nil || after.LibraryName != "By the CLI" {
		t.Errorf("the CLI writer's save: %v, %v", after, err)
	}
}

// TestTheBootReadOfAMissingConfigSaysItIsMissing: a config that is not there
// is the read's not-exist error, naming the config as it was given, at once:
// its name resolves on both sides of the read, and the file it opens is not
// there, which no read again changes.
func TestTheBootReadOfAMissingConfigSaysItIsMissing(t *testing.T) {
	path := filepath.Join(realTempDir(t), "bridge.yaml")
	reads := 0
	_, _, _, err := loadFileAt(path, bootHooks{beforeRead: func() { reads++ }})
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNameMoving) || !strings.Contains(err.Error(), path) {
		t.Errorf("the boot read of a config that is not there = %v, want a not-exist error naming %s", err, path)
	}
	if reads != 1 {
		t.Errorf("the file was read %d times, want once", reads)
	}
}
