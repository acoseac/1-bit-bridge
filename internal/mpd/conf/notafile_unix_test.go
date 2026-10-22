//go:build unix

package conf

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// notAFileBound is how long a read of a configuration with a named pipe in
// it may take: a refusal is an open and a stat.
const notAFileBound = 5 * time.Second

// TestANamedPipeIsRefusedWithoutWaiting: an open of a named pipe for
// reading waits for a writer, and nothing cancels it. mpd.conf, an include
// of either kind, and a file the player detection reads (ReadLogical) that
// is a named pipe are refused at once: the main file as the read's error,
// an include as the file's problem, and the read returns. (A device would
// be refused the same way, by fsutil.NotAFile; a test cannot make one.)
func TestANamedPipeIsRefusedWithoutWaiting(t *testing.T) {
	root := t.TempDir()
	pipe := filepath.Join(root, "pipe.conf")
	fsutiltest.MakeFIFO(t, pipe)
	mustWrite(t, filepath.Join(root, "mpd.conf"),
		"include \"pipe.conf\"\ninclude_optional \"pipe.conf\"\nmusic_directory \"/kept\"\n")

	var f File
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err = ReadFile(root, "/mpd.conf", filepath.Join(root, "mpd.conf"))
	}()
	if !fsutiltest.AwaitPastFIFOs(t, "an include that is a named pipe", notAFileBound, done, pipe) {
		return
	}
	if err != nil || f.MusicDirectory != "/kept" {
		t.Fatalf("err %v music %q", err, f.MusicDirectory)
	}
	if n := strings.Count(strings.Join(f.Problems, "\n"), "unreadable"); n != 2 {
		t.Fatalf("problems %q, want both includes of the pipe to be unreadable", f.Problems)
	}

	done = make(chan struct{})
	go func() {
		defer close(done)
		_, err = ReadFile(root, "/pipe.conf", pipe)
	}()
	if !fsutiltest.AwaitPastFIFOs(t, "mpd.conf that is a named pipe", notAFileBound, done, pipe) {
		return
	}
	if err == nil {
		t.Fatal("a named pipe read as mpd.conf")
	}

	done = make(chan struct{})
	go func() {
		defer close(done)
		_, err = ReadLogical(root, "/pipe.conf", 1<<10)
	}()
	if !fsutiltest.AwaitPastFIFOs(t, "a detection file that is a named pipe", notAFileBound, done, pipe) {
		return
	}
	if err == nil {
		t.Fatal("ReadLogical read a named pipe")
	}
}
