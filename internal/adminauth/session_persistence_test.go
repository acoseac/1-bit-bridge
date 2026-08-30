package adminauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newPersistStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adminauth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if _, err := s.MintInitial("admin"); err != nil {
		t.Fatalf("MintInitial: %v", err)
	}
	return s, path
}

// TestSessionSurvivesRestart is the whole point of the change: a
// restart must not sign the operator out.
//
// On a single box a restart is a deliberate act by the person who is
// about to log back in. On a hosted bridge it is an auto-install, a
// settings change that needs a bounce, or a container reschedule —
// none of which the operator asked for, and after which "you are
// signed out again" reads as a fault.
func TestSessionSurvivesRestart(t *testing.T) {
	s, path := newPersistStore(t)
	raw, err := s.CreateSession("admin")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Reopen: a new process against the same file.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	sess, err := s2.ValidateSession(raw)
	if err != nil {
		t.Fatalf("session did not survive the restart: %v", err)
	}
	if sess.Username != "admin" {
		t.Errorf("username = %q, want admin", sess.Username)
	}
}

// TestLogoutSurvivesRestart is the half that would be a security bug
// if it were only debounced: a revocation left unwritten is UNDONE by
// a restart, so a logout would silently not be a logout.
func TestLogoutSurvivesRestart(t *testing.T) {
	s, path := newPersistStore(t)
	raw, _ := s.CreateSession("admin")
	s.DeleteSession(raw)

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s2.ValidateSession(raw); err == nil {
		t.Fatal("a logged-out session came back after a restart — the revocation " +
			"was not durable")
	}
}

// TestExpiredSessionsAreDroppedAtLoad: restoring an expired session
// would let a restart EXTEND a login past its deadline, which is the
// opposite of what the hard cap exists for.
func TestExpiredSessionsAreDroppedAtLoad(t *testing.T) {
	s, path := newPersistStore(t)
	raw, _ := s.CreateSession("admin")

	// Rewrite the file with the session aged past the hard cap, which
	// is what a store written eight days ago looks like.
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw2, &f); err != nil {
		t.Fatal(err)
	}
	var sessions map[string]*Session
	if err := json.Unmarshal(f["sessions"], &sessions); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-(SessionHardCap + time.Hour))
	for k := range sessions {
		sessions[k].IssuedAt = old
		sessions[k].LastUsedAt = old
	}
	enc, _ := json.Marshal(sessions)
	f["sessions"] = enc
	out, _ := json.Marshal(f)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n := s2.SessionCount(); n != 0 {
		t.Errorf("SessionCount = %d, want 0 — an expired session was restored", n)
	}
	if _, err := s2.ValidateSession(raw); err == nil {
		t.Fatal("an expired session validated after a restart")
	}
}

// TestLegacyStoreFileStillLoads: every install predating this change
// has a bare userRecord on disk. It must keep working, and the next
// write must upgrade it in place with no migration step.
func TestLegacyStoreFileStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adminauth.json")

	// Produce a genuine legacy file by writing what the pre-change
	// persist() wrote: the bare record, no envelope.
	seed, _ := OpenStore(path)
	if _, err := seed.MintInitial("admin"); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(path)
	var env struct {
		User json.RawMessage `json:"user"`
	}
	if err := json.Unmarshal(blob, &env); err != nil || len(env.User) == 0 {
		t.Fatalf("fixture setup: current persist() did not write an envelope: %v", err)
	}
	if err := os.WriteFile(path, env.User, 0o600); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env.User), "passwordHash") {
		t.Fatal("fixture setup: legacy shape must carry a top-level passwordHash — " +
			"that key is the discriminator load() keys on")
	}

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("legacy file failed to load: %v", err)
	}
	if !s.IsInitialised() {
		t.Fatal("legacy credential was not read")
	}
	if err := s.Verify("admin", "wrong-password"); err == nil {
		t.Fatal("Verify accepted a wrong password — the hash did not survive the read")
	}
	// And the upgrade: a login rewrites the file in the new shape.
	if _, err := s.CreateSession("admin"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), `"sessions"`) {
		t.Error("the file was not upgraded to the envelope on the next write")
	}
	s2, err := OpenStore(path)
	if err != nil || !s2.IsInitialised() {
		t.Fatalf("upgraded file no longer loads: %v", err)
	}
}

// TestLastUsedIsDebouncedNotWrittenPerRequest pins the reason the
// hot path is not an fsync. Without the debounce, every authenticated
// request on the console — and there are many per page — would write
// and fsync the credentials file.
func TestLastUsedIsDebouncedNotWrittenPerRequest(t *testing.T) {
	s, path := newPersistStore(t)
	raw, _ := s.CreateSession("admin")

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Freeze the clock so nothing can cross the flush interval.
	frozen := time.Now()
	s.now = func() time.Time { return frozen }

	for i := 0; i < 25; i++ {
		if _, err := s.ValidateSession(raw); err != nil {
			t.Fatalf("validate %d: %v", i, err)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("the file was rewritten inside the debounce window — 25 validates " +
			"must not produce 25 writes")
	}

	// FlushSessions must land what the debounce held back, and it is
	// what shutdown calls.
	s.now = func() time.Time { return frozen.Add(time.Minute) }
	if err := s.FlushSessions(); err != nil {
		t.Fatalf("FlushSessions: %v", err)
	}
	flushed, _ := os.ReadFile(path)
	if string(flushed) == string(before) {
		t.Error("FlushSessions wrote nothing — debounced activity would be lost at exit")
	}
}

// TestFirstValidateAfterStartupDoesNotWrite: without seeding the
// debounce window at load, lastSessionFlush is the zero time, so the
// first authenticated request of every boot is "due" and fsyncs the
// credentials file — to persist timestamps that were just read off disk
// unchanged.
func TestFirstValidateAfterStartupDoesNotWrite(t *testing.T) {
	s, path := newPersistStore(t)
	raw, _ := s.CreateSession("admin")

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ValidateSession(raw); err != nil {
		t.Fatalf("validate: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("the first validate after startup rewrote the store — the debounce " +
			"window must start at load, not at the zero time")
	}
}

// No test here for "an empty sessions map is omitted": encoding/json's
// omitempty already drops a non-nil empty map, so both spellings emit
// the same bytes and any such test would be vacuous. Verified with a
// 10-line program rather than from memory — the docs define empty as
// "any array, slice, map, or string of length zero", which includes a
// non-nil one. (Declined finding, Gemini on PR #800.)
