package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// The tests in this file land a sibling process's write (`bridge pair`,
// `bridge token revoke|rotate|expire`) inside one of the running bridge's
// writes, between the staging of the file (a temp file, a write and an
// fsync: milliseconds) and its commit, through beforeCommitHook. A commit
// that renamed over the sibling's put back the list read before it, so a
// token `bridge pair` minted vanished from tokens.json and one `bridge
// token revoke` removed came back.

// commitFixture is a running bridge's store holding three paired devices,
// and a sibling process's store on the same file, opened before the
// running bridge writes, as `bridge pair` would be.
type commitFixture struct {
	path    string
	running *Store
	sibling *Store

	own, victim, other          Token
	ownRaw, victimRaw, otherRaw string
}

func newCommitFixture(t *testing.T) *commitFixture {
	t.Helper()
	s, path := newTmpStore(t)
	f := &commitFixture{path: path, running: s}
	var err error
	if f.ownRaw, f.own, err = s.Mint("serve-process"); err != nil {
		t.Fatal(err)
	}
	if f.victimRaw, f.victim, err = s.Mint("victim"); err != nil {
		t.Fatal(err)
	}
	if f.otherRaw, f.other, err = s.Mint("other"); err != nil {
		t.Fatal(err)
	}
	if f.sibling, err = OpenStore(path); err != nil {
		t.Fatal(err)
	}
	return f
}

// inCommitWindow arms beforeCommitHook so that sibling runs inside up to
// times of this process's writes, each time between the write's staging
// and its commit. A write that sibling makes passes through the same hook
// untouched. staged reports how often a write reached the hook outside
// sibling, one per staging; ran, how often sibling ran.
func inCommitWindow(t *testing.T, times int, sibling func()) (staged, ran func() int) {
	t.Helper()
	nStaged, nRan, inSibling := 0, 0, false
	beforeCommitHook = func() {
		if inSibling {
			return
		}
		nStaged++
		if nRan == times {
			return
		}
		nRan++
		inSibling = true
		defer func() { inSibling = false }()
		sibling()
	}
	t.Cleanup(func() { beforeCommitHook = nil })
	return func() int { return nStaged }, func() int { return nRan }
}

// holdsRaw reports whether s holds the token raw validates as. It reads
// the list rather than calling Validate, which bumps LastUsedAt and can
// write.
func holdsRaw(s *Store, raw string) bool {
	if raw == "" {
		return false
	}
	sum := sha256.Sum256([]byte(raw))
	want := hex.EncodeToString(sum[:])
	for _, tok := range s.List() {
		if tok.Hash == want {
			return true
		}
	}
	return false
}

// tokenIn returns the token with id as s holds it, the zero Token if none.
func tokenIn(s *Store, id string) Token {
	tok, _ := s.Get(id)
	return tok
}

// reopenStore opens the file afresh: what the writes left ON DISK.
func reopenStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return s
}

// commitWrite is one write, made by the running bridge or by a sibling.
// do makes it and returns what it must have left: a check of a store that
// answers "" when the write is there.
type commitWrite struct {
	name string
	do   func(t *testing.T, f *commitFixture) (check func(s *Store) string)
}

// runningBridgeWrites are the running bridge's writes: the three that put
// down its observations of the devices, and the console's four.
func runningBridgeWrites() []commitWrite {
	lastUsedFrom := func(f *commitFixture, before time.Time) func(*Store) string {
		return func(s *Store) string {
			if got := tokenIn(s, f.own.ID).LastUsedAt; got.Before(before) {
				return fmt.Sprintf("the LastUsedAt the running bridge wrote is not there (%v, want at or after %v)", got, before)
			}
			return ""
		}
	}
	return []commitWrite{
		{"Validate's debounced write", func(t *testing.T, f *commitFixture) func(*Store) string {
			f.running.setLastUsedFlushForTest(time.Now().Add(-2 * lastUsedFlushInterval))
			before := time.Now()
			if _, ok := f.running.Validate(f.ownRaw); !ok {
				t.Fatal("the running bridge refused its own device")
			}
			return lastUsedFrom(f, before)
		}},
		{"RecordClientVersion's debounced write", func(t *testing.T, f *commitFixture) func(*Store) string {
			f.running.setLastUsedFlushForTest(time.Now().Add(-2 * lastUsedFlushInterval))
			f.running.RecordClientVersion(f.own.ID, "9.9.9")
			return func(s *Store) string {
				if got := tokenIn(s, f.own.ID).LastClientVersion; got != "9.9.9" {
					return fmt.Sprintf("the client version the running bridge wrote is not there (%q)", got)
				}
				return ""
			}
		}},
		{"FlushLastUsed", func(t *testing.T, f *commitFixture) func(*Store) string {
			// Inside the debounce, so the Validate writes nothing itself
			// and the flush is the write.
			f.running.setLastUsedFlushForTest(time.Now())
			before := time.Now()
			if _, ok := f.running.Validate(f.ownRaw); !ok {
				t.Fatal("the running bridge refused its own device")
			}
			if err := f.running.FlushLastUsed(); err != nil {
				t.Errorf("FlushLastUsed: %v", err)
			}
			return lastUsedFrom(f, before)
		}},
		{"Mint", func(t *testing.T, f *commitFixture) func(*Store) string {
			raw, _, err := f.running.Mint("console-pair")
			if err != nil {
				t.Errorf("Mint: %v", err)
			}
			return func(s *Store) string {
				if !holdsRaw(s, raw) {
					return "the token the running bridge minted is not there"
				}
				return ""
			}
		}},
		{"Revoke", func(t *testing.T, f *commitFixture) func(*Store) string {
			if err := f.running.Revoke(f.other.ID); err != nil {
				t.Errorf("Revoke: %v", err)
			}
			return func(s *Store) string {
				if holdsRaw(s, f.otherRaw) {
					return "the token the running bridge revoked is back"
				}
				return ""
			}
		}},
		{"Rotate", func(t *testing.T, f *commitFixture) func(*Store) string {
			raw, _, err := f.running.Rotate(f.other.ID)
			if err != nil {
				t.Errorf("Rotate: %v", err)
			}
			return func(s *Store) string {
				if !holdsRaw(s, raw) || holdsRaw(s, f.otherRaw) {
					return "the rotation the running bridge made is not there"
				}
				return ""
			}
		}},
		{"SetExpiry", func(t *testing.T, f *commitFixture) func(*Store) string {
			exp := time.Now().Add(time.Hour).UTC()
			if _, err := f.running.SetExpiry(f.other.ID, &exp); err != nil {
				t.Errorf("SetExpiry: %v", err)
			}
			return func(s *Store) string {
				if got := tokenIn(s, f.other.ID).ExpiresAt; got == nil || !got.Equal(exp) {
					return fmt.Sprintf("the expiry the running bridge set is not there (%v)", got)
				}
				return ""
			}
		}},
	}
}

// siblingWrites are the writes a CLI makes beside a running bridge, each
// on the victim device except the pairing.
func siblingWrites() []commitWrite {
	return []commitWrite{
		{"bridge pair", func(t *testing.T, f *commitFixture) func(*Store) string {
			raw, _, err := f.sibling.Mint("external-pair")
			if err != nil {
				t.Errorf("sibling Mint: %v", err)
			}
			return func(s *Store) string {
				if !holdsRaw(s, raw) {
					return "the token `bridge pair` minted is gone"
				}
				return ""
			}
		}},
		{"bridge token revoke", func(t *testing.T, f *commitFixture) func(*Store) string {
			if err := f.sibling.Revoke(f.victim.ID); err != nil {
				t.Errorf("sibling Revoke: %v", err)
			}
			return func(s *Store) string {
				if holdsRaw(s, f.victimRaw) {
					return "the token `bridge token revoke` removed is back"
				}
				return ""
			}
		}},
		{"bridge token rotate", func(t *testing.T, f *commitFixture) func(*Store) string {
			raw, _, err := f.sibling.Rotate(f.victim.ID)
			if err != nil {
				t.Errorf("sibling Rotate: %v", err)
			}
			return func(s *Store) string {
				if !holdsRaw(s, raw) || holdsRaw(s, f.victimRaw) {
					return "the rotation `bridge token rotate` made is undone"
				}
				return ""
			}
		}},
		{"bridge token expire", func(t *testing.T, f *commitFixture) func(*Store) string {
			past := time.Now().Add(-time.Minute).UTC()
			if _, err := f.sibling.SetExpiry(f.victim.ID, &past); err != nil {
				t.Errorf("sibling SetExpiry: %v", err)
			}
			return func(s *Store) string {
				if got := tokenIn(s, f.victim.ID).ExpiresAt; got == nil || !got.Equal(past) {
					return fmt.Sprintf("the expiry `bridge token expire` set is gone (%v)", got)
				}
				return ""
			}
		}},
	}
}

// TestASiblingWriteDuringACommitIsNotUndone lands each sibling write inside
// each of the running bridge's writes, after the staging and before the
// commit. The commit re-reads the file just before its rename and rebuilds
// its list around a change, so the file ends up holding both writes, and
// so does the running bridge's memory: a device paired meanwhile is
// accepted, a device revoked meanwhile is refused.
func TestASiblingWriteDuringACommitIsNotUndone(t *testing.T) {
	for _, w := range runningBridgeWrites() {
		for _, sb := range siblingWrites() {
			t.Run(w.name+"/"+sb.name, func(t *testing.T) {
				f := newCommitFixture(t)
				var siblingLanded func(*Store) string
				_, ran := inCommitWindow(t, 1, func() { siblingLanded = sb.do(t, f) })
				landed := w.do(t, f)
				if ran() == 0 {
					t.Fatal("the sibling never ran: the write did not reach its commit")
				}
				for _, where := range []struct {
					name string
					s    *Store
				}{
					{"on disk", reopenStore(t, f.path)},
					{"in the running bridge", f.running},
				} {
					if msg := siblingLanded(where.s); msg != "" {
						t.Errorf("%s: %s", where.name, msg)
					}
					if msg := landed(where.s); msg != "" {
						t.Errorf("%s: %s", where.name, msg)
					}
				}
			})
		}
	}
}

// TestAWriteDoesNotBringBackATokenRevokedDuringIt: a rotation or an expiry
// the console makes on a device that `bridge token revoke` removes while
// the write is staging finds, rebuilt from the file, nothing to change,
// and answers ErrNotFound. Committed from the list read before the revoke,
// it put the device back with a new hash or a new expiry.
func TestAWriteDoesNotBringBackATokenRevokedDuringIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(f *commitFixture) error
	}{
		{"Rotate", func(f *commitFixture) error {
			_, _, err := f.running.Rotate(f.victim.ID)
			return err
		}},
		{"SetExpiry", func(f *commitFixture) error {
			exp := time.Now().Add(time.Hour).UTC()
			_, err := f.running.SetExpiry(f.victim.ID, &exp)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommitFixture(t)
			_, ran := inCommitWindow(t, 1, func() {
				if err := f.sibling.Revoke(f.victim.ID); err != nil {
					t.Errorf("sibling Revoke: %v", err)
				}
			})
			err := tc.write(f)
			if ran() == 0 {
				t.Fatal("the revoke never ran: the write did not reach its commit")
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s of a device revoked during its write = %v, want ErrNotFound", tc.name, err)
			}
			if _, err := reopenStore(t, f.path).Get(f.victim.ID); !errors.Is(err, ErrNotFound) {
				t.Error("on disk: the revoked device is back")
			}
			if _, err := f.running.Get(f.victim.ID); !errors.Is(err, ErrNotFound) {
				t.Error("in the running bridge: the revoked device is back")
			}
		})
	}
}

// TestAWriteGivesUpOnAFileThatKeepsChanging: with a sibling committing
// inside every attempt, a write stops after maxCommitAttempts with
// errStoreMoved, having written nothing. Every token the sibling minted
// is on disk and the one the write was minting is not.
func TestAWriteGivesUpOnAFileThatKeepsChanging(t *testing.T) {
	f := newCommitFixture(t)
	var minted []string
	// More sibling writes than attempts, so a write that is not bounded
	// still ends, and fails the count below rather than hanging the test.
	_, ran := inCommitWindow(t, maxCommitAttempts+2, func() {
		raw, _, err := f.sibling.Mint(fmt.Sprintf("external-%d", len(minted)))
		if err != nil {
			t.Errorf("sibling Mint: %v", err)
			return
		}
		minted = append(minted, raw)
	})
	_, _, err := f.running.Mint("console-pair")
	if !errors.Is(err, errStoreMoved) {
		t.Errorf("Mint against a file that changes during every attempt = %v, want errStoreMoved", err)
	}
	if ran() != maxCommitAttempts {
		t.Errorf("the write was staged %d times, want %d (maxCommitAttempts)", ran(), maxCommitAttempts)
	}
	disk := reopenStore(t, f.path)
	for i, raw := range minted {
		if !holdsRaw(disk, raw) {
			t.Errorf("on disk: the sibling's token %d is gone", i)
		}
	}
	for _, tok := range disk.List() {
		if tok.Name == "console-pair" {
			t.Error("on disk: the write that gave up is there")
		}
	}
}

// TestAWriteThatCannotReReadTheFileDoesNotCommit: a write whose re-read
// before the commit fails writes nothing and reports it, and stages no
// second time. The file may hold a token this process has never seen, as
// here, where `bridge pair` minted one and left the file unreadable to the
// running bridge (a `sudo bridge pair` beside a service install leaves a
// root-owned 0600 tokens.json).
func TestAWriteThatCannotReReadTheFileDoesNotCommit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(f *commitFixture) error
	}{
		{"FlushLastUsed", func(f *commitFixture) error { return f.running.FlushLastUsed() }},
		{"Mint", func(f *commitFixture) error {
			_, _, err := f.running.Mint("console-pair")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommitFixture(t)
			staged, _ := inCommitWindow(t, 1, func() {
				if _, _, err := f.sibling.Mint("external-pair"); err != nil {
					t.Errorf("sibling Mint: %v", err)
				}
				makeTokenFileUnreadable(t, f.path)
			})
			err := tc.write(f)
			if err == nil {
				t.Errorf("%s reported success after it could not re-read the file", tc.name)
			}
			if staged() != 1 {
				t.Errorf("the write was staged %d times, want 1: a read that fails ends it", staged())
			}
			if got := tokenNamesOnDisk(t, f.path); !got["external-pair"] {
				t.Error("on disk: the write committed over a file it could not read, and the sibling's token is gone")
			}
		})
	}
}

// TestAWriteDoesNotRebuildFromAFileItCannotParse: a change the re-read
// before the commit finds, and the rebuild's reload cannot parse, ends the
// write. Nothing is written over the file, the error surfaces, and nothing
// is staged again: a reload that fails aborts the write here as it does
// before the first staging.
func TestAWriteDoesNotRebuildFromAFileItCannotParse(t *testing.T) {
	f := newCommitFixture(t)
	damaged := []byte("[{\"id\": \n")
	staged, _ := inCommitWindow(t, 1, func() {
		if err := os.WriteFile(f.path, damaged, 0o600); err != nil {
			t.Errorf("damage the file: %v", err)
		}
	})
	if err := f.running.FlushLastUsed(); err == nil {
		t.Error("FlushLastUsed reported success over a file it could not parse")
	}
	if staged() != 1 {
		t.Errorf("the write was staged %d times, want 1: a reload that fails ends it", staged())
	}
	if got, err := os.ReadFile(f.path); err != nil || !bytes.Equal(got, damaged) {
		t.Errorf("the write replaced a file it could not parse (%d bytes there now, err %v)", len(got), err)
	}
}

// TestAnUncontendedWriteStagesOnce: with no sibling, every write stages
// once, including the first write after the file is deleted. The re-read
// before each commit compares against the file this process last read or
// wrote; a comparison against anything else reads every commit as a change
// and rebuilds it, a second write and fsync each time.
func TestAnUncontendedWriteStagesOnce(t *testing.T) {
	s, path := newTmpStore(t)
	staged, _ := inCommitWindow(t, 0, func() {})
	var raw string
	var tok Token
	stale := time.Now().Add(-2 * lastUsedFlushInterval)
	for _, step := range []struct {
		name string
		do   func() error
	}{
		{"Mint into a missing file", func() (err error) {
			raw, tok, err = s.Mint("device")
			return err
		}},
		{"Mint", func() error {
			_, _, err := s.Mint("second")
			return err
		}},
		{"Validate's debounced write", func() error {
			s.setLastUsedFlushForTest(stale)
			if _, ok := s.Validate(raw); !ok {
				return errors.New("validate miss")
			}
			return nil
		}},
		{"RecordClientVersion's debounced write", func() error {
			s.setLastUsedFlushForTest(stale)
			s.RecordClientVersion(tok.ID, "9.9.9")
			return nil
		}},
		{"Rotate", func() (err error) {
			raw, _, err = s.Rotate(tok.ID)
			return err
		}},
		{"SetExpiry", func() error {
			exp := time.Now().Add(time.Hour)
			_, err := s.SetExpiry(tok.ID, &exp)
			return err
		}},
		{"FlushLastUsed", s.FlushLastUsed},
		{"Revoke", func() error { return s.Revoke(tok.ID) }},
		{"FlushLastUsed after the file is deleted", func() error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return s.FlushLastUsed()
		}},
		{"Mint after the file is deleted", func() error {
			if err := os.Remove(path); err != nil {
				return err
			}
			_, _, err := s.Mint("third")
			return err
		}},
	} {
		before := staged()
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if n := staged() - before; n != 1 {
			t.Errorf("%s was staged %d times, want 1", step.name, n)
		}
	}
}

// TestASiblingWriteRightAfterACommitIsNotTakenForIt: a sibling commit that
// lands just after the running bridge's rename, while RenameWithRetry
// fsyncs the directory (a median of 0.5 ms on ext4 and 2.8 ms on APFS,
// measured), is not recorded as the file the running bridge wrote. A
// stat of the path taken after that fsync described the sibling's file,
// so reloadIfStale saw nothing new: the running bridge refused a device
// paired there and accepted one revoked there, until it next wrote.
func TestASiblingWriteRightAfterACommitIsNotTakenForIt(t *testing.T) {
	for _, sb := range siblingWrites() {
		t.Run(sb.name, func(t *testing.T) {
			f := newCommitFixture(t)
			var siblingLanded func(*Store) string
			fired := false
			prev := atomicwrite.SetRenameFuncForTest(func(src, dst string) error {
				err := os.Rename(src, dst)
				if err == nil && !fired {
					fired = true
					siblingLanded = sb.do(t, f)
				}
				return err
			})
			t.Cleanup(func() { atomicwrite.SetRenameFuncForTest(prev) })

			if err := f.running.FlushLastUsed(); err != nil {
				t.Fatalf("FlushLastUsed: %v", err)
			}
			if !fired {
				t.Fatal("the sibling never ran: the flush did not rename")
			}
			if msg := siblingLanded(reopenStore(t, f.path)); msg != "" {
				t.Errorf("on disk: %s", msg)
			}
			if msg := siblingLanded(f.running); msg != "" {
				t.Errorf("in the running bridge: %s", msg)
			}
		})
	}
}
