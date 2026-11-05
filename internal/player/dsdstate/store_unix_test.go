//go:build unix

package dsdstate

import (
	"path/filepath"
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
