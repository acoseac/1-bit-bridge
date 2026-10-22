package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testQueueFile is the queue file's format as the tests read it, apart
// from the engine's own reader.
type testQueueFile struct {
	Version  int    `json:"version"`
	Revision uint64 `json:"revision"`
	IDs      struct {
		Next     uint64 `json:"next"`
		Reserved uint64 `json:"reserved"`
	} `json:"ids"`
	Tokens struct {
		Current  uint32 `json:"current"`
		Reserved uint64 `json:"reserved"`
	} `json:"tokens"`
	Repeat   bool   `json:"repeat"`
	Shuffle  bool   `json:"shuffle"`
	Selected uint32 `json:"selected"`
	Entries  []struct {
		ID   uint32 `json:"id"`
		Ref  string `json:"ref"`
		Meta []byte `json:"meta"`
	} `json:"entries"`
}

// readQueueFile reads queue file bytes.
func readQueueFile(t *testing.T, data []byte) testQueueFile {
	t.Helper()
	var f testQueueFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("the queue file does not parse: %v", err)
	}
	return f
}

// idsOfFile is a queue file's IDs in order.
func idsOfFile(f testQueueFile) []uint32 {
	var out []uint32
	for _, e := range f.Entries {
		out = append(out, e.ID)
	}
	return out
}

// TestAMaximumSizeQueueRoundTrips pins the file's cap against the real
// encoding of a full queue: 1,000 entries carrying 16 MiB of metadata,
// sized so base64 rounds up on each, and references every byte of which
// JSON escapes, saved and loaded back exactly.
func TestAMaximumSizeQueueRoundTrips(t *testing.T) {
	h := startHarness(t)
	worst := strings.Repeat(`"\<&`, MaxReferenceBytes/4)
	last := uint32(0)
	for i := 0; i < MaxEntries; i++ {
		n := 16777
		if i < 72 {
			n = 16780
		}
		meta := make([]byte, n)
		for j := range meta {
			meta[j] = byte(i*31 + j)
		}
		id, err := h.eng.InsertAfter(bg, last, ref(worst), meta)
		h.must("insert", err)
		last = id
	}
	h.must("repeat", h.eng.SetRepeat(bg, true))
	before := h.snap()
	if total := before.q.meta; total != MaxTotalMetadataBytes {
		t.Fatalf("the queue holds %d bytes of metadata, want %d", total, MaxTotalMetadataBytes)
	}
	h.restart()
	info, err := os.Stat(h.queuePath())
	h.must("stat the queue file", err)
	if info.Size() > maxQueueFileBytes {
		t.Fatalf("a full queue's file is %d bytes, over the cap %d", info.Size(), maxQueueFileBytes)
	}
	after := h.snap()
	if !slices.Equal(after.IDs(), before.IDs()) || after.Selected() != before.Selected() || !after.Repeat() {
		t.Fatalf("the reloaded queue differs: %d entries, selected %d, repeat %v", after.Len(), after.Selected(), after.Repeat())
	}
	for _, id := range before.IDs() {
		want, _ := before.Read(id)
		got, _ := after.Read(id)
		if got.Ref != want.Ref || !bytes.Equal(got.Meta, want.Meta) {
			t.Fatalf("entry %d reloaded differently", id)
		}
	}
	if after.Token() <= before.Token() {
		t.Fatalf("the token did not advance across the restart: %d then %d", before.Token(), after.Token())
	}
}

// TestABurstWritesABoundedNumberOfTimes pins the coalescing: 500 inserts
// over five seconds write at most once per maximum dirty age and once
// after, and the file after the flush is the state.
func TestABurstWritesABoundedNumberOfTimes(t *testing.T) {
	h := startHarness(t)
	for i := 0; i < 500; i++ {
		h.insert("t")
		h.clk.Advance(10 * time.Millisecond)
	}
	h.clk.Advance(writeCoalesce)
	h.waitSnap("written", func(sn *Snapshot) bool { p := sn.Persist(); return p.Written == p.Revision })
	if n, bound := h.files.writes(), int(5*time.Second/writeMaxAge)+2; n > bound {
		t.Fatalf("500 inserts over 5 s wrote %d times, more than %d", n, bound)
	}
	h.shutdown()
	sn := h.snap()
	data, err := os.ReadFile(h.queuePath())
	h.must("read the queue file", err)
	f := readQueueFile(t, data)
	if !slices.Equal(idsOfFile(f), sn.IDs()) || f.Selected != sn.Selected() || f.Tokens.Current != sn.Token() {
		t.Fatalf("the file after the flush is not the state: %d entries, selected %d, token %d", len(f.Entries), f.Selected, f.Tokens.Current)
	}
}

// TestTheWriterCoalescesAndBoundsTheDirtyAge pins the writer's timing on
// the clock: a change is written once writeCoalesce has passed with no
// other, and under changes that keep coming, writeMaxAge after the first.
// The writer's own report that it is waiting, with the newest revision it
// decided on, says it has seen a change without a sleep.
func TestTheWriterCoalescesAndBoundsTheDirtyAge(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var seen uint64
	idle := make(chan struct{}, 1)
	h.tweak = func(c *Config) {
		c.seams.writerWaits = func(rev uint64) {
			mu.Lock()
			seen = max(seen, rev)
			mu.Unlock()
			poke(idle)
		}
	}
	h.boot()
	waitIdle := func() {
		t.Helper()
		rev := h.snap().Persist().Revision
		timer := time.NewTimer(*watchdog)
		defer timer.Stop()
		for {
			mu.Lock()
			s := seen
			mu.Unlock()
			if s >= rev {
				return
			}
			select {
			case <-idle:
			case <-timer.C:
				t.Fatalf("waited %v for the writer to see revision %d", *watchdog, rev)
			}
		}
	}
	h.insert("a")
	waitIdle()
	if n := h.files.attempts(); n != 0 {
		t.Fatalf("a change was written at once, %d times, not after %v", n, writeCoalesce)
	}
	h.clk.Advance(writeCoalesce)
	h.files.waitFor(t, "the coalesced write", func() bool { return h.files.queueAttempts == 1 })

	start := h.clk.Now()
	for {
		h.insert("t")
		waitIdle()
		elapsed := h.clk.Now().Sub(start)
		if n := h.files.attempts(); n > 1 {
			if elapsed < writeMaxAge {
				t.Fatalf("changes every 100 ms were written after %v, before the maximum age %v", elapsed, writeMaxAge)
			}
			break
		}
		if elapsed > writeMaxAge {
			t.Fatalf("changes every 100 ms were not written %v after the first", elapsed)
		}
		h.clk.Advance(100 * time.Millisecond)
	}
}

// TestAnOlderWriteNeverLandsAfterANewerOne pins the one writer: a write
// held and released late lands, and then the newer state lands after it,
// never the other way round.
func TestAnOlderWriteNeverLandsAfterANewerOne(t *testing.T) {
	h := startHarness(t)
	hold := newGate()
	h.files.mu.Lock()
	h.files.holdQueue[1] = hold
	h.files.mu.Unlock()
	h.insert("a")
	h.clk.Advance(writeCoalesce)
	h.files.waitFor(t, "the first write", func() bool { return h.files.queueAttempts == 1 })
	h.insert("b", "c")
	h.clk.Advance(writeMaxAge)
	hold.open()
	sn := h.waitSnap("written", func(sn *Snapshot) bool { p := sn.Persist(); return p.Written == p.Revision })
	h.files.mu.Lock()
	writes := slices.Clone(h.files.queueWrites)
	h.files.mu.Unlock()
	var revs []uint64
	for _, w := range writes {
		revs = append(revs, readQueueFile(t, w).Revision)
	}
	if !slices.IsSorted(revs) || len(slices.Compact(slices.Clone(revs))) != len(revs) {
		t.Fatalf("writes landed in the order %v", revs)
	}
	data, err := os.ReadFile(h.queuePath())
	h.must("read the queue file", err)
	if f := readQueueFile(t, data); f.Revision != sn.Persist().Revision || len(f.Entries) != 3 {
		t.Fatalf("the file holds revision %d with %d entries, want the newest, %d with 3", f.Revision, len(f.Entries), sn.Persist().Revision)
	}
}

// TestAFailingWriteStaysDirtyAndRetries pins a write that fails: the state
// stays dirty, one log line, retries with backoff, and the newest state
// lands once the disk takes it.
func TestAFailingWriteStaysDirtyAndRetries(t *testing.T) {
	h := startHarness(t)
	full := errors.New("no space left on device")
	h.files.mu.Lock()
	h.files.failQueue = func(n int) error {
		if n <= 3 {
			return full
		}
		return nil
	}
	h.files.mu.Unlock()
	h.insert("a")
	h.clk.Advance(writeCoalesce)
	sn := h.waitSnap("the first failure", func(sn *Snapshot) bool { return sn.Persist().Failures == 1 })
	if p := sn.Persist(); p.Written >= p.Revision || !strings.Contains(p.Err, "no space") {
		t.Fatalf("after a failed write %+v, want dirty with the error", p)
	}
	h.insert("b")
	backoff := writeRetryMin
	for k := 2; k <= 4; k++ {
		h.clk.Advance(backoff)
		backoff *= 2
		h.files.waitFor(t, "the retry", func() bool { return h.files.queueAttempts >= k })
		if k < 4 {
			h.waitSnap("the failure", func(sn *Snapshot) bool { return sn.Persist().Failures == k })
		}
	}
	sn = h.waitSnap("written", func(sn *Snapshot) bool { p := sn.Persist(); return p.Written == p.Revision && p.Err == "" })
	if n := h.logs.count("queue file write failed"); n != 1 {
		t.Fatalf("%d log lines for three failed writes, want 1: %v", n, h.logs.all())
	}
	data, err := os.ReadFile(h.queuePath())
	h.must("read the queue file", err)
	if f := readQueueFile(t, data); len(f.Entries) != 2 {
		t.Fatalf("the file holds %d entries, want the newest state's 2", len(f.Entries))
	}
}

// TestAMutationDuringTheShutdownFlushIsRefused pins the order of shutdown:
// mutations are refused from its first moment, so nothing lands after the
// flush.
func TestAMutationDuringTheShutdownFlushIsRefused(t *testing.T) {
	h := startHarness(t)
	h.insert("a")
	hold := newGate()
	h.files.mu.Lock()
	h.files.holdQueue[h.files.queueAttempts+1] = hold
	h.files.mu.Unlock()
	h.cancel()
	h.files.waitFor(t, "the flush", func() bool { return h.files.queueAttempts >= 1 })
	_, err := h.eng.InsertAfter(bg, 0, ref("b"), nil)
	h.refused("an insert during the flush", err, ReasonClosed)
	hold.open()
	h.drive(h.ran)
	h.down = true
	data, err := os.ReadFile(h.queuePath())
	h.must("read the queue file", err)
	if f := readQueueFile(t, data); len(f.Entries) != 1 || f.Entries[0].Ref != "a" {
		t.Fatalf("the flushed file holds %+v", f.Entries)
	}
}

// savedQueue runs an engine that saves the queue a, b with b selected, and
// returns the harness, stopped, and the file's JSON as a map to corrupt.
func savedQueue(t *testing.T) (*harness, map[string]any) {
	t.Helper()
	h := startHarness(t)
	ids := h.insert("a", "b")
	h.must("delete a", h.eng.Delete(bg, ids[0]))
	h.insert("c")
	h.shutdown()
	h.joinAfter(h.eng)
	data, err := os.ReadFile(h.queuePath())
	h.must("read the queue file", err)
	var m map[string]any
	h.must("parse", json.Unmarshal(data, &m))
	return h, m
}

// writeJSON writes m as the queue file.
func writeJSON(t *testing.T, h *harness, m map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.queuePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

// entriesOfFile is the file map's entries.
func entriesOfFile(m map[string]any) []any { return m["entries"].([]any) }

// metaEntries is n entries with IDs from 1, each with size bytes of
// metadata.
func metaEntries(n, size int) []any {
	out := make([]any, n)
	meta := base64.StdEncoding.EncodeToString(make([]byte, size))
	for i := range out {
		out[i] = map[string]any{"id": i + 1, "ref": "r", "meta": meta}
	}
	return out
}

// TestEachKindOfBadFileIsMovedAside pins every way a queue file can be bad:
// the engine starts empty, logs the reason once, renames the file aside
// with a timestamp and never deletes it, and goes on to write a new one.
func TestEachKindOfBadFileIsMovedAside(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  func(t *testing.T, h *harness, m map[string]any) []byte
	}{
		{"unreadable", func(t *testing.T, h *harness, m map[string]any) []byte {
			h.files.readErr["queue.json"] = fs.ErrPermission
			data, _ := os.ReadFile(h.queuePath())
			return data
		}},
		{"oversized", func(t *testing.T, h *harness, m map[string]any) []byte {
			data := append([]byte(`{"version":1}`), bytes.Repeat([]byte(" "), maxQueueFileBytes)...)
			h.must("write", os.WriteFile(h.queuePath(), data, 0o600))
			return data
		}},
		{"not JSON", func(t *testing.T, h *harness, m map[string]any) []byte {
			h.must("write", os.WriteFile(h.queuePath(), []byte("{"), 0o600))
			return []byte("{")
		}},
		{"another version", func(t *testing.T, h *harness, m map[string]any) []byte {
			m["version"] = 2
			return writeJSON(t, h, m)
		}},
		{"a zero ID", func(t *testing.T, h *harness, m map[string]any) []byte {
			entriesOfFile(m)[0].(map[string]any)["id"] = 0
			return writeJSON(t, h, m)
		}},
		{"a duplicate ID", func(t *testing.T, h *harness, m map[string]any) []byte {
			es := entriesOfFile(m)
			es[1].(map[string]any)["id"] = es[0].(map[string]any)["id"]
			return writeJSON(t, h, m)
		}},
		{"an ID past its reservation", func(t *testing.T, h *harness, m map[string]any) []byte {
			reserved := m["ids"].(map[string]any)["reserved"].(float64)
			entriesOfFile(m)[0].(map[string]any)["id"] = reserved + 1
			return writeJSON(t, h, m)
		}},
		// A counter a start would wrap (startCounter adds a block): no run
		// writes one, and taken it could hand out an ID or token again.
		{"IDs reserved where a start would wrap", func(t *testing.T, h *harness, m map[string]any) []byte {
			m["ids"].(map[string]any)["reserved"] = json.Number(strconv.FormatUint(math.MaxUint64-idBlock, 10))
			return writeJSON(t, h, m)
		}},
		{"a next ID where a start would wrap", func(t *testing.T, h *harness, m map[string]any) []byte {
			m["ids"].(map[string]any)["next"] = json.Number(strconv.FormatUint(math.MaxUint64, 10))
			return writeJSON(t, h, m)
		}},
		{"tokens reserved where a start would wrap", func(t *testing.T, h *harness, m map[string]any) []byte {
			m["tokens"].(map[string]any)["reserved"] = json.Number(strconv.FormatUint(math.MaxUint64-1, 10))
			return writeJSON(t, h, m)
		}},
		{"a bad reference", func(t *testing.T, h *harness, m map[string]any) []byte {
			entriesOfFile(m)[0].(map[string]any)["ref"] = "bad\u0001"
			return writeJSON(t, h, m)
		}},
		{"more entries than the queue holds", func(t *testing.T, h *harness, m map[string]any) []byte {
			m["entries"] = metaEntries(MaxEntries+1, 0)
			m["selected"] = 1
			return writeJSON(t, h, m)
		}},
		{"an entry's metadata past its bound", func(t *testing.T, h *harness, m map[string]any) []byte {
			m["entries"] = metaEntries(1, MaxMetadataBytes+1)
			m["selected"] = 1
			return writeJSON(t, h, m)
		}},
		{"metadata past the queue's bound", func(t *testing.T, h *harness, m map[string]any) []byte {
			m["entries"] = metaEntries(MaxTotalMetadataBytes/MaxMetadataBytes+1, MaxMetadataBytes)
			m["selected"] = 1
			return writeJSON(t, h, m)
		}},
		{"shuffle on", func(t *testing.T, h *harness, m map[string]any) []byte {
			m["shuffle"] = true
			return writeJSON(t, h, m)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, m := savedQueue(t)
			if ids := m["ids"].(map[string]any); ids["reserved"].(float64) < MaxEntries+1 {
				t.Fatalf("the saved reservation %v is too small for the test", ids["reserved"])
			}
			original := tc.bad(t, h, m)
			lines := len(h.logs.all())
			h.boot()
			if n := h.snap().Len(); n != 0 {
				t.Fatalf("a bad file loaded %d entries", n)
			}
			aside, _ := filepath.Glob(h.queuePath() + ".bad-*")
			if len(aside) != 1 {
				t.Fatalf("moved aside: %v, want one file", aside)
			}
			got, err := os.ReadFile(aside[0])
			h.must("read the file moved aside", err)
			if !bytes.Equal(got, original) {
				t.Fatal("the file moved aside is not the original")
			}
			if _, err := os.Lstat(h.queuePath()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the bad file is still at the queue's path: %v", err)
			}
			moved := 0
			for _, l := range h.logs.all()[lines:] {
				if strings.Contains(l, "moved aside") {
					moved++
				}
			}
			if moved != 1 {
				t.Fatalf("%d log lines say the file was moved aside, want 1: %v", moved, h.logs.all()[lines:])
			}
			delete(h.files.readErr, "queue.json")
			h.insert("fresh")
			h.restart()
			if ids := h.snap().IDs(); len(ids) != 1 {
				t.Fatalf("after a bad file the engine saved %v", ids)
			}
		})
	}
}

// TestADanglingSelectionIsClearedAtLoad pins a queue file whose selection
// its queue does not hold: the queue loads whole with nothing selected, the
// log says so once, and nothing is moved aside.
func TestADanglingSelectionIsClearedAtLoad(t *testing.T) {
	h, m := savedQueue(t)
	var want []uint32
	for _, e := range entriesOfFile(m) {
		want = append(want, uint32(e.(map[string]any)["id"].(float64)))
	}
	m["selected"] = 4242
	writeJSON(t, h, m)
	h.boot()
	if sn := h.snap(); !slices.Equal(sn.IDs(), want) || sn.Selected() != 0 {
		t.Fatalf("loaded %v with %d selected, want %v with nothing selected", sn.IDs(), sn.Selected(), want)
	}
	if aside, _ := filepath.Glob(h.queuePath() + ".bad-*"); len(aside) != 0 {
		t.Fatalf("a queue with a dangling selection was moved aside: %v", aside)
	}
	if n := h.logs.count("selection is not in its queue"); n != 1 {
		t.Fatalf("%d log lines for the dangling selection, want 1: %v", n, h.logs.all())
	}
}

// TestAFailedMoveAsideKeepsTheOriginal pins a bad file the engine cannot
// rename: it stays where it is, byte for byte, the engine starts empty and
// writes no queue file over it, the log says so, and edits wait until the
// operator moves it.
func TestAFailedMoveAsideKeepsTheOriginal(t *testing.T) {
	h, _ := savedQueue(t)
	h.must("write", os.WriteFile(h.queuePath(), []byte("{"), 0o600))
	h.files.renameErr = errors.New("read-only file system")
	h.boot()
	if h.snap().Len() != 0 {
		t.Fatal("the bad file loaded")
	}
	if ed := h.snap().Edits(); ed.Refused != ReasonFileStuck {
		t.Fatalf("edits %+v, want refused %s", ed, ReasonFileStuck)
	}
	_, err := h.eng.InsertAfter(bg, 0, ref("a"), nil)
	h.refused("an insert while the bad file stays", err, ReasonFileStuck)
	h.must("repeat", h.eng.SetRepeat(bg, true))
	h.clk.Advance(writeMaxAge)
	if n := h.logs.count("could not be moved aside"); n != 1 {
		t.Fatalf("%d log lines say so, want 1: %v", n, h.logs.all())
	}
	h.restart()
	if got, _ := os.ReadFile(h.queuePath()); string(got) != "{" {
		t.Fatalf("the original was overwritten with %q", got)
	}
	h.files.mu.Lock()
	h.files.renameErr = nil
	h.files.mu.Unlock()
	h.must("the operator moves it aside", os.Rename(h.queuePath(), h.queuePath()+".mine"))
	if _, err := h.eng.InsertAfter(bg, 0, ref("a"), nil); err != nil {
		t.Fatalf("an insert once the file is moved: %v", err)
	}
	h.restart()
	if n := h.snap().Len(); n != 1 {
		t.Fatalf("the engine saved %d entries once the file was moved", n)
	}
}

// TestABadFileWithNoNameToMoveToStays pins the name a bad file is moved to:
// a name is taken only when Lstat says nothing is there. An Lstat that fails
// any other way (a parent without search permission, ENOTDIR, an I/O
// error) is asked once and logged with its error, and a run of names every
// one of which is taken ends at maxAsideNames; either leaves the bad file
// where it is, with inserts refused: New returns, and no name is probed for
// ever.
func TestABadFileWithNoNameToMoveToStays(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lstat func(t *testing.T, h *harness) func(string) (fs.FileInfo, error)
		// asks is the number of Lstats the move may make; logged, what its
		// log line says.
		asks   int
		logged string
	}{
		{"an Lstat that keeps failing", func(*testing.T, *harness) func(string) (fs.FileInfo, error) {
			return func(path string) (fs.FileInfo, error) {
				return nil, &fs.PathError{Op: "lstat", Path: path, Err: fs.ErrPermission}
			}
		}, 1, fs.ErrPermission.Error()},
		{"every name taken", func(t *testing.T, h *harness) func(string) (fs.FileInfo, error) {
			taken, err := os.Lstat(h.dir)
			if err != nil {
				t.Fatal(err)
			}
			return func(path string) (fs.FileInfo, error) {
				if strings.Contains(path, ".bad-") {
					return taken, nil
				}
				return os.Lstat(path)
			}
		}, maxAsideNames + 1, "names beside it are taken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := savedQueue(t)
			h.must("write", os.WriteFile(h.queuePath(), []byte("{"), 0o600))
			lstat := tc.lstat(t, h)
			var asks atomic.Int64
			h.tweak = func(c *Config) {
				c.seams.lstat = func(path string) (fs.FileInfo, error) {
					asks.Add(1)
					return lstat(path)
				}
			}
			h.be = newFakeBackend(t)
			built := make(chan *Engine, 1)
			go func() {
				e, err := New(h.config())
				if err != nil {
					t.Errorf("New: %v", err)
				}
				built <- e
			}()
			var e *Engine
			select {
			case e = <-built:
			case <-time.After(*watchdog):
				t.Fatalf("New did not return within %v: a name for the bad file is probed for ever", *watchdog)
			}
			if got, _ := os.ReadFile(h.queuePath()); string(got) != "{" {
				t.Fatalf("the bad file is now %q", got)
			}
			if aside, _ := filepath.Glob(h.queuePath() + ".bad-*"); len(aside) != 0 {
				t.Fatalf("moved aside to %v", aside)
			}
			if ed := e.Snapshot().Edits(); ed.Refused != ReasonFileStuck {
				t.Fatalf("edits %+v, want refused %s", ed, ReasonFileStuck)
			}
			if n := h.logs.count("could not be moved aside"); n != 1 || h.logs.count(tc.logged) != 1 {
				t.Fatalf("%d log lines say so, want 1 that says %q: %v", n, tc.logged, h.logs.all())
			}
			if n := asks.Load(); n != int64(tc.asks) {
				t.Fatalf("the move asked Lstat %d times, want %d", n, tc.asks)
			}
		})
	}
}

// TestLoadStartsPastTheReservationAndTheLargestID pins where a run's IDs
// begin: one block past the larger of the reservation file's and the
// queue file's largest ID.
func TestLoadStartsPastTheReservationAndTheLargestID(t *testing.T) {
	t.Run("the reservation is larger", func(t *testing.T) {
		h := newHarness(t)
		writeReserveFile(t, h, 30000, 500)
		h.boot()
		if id := h.insert("a")[0]; id != 30000+idBlock+1 {
			t.Fatalf("the first ID is %d, want %d", id, 30000+idBlock+1)
		}
	})
	t.Run("the largest ID is larger", func(t *testing.T) {
		h, m := savedQueue(t)
		ids := m["ids"].(map[string]any)
		ids["next"], ids["reserved"] = 20001, 20000
		entriesOfFile(m)[0].(map[string]any)["id"] = 20000
		m["selected"] = 20000
		writeJSON(t, h, m)
		writeReserveFile(t, h, 10000, 500)
		h.boot()
		if id := h.insert("a")[0]; id != 20000+idBlock+1 {
			t.Fatalf("the first ID is %d, want %d", id, 20000+idBlock+1)
		}
	})
}

// TestAStartIsStopped pins the state after a restart: stopped, with the
// queue, the modes and the selection as saved, and the token advanced.
func TestAStartIsStopped(t *testing.T) {
	h := startHarness(t)
	h.must("repeat", h.eng.SetRepeat(bg, true))
	ids, _ := h.playing(1, "a", "b", "c")
	token := h.snap().Token()
	h.restart()
	sn := h.snap()
	if sn.State() != Stopped || !slices.Equal(sn.IDs(), ids) || sn.Selected() != ids[1] || !sn.Repeat() || sn.Token() <= token {
		t.Fatalf("after a restart %s, IDs %v, selected %d, repeat %v, token %d (was %d)", sn.State(), sn.IDs(), sn.Selected(), sn.Repeat(), sn.Token(), token)
	}
	if h.be.starts() != 0 {
		t.Fatal("a restart started a session")
	}
}
