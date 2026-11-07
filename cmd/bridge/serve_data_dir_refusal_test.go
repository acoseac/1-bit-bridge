package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// Backlog B727's fifth item: serve refuses a data dir its record names while
// that data dir's volume is away, and its service manager starts it again,
// systemd's unit every 2 s, until the volume is mounted. Every start printed
// the whole refusal, a journal line every 2 s. Each start is a new process,
// so the state lives on disk, in a note beside the record (in the config's
// directory, never the data dir, the bare mount point): a start says the
// refusal once per state, and again a day later; a start that finds its data
// dir again removes the note and says so.

// refusalNoteFile is where serve notes that it said its refusal of the data
// dir the record beside the config at cfg names.
func refusalNoteFile(cfg string) string { return dataDirRecordFile(cfg) + ".refused" }

// saidAtOf reads the note beside cfg's record: the data dir it names and
// when the refusal was said.
func saidAtOf(t *testing.T, cfg string) (dataDir string, at time.Time) {
	t.Helper()
	b, err := os.ReadFile(refusalNoteFile(cfg))
	if err != nil {
		t.Fatalf("no note that serve said its refusal: %v", err)
	}
	var n struct {
		DataDir string    `json:"dataDir"`
		SaidAt  time.Time `json:"saidAt"`
	}
	if err := json.Unmarshal(b, &n); err != nil {
		t.Fatalf("the note does not read: %v (%q)", err, b)
	}
	return n.DataDir, n.SaidAt
}

// refuseTimes runs serve n times over cfg, each a new start of a service
// manager's retries, and returns whether each said the refusal of dataDir;
// every one must refuse, exit 1 and serve nothing.
func refuseTimes(t *testing.T, cfg, dataDir string, n int) []bool {
	t.Helper()
	var said []bool
	for i := 0; i < n; i++ {
		code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: cfg})
		if served || code != 1 {
			t.Fatalf("start %d over a data dir whose volume is away: served %v, exit %d; stdout=%s stderr=%s", i+1, served, code, stdout, stderr)
		}
		said = append(said, strings.Contains(stderr, dataDirGoneLine(dataDir)))
	}
	return said
}

// TestServeSaysItsRefusalOncePerState: a serve over a data dir below a
// volume's mount point, the volume then taken away; three starts in a row,
// as a service manager's retries, each refuse, and the first alone says why;
// nothing is made in the data dir's place, and the note beside the record
// names the data dir. A note a day old is said again. Once the volume is
// back serve starts on it, removes the note and says so once, and the next
// refusal after the volume goes again is said. On main every start said the
// refusal and no note was kept.
func TestServeSaysItsRefusalOncePerState(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mountPoint := filepath.Join(base, "usb")
	for _, d := range []string{lib, mountPoint} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := filepath.Join(mountPoint, "bridge")
	cfg := writeServeConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
	stopServed(t, bootServe(t, "--config", cfg))
	volume := filepath.Join(base, "volume")
	if err := os.Rename(dataDir, volume); err != nil {
		t.Fatal(err)
	}

	if said := refuseTimes(t, cfg, dataDir, 3); !said[0] || said[1] || said[2] {
		t.Errorf("starts said the refusal %v, want the first alone", said)
	}
	if entries, err := os.ReadDir(mountPoint); err != nil || len(entries) != 0 {
		t.Errorf("the refusals made %d entr(ies) in the bare mount point (%v)", len(entries), err)
	}
	if got, at := saidAtOf(t, cfg); got != dataDir || time.Since(at) > time.Hour || time.Since(at) < 0 {
		t.Errorf("the note names %q said at %v, want %q said just now", got, at, dataDir)
	}

	// A day later the refusal is said again, once.
	old, err := json.Marshal(struct {
		DataDir string    `json:"dataDir"`
		SaidAt  time.Time `json:"saidAt"`
	}{dataDir, time.Now().Add(-25 * time.Hour).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refusalNoteFile(cfg), old, 0o600); err != nil {
		t.Fatal(err)
	}
	if said := refuseTimes(t, cfg, dataDir, 2); !said[0] || said[1] {
		t.Errorf("a day after the refusal was said, starts said it %v, want the first alone", said)
	}

	// The volume is back: serve starts on it, and says once that it is.
	if err := os.Rename(volume, dataDir); err != nil {
		t.Fatal(err)
	}
	rec := loggingtest.Record(t)
	stopServed(t, bootServe(t, "--config", cfg))
	if _, err := os.Stat(refusalNoteFile(cfg)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the note outlived the start that found the data dir: %v", err)
	}
	if lines := rec.Carrying("holds its database or its lock file again"); len(lines) != 1 {
		t.Errorf("the start that found the data dir again said so %d time(s), want once:\n%s", len(lines), strings.Join(lines, "\n"))
	}

	// Away again: a new state, said again.
	if err := os.Rename(dataDir, volume); err != nil {
		t.Fatal(err)
	}
	if said := refuseTimes(t, cfg, dataDir, 2); !said[0] || said[1] {
		t.Errorf("after the volume went again, starts said the refusal %v, want the first alone", said)
	}
}

// TestTheEndOfARefusalIsSaidAsItIs: the first start after a refusal that
// gets past the data dir's preparation and its lock removes the note and
// says how the refusal ended: that the data dir is back with its files only
// where it is (its volume mounted again), and otherwise that the refusal no
// longer applies (dataDir set to another directory, or the record removed
// with the volume still away, where serve makes the data dir anew). A start
// that its preparation or its lock refuses after the check (a regular file
// where the data dir goes, another serve holding the lock) says neither and
// keeps the note. On B727's first round every start that passed the check
// said the data dir was back, before its preparation and its lock, and
// removed the note.
func TestTheEndOfARefusalIsSaidAsItIs(t *testing.T) {
	const back, over = "holds its database or its lock file again", "no longer applies"
	type setup struct{ base, lib, dataDir, volume, cfg string }
	for _, tc := range []struct {
		name   string
		then   func(t *testing.T, s setup) serveOpts
		serves bool
		says   string
	}{
		{"the volume is back", func(t *testing.T, s setup) serveOpts {
			if err := os.Rename(s.volume, s.dataDir); err != nil {
				t.Fatal(err)
			}
			return serveOpts{configPath: s.cfg}
		}, true, back},
		{"dataDir is set to another directory", func(t *testing.T, s setup) serveOpts {
			writeServeConfig(t, s.cfg, s.lib, filepath.Join(s.base, "elsewhere"), "127.0.0.1:0", "127.0.0.1:0")
			return serveOpts{configPath: s.cfg}
		}, true, over},
		{"the record is removed with the volume still away", func(t *testing.T, s setup) serveOpts {
			if err := os.Remove(dataDirRecordFile(s.cfg)); err != nil {
				t.Fatal(err)
			}
			return serveOpts{configPath: s.cfg}
		}, true, over},
		{"another serve holds the data dir's lock", func(t *testing.T, s setup) serveOpts {
			if err := os.Rename(s.volume, s.dataDir); err != nil {
				t.Fatal(err)
			}
			return serveOpts{configPath: s.cfg, dataDirTryLock: lockAnswer(fsutil.ErrLocked)}
		}, false, ""},
		{"a regular file stands where the data dir goes", func(t *testing.T, s setup) serveOpts {
			if err := os.WriteFile(s.dataDir, []byte("not a directory\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return serveOpts{configPath: s.cfg}
		}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			lib := filepath.Join(base, "Music")
			mountPoint := filepath.Join(base, "usb")
			for _, d := range []string{lib, mountPoint} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			s := setup{base: base, lib: lib, dataDir: filepath.Join(mountPoint, "bridge"), volume: filepath.Join(base, "volume")}
			s.cfg = writeServeConfig(t, filepath.Join(base, "bridge.yaml"), lib, s.dataDir, "127.0.0.1:0", "127.0.0.1:0")
			stopServed(t, bootServe(t, "--config", s.cfg))
			if err := os.Rename(s.dataDir, s.volume); err != nil {
				t.Fatal(err)
			}
			if said := refuseTimes(t, s.cfg, s.dataDir, 1); !said[0] {
				t.Fatal("the refusal was not said")
			}

			opts := tc.then(t, s)
			rec := loggingtest.Record(t)
			code, served, _, stderr := runServeWith(t, opts)
			if served != tc.serves || (!served && code != 1) {
				t.Fatalf("serve served %v, exit %d, want served %v; stderr=%s", served, code, tc.serves, stderr)
			}
			for _, line := range []string{back, over} {
				want := 0
				if line == tc.says {
					want = 1
				}
				if got := rec.Carrying(line); len(got) != want {
					t.Errorf("the start said %q %d time(s), want %d:\n%s", line, len(got), want, strings.Join(rec.All(), "\n"))
				}
			}
			_, err := os.Stat(refusalNoteFile(s.cfg))
			if tc.serves && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the note outlived the start that got past the refusal: %v", err)
			}
			if !tc.serves && err != nil {
				t.Errorf("a start refused after the check removed the note: %v", err)
			}
		})
	}
}

// TestTheEndOfARefusalCallsBackOnlyTheDataDirItRefused: the line that a
// refused data dir is back is said only where the note names the data dir
// this start's config names and that data dir held its database or its lock
// file before this start's preparation; a data dir that held neither (the
// record removed with the volume still away), and a note naming another data
// dir (one a start that could not remove it left), end in the line that the
// refusal no longer applies.
func TestTheEndOfARefusalCallsBackOnlyTheDataDirItRefused(t *testing.T) {
	const back, over = "holds its database or its lock file again", "no longer applies"
	for _, tc := range []struct {
		name               string
		refused, startsOn string
		held               bool
		says               string
	}{
		{"the data dir the note names, back", "/mnt/usb/bridge", "/mnt/usb/bridge", true, back},
		{"the data dir the note names, holding neither file", "/mnt/usb/bridge", "/mnt/usb/bridge", false, over},
		{"another data dir than the note names, holding its files", "/mnt/nas/bridge", "/mnt/usb/bridge", true, over},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), ".bridge.yaml.datadir")
			sayServeRefusal(&dataDirGoneError{dataDir: tc.refused, record: record}, io.Discard, time.Now())
			rec := loggingtest.Record(t)
			endServeRefusal(record, tc.startsOn, tc.held)
			for _, line := range []string{back, over} {
				want := 0
				if line == tc.says {
					want = 1
				}
				if got := rec.Carrying(line); len(got) != want {
					t.Errorf("the start said %q %d time(s), want %d:\n%s", line, len(got), want, strings.Join(rec.All(), "\n"))
				}
			}
		})
	}
}

// TestServeSaysTheRefusalOfADataDirItsNoteDoesNotName: the note is of one
// state, the refusal of the data dir it names, so a refusal of another data
// dir is said, with the note just written, and the start after it is quiet.
func TestServeSaysTheRefusalOfADataDirItsNoteDoesNotName(t *testing.T) {
	record := filepath.Join(t.TempDir(), ".bridge.yaml.datadir")
	now := time.Now()
	var said []bool
	for i, dataDir := range []string{"/mnt/usb/bridge", "/mnt/usb/bridge", "/mnt/nas/bridge", "/mnt/nas/bridge"} {
		var out strings.Builder
		sayServeRefusal(&dataDirGoneError{dataDir: dataDir, record: record}, &out, now.Add(time.Duration(i)*2*time.Second))
		said = append(said, strings.Contains(out.String(), "serve: the data directory "+dataDir+" held this bridge's database"))
	}
	if !slices.Equal(said, []bool{true, false, true, false}) {
		t.Errorf("starts refusing two data dirs in turn said the refusal %v, want [true false true false]", said)
	}
}

// TestTheRefusalsNoteMakesNoDirectory: the note goes beside the record, in
// the directory the record was read from; a directory that went since (the
// config's, inside the data dir, on the volume that went) is not made again
// for it, which would make the data dir being refused in the bare mount
// point. The refusal is said all the same, and the next start says it again.
func TestTheRefusalsNoteMakesNoDirectory(t *testing.T) {
	base := t.TempDir()
	mountPoint := filepath.Join(base, "usb")
	if err := os.Mkdir(mountPoint, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(mountPoint, "bridge")
	record := filepath.Join(dataDir, "config", ".bridge.yaml.datadir")
	var out strings.Builder
	sayServeRefusal(&dataDirGoneError{dataDir: dataDir, record: record}, &out, time.Now())
	if !strings.Contains(out.String(), dataDirGoneLine(dataDir)) {
		t.Errorf("the refusal was not said:\n%s", out.String())
	}
	if entries, err := os.ReadDir(mountPoint); err != nil || len(entries) != 0 {
		var names []string
		_ = filepath.WalkDir(mountPoint, func(p string, _ fs.DirEntry, _ error) error {
			if r, err := filepath.Rel(mountPoint, p); err == nil && r != "." {
				names = append(names, filepath.ToSlash(r))
			}
			return nil
		})
		t.Errorf("the note made %v in the bare mount point (%v)", names, err)
	}
}

// TestAStartThatCannotRemoveTheNoteSaysSo: the start that finds its data dir
// again and cannot remove the note (its directory made read-only since) says
// at Warn that a refusal soon after may not be said, and leaves the note.
func TestAStartThatCannotRemoveTheNoteSaysSo(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory whose mode refuses this user a removal")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, ".bridge.yaml.datadir")
	sayServeRefusal(&dataDirGoneError{dataDir: "/mnt/usb/bridge", record: record}, io.Discard, time.Now())
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	rec := loggingtest.Record(t)
	endServeRefusal(record, "/mnt/usb/bridge", true)
	if lines := rec.Carrying("could not remove the note that serve refused its data directory"); len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN") {
		t.Errorf("a start that could not remove the note said so %d time(s), want once at Warn:\n%s", len(lines), strings.Join(rec.All(), "\n"))
	}
	if lines := rec.Carrying("holds its database or its lock file again"); len(lines) != 0 {
		t.Errorf("a start that could not remove the note said the refusal was over:\n%s", strings.Join(lines, "\n"))
	}
	if _, err := os.Stat(record + dataDirRefusalSuffix); err != nil {
		t.Errorf("the note that could not be removed is not there: %v", err)
	}
}

// TestServeSaysItsRefusalAtEveryStartWhereItCannotKeepTheNote — the control,
// which passes on main: where the config's directory takes no new file, no
// note is kept, so every start says the refusal again, and refuses as
// before.
func TestServeSaysItsRefusalAtEveryStartWhereItCannotKeepTheNote(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory whose mode refuses this user a new file")
	}
	base := t.TempDir()
	cfgDir := filepath.Join(base, "config")
	lib := filepath.Join(base, "Music")
	mountPoint := filepath.Join(base, "usb")
	for _, d := range []string{cfgDir, lib, mountPoint} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := filepath.Join(mountPoint, "bridge")
	cfg := writeServeConfig(t, filepath.Join(cfgDir, "bridge.yaml"), lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
	writeDataDirRecord(t, cfg, dataDir)
	if err := os.Chmod(cfgDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })
	if said := refuseTimes(t, cfg, dataDir, 3); !said[0] || !said[1] || !said[2] {
		t.Errorf("starts said the refusal %v where no note can be kept, want every one", said)
	}
}
