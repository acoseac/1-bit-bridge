package auth

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// makeTokenFileUnreadable renders path unreadable while leaving it
// stat-able, so reloadIfStale still notices the sibling write (mtime +
// size both moved) and then FAILS to read it — the shape of the real
// hazard, where the file on disk is a perfectly good token store this
// process merely cannot see right now.
//
// Skips where the injection doesn't hold: Windows ignores the Go file
// mode for read access (its protection is the per-user-profile NTFS ACL
// on %LOCALAPPDATA%), and root bypasses the permission bits entirely.
// The bug and the fix are platform-independent; only this way of
// provoking a read error is not.
func makeTokenFileUnreadable(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file mode does not gate read access on Windows; no portable way to force a read error here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root — permission bits do not produce EACCES")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod 0: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("filesystem ignores the permission bits (read still succeeded)")
	}
}

// tokenNamesOnDisk reopens the store from scratch and reports which
// token names actually survived on disk.
func tokenNamesOnDisk(t *testing.T, path string) map[string]bool {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("restore mode: %v", err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got := map[string]bool{}
	for _, tok := range s.List() {
		got[tok.Name] = true
	}
	return got
}

// Validate's debounced persist must ABORT when its pre-persist reload
// fails — never write the slice it just failed to refresh.
//
// The reload exists because s.mu is process-local: a sibling
// `bridge pair` can rewrite tokens.json inside the window between
// Validate's top-of-method reload and its debounced persist. Dropping
// the reload's error and persisting anyway puts the pre-reload slice
// back on disk, silently deleting the sibling's freshly-minted token —
// the just-paired device then 401s from the next reload on, with
// nothing in the logs tying it to a login that happened minutes earlier.
//
// FlushLastUsed already guards the identical hazard and states the
// trade: dropping a debounced timestamp is recoverable (the in-memory
// bump survives — reload mutates nothing on its error paths — and lands
// at the next successful flush or at shutdown); an overwrite that
// deletes a sibling's token is not.
func TestValidateSkipsPersistWhenPreflightReloadFails(t *testing.T) {
	s1, path := newTmpStore(t)
	rawOwn, _, err := s1.Mint("serve-process")
	if err != nil {
		t.Fatalf("s1 Mint: %v", err)
	}

	// Sibling store, opened before the mint so it knows serve-process too.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	// Force the debounce window open so Validate reaches the persist branch.
	s1.setLastUsedFlushForTest(time.Now().Add(-2 * lastUsedFlushInterval))

	fired := false
	beforeValidatePersistHook = func() {
		if fired {
			return
		}
		fired = true
		// The sibling's pair lands in the exact reload↔persist window...
		if _, _, err := s2.Mint("external-pair"); err != nil {
			t.Errorf("sibling Mint: %v", err)
		}
		// ...and this process cannot read the result.
		makeTokenFileUnreadable(t, path)
	}
	defer func() { beforeValidatePersistHook = nil }()

	// The validation verdict itself must be unaffected — the token
	// matched, and a persistence problem is not the caller's business.
	if _, ok := s1.Validate(rawOwn); !ok {
		t.Fatal("Validate must still report the match; only the debounced write is skipped")
	}
	if !fired {
		t.Fatal("hook never fired — Validate did not reach the debounced-persist branch")
	}

	got := tokenNamesOnDisk(t, path)
	if !got["external-pair"] {
		t.Error("the debounced persist ran after a FAILED reload and wrote a stale slice, " +
			"deleting the sibling's freshly-minted token from tokens.json")
	}
	if !got["serve-process"] {
		t.Error("lost the pre-existing token")
	}
}

// RecordClientVersion carries the identical shape — same reload, same
// debounce, same hazard. It needs no injection hook because it has no
// top-of-method reload: the only reload it performs is the pre-persist
// one, so making the file unreadable up front lands squarely on it.
func TestRecordClientVersionSkipsPersistWhenPreflightReloadFails(t *testing.T) {
	s1, path := newTmpStore(t)
	_, own, err := s1.Mint("serve-process")
	if err != nil {
		t.Fatalf("s1 Mint: %v", err)
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.Mint("external-pair"); err != nil {
		t.Fatalf("sibling Mint: %v", err)
	}

	// s1's in-memory slice is now stale (it never saw external-pair) and
	// it has no way to refresh.
	makeTokenFileUnreadable(t, path)
	s1.setLastUsedFlushForTest(time.Now().Add(-2 * lastUsedFlushInterval))

	s1.RecordClientVersion(own.ID, "1.9.0")

	got := tokenNamesOnDisk(t, path)
	if !got["external-pair"] {
		t.Error("the debounced client-version persist ran after a FAILED reload and " +
			"wrote a stale slice, deleting the sibling's freshly-minted token")
	}
	if !got["serve-process"] {
		t.Error("lost the pre-existing token")
	}
}
