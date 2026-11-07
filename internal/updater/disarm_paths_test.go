package updater

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func requirePathlessDisarm(t *testing.T, dir string, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("wanted a refusal")
	}
	for _, path := range []string{dir, StatePath(dir)} {
		if strings.Contains(err.Error(), path) {
			t.Errorf("refusal carries %q: %v", path, err)
		}
	}
	if !strings.Contains(err.Error(), "install marker") {
		t.Errorf("refusal lost its context: %v", err)
	}
}

func TestDisarmRefusalsKeepCausesWithoutPaths(t *testing.T) {
	t.Run("cannot remove", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(StatePath(dir), "held"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, readErr := LoadState(dir)
		removeErr := os.Remove(StatePath(dir))
		var readPath, removePath *fs.PathError
		if !errors.As(readErr, &readPath) || !errors.As(removeErr, &removePath) {
			t.Fatal("fixture did not produce file errors")
		}
		_, err := disarmHeld(dir, "0.2.3", nil, false, migrationGate{})
		requirePathlessDisarm(t, dir, err)
		for _, cause := range []error{readPath.Err, removePath.Err} {
			if !errors.Is(err, cause) {
				t.Errorf("refusal lost cause %v: %v", cause, err)
			}
		}
	})
	t.Run("cannot make removal durable", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(StatePath(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		cause := &fs.PathError{Op: "open", Path: dir, Err: fs.ErrNotExist}
		t.Cleanup(SetMarkerBarrierForTest(func(string) error { return cause }))
		_, err := disarmHeld(dir, "0.2.3", nil, false, migrationGate{})
		requirePathlessDisarm(t, dir, err)
		if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, cause) {
			t.Errorf("refusal lost removal's cause: %v", err)
		}
	})
	for _, lockErr := range []*StateLockError{
		{Err: syscall.EIO}, {held: true, Err: syscall.EAGAIN}, {noLock: true, Err: syscall.ENOLCK},
	} {
		t.Run(lockErr.Error(), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(StatePath(dir), 0o700); err != nil {
				t.Fatal(err)
			}
			_, err := disarmUnlocked(dir, "0.2.3", false, migrationGate{}, lockErr)
			requirePathlessDisarm(t, dir, err)
			if !errors.Is(err, lockErr) || !errors.Is(err, lockErr.Err) {
				t.Errorf("refusal lost its lock cause: %v", err)
			}
			if lockErr.held && !errors.Is(err, ErrStateLockHeld) {
				t.Errorf("refusal lost held-lock sentinel: %v", err)
			}
		})
	}
}

func TestDisarmUnreadableKeepsTheOriginalPathError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(StatePath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := AdmitBoot(t.Context(), dir, "0.2.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	var pe *fs.PathError
	if !errors.As(res.Unreadable, &pe) || pe.Path != StatePath(dir) || !strings.Contains(res.Unreadable.Error(), pe.Path) {
		t.Fatalf("Unreadable lost the original error: %v", res.Unreadable)
	}
}
