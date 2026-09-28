package adminauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// plantTicketFile writes the file a ticket with the given raw value would
// have, with INDENTED JSON, and returns its path.
//
// Indentation is the planted CONTENT that says whether a rewrite happened:
// writeTicketFileLocked marshals compactly, so a rewrite destroys it. Content
// rather than an mtime comparison on purpose — Windows' wall clock has ~15.6 ms
// granularity, so two writes inside one tick leave mtimes equal and an
// mtime-based check silently passes on the platform most likely to break.
func plantTicketFile(t *testing.T, s *Store, raw string, tk persistedTicket) string {
	t.Helper()
	b, err := json.MarshalIndent(tk, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	path := s.ticketFilePath(hashTicket(raw))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// dirSnapshot reads every file in dir, by name.
func dirSnapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

// TestAFailedRedemptionWritesNothing pins the unauthenticated branch.
//
// POST /login/ticket is reachable by anyone past csrfGuard, and a redemption
// holds s.mu, the mutex every authenticated console request takes. A ticket
// that does not exist opens one name that is not there and changes nothing:
// no listing, no prune and no rewrite, whatever else the directory holds.
// When every ticket shared one file, this branch read it whole and rewrote it
// whenever a record in it had expired, and before 2026-09-09 on every probe
// (3.93 ms a request, the LOUPE measured).
func TestAFailedRedemptionWritesNothing(t *testing.T) {
	s := ticketStore(t)
	now := time.Now()
	plantTicketFile(t, s, "a-live-one", persistedTicket{Username: "admin", ExpiresAt: now.Add(time.Hour).UnixNano()})
	plantTicketFile(t, s, "an-expired-one", persistedTicket{Username: "admin", ExpiresAt: now.Add(-time.Hour).UnixNano()})
	if err := os.WriteFile(s.ticketFilePath(hashTicket("a-damaged-one")), []byte(`{"username": `), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(s.path)
	before := dirSnapshot(t, dir)

	if _, err := s.RedeemLoginTicket("no-such-ticket"); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("a bogus ticket = %v, want ErrTicketInvalid", err)
	}
	if after := dirSnapshot(t, dir); !maps.EqualFunc(before, after, func(a, b []byte) bool { return string(a) == string(b) }) {
		t.Errorf("a failed redemption changed the directory:\nbefore %v\nafter  %v", names(before), names(after))
	}
}

// TestAMintRemovesOnlyTicketFilesThatCanNeverRedeem pins the prune, which
// lives in a mint (an operator's act) since the redemption's miss branch
// stopped writing: an expired ticket's file and a damaged one go, a live
// ticket's file stays byte for byte as it was, and a file that is not a
// ticket this store could have written stays too: a name without 64 lowercase
// hex characters, and a dot-prefixed staging file another mint may be about
// to rename.
func TestAMintRemovesOnlyTicketFilesThatCanNeverRedeem(t *testing.T) {
	s := ticketStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	live := plantTicketFile(t, s, "a-live-one", persistedTicket{Username: "admin", ExpiresAt: now.Add(time.Hour).UnixNano()})
	expired := plantTicketFile(t, s, "an-expired-one", persistedTicket{Username: "admin", ExpiresAt: now.Add(-time.Hour).UnixNano()})
	// Exactly at its expiry: the instant ticketLive calls dead.
	atExpiry := plantTicketFile(t, s, "expiring-now", persistedTicket{Username: "admin", ExpiresAt: now.UnixNano()})
	damaged := s.ticketFilePath(hashTicket("a-damaged-one"))
	if err := os.WriteFile(damaged, []byte(`{"username": `), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(s.path)
	digest := hashTicket("a-stranger")
	strangers := []string{
		"adminauth-ticket-not-a-digest.json",
		"adminauth-ticket-" + strings.ToUpper(digest) + ".json",
		"." + filepath.Base(s.ticketFilePath(digest)) + "-123456",
	}
	for _, name := range strangers {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a ticket's"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	liveBefore, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{expired, atExpiry, damaged} {
		if _, err := os.Stat(gone); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived the mint's prune (stat err=%v)", filepath.Base(gone), err)
		}
	}
	if got, err := os.ReadFile(live); err != nil || string(got) != string(liveBefore) {
		t.Errorf("the mint touched a live ticket's file (err=%v):\n%s", err, got)
	}
	for _, name := range strangers {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != "not a ticket's" {
			t.Errorf("the mint touched %s, which is not a ticket file (err=%v)", name, err)
		}
	}
	if _, err := os.Stat(s.ticketFilePath(hashTicket(raw))); err != nil {
		t.Errorf("the minted ticket's file is missing: %v", err)
	}
	if n := len(ticketFiles(t, s)); n != 2 {
		t.Errorf("%d ticket files after the mint, want 2 (the live one and the new one)", n)
	}
}

// TestTheFirstMintRemovesTheSharedTicketFile: adminauth-tickets.json, the one
// file every live ticket shared until 2026-09-28, is read by nothing now, and
// what it held expired within MaxLoginTicketTTL of the upgrade. The first
// mint removes it, so an upgraded install does not keep a dead file of
// digests forever.
func TestTheFirstMintRemovesTheSharedTicketFile(t *testing.T) {
	s := ticketStore(t)
	legacy := filepath.Join(filepath.Dir(s.path), "adminauth-tickets.json")
	body, err := json.Marshal(map[string]persistedTicket{
		hashTicket("an-old-link"): {Username: "admin", ExpiresAt: time.Now().Add(time.Hour).UnixNano()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if legacy != s.legacyTicketPath() {
		t.Fatalf("legacyTicketPath = %s, want %s", s.legacyTicketPath(), legacy)
	}

	if _, err := s.MintLoginTicket("admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the shared ticket file survived the first mint (stat err=%v)", err)
	}
}

// TestAnUnreadableTicketIsAStoreFaultNotAStaleLink: a ticket whose file
// cannot be read has established nothing, so the redemption is a store
// fault, never ErrTicketInvalid, whose stale-link page would send the holder
// for a fresh link while this one waits. Until 2026-09-28 every read error
// read as "no tickets", which the handler turned into exactly that page,
// against its own docblock. The ticket is not spent: once its file reads
// again, the same link redeems.
func TestAnUnreadableTicketIsAStoreFaultNotAStaleLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file mode does not gate reading on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	s := ticketStore(t)
	raw, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	path := s.ticketFilePath(hashTicket(raw))
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	user, err := s.RedeemLoginTicket(raw)
	if err == nil {
		t.Fatalf("an unreadable ticket redeemed as %q", user)
	}
	if errors.Is(err, ErrTicketInvalid) {
		t.Errorf("an unreadable ticket was reported as an invalid one: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the failed redemption removed the ticket's file: %v", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if user, err := s.RedeemLoginTicket(raw); err != nil || user != "admin" {
		t.Errorf("the ticket once its file reads again = (%q, %v), want (admin, nil)", user, err)
	}
}

// TestTicketAbsentTakesAWindowsPermissionErrorForGone pins ticketAbsent's
// table on every platform, which is why it takes the GOOS as a parameter.
// A missing file is gone everywhere. A permission error is gone on Windows
// alone, where a file whose delete is pending answers ERROR_ACCESS_DENIED to
// every open until the last handle on it closes, and a store fault
// everywhere else. Nothing else is gone anywhere.
func TestTicketAbsentTakesAWindowsPermissionErrorForGone(t *testing.T) {
	pathErr := func(op string, err error) error {
		return &fs.PathError{Op: op, Path: "adminauth-ticket-x.json", Err: err}
	}
	for _, tc := range []struct {
		name string
		err  error
		goos string
		want bool
	}{
		{"ENOENT on linux", pathErr("open", syscall.ENOENT), "linux", true},
		{"ENOENT on darwin", pathErr("open", syscall.ENOENT), "darwin", true},
		{"ENOENT on windows", pathErr("open", syscall.ENOENT), "windows", true},
		{"fs.ErrNotExist on windows", fmt.Errorf("remove: %w", fs.ErrNotExist), "windows", true},
		{"EACCES on linux", pathErr("open", syscall.EACCES), "linux", false},
		{"EACCES on darwin", pathErr("remove", syscall.EACCES), "darwin", false},
		{"fs.ErrPermission on linux", pathErr("open", fs.ErrPermission), "linux", false},
		{"fs.ErrPermission on darwin", pathErr("remove", fs.ErrPermission), "darwin", false},
		{"fs.ErrPermission on windows", pathErr("open", fs.ErrPermission), "windows", true},
		{"EACCES on windows", pathErr("remove", syscall.EACCES), "windows", true},
		{"EIO on windows", pathErr("read", syscall.EIO), "windows", false},
		{"EIO on linux", pathErr("read", syscall.EIO), "linux", false},
		{"a damaged ticket on windows", fmt.Errorf("%w: x", errTicketDamaged), "windows", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ticketAbsent(tc.err, tc.goos); got != tc.want {
				t.Errorf("ticketAbsent(%v, %q) = %v, want %v", tc.err, tc.goos, got, tc.want)
			}
		})
	}
}

// names lists a snapshot's file names, for a failure message.
func names(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
