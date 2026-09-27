package auth

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
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

// debouncedWriter is one of the two writes lastUsedFlushInterval debounces:
// failures are the Error lines a failed attempt logs, and observe makes the
// i-th request's observation and returns what must be on disk once a write
// has landed it.
type debouncedWriter struct {
	name     string
	failures []string
	observe  func(t *testing.T, f *commitFixture, i int) (landed func(*Store) string)
}

// debouncedWriters are Validate's debounced write and RecordClientVersion's.
func debouncedWriters() []debouncedWriter {
	return []debouncedWriter{
		{"Validate", []string{
			"reload before persisting LastUsedAt; skipping persist to avoid clobbering a sibling write",
			"persist LastUsedAt",
		}, func(t *testing.T, f *commitFixture, _ int) func(*Store) string {
			before := time.Now()
			if _, ok := f.running.Validate(f.ownRaw); !ok {
				t.Fatal("the running bridge refused its own device")
			}
			return lastUsedSince(f, before)
		}},
		{"RecordClientVersion", []string{
			"reload before persisting client-version; skipping persist to avoid clobbering a sibling write",
			"persist client-version",
		}, func(_ *testing.T, f *commitFixture, i int) func(*Store) string {
			// A version per request, as a client rotating its header sends:
			// the same one again returns before the debounce is consulted.
			ver := fmt.Sprintf("2.0.%d", i)
			f.running.RecordClientVersion(f.own.ID, ver)
			return func(s *Store) string {
				got := tokenIn(s, f.own.ID).LastClientVersion
				return failUnless(got == ver, fmt.Sprintf("the client version %q is not there (%q)", ver, got))
			}
		}},
	}
}

// writeFailure makes the running bridge's debounced write fail. staged is
// how often one failed attempt stages the file: never when the reload ahead
// of the write fails, once when the write itself does. fail returns what
// undoes the failure, and a check of what must be on disk once it is undone
// (nil for none).
type writeFailure struct {
	name   string
	staged int
	fail   func(t *testing.T, f *commitFixture) (mend func(), landed func(*Store) string)
}

// errRenameRefused is the rename writeFailures' last row refuses with.
var errRenameRefused = errors.New("rename refused by the test")

// writeFailures fail the debounced write two ways at the reload ahead of
// it and two in the write itself.
func writeFailures() []writeFailure {
	return []writeFailure{
		{"a sibling's pair leaves tokens.json unreadable", 0,
			func(t *testing.T, f *commitFixture) (func(), func(*Store) string) {
				// `sudo bridge pair` beside a service install: the new
				// file is root's, 0600.
				landed := siblingPairs(t, f)
				makeTokenFileUnreadable(t, f.path)
				return func() { restoreTokenFileMode(t, f.path) }, landed
			}},
		{"tokens.json turns unreadable in place", 1,
			func(t *testing.T, f *commitFixture) (func(), func(*Store) string) {
				// No write, so the reload finds nothing to read and the
				// write's own re-read before its commit is what fails.
				makeTokenFileUnreadable(t, f.path)
				return func() { restoreTokenFileMode(t, f.path) }, nil
			}},
		{"tokens.json is damaged", 0,
			func(t *testing.T, f *commitFixture) (func(), func(*Store) string) {
				return damageTokenFile(t, f.path), nil
			}},
		{"the rename fails", 1,
			func(t *testing.T, _ *commitFixture) (func(), func(*Store) string) {
				prev := atomicwrite.SetRenameFuncForTest(func(string, string) error { return errRenameRefused })
				mend := func() { atomicwrite.SetRenameFuncForTest(prev) }
				t.Cleanup(mend)
				return mend, nil
			}},
	}
}

// restoreTokenFileMode undoes makeTokenFileUnreadable.
func restoreTokenFileMode(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("restore mode: %v", err)
	}
}

// damageTokenFile replaces the store with two bytes no reload can parse,
// and returns what puts the file back as it was. Unlike a permission error
// it fails the reload on every platform, as root too.
func damageTokenFile(t *testing.T, path string) (mend func()) {
	t.Helper()
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[{"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.WriteFile(path, good, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAFailedDebouncedWriteStartsTheNextWindow: once the window has passed,
// a debounced write that fails still starts the next one, so ten requests
// in a row make one attempt, one Error line and at most one staging, not
// ten. Only a successful write used to start it, so while the write could
// not succeed (a tokens.json the running bridge cannot read, since a `sudo
// bridge pair` beside a service install leaves it root's; a damaged file; a
// write that cannot land) every authenticated request re-read the file and
// logged an Error, and one whose write failed after its staging paid an
// fsync too, under the mutex every request takes. The observations stay in
// memory: a window later the next request tries again, and the shutdown
// flush, which asks nothing of the window, lands them once the failure is
// gone.
func TestAFailedDebouncedWriteStartsTheNextWindow(t *testing.T) {
	for _, w := range debouncedWriters() {
		for _, fl := range writeFailures() {
			t.Run(w.name+"/"+fl.name, func(t *testing.T) { failedWriteStartsTheWindow(t, w, fl) })
		}
	}
}

// failedWriteStartsTheWindow is one row of
// TestAFailedDebouncedWriteStartsTheNextWindow.
func failedWriteStartsTheWindow(t *testing.T, w debouncedWriter, fl writeFailure) {
	const requests = 10
	f := newCommitFixture(t)
	mend, siblingLanded := fl.fail(t, f)
	rec := loggingtest.Record(t)
	staged, _ := inCommitWindow(t, 0, func() {})
	past := time.Now().Add(-2 * lastUsedFlushInterval)

	f.running.setLastUsedFlushForTest(past)
	var landed func(*Store) string
	for i := range requests {
		landed = w.observe(t, f, i)
	}
	if got := rec.Failures(w.failures...); len(got) != 1 {
		t.Errorf("%d requests past the window logged %d failed writes, want 1:\n%s",
			requests, len(got), strings.Join(got, "\n"))
	}
	if got := staged(); got != fl.staged {
		t.Errorf("%d requests past the window staged the file %d times, want %d", requests, got, fl.staged)
	}

	// A window later, the next request tries again.
	f.running.setLastUsedFlushForTest(past)
	landed = w.observe(t, f, requests)
	if got := rec.Failures(w.failures...); len(got) != 2 {
		t.Errorf("the request a window later: %d failed writes logged in all, want 2", len(got))
	}
	if got := staged(); got != 2*fl.staged {
		t.Errorf("the request a window later: %d stagings in all, want %d", got, 2*fl.staged)
	}

	// The failure gone, the shutdown flush lands what the failed attempts
	// left in memory, inside the window the last of them started.
	mend()
	if err := f.running.FlushLastUsed(); err != nil {
		t.Fatalf("FlushLastUsed: %v", err)
	}
	checks := []func(*Store) string{landed}
	if siblingLanded != nil {
		checks = append(checks, siblingLanded)
	}
	requireLanded(t, "on disk", reopenStore(t, f.path), checks...)
}

// storeFailure makes the running bridge's store unreadable to it, and
// returns what undoes that. permission says the failure is a permission
// error, which the report names the uid and the remedy for.
type storeFailure struct {
	name       string
	permission bool
	fail       func(t *testing.T, f *commitFixture) (mend func())
}

// TestAStoreThatCannotBeReadIsReportedOnce: while the running bridge cannot
// read tokens.json it checks devices against the tokens it last read, so a
// device `bridge pair` paired since is refused and one `bridge token revoke`
// removed since is still accepted, and until this was reported nothing said
// so: a 401 is not logged, and the only other line, the Error a failed
// debounced write logs, is about a timestamp and comes only from a device
// the bridge already knew. One Warn when the store first cannot be read,
// naming the file and the error, and for a permission error the uid and
// the remedy; one Info once it can be read again, which the next request
// does, with no restart.
func TestAStoreThatCannotBeReadIsReportedOnce(t *testing.T) {
	for _, tc := range []storeFailure{
		{"unreadable", true, func(t *testing.T, f *commitFixture) func() {
			makeTokenFileUnreadable(t, f.path)
			return func() { restoreTokenFileMode(t, f.path) }
		}},
		{"damaged", false, func(t *testing.T, f *commitFixture) func() {
			return damageTokenFile(t, f.path)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { unreadableStoreReportedOnce(t, tc) })
	}
}

// unreadableStoreReportedOnce is one row of
// TestAStoreThatCannotBeReadIsReportedOnce.
func unreadableStoreReportedOnce(t *testing.T, tc storeFailure) {
	const (
		unreadable = "token store unreadable; checking devices against the tokens last read"
		readable   = "token store readable again"
		requests   = 10
	)
	f := newCommitFixture(t)
	pairedRaw, _, err := f.sibling.Mint("external-pair")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.sibling.Revoke(f.victim.ID); err != nil {
		t.Fatal(err)
	}
	mend := tc.fail(t, f)
	rec := loggingtest.Record(t)

	own := verdict{"the running bridge's own device", f.ownRaw, true}
	revoked := verdict{"the device `bridge token revoke` removed", f.victimRaw, true}
	paired := verdict{"the device `bridge pair` paired", pairedRaw, false}
	for range requests {
		requireVerdicts(t, "while the store cannot be read", f.running, own, revoked, paired)
	}
	lines := rec.Lines(unreadable)
	if len(lines) != 1 {
		t.Errorf("%d requests logged %d lines saying the store cannot be read, want 1:\n%s",
			requests, len(lines), strings.Join(lines, "\n"))
	}
	if len(lines) > 0 {
		requireReport(t, lines[0], f.path, tc.permission)
	}
	if got := rec.Lines(readable); len(got) != 0 {
		t.Errorf("reported readable while it was not:\n%s", strings.Join(got, "\n"))
	}

	mend()
	revoked.accept, paired.accept = false, true
	for range requests {
		requireVerdicts(t, "once the store can be read again", f.running, own, revoked, paired)
	}
	if got := rec.Lines(readable); len(got) != 1 {
		t.Errorf("%d lines saying the store is readable again, want 1:\n%s", len(got), strings.Join(got, "\n"))
	}
	if got := rec.Lines(unreadable); len(got) != 1 {
		t.Errorf("%d lines saying the store cannot be read in all, want 1:\n%s", len(got), strings.Join(got, "\n"))
	}
}

// requireReport checks the line saying the store cannot be read: a Warn
// naming the file and the cause, and the uid and the remedy only for a
// permission error, since a chown mends nothing else.
func requireReport(t *testing.T, line, path string, permission bool) {
	t.Helper()
	cause := "parse token store"
	if permission {
		cause = "permission denied"
	}
	for _, want := range []string{"WARN ", " path=" + path + " ", cause} {
		if !strings.Contains(line, want) {
			t.Errorf("the line does not carry %q:\n%s", want, line)
		}
	}
	for _, attr := range []string{" uid=", " hint="} {
		if got := strings.Contains(line, attr); got != permission {
			t.Errorf("the line carries %q: %v, want %v:\n%s", attr, got, permission, line)
		}
	}
}

// verdict is what Validate must answer for one device's raw token.
type verdict struct {
	device string
	raw    string
	accept bool
}

// requireVerdicts reports each device s does not answer as its verdict says.
func requireVerdicts(t *testing.T, when string, s *Store, want ...verdict) {
	t.Helper()
	for _, v := range want {
		if _, ok := s.Validate(v.raw); ok != v.accept {
			t.Errorf("%s: Validate = %v for %s, want %v", when, ok, v.device, v.accept)
		}
	}
}
