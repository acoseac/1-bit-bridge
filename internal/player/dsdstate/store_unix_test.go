//go:build unix

package dsdstate

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

// TestAStateFileTheStoreReadsIsItsOwn: the store reads only its own file,
// opened as a file alone, so a named pipe at the name is refused at once,
// never waited on.
func TestAStateFileTheStoreReadsIsItsOwn(t *testing.T) {
	data := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(data, FileName), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan Report, 1)
	go func() { done <- openAt(t, data, options{}).Report() }()
	if r := <-done; r.Outcome != Unread {
		t.Fatalf("Open of a named pipe = %+v, want it left unread", r)
	}
}

// TestADanglingLinkIsAnEntryThatDoesNotRead: a link at the state's name
// that leads nowhere is an entry there that does not read, never a fresh
// install, though opening it answers as no file does (Codex on #1405): Open
// refuses DSD in memory, no write replaces the link, and the link is left as
// it was; once the operator removes it, a write is made again, on the state
// that refuses, and a choice clears the refusal.
func TestADanglingLinkIsAnEntryThatDoesNotRead(t *testing.T) {
	durable(t)
	data := t.TempDir()
	path := filepath.Join(data, FileName)
	const gone = "player-dsd.json.gone"
	if err := os.Symlink(gone, path); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, data, options{})
	if r := s.Report(); r.Outcome != Unread || r.Why == nil {
		t.Fatalf("Open of a link to nothing = %+v, want an entry that does not read, never a fresh install", r)
	}
	if !reflect.DeepEqual(s.State(), Refusing(t0)) {
		t.Fatalf("the state of a link to nothing is %s, want Refusing", show(t, s.State()))
	}
	if err := s.Update(chooseNative(t)); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("a write over the link = %v, want ErrNotWritable", err)
	}
	if target, err := os.Readlink(path); err != nil || target != gone {
		t.Fatalf("the link is now %q, %v; want it left as it was", target, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(chooseNative(t)); err != nil {
		t.Fatalf("a write once the link was removed = %v", err)
	}
	if got := fileState(t, data); got.Unreadable() || got.DefaultMode != ModeOff {
		t.Fatalf("the state written once the link was removed is %s: want the choice made on the state that refuses, the refusal cleared and defaultMode off", show(t, got))
	}
}
