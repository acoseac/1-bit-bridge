package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	t.Cleanup(atomicwrite.SetMediumForTest(onStorageThatOutlivesAReboot))
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
	t.Cleanup(atomicwrite.SetMediumForTest(onStorageThatOutlivesAReboot))
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
	t.Cleanup(atomicwrite.SetMediumForTest(onStorageThatOutlivesAReboot))
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
// directory above it that cannot be read, or is not a directory; an error
// that names no directory falls back to the reservation file's; and mounts
// the platform gives no identity for are named as that, never as a
// directory. The barrier and the probe are the test's, so it runs on every
// platform.
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
	t.Cleanup(atomicwrite.SetMediumForTest(onStorageThatOutlivesAReboot))
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
		{"a platform that gives no mount identity", func(string) error {
			return fmt.Errorf("%w: the platform gives no mount ID for %s", atomicwrite.ErrMountsUnknown, data)
		}, "player: the platform gives no mount identity for the data directory " + data},
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

// onStorageThatOutlivesAReboot is the medium check for a test that drives
// the real writer over a barrier of its own: the medium, like the barrier, is
// the test's, since the test's temporary directory may itself be on tmpfs,
// which the writer refuses (TestNewRefusesADataDirectoryThatDoesNotOutliveAReboot
// is that refusal's).
func onStorageThatOutlivesAReboot(string) error { return nil }

// TestNewRefusesADataDirectoryThatDoesNotOutliveAReboot pins New's refusal of
// a data directory on storage that does not keep what is written to it
// through a reboot, the orchestrator's decision on the Claude API's round 9
// finding (fail closed): tmpfs, ramfs, or an overlay whose upper layer is one,
// as Raspberry Pi OS's "Overlay File System" sets up, where every barrier
// answers and a reboot takes the reservation and keeps player-uuid, so the
// player would hand out its IDs and tokens again with no crash at all. The
// refusal names the data directory, says the IDs and tokens would repeat after
// a reboot, and says to put the data directory on persistent storage; so does
// the refusal of storage that cannot be told to outlive one. Through the real
// probe, whose barrier and medium are the test's.
func TestNewRefusesADataDirectoryThatDoesNotOutliveAReboot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the strict writer asks no medium where the platform gives no mount identity: Windows refuses before it looks (ErrNoDirectoryBarrier)")
	}
	h := newHarness(t)
	data, err := filepath.EvalSymlinks(h.dir)
	h.must("resolve the data directory", err)
	// Registered before the engine is built, so it is restored after every
	// engine goroutine that could read it has ended.
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	h.be = newFakeBackend(t)
	for _, tc := range []struct {
		name   string
		medium error
		is     error
		begins string
	}{
		{"tmpfs", fmt.Errorf("%w: %s is on tmpfs", atomicwrite.ErrVolatile, data), atomicwrite.ErrVolatile,
			"player: the data directory " + data + " is on storage that does not outlive a reboot"},
		{"storage that cannot be told", fmt.Errorf("%w: %s is on an overlay whose upper layer cannot be read here", atomicwrite.ErrMediumUnknown, data), atomicwrite.ErrMediumUnknown,
			"player: whether the data directory " + data + " outlives a reboot cannot be told"},
	} {
		restore := atomicwrite.SetMediumForTest(func(string) error { return tc.medium })
		cfg := h.config()
		cfg.DataDir, cfg.ReservePath = data, filepath.Join(data, "reserve.json")
		cfg.seams.probe = nil
		_, err := New(cfg)
		restore()
		switch {
		case err == nil:
			t.Errorf("%s: New built an engine", tc.name)
		case !errors.Is(err, tc.is) || !strings.HasPrefix(err.Error(), tc.begins):
			t.Errorf("%s: New = %q, want it to begin %q", tc.name, err, tc.begins)
		case !strings.Contains(err.Error(), "the player's IDs and tokens would repeat after a reboot") || !strings.Contains(err.Error(), "put the data directory on persistent storage"):
			t.Errorf("%s: New = %q, want it to say the player's IDs and tokens would repeat after a reboot, and to put the data directory on persistent storage", tc.name, err)
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
// directory (New never creates it), and the reservation file must lie
// plainly below it: not outside it, not spelled with a ".." element (with a
// link before it, round 6 reserved in one file and read another), and, where
// symbolic links are to be had, through no link below it, whether the link
// leads out of it or stays inside; each is refused with nothing written. The
// probe is the real one, over a barrier the test answers, so it runs on
// Windows too.
func TestNewRequiresADataDirectoryTheReservationLiesBelow(t *testing.T) {
	restore := atomicwrite.SetSyncDirForTest(func(string) error { return nil })
	t.Cleanup(restore)
	t.Cleanup(atomicwrite.SetMediumForTest(onStorageThatOutlivesAReboot))
	h := newHarness(t)
	h.be = newFakeBackend(t)
	data := filepath.Join(h.dir, "data")
	file := filepath.Join(h.dir, "a-file")
	outside := filepath.Join(h.dir, "outside")
	real := filepath.Join(data, "real", "sub")
	for _, d := range []string{data, outside, real} {
		h.must("make "+d, os.MkdirAll(d, 0o700))
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
		{"a reservation file with a \"..\" element", data, data + string(filepath.Separator) + "real" + string(filepath.Separator) + ".." + string(filepath.Separator) + "queue.json", "is not below the data directory", atomicwrite.ErrNotUnderRoot},
	}
	if link := filepath.Join(data, "link"); os.Symlink(outside, link) == nil {
		inner := filepath.Join(data, "inner")
		h.must("link inside the data directory", os.Symlink(real, inner))
		refusals = append(refusals,
			refusal{"a reservation file a link below it leads out of", data, filepath.Join(link, "queue.json"), "is not below the data directory", atomicwrite.ErrNotUnderRoot},
			refusal{"a reservation file through a link that stays inside it", data, filepath.Join(inner, "queue.json"), "is not below the data directory", atomicwrite.ErrNotUnderRoot},
			refusal{"a reservation file through a link and back", data, inner + string(filepath.Separator) + ".." + string(filepath.Separator) + "queue.json", "is not below the data directory", atomicwrite.ErrNotUnderRoot})
	} else {
		t.Log("no symbolic link here: the cases of a link below the data directory are not run")
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

// TestNewRefusesAFilePathThatNamesADirectory pins a queue or reservation
// file path whose last element names a directory, a trailing separator or a
// ".": New refuses it, naming the path, with nothing written, so no run takes
// it and leaves its restart stuck. Codex on round 7: round 7's strict writer
// cleaned data/ids/ to data/ids and wrote the reservation there, the engine
// read data/ids/ at the restart, which a regular file answers ENOTDIR, could
// not move it aside under that name either, and refused every insert for
// ever (ReasonFileStuck); a queue path so spelled made the queue writer
// create a directory in the file's place. Should New take such a path, the
// test runs it through a restart to say how it breaks. The probe is the real
// one, over a barrier the test answers, so it runs on Windows too.
func TestNewRefusesAFilePathThatNamesADirectory(t *testing.T) {
	restore := atomicwrite.SetSyncDirForTest(func(string) error { return nil })
	t.Cleanup(restore)
	t.Cleanup(atomicwrite.SetMediumForTest(onStorageThatOutlivesAReboot))
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		name               string
		queuePath, reserve string
		says               string
	}{
		{"a reservation path with a trailing separator", "queue.json", "ids" + sep, "reservation"},
		{"a reservation path ending in \".\"", "queue.json", "ids" + sep + ".", "reservation"},
		{"a queue path with a trailing separator", "queue.json" + sep, "", "queue"},
		{"a queue path ending in \".\"", "queue.json" + sep + ".", "", "queue"},
		// Its reservation path, x/...ids, is a plain file's: only the queue
		// path's own check can refuse it.
		{"a queue path ending in \"..\"", "x" + sep + "..", "", "queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.tweak = func(c *Config) {
				c.QueuePath = h.dir + sep + tc.queuePath
				c.ReservePath = ""
				if tc.reserve != "" {
					c.ReservePath = h.dir + sep + tc.reserve
				}
				c.seams.writeReserve, c.seams.probe = nil, nil
			}
			h.be = newFakeBackend(t)
			cfg := h.config()
			e, err := New(cfg)
			if err == nil {
				// Round 7's way: run it, take an ID, and start again on the same
				// files.
				h.eng = e
				h.run()
				h.insert("a")
				h.shutdown()
				h.joinAfter(e)
				h.be = newFakeBackend(t)
				again, err := New(h.config())
				refused := "New refused it: " + fmt.Sprint(err)
				if err == nil {
					refused = string(again.Snapshot().Edits().Refused)
				}
				t.Fatalf("New took the %s path; at the restart the edits are refused with %q", tc.says, refused)
			}
			if !strings.Contains(err.Error(), "names a directory") || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("New = %v, want it to say the %s file's path names a directory", err, tc.says)
			}
			if entries, rerr := os.ReadDir(h.dir); rerr != nil || len(entries) != 0 {
				t.Errorf("a refused New left %v (%v)", entries, rerr)
			}
		})
	}
}

// TestADataDirectoryThatIsALinkIsTaken pins the layout some installs have:
// the data directory a symbolic link to where the data lives. The strict
// writer resolves it once, so New starts, the reservation is written
// through the real writer below where the link points, and a restart reads
// it back: the IDs go on past the first run's. The barrier is the test's, so
// it runs on Windows too, where symbolic links can be made.
func TestADataDirectoryThatIsALinkIsTaken(t *testing.T) {
	restore := atomicwrite.SetSyncDirForTest(func(string) error { return nil })
	t.Cleanup(restore)
	t.Cleanup(atomicwrite.SetMediumForTest(onStorageThatOutlivesAReboot))
	h := newHarness(t)
	real := filepath.Join(h.dir, "real")
	link := filepath.Join(h.dir, "link")
	h.must("make the data", os.Mkdir(real, 0o700))
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symbolic link here: %v", err)
	}
	h.tweak = func(c *Config) {
		c.DataDir, c.QueuePath = link, filepath.Join(link, "queue.json")
		c.seams.writeReserve, c.seams.probe = nil, nil
	}
	h.boot()
	first := h.insert("a", "b")
	h.restart()
	if _, err := os.Stat(filepath.Join(real, "queue.json.ids")); err != nil {
		t.Fatalf("the reservation is not where the data directory's link points: %v", err)
	}
	if id := h.insert("c")[0]; id <= first[1] {
		t.Fatalf("after a restart through the link, ID %d, not past %d", id, first[1])
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

// TestNewFramesAnOverlayBindAsACrashRisk pins New's refusal through its real
// probe: persistent storage does not fix a bind's unreachable copy-ups.
func TestNewFramesAnOverlayBindAsACrashRisk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses before the medium check")
	}
	h := newHarness(t)
	data, err := filepath.EvalSymlinks(h.dir)
	h.must("resolve", err)
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error {
		return fmt.Errorf("%w: use the overlay's own mount", atomicwrite.ErrOverlayBind)
	}))
	h.be = newFakeBackend(t)
	cfg := h.config()
	cfg.DataDir, cfg.ReservePath = data, filepath.Join(data, "reserve.json")
	cfg.seams.probe = nil
	_, err = New(cfg)
	if !errors.Is(err, atomicwrite.ErrOverlayBind) || !errors.Is(err, atomicwrite.ErrMediumUnknown) {
		t.Fatalf("New = %v, want both bind and medium markers", err)
	}
	for _, want := range []string{data, "crash", "persistent storage", "IDs and tokens", "overlay's own mount"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New = %v, missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "reboot") || strings.Contains(err.Error(), "put the data directory on persistent storage") {
		t.Errorf("New = %v, wrongly frames the bind as volatile storage", err)
	}
}

// TestTheReserveWriteSeamHoldsAndFailsTheWrite pins Config.Seams.ReserveWrite,
// through which the OpenHome surface's tests hold the engine in a reservation
// write (B470 PR 5a4, round 2): it runs before each write, New's first among
// them; an error it returns fails that write as the filesystem's refusal
// would, so no token is published and the edits that need a value are
// refused until one lands; while it blocks, an insert that needs a value
// parks behind the write, answering nothing, and it lands once the seam
// returns nil. Every other test runs with no Seams, which changes nothing.
func TestTheReserveWriteSeamHoldsAndFailsTheWrite(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 8)
	hold := newGate()
	h := newHarness(t)
	h.tweak = func(c *Config) {
		c.Seams = &Seams{ReserveWrite: func() error {
			entered <- struct{}{}
			if calls.Add(1) == 1 {
				return errors.New("read-only file system")
			}
			<-hold
			return nil
		}}
	}
	h.boot()
	// Registered after boot, so it runs before the engine's shutdown, which
	// waits for a reservation write under way.
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			hold.open()
		}
	})
	<-entered
	if n := calls.Load(); n != 1 {
		t.Fatalf("the seam ran %d times by New's return, want once, for the run's first write", n)
	}
	h.files.mu.Lock()
	written := h.files.reserveCalls
	h.files.mu.Unlock()
	if written != 0 {
		t.Fatalf("the write the seam failed reached the file %d times", written)
	}
	if tok, _ := h.snap().IDArray(); tok != 0 || h.snap().Edits().Refused != ReasonReservation {
		t.Fatalf("after the seam failed the run's first write: token %d, edits %+v; want no token and the edits refused", tok, h.snap().Edits())
	}
	insert := async(func() error { _, err := h.eng.InsertAfter(bg, 0, ref("a"), nil); return err })
	select {
	case <-entered:
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the insert's reservation write", *watchdog)
	}
	select {
	case err := <-insert:
		t.Fatalf("the insert answered %v while the seam holds its write", err)
	default:
	}
	hold.open()
	h.must("the insert once its write lands", h.result("the insert", insert))
	if n := h.snap().Len(); n != 1 {
		t.Fatalf("the queue holds %d entries, want the insert's", n)
	}
	if tok, _ := h.snap().IDArray(); tok == 0 || h.snap().Edits().Refused != "" {
		t.Fatalf("once the write landed: token %d, edits %+v", tok, h.snap().Edits())
	}
}
