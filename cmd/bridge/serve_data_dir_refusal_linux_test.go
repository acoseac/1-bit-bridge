package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openTerminal opens a pseudo-terminal: what is written to tty is read from
// master. It skips where the host gives none.
func openTerminal(t *testing.T) (master, tty *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	fd := int(m.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}

// TestServeSaysItsRefusalToATerminalAtEveryStart: a serve started by hand, its
// stderr a terminal, is told why at every start, however fresh the note, and
// still notes that the refusal was said, for the service manager's next start.
func TestServeSaysItsRefusalToATerminalAtEveryStart(t *testing.T) {
	master, tty := openTerminal(t)
	if !writesToATerminal(tty) {
		t.Fatal("the pseudo-terminal does not read as a terminal")
	}
	record := filepath.Join(t.TempDir(), ".bridge.yaml.datadir")
	gone := &dataDirGoneError{dataDir: "/mnt/usb/bridge", record: record}
	now := time.Now()
	for i := 0; i < 3; i++ {
		sayServeRefusal(gone, tty, now.Add(time.Duration(i)*2*time.Second))
	}
	if err := tty.Close(); err != nil {
		t.Fatal(err)
	}
	// Once its other end is closed, a read of the master answers what was
	// written and then EIO.
	got, err := io.ReadAll(master)
	if err != nil && !errors.Is(err, syscall.EIO) {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), "serve: the data directory /mnt/usb/bridge held this bridge's database"); n != 3 {
		t.Errorf("three starts by hand said the refusal %d time(s), want 3:\n%s", n, got)
	}
	if note := readDataDirRefusalNote(record + dataDirRefusalSuffix); note.DataDir != gone.dataDir || !note.SaidAt.Equal(now.Add(4*time.Second).UTC()) {
		t.Errorf("the note after the starts by hand is %+v, want %q said at the last", note, gone.dataDir)
	}
}
