package engine

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// TestAFailingReservationWriteRefusesEdits pins a reservation that cannot
// be made durable: every insert and every other token-advancing edit is
// refused with the reason, the rest still works, the write is never retried
// in the background, and the next edit that needs it tries again.
func TestAFailingReservationWriteRefusesEdits(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	h.restartWith(func() {
		h.files.failReserve = errors.New("read-only file system")
	})
	if ed := h.snap().Edits(); ed.Refused != ReasonReservation {
		t.Fatalf("edits %+v, want refused %s", ed, ReasonReservation)
	}
	_, err := h.eng.InsertAfter(bg, 0, ref("x"), nil)
	h.refused("insert", err, ReasonReservation)
	h.refused("delete", h.eng.Delete(bg, ids[0]), ReasonReservation)
	h.refused("delete all", h.eng.DeleteAll(bg), ReasonReservation)
	h.must("repeat, which advances no token", h.eng.SetRepeat(bg, true))
	h.available()
	h.must("play, which advances no token", h.eng.PlayID(bg, ids[1]))
	if h.snap().Len() != 2 {
		t.Fatal("a refused edit changed the queue")
	}
	h.files.mu.Lock()
	tried := h.files.reserveAttempts
	h.files.mu.Unlock()
	h.clk.Advance(time.Hour)
	h.files.mu.Lock()
	again := h.files.reserveAttempts
	h.files.failReserve = nil
	h.files.mu.Unlock()
	if again != tried {
		t.Fatalf("the reservation was retried in the background: %d attempts, then %d", tried, again)
	}
	if _, err := h.eng.InsertAfter(bg, 0, ref("x"), nil); err != nil {
		t.Fatalf("an insert once the write can land: %v", err)
	}
	if ed := h.snap().Edits(); ed.Refused != "" {
		t.Fatalf("edits %+v once the reservation landed", ed)
	}
}

// TestAQueueSavedWithoutItsRunsReservationLoads pins the file a run whose
// reservation failed writes: its counters are past the reservation, and the
// next start loads it as it was.
func TestAQueueSavedWithoutItsRunsReservationLoads(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	h.restartWith(func() { h.files.failReserve = errors.New("read-only file system") })
	h.must("repeat", h.eng.SetRepeat(bg, true))
	h.restartWith(func() { h.files.failReserve = nil })
	sn := h.snap()
	if got := sn.IDs(); len(got) != 2 || got[0] != ids[0] || !sn.Repeat() {
		t.Fatalf("the queue saved by a run without its reservation loaded as %v, repeat %v", got, sn.Repeat())
	}
	if aside, _ := filepath.Glob(h.queuePath() + ".bad-*"); len(aside) != 0 {
		t.Fatalf("a good queue file was moved aside: %v", aside)
	}
}

// TestAStartWithoutItsReservationPublishesNoToken pins the start's token:
// published only once a durable reservation covers it, and 0, none, until
// then. Two starts in a row whose reservation write fails, or whose bad
// reservation file stays where it is, with no queue write between them,
// publish no token, so never the same one twice; the first start that can
// reserve publishes one past every token an earlier run published.
func TestAStartWithoutItsReservationPublishesNoToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(h *harness)
		mend    func(h *harness)
		refused Reason
	}{
		{"the write fails", func(h *harness) {
			h.files.failReserve = errors.New("read-only file system")
		}, func(h *harness) {
			h.files.failReserve = nil
		}, ReasonReservation},
		{"the bad file stays", func(h *harness) {
			h.must("corrupt", os.WriteFile(h.reservePath(), []byte("not json"), 0o600))
			h.files.renameErr = errors.New("read-only file system")
		}, func(h *harness) {
			h.files.renameErr = nil
		}, ReasonFileStuck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			h.insert("a", "b")
			published := h.snap().Token()
			var tokens []uint32
			h.restartWith(func() { tc.breakIt(h) })
			for run := 0; run < 2; run++ {
				if run > 0 {
					h.restartWith(func() {})
				}
				sn := h.snap()
				if ed := sn.Edits(); ed.Refused != tc.refused {
					t.Fatalf("start %d: edits %+v, want refused %s", run+1, ed, tc.refused)
				}
				tokens = append(tokens, sn.Token())
			}
			if tokens[0] != 0 || tokens[1] != 0 {
				t.Fatalf("two starts with no durable reservation published tokens %v, want none (0)", tokens)
			}
			h.restartWith(func() { tc.mend(h) })
			if tok := h.snap().Token(); tok <= published {
				t.Fatalf("the first start that can reserve published token %d, not past %d", tok, published)
			}
		})
	}
}

// TestAReservationIsMadeOnlyWithItsDirectoryEntry pins the reservation's
// durability through the engine's own writer, atomicwrite's: a reservation
// whose directory entry could not be fsynced is not made, so nothing it
// covers is handed out, and a crash that rolls the reservation file back
// to its last durable content (what an entry that was never fsynced
// allows) never yields an ID or a token a run handed out before. The
// barrier is the test's on every platform, one that answers and then one
// that fails, so it runs on Windows too, which has none of its own. New's
// probe is the harness's, which passes, so the barrier fails for the writes
// alone, as one that fails now and then does; one that fails every time is
// New's refusal (TestNewRefusesADirectoryThatCannotBeSynced).
func TestAReservationIsMadeOnlyWithItsDirectoryEntry(t *testing.T) {
	var broken atomic.Bool
	errNoSync := errors.New("fsync: input/output error")
	// Registered before the harness, so it is restored after every engine
	// goroutine that could read it has ended.
	restore := atomicwrite.SetSyncDirForTest(func(string) error {
		if broken.Load() {
			return errNoSync
		}
		return nil
	})
	t.Cleanup(restore)
	h := newHarness(t)
	h.tweak = func(c *Config) { c.seams.writeReserve = nil }
	h.boot()
	seen := map[uint32]bool{}
	for _, id := range h.insert("a", "b") {
		seen[id] = true
	}
	published := h.snap().Token()
	h.shutdown()
	h.joinAfter(h.eng)
	durable, err := os.ReadFile(h.reservePath())
	h.must("read the reservation file", err)

	broken.Store(true)
	h.boot()
	sn := h.snap()
	published = max(published, sn.Token())
	if ed := sn.Edits(); ed.Refused != ReasonReservation {
		t.Errorf("edits %+v with the reservation's directory entry not durable, want refused %s", ed, ReasonReservation)
	}
	if id, err := h.eng.InsertAfter(bg, 0, ref("x"), nil); err == nil {
		seen[id] = true
		published = max(published, h.snap().Token())
		t.Errorf("an insert took ID %d with the reservation's directory entry not durable", id)
	}
	// A crash: this run's queue writes are lost, and the reservation
	// file's renames roll back to the last durable one.
	h.files.mu.Lock()
	h.files.dropQueue = true
	h.files.mu.Unlock()
	h.shutdown()
	h.joinAfter(h.eng)
	h.must("roll the reservation file back", os.WriteFile(h.reservePath(), durable, 0o600))
	h.files.mu.Lock()
	h.files.dropQueue = false
	h.files.mu.Unlock()
	broken.Store(false)
	h.boot()
	if tok := h.snap().Token(); tok <= published {
		t.Fatalf("after the rollback the token is %d, not past %d, which a run published", tok, published)
	}
	if id := h.insert("y")[0]; seen[id] {
		t.Fatalf("after the rollback ID %d was handed out again", id)
	}
}

// TestNewRefusesADirectoryThatCannotBeSynced pins New's probe of the
// barrier: a reservation directory whose fsync fails on every call, as on a
// mount that cannot sync a directory, is a configuration error that names
// the directory and says where the data directory must go, at every start,
// with nothing read, moved aside or written; once the directory can be
// synced, the queue loads as it was. Without the probe the engine started
// and refused every edit for ever (reservation_not_durable), with nothing
// naming the cause. The barrier is the test's, so it runs on Windows too.
func TestNewRefusesADirectoryThatCannotBeSynced(t *testing.T) {
	var broken atomic.Bool
	errNoSync := errors.New("sync dir: invalid argument")
	// Registered before the harness, so it is restored after every engine
	// goroutine that could read it has ended.
	restore := atomicwrite.SetSyncDirForTest(func(string) error {
		if broken.Load() {
			return errNoSync
		}
		return nil
	})
	t.Cleanup(restore)
	h := newHarness(t)
	h.tweak = func(c *Config) { c.seams.writeReserve, c.seams.probe = nil, nil }
	h.boot()
	ids := h.insert("a", "b")
	h.shutdown()
	h.joinAfter(h.eng)
	queue, err := os.ReadFile(h.queuePath())
	h.must("read the queue file", err)
	reserved, err := os.ReadFile(h.reservePath())
	h.must("read the reservation file", err)

	broken.Store(true)
	for start := 1; start <= 2; start++ {
		_, err := New(h.config())
		switch {
		case err == nil:
			t.Fatalf("start %d: New over a directory that cannot be synced built an engine, want a configuration error", start)
		case !errors.Is(err, errNoSync):
			t.Fatalf("start %d: New = %v, want the barrier's failure", start, err)
		case !strings.Contains(err.Error(), h.dir) || !strings.Contains(err.Error(), "a filesystem that can sync a directory"):
			t.Fatalf("start %d: New = %q, want it to name the directory %s and where the data directory must go", start, err, h.dir)
		}
	}
	if got, _ := os.ReadFile(h.queuePath()); !bytes.Equal(got, queue) {
		t.Fatal("a refused start changed the queue file")
	}
	if got, _ := os.ReadFile(h.reservePath()); !bytes.Equal(got, reserved) {
		t.Fatal("a refused start changed the reservation file")
	}
	if aside, _ := filepath.Glob(filepath.Join(h.dir, "*.bad-*")); len(aside) != 0 {
		t.Fatalf("a refused start moved files aside: %v", aside)
	}

	broken.Store(false)
	h.boot()
	if got := h.snap().IDs(); !slices.Equal(got, ids) {
		t.Fatalf("once the directory syncs the queue loads as %v, want %v", got, ids)
	}
}

// TestNewRefusesAReservationDirectoryThatIsAFile pins New's probe on a
// reservation path whose directory is a regular file: the barrier, which
// opens and fsyncs whatever is there, answers nil for a file, and the
// reservation could never be written, so New refuses it, naming the file as
// what is not a directory. The barrier is the test's, answering nil, so only
// the probe's own check can refuse, on Windows too.
func TestNewRefusesAReservationDirectoryThatIsAFile(t *testing.T) {
	restore := atomicwrite.SetSyncDirForTest(func(string) error { return nil })
	t.Cleanup(restore)
	h := newHarness(t)
	file := filepath.Join(h.dir, "existing-file")
	h.must("write the file", os.WriteFile(file, []byte("x"), 0o600))
	h.be = newFakeBackend(t)
	cfg := h.config()
	cfg.ReservePath = filepath.Join(file, "ids")
	cfg.seams.probe = nil
	_, err := New(cfg)
	if !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("New with the reservation's directory a regular file = %v, want a refusal naming %s as not a directory", err, file)
	}
}

// TestNewSaysWhenTheReservationDirectoryCannotBeRead pins New's refusal of
// a reservation directory the probe could not read, as a Stat that fails
// with EACCES or EIO: the error names the directory, says it cannot be
// read, and carries the probe's error. It does not advise moving the data
// directory to a filesystem that can sync a directory: that is for a
// barrier that ran and failed (atomicwrite.ErrBarrierFailed,
// TestNewRefusesADirectoryThatCannotBeSynced), and nothing failed to sync
// here. The probe is the test's, so it runs on every platform.
func TestNewSaysWhenTheReservationDirectoryCannotBeRead(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.EIO} {
		t.Run(errno.Error(), func(t *testing.T) {
			h := newHarness(t)
			h.be = newFakeBackend(t)
			cfg := h.config()
			cfg.seams.probe = func(string) error { return &fs.PathError{Op: "stat", Path: h.dir, Err: errno} }
			_, err := New(cfg)
			switch {
			case err == nil:
				t.Fatal("New over a reservation directory it cannot read built an engine")
			case !errors.Is(err, errno):
				t.Fatalf("New = %v, want the probe's %v", err, errno)
			case !strings.Contains(err.Error(), h.dir) || !strings.Contains(err.Error(), "cannot be read"):
				t.Fatalf("New = %q, want it to say the directory %s cannot be read", err, h.dir)
			case strings.Contains(err.Error(), "a filesystem that can sync a directory"):
				t.Fatalf("New = %q: it advises moving the data directory for a directory it could not read", err)
			}
		})
	}
}

// TestNewNamesTheDirectoryTheProbeRefused pins the directory New's refusal
// names: the one the probe's error names, the reservation file's directory or
// one above it up to the data directory. Argon on round 6: a barrier that
// failed on the directory above the reservation file's was blamed on the
// reservation file's own, which had synced. Here through the real probe, its
// barrier failing for that directory alone, and through the probe's seam, a
// directory above it that cannot be read, or is not a directory; and an error
// that names no directory falls back to the reservation file's. The barrier
// and the probe are the test's, so it runs on every platform.
func TestNewNamesTheDirectoryTheProbeRefused(t *testing.T) {
	h := newHarness(t)
	// The data directory as its links resolve (the system's temporary
	// directory is a link on macOS), so the barrier is asked for the names
	// built here.
	data, err := filepath.EvalSymlinks(h.dir)
	h.must("resolve the data directory", err)
	above := filepath.Join(data, "player")
	own := filepath.Join(above, "ids")
	h.must("make the reservation's directory", os.MkdirAll(own, 0o700))
	reserve := filepath.Join(own, "reserve.json")
	errNoSync := errors.New("invalid argument")
	// Registered before the engine is built, so it is restored after every
	// engine goroutine that could read it has ended.
	restore := atomicwrite.SetSyncDirForTest(func(dir string) error {
		if dir == above {
			return errNoSync
		}
		return nil
	})
	t.Cleanup(restore)
	h.be = newFakeBackend(t)
	for _, tc := range []struct {
		name  string
		probe func(string) error // nil: the real one
		// begins is how the refusal begins: it names the directory that
		// failed.
		begins string
	}{
		{"a barrier that failed above the reservation's directory", nil, "player: the directory " + above + " cannot be synced"},
		{"a directory above it that cannot be read", func(string) error {
			return &fs.PathError{Op: "open", Path: above, Err: syscall.EACCES}
		}, "player: the directory " + above + " cannot be read"},
		{"a directory above it that is a regular file", func(string) error {
			return &fs.PathError{Op: "probe", Path: above, Err: syscall.ENOTDIR}
		}, "player: " + above + " is not a directory"},
		{"an error that names no directory", func(string) error { return syscall.EIO }, "player: the directory " + own + " cannot be read"},
	} {
		cfg := h.config()
		cfg.DataDir, cfg.ReservePath = data, reserve
		cfg.seams.probe = tc.probe
		_, err := New(cfg)
		if err == nil {
			t.Errorf("%s: New built an engine", tc.name)
			continue
		}
		if !strings.HasPrefix(err.Error(), tc.begins) {
			t.Errorf("%s: New = %q, want it to begin %q", tc.name, err, tc.begins)
		}
		if !strings.HasPrefix(tc.begins, "player: the directory "+own) && strings.Contains(err.Error(), own+" ") {
			t.Errorf("%s: New = %q, which blames the reservation file's own directory %s", tc.name, err, own)
		}
	}
}

// TestABadReservationFileIsMovedAside pins the reservation file's own
// checks: moved aside, logged, and the IDs then start past the queue
// file's.
func TestABadReservationFileIsMovedAside(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	h.shutdown()
	h.joinAfter(h.eng)
	h.must("corrupt", os.WriteFile(h.reservePath(), []byte("not json"), 0o600))
	h.boot()
	aside, _ := filepath.Glob(h.reservePath() + ".bad-*")
	if len(aside) != 1 {
		t.Fatalf("moved aside: %v", aside)
	}
	if id := h.insert("c")[0]; id <= ids[1] {
		t.Fatalf("an ID after a bad reservation file is %d, not past the queue's %d", id, ids[1])
	}
	if h.logs.count("moved aside") != 1 {
		t.Fatalf("logs %v", h.logs.all())
	}
}

// TestNewRequiresADataDirectoryTheReservationLiesBelow pins New's checks of
// the reservations' durable root: a DataDir is required, must exist and be a
// directory (New never creates it), and the reservation file must lie below
// it, as written and, where symbolic links are to be had, as its links
// resolve; each is refused with nothing written. The probe is the real one,
// over a barrier the test answers, so it runs on Windows too.
func TestNewRequiresADataDirectoryTheReservationLiesBelow(t *testing.T) {
	restore := atomicwrite.SetSyncDirForTest(func(string) error { return nil })
	t.Cleanup(restore)
	h := newHarness(t)
	h.be = newFakeBackend(t)
	data := filepath.Join(h.dir, "data")
	file := filepath.Join(h.dir, "a-file")
	outside := filepath.Join(h.dir, "outside")
	for _, d := range []string{data, outside} {
		h.must("make "+d, os.Mkdir(d, 0o700))
	}
	h.must("write the file", os.WriteFile(file, []byte("x"), 0o600))
	type refusal struct {
		name               string
		dataDir, queuePath string
		says               string
		is                 error
	}
	refusals := []refusal{
		{"no DataDir", "", filepath.Join(data, "queue.json"), "a DataDir is required", nil},
		{"a DataDir that does not exist", filepath.Join(h.dir, "missing"), filepath.Join(h.dir, "missing", "queue.json"), "must exist", fs.ErrNotExist},
		{"a DataDir that is a regular file", file, filepath.Join(file, "queue.json"), "is not a directory", nil},
		{"a reservation file outside it", data, filepath.Join(outside, "queue.json"), "is not below the data directory", atomicwrite.ErrNotUnderRoot},
	}
	if link := filepath.Join(data, "link"); os.Symlink(outside, link) == nil {
		refusals = append(refusals, refusal{"a reservation file a link below it leads out of", data, filepath.Join(link, "queue.json"), "is not below the data directory", atomicwrite.ErrNotUnderRoot})
	} else {
		t.Log("no symbolic link here: the case of a link leading out of the data directory is not run")
	}
	for _, tc := range refusals {
		cfg := h.config()
		cfg.DataDir, cfg.QueuePath = tc.dataDir, tc.queuePath
		cfg.seams.probe, cfg.seams.writeReserve = nil, nil
		_, err := New(cfg)
		switch {
		case err == nil:
			t.Errorf("%s: New built an engine", tc.name)
		case !strings.Contains(err.Error(), tc.says) || (tc.is != nil && !errors.Is(err, tc.is)):
			t.Errorf("%s: New = %v, want it to say %q (%v)", tc.name, err, tc.says, tc.is)
		}
		if _, err := os.Lstat(tc.queuePath + ".ids"); err == nil {
			t.Errorf("%s: a refused New wrote the reservation file", tc.name)
		}
	}
}

// restartWith shuts the engine down, runs f, and boots another on the same
// files.
func (h *harness) restartWith(f func()) {
	h.t.Helper()
	old := h.eng
	h.shutdown()
	h.joinAfter(old)
	h.files.mu.Lock()
	f()
	h.files.mu.Unlock()
	h.boot()
}
