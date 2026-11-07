package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Serve's refusal of a data dir whose volume is away, said once per state
// (backlog B727's fifth item). The service manager starts serve again until
// the volume is mounted (systemd's unit every 2 s, launchd, the SCM's
// recovery, a container's restart policy), and every start printed the whole
// refusal: a journal line every 2 s for as long as the volume was away. Each
// start is a new process, so what was said lives on disk, in a note beside
// the record (`.bridge.yaml.datadir.refused`), in the config's directory
// where the record is, never in the data dir, which is the bare mount point:
// the data dir serve refused, as its config gives it, and when that was said.
// A start says the refusal when the note says otherwise (none, the first
// refusal or the first since a start found its data dir; another data dir) or
// was written a day or more ago (the variant watcher's latch's rule, so a
// journal read later has a line of the day), and always to a terminal (a
// start by hand); the first start that finds its data dir again removes the
// note and says so. A note that cannot be written leaves the next start to
// say the refusal again; what serve refuses, and how, is the same either way.

// dataDirRefusalSuffix names the note beside the record.
const dataDirRefusalSuffix = ".refused"

// dataDirRefusalNoteMax is the most of a note readDataDirRefusalNote reads.
const dataDirRefusalNoteMax = 64 << 10

// dataDirRefusalRepeat is how long a refusal said once goes unsaid at the
// starts after it.
const dataDirRefusalRepeat = 24 * time.Hour

// dataDirRefusalNote is what the note holds.
type dataDirRefusalNote struct {
	// DataDir is the data dir serve refused, as its config gives it.
	DataDir string `json:"dataDir"`
	// SaidAt is when a start last said the refusal.
	SaidAt time.Time `json:"saidAt"`
}

// readDataDirRefusalNote answers the note at path, the zero note for none or
// for one that does not read.
func readDataDirRefusalNote(path string) dataDirRefusalNote {
	f, _, err := fsutil.OpenAsFile(path)
	if err != nil {
		return dataDirRefusalNote{}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, dataDirRefusalNoteMax))
	if err != nil {
		return dataDirRefusalNote{}
	}
	var n dataDirRefusalNote
	if json.Unmarshal(b, &n) != nil {
		return dataDirRefusalNote{}
	}
	return n
}

// sayServeRefusal writes serve's refusal gone to stderr, at now, unless a
// start said it already for this state (the note beside gone's record names
// gone's data dir and was written less than dataDirRefusalRepeat ago, and
// stderr is no terminal), and then notes that it was said.
func sayServeRefusal(gone *dataDirGoneError, stderr io.Writer, now time.Time) {
	path := gone.record + dataDirRefusalSuffix
	note := readDataDirRefusalNote(path)
	since := now.Sub(note.SaidAt)
	if note.DataDir == gone.dataDir && since >= 0 && since < dataDirRefusalRepeat && !writesToATerminal(stderr) {
		return
	}
	fmt.Fprintf(stderr, "serve: %v\n", gone)
	b, err := json.Marshal(dataDirRefusalNote{DataDir: gone.dataDir, SaidAt: now.UTC()})
	if err == nil {
		// Unsaid where it fails: the next start finds no note and says the
		// refusal again.
		_ = atomicwrite.WriteBytes(path, append(b, '\n'), ".bridge-datadir-*.tmp")
	}
}

// endServeRefusal removes the note beside the record at record, the first
// time a start finds its data dir after a refusal, and says once that the
// refusal is over. Nothing where the note is not there.
func endServeRefusal(record, dataDir string) {
	if record == "" {
		return
	}
	path := record + dataDirRefusalSuffix
	note := readDataDirRefusalNote(path)
	if err := os.Remove(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("could not remove the note that serve refused its data directory: the next start refused there may not say why",
				"note", path, "err", err)
		}
		return
	}
	logger.Info("the data directory serve refused while its volume was away holds its database or its lock file again: serve starts on it",
		"dataDir", dataDir, "refusedDataDir", note.DataDir, "lastSaid", note.SaidAt)
}

// writesToATerminal reports whether w is a terminal: a start by hand, which
// is told why every time.
func writesToATerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
