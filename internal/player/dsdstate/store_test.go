package dsdstate

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// chooseNative is a write the app could make: native for the GO bar, the
// revision one more.
func chooseNative(t *testing.T) func(*State) error {
	return func(st *State) error {
		d := st.entry(goBar(t))
		rev := uint64(1)
		if d.Choice != nil {
			rev = d.Choice.Revision + 1
		}
		d.Choice = &Choice{Mode: ModeNative, Revision: rev, SetAt: t0, SetBy: ByApp}
		st.ClearWarningsFor(goBar(t))
		return nil
	}
}

// TestAWriteIsDurableBeforeItReturns: a write returns only once the file and
// its directory entry, up to the data directory, are synced, through the
// writer the player's ID reservations use (atomicwrite.WriteBytesDurable):
// by the time Update returns, the file holds the new state and the data
// directory has been synced since the write began; a directory sync that
// fails is the write's error, and the state stays as it was, as a write that
// is not counted made, and so does a writer that fails; the store's own
// writer is what a test injects (options.write), never a package seam.
func TestAWriteIsDurableBeforeItReturns(t *testing.T) {
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	data := t.TempDir()
	resolved, err := filepath.EvalSymlinks(data)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var synced []string
	failing := false
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(dir string) error {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return errors.New("the directory would not sync")
		}
		synced = append(synced, dir)
		return nil
	}))
	s := openAt(t, data, options{})
	if err := s.Update(chooseNative(t)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := slices.Clone(synced)
	mu.Unlock()
	if !slices.Contains(got, resolved) {
		t.Fatalf("Update returned with the directories %v synced, without the data directory %s: not durable", got, resolved)
	}
	if mode, _, own := fileState(t, data).ChoiceFor(goBar(t)); mode != ModeNative || !own {
		t.Fatalf("the file holds %q (own %v) as Update returns", mode, own)
	}

	before := s.State()
	mu.Lock()
	failing = true
	mu.Unlock()
	err = s.Update(func(st *State) error {
		st.entry(goBar(t)).Choice = &Choice{Mode: ModeOff, Revision: 2, SetAt: t0, SetBy: ByApp}
		return nil
	})
	if !errors.Is(err, atomicwrite.ErrBarrierFailed) {
		t.Fatalf("Update over a directory that would not sync = %v, want the barrier's failure", err)
	}
	if !reflect.DeepEqual(s.State(), before) {
		t.Fatalf("a write that was not durable changed the store's state to %s", show(t, s.State()))
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	refused := openAt(t, t.TempDir(), options{write: func(string, []byte) error { return errors.New("the disk is full") }})
	if err := refused.Update(chooseNative(t)); err == nil || !strings.Contains(err.Error(), "the disk is full") {
		t.Fatalf("Update through a failing writer = %v", err)
	}
	if !reflect.DeepEqual(refused.State(), Defaults()) {
		t.Fatalf("a write that failed changed the state to %s", show(t, refused.State()))
	}
}

// TestAWriteWhereNoDirectoryCanBeSyncedIsRefused: where the platform has no
// directory barrier (Windows; the player runs on Linux alone), every write
// is refused with atomicwrite.ErrNoDirectoryBarrier and nothing is written:
// no write could be counted durable there.
func TestAWriteWhereNoDirectoryCanBeSyncedIsRefused(t *testing.T) {
	t.Cleanup(atomicwrite.SetSyncDirForTest(nil))
	data := t.TempDir()
	s := openAt(t, data, options{})
	if err := s.Update(chooseNative(t)); !errors.Is(err, atomicwrite.ErrNoDirectoryBarrier) {
		t.Fatalf("Update with no directory barrier = %v, want ErrNoDirectoryBarrier", err)
	}
	if _, err := os.Lstat(filepath.Join(data, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused write left a file: %v", err)
	}
}

// TestADamagedStateRefusesDSDAndIsMovedAside: a file that is there and does
// not decode is never deleted and never read as the defaults (the design's
// section 2.5). Its bytes are kept, byte for byte, beside it under a
// timestamped name, and a fresh state takes its place: defaultMode off,
// state_unreadable, which refuses DSD (Unreadable), and the marker, so no
// migration runs over it. A reopen reads that fresh state, still refusing,
// and a choice written then clears the refusal for good, defaultMode off
// still applying to every other DAC. Each kind of damage: bytes that are not
// JSON, a field schema 1 does not have, another schema, and a value no
// writer writes.
func TestADamagedStateRefusesDSDAndIsMovedAside(t *testing.T) {
	durable(t)
	for name, bad := range map[string]string{
		"not_json":         "{\"schemaVersion\": 1, \"defaultMode\": \"au",
		"an_unknown_field": `{"schemaVersion": 1, "defaultMode": "auto", "dacs": [], "migratedFrom": null, "warnings": [], "volume": 11}`,
		"another_schema":   `{"schemaVersion": 2, "defaultMode": "auto", "dacs": [], "migratedFrom": null, "warnings": []}`,
		"an_empty_rate_list": `{"schemaVersion": 1, "defaultMode": "auto", "dacs": [{"key": {"vendorId": "20b1", "productId": "3009", "interface": 1, "product": "GO bar"}, "choice": null, "dopConfirmed": null,
  "firstUse": [{"transport": "native", "bcdDevice": "0123", "bitRates": [], "acknowledgedAt": "2026-11-05T10:00:00Z"}], "listener": [], "evidence": []}], "migratedFrom": null, "warnings": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			path := filepath.Join(data, FileName)
			if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
				t.Fatal(err)
			}
			s := openAt(t, data, options{})
			r := s.Report()
			if r.Outcome != Damaged || r.Why == nil || r.Kept != nil {
				t.Fatalf("Open = %+v, want a damaged file kept aside and replaced", r)
			}
			if want := path + ".bad-20261105T100000.000000000Z"; r.Aside != want {
				t.Fatalf("the bad bytes are kept at %s, want %s", r.Aside, want)
			}
			if kept, err := os.ReadFile(r.Aside); err != nil || string(kept) != bad {
				t.Fatalf("the kept copy is %q, %v; want the bad bytes, byte for byte", kept, err)
			}
			if st := s.State(); !st.Unreadable() || !reflect.DeepEqual(st, Refusing(t0)) {
				t.Fatalf("the state after damage is %s, want Refusing", show(t, st))
			}
			if got := fileState(t, data); !reflect.DeepEqual(got, Refusing(t0)) {
				t.Fatalf("the file after damage holds %s, want Refusing, so the refusal survives a restart", show(t, got))
			}
			reopened := openAt(t, data, options{})
			if r := reopened.Report(); r.Outcome != Loaded || !reopened.State().Unreadable() {
				t.Fatalf("a reopen reads %+v, unreadable %v: want the refusal still", r, reopened.State().Unreadable())
			}
			if err := reopened.Update(chooseNative(t)); err != nil {
				t.Fatal(err)
			}
			after := fileState(t, data)
			if after.Unreadable() || after.DefaultMode != ModeOff {
				t.Fatalf("after a choice the state is %s: want the refusal cleared and defaultMode off", show(t, after))
			}
			if mode, _, own := after.ChoiceFor(hugo(t)); mode != ModeOff || own {
				t.Fatalf("another DAC takes %q after the choice, want off, never converted on a guess", mode)
			}
			if _, err := os.Stat(r.Aside); err != nil {
				t.Fatalf("the kept copy is gone: %v", err)
			}
		})
	}
}

// TestNoMomentLeavesTheStateWithoutItsRefusal: the bad bytes are copied
// aside before the name is replaced, so the name never holds nothing (the
// design's section 2.5, the review's binding note 1): a replacement that
// fails once the copy is made leaves the bad bytes at the name and the
// refusal in memory, never the defaults, and a reopen refuses again, keeping
// another copy, and replaces the file then; a copy that cannot be made
// leaves the file where it is, refused, and no write replaces it.
func TestNoMomentLeavesTheStateWithoutItsRefusal(t *testing.T) {
	durable(t)
	data := t.TempDir()
	path := filepath.Join(data, FileName)
	const bad = "not a state"
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	real := func(p string, b []byte) error { return atomicwrite.WriteBytesDurable(data, p, b, tmpPrefix) }
	var order []string
	failTheName := func(p string, b []byte) error {
		order = append(order, filepath.Base(p))
		if p == path {
			return errors.New("the disk is full")
		}
		return real(p, b)
	}
	s := openAt(t, data, options{write: failTheName})
	r := s.Report()
	if r.Outcome != Damaged || r.Kept == nil || r.Aside == "" {
		t.Fatalf("Open with the replacement failing = %+v", r)
	}
	if want := []string{filepath.Base(r.Aside), FileName}; !slices.Equal(order, want) {
		t.Fatalf("the writes were %v, want the copy aside first, then the name", order)
	}
	if !s.State().Unreadable() {
		t.Fatalf("the state in memory is %s, want the refusal", show(t, s.State()))
	}
	if b, _ := os.ReadFile(path); string(b) != bad {
		t.Fatalf("the name holds %q, want the bad bytes still", b)
	}
	clock := t0.Add(1e9)
	reopened := openAt(t, data, options{now: func() time.Time { return clock }})
	if r2 := reopened.Report(); r2.Outcome != Damaged || r2.Kept != nil || r2.Aside == r.Aside || !reopened.State().Unreadable() {
		t.Fatalf("the reopen = %+v, want the file damaged again, kept under another name, and replaced", r2)
	}
	if !fileState(t, data).Unreadable() {
		t.Fatal("the reopen's replacement does not refuse")
	}

	stuck := t.TempDir()
	if err := os.WriteFile(filepath.Join(stuck, FileName), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	noCopy := openAt(t, stuck, options{write: func(p string, b []byte) error { return errors.New("the disk is full") }})
	if r := noCopy.Report(); r.Outcome != Unread || r.Kept == nil {
		t.Fatalf("Open with no copy possible = %+v, want the file left where it is", r)
	}
	if err := noCopy.Update(chooseNative(t)); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("a write over a file that could not be kept aside = %v, want ErrNotWritable", err)
	}
	if b, _ := os.ReadFile(filepath.Join(stuck, FileName)); string(b) != bad || !noCopy.State().Unreadable() {
		t.Fatalf("the file holds %q, unreadable %v", b, noCopy.State().Unreadable())
	}
}

// TestAStateThatCannotBeReadIsKeptAndRefused: a file that cannot be read,
// or not whole (a directory at the name, a file past the bound), is left
// where it is and refused in memory: no copy of what could not be read, and
// no write over it until it is moved.
func TestAStateThatCannotBeReadIsKeptAndRefused(t *testing.T) {
	durable(t)
	for _, tc := range []struct {
		name string
		make func(t *testing.T, path string)
	}{
		{"a_directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"past_the_bound", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat(" ", maxFileBytes+1)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			path := filepath.Join(data, FileName)
			tc.make(t, path)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			s := openAt(t, data, options{})
			if r := s.Report(); r.Outcome != Unread || r.Why == nil {
				t.Fatalf("Open = %+v, want the file left unread", r)
			}
			if !reflect.DeepEqual(s.State(), Refusing(t0)) {
				t.Fatalf("the state is %s, want Refusing", show(t, s.State()))
			}
			if err := s.Update(chooseNative(t)); !errors.Is(err, ErrNotWritable) {
				t.Fatalf("a write over it = %v, want ErrNotWritable", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() {
				t.Fatalf("the entry changed: %v", err)
			}
			if aside, _ := filepath.Glob(path + ".bad-*"); len(aside) != 0 {
				t.Fatalf("a copy was made of what could not be read: %v", aside)
			}
		})
	}
}

// TestAMissingStateIsTheDefaultsAndWritesNothing: no file is a fresh
// install, never a damaged one: the defaults, nothing refused, and nothing
// written until the first write that changes something.
func TestAMissingStateIsTheDefaultsAndWritesNothing(t *testing.T) {
	durable(t)
	data := t.TempDir()
	s := openAt(t, data, options{})
	if r := s.Report(); r.Outcome != Missing || r.Why != nil {
		t.Fatalf("Open of no file = %+v", r)
	}
	if st := s.State(); !reflect.DeepEqual(st, Defaults()) || st.Unreadable() {
		t.Fatalf("the state of no file is %s, want the defaults", show(t, st))
	}
	if err := s.Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(data); len(entries) != 0 {
		t.Fatalf("a write that changed nothing wrote %v", entries)
	}
	if err := s.Update(chooseNative(t)); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(data); len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatalf("the first write left %v", entries)
	}
}

// TestWritesSerializeOnTheStore: the store is the one writer, and writes
// made at once land one at a time, each on the state the last one left: no
// write is lost and every revision is taken once.
func TestWritesSerializeOnTheStore(t *testing.T) {
	durable(t)
	data := t.TempDir()
	s := openAt(t, data, options{})
	const writers = 16
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Update(chooseNative(t)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	d, _ := fileState(t, data).DAC(goBar(t))
	if d.Choice == nil || d.Choice.Revision != writers {
		t.Fatalf("after %d writes at once the revision is %+v, want %d", writers, d.Choice, writers)
	}
}

// sizedState is a state of one DAC whose file, as encode writes it, is n
// bytes long: its product string makes up the difference.
func sizedState(t *testing.T, n int) State {
	t.Helper()
	of := func(product string) State {
		k := newKey(t, 0x20b1, 0x3009, ptr(1), &product)
		return State{DefaultMode: ModeAuto, DACs: []DAC{{Key: k, Choice: &Choice{Mode: ModeNative, Revision: 1, SetAt: t0, SetBy: ByApp}}}}
	}
	b, err := of("x").encode()
	if err != nil {
		t.Fatal(err)
	}
	return of(strings.Repeat("x", n-len(b)+1))
}

// TestAWriteTheReaderWouldRefuseIsNotMade: a write never leaves a file the
// next start refuses (Codex on #1405: the structural bounds let a state of
// 100 DACs with 64 answers each pass at about 1.1 MB, past the 1 MiB the
// reader reads). A state whose file is exactly the reader's bound is written
// and read back; one byte past it, the write is refused, and the state and
// the file stay as they were, as for any write not made.
func TestAWriteTheReaderWouldRefuseIsNotMade(t *testing.T) {
	durable(t)
	data := t.TempDir()
	path := filepath.Join(data, FileName)
	s := openAt(t, data, options{})
	at := sizedState(t, maxFileBytes)
	if err := s.Update(func(st *State) error { *st = at.Clone(); return nil }); err != nil {
		t.Fatalf("a write of a state at the reader's bound = %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil || len(written) != maxFileBytes {
		t.Fatalf("the file at the bound is %d bytes, %v; want %d", len(written), err, maxFileBytes)
	}
	reopened := openAt(t, data, options{})
	if r := reopened.Report(); r.Outcome != Loaded || !reflect.DeepEqual(reopened.State().normalized(), at.normalized()) {
		t.Fatalf("the state at the bound reads back as %+v", r)
	}
	before := s.State()
	past := sizedState(t, maxFileBytes+1)
	if b, err := sizedState(t, 4096).encode(); err != nil || len(b) != 4096 {
		t.Fatalf("the sized fixture is %d bytes, %v, want 4096: its size is not the one asked for", len(b), err)
	}
	if err := s.Update(func(st *State) error { *st = past.Clone(); return nil }); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a write past the reader's bound = %v, want ErrTooLarge: it would leave a file the next start refuses", err)
	}
	if !reflect.DeepEqual(s.State(), before) {
		t.Fatal("a refused write changed the store's state")
	}
	if now, _ := os.ReadFile(path); string(now) != string(written) {
		t.Fatal("a refused write changed the file")
	}
}

// TestAnUnreadableStateMovedAwayIsWritableAgain: a state that could not be
// read is never written over, and once the operator has moved it away the
// store sees so at its next write, which is made on the state that refuses,
// so a choice clears the refusal with no restart (the Claude API reviewer on
// #1405: the store refused every write until a restart); an entry still
// there is still refused. Once its own write has landed the store stops
// looking, and the next write is made over its own file.
func TestAnUnreadableStateMovedAwayIsWritableAgain(t *testing.T) {
	durable(t)
	data := t.TempDir()
	path := filepath.Join(data, FileName)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, data, options{})
	for range 2 {
		if err := s.Update(chooseNative(t)); !errors.Is(err, ErrNotWritable) {
			t.Fatalf("a write over the entry still there = %v, want ErrNotWritable", err)
		}
	}
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(chooseNative(t)); err != nil {
		t.Fatalf("a write once the operator moved the entry away = %v", err)
	}
	want := Refusing(t0)
	want.Warnings = nil
	want.DACs = []DAC{{Key: goBar(t), Choice: &Choice{Mode: ModeNative, Revision: 1, SetAt: t0, SetBy: ByApp}}}
	if got := fileState(t, data); !reflect.DeepEqual(got.normalized(), want.normalized()) {
		t.Fatalf("the state written is %s, want the choice on the state that refuses, its refusal cleared", show(t, got))
	}
	if st := s.State(); st.Unreadable() {
		t.Fatal("the store still refuses after the choice")
	}
	if err := s.Update(chooseNative(t)); err != nil {
		t.Fatalf("a write over the store's own file = %v: the store kept looking once its own write had landed", err)
	}
	if d, _ := fileState(t, data).DAC(goBar(t)); d.Choice == nil || d.Choice.Revision != 2 {
		t.Fatalf("the second write left %+v, want the choice at revision 2", d.Choice)
	}
}

// TestAStoreLooksAgainUntilAWriteOfItsOwnHasLanded: a store whose state
// could not be read stops looking at the name only once a write of its own
// has landed there (the Claude API reviewer on round 2: the first look that
// found the entry gone ended the looking before the write, so a write not
// made after the operator moved the file, and the file put back, left every
// later write replacing it unread). After each write that is not made (a
// change refused, as a stale compare-and-set is; a state the reader would
// refuse; a write that failed; a change that changes nothing), the entry is
// put back, and the next write is refused with the entry as it was.
func TestAStoreLooksAgainUntilAWriteOfItsOwnHasLanded(t *testing.T) {
	durable(t)
	stale := errors.New("the revision is stale")
	full := errors.New("the disk is full")
	for _, tc := range []struct {
		name string
		fn   func(t *testing.T) func(*State) error
		fail bool
		want error
	}{
		{"a_change_refused", func(*testing.T) func(*State) error { return func(*State) error { return stale } }, false, stale},
		{"a_state_the_reader_would_refuse", func(t *testing.T) func(*State) error {
			past := sizedState(t, maxFileBytes+1)
			return func(st *State) error { *st = past.Clone(); return nil }
		}, false, ErrTooLarge},
		{"a_write_that_failed", chooseNative, true, full},
		{"a_change_that_changes_nothing", func(*testing.T) func(*State) error { return func(*State) error { return nil } }, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			path := filepath.Join(data, FileName)
			if err := os.WriteFile(path, []byte(strings.Repeat(" ", maxFileBytes+1)), 0o600); err != nil {
				t.Fatal(err)
			}
			failing := false
			s := openAt(t, data, options{write: func(p string, b []byte) error {
				if failing {
					return full
				}
				return atomicwrite.WriteBytesDurable(data, p, b, tmpPrefix)
			}})
			if r := s.Report(); r.Outcome != Unread {
				t.Fatalf("Open = %+v, want the file left unread", r)
			}
			moved := path + ".moved"
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			failing = tc.fail
			if err := s.Update(tc.fn(t)); !errors.Is(err, tc.want) {
				t.Fatalf("the write after the move = %v, want %v", err, tc.want)
			}
			failing = false
			if err := os.Rename(moved, path); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Update(chooseNative(t)); !errors.Is(err, ErrNotWritable) {
				t.Fatalf("a write once the entry is back = %v, want ErrNotWritable: the store stopped looking before a write of its own landed", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() {
				t.Fatalf("the entry put back was replaced: %v", err)
			}
		})
	}
}
