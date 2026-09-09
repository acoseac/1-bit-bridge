package adminauth

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// plantIndentedTicketFile rewrites the ticket sidecar with INDENTED JSON.
//
// Indentation is the planted CONTENT that says whether a rewrite happened:
// writeTicketsLocked marshals compactly, so any rewrite destroys it. Content
// rather than an mtime comparison on purpose — Windows' wall clock has ~15.6 ms
// granularity, so two writes inside one tick leave mtimes equal and an
// mtime-based check silently passes on the platform most likely to break.
func plantIndentedTicketFile(t *testing.T, s *Store, tickets map[string]persistedTicket) {
	t.Helper()
	b, err := json.MarshalIndent(tickets, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ticketPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func ticketFileWasRewritten(t *testing.T, s *Store) bool {
	t.Helper()
	b, err := os.ReadFile(s.ticketPath())
	if err != nil {
		t.Fatal(err)
	}
	return !strings.Contains(string(b), "\n    ")
}

// TestFailedRedemptionWithNothingToPruneWritesNothing is the DoS lever.
//
// GET /login/ticket is unauthenticated, unthrottled and past csrfGuard, and
// its miss branch used to rewrite the file unconditionally — a CreateTemp +
// Write + Chmod + Sync + Close + rename-with-parent-fsync per bogus probe,
// under the same s.mu that ValidateSession takes on every authenticated
// console request. The comment beside it always said "still rewrite when
// pruning removed something"; there was simply no way to ask.
func TestFailedRedemptionWithNothingToPruneWritesNothing(t *testing.T) {
	s := ticketStore(t)
	live := map[string]persistedTicket{
		hashTicket("a-live-one"): {Username: "admin", ExpiresAt: time.Now().Add(time.Hour).UnixNano()},
	}
	plantIndentedTicketFile(t, s, live)

	if _, err := s.RedeemLoginTicket("no-such-ticket"); err == nil {
		t.Fatal("a bogus ticket was accepted")
	}
	if ticketFileWasRewritten(t, s) {
		t.Error("a failed redemption rewrote the ticket file with nothing to prune")
	}
}

// TestFailedRedemptionStillPrunesExpiredRecords is the other half, and the
// reason the gate is `pruned` rather than "never write on a miss": expired
// records must not accumulate on a store nobody successfully logs into.
//
// It is also the negative control for the test above — without it, a store
// that never wrote on a miss at all would pass that one.
func TestFailedRedemptionStillPrunesExpiredRecords(t *testing.T) {
	s := ticketStore(t)
	// One live record beside the expired one, so the write path under test is
	// the REWRITE. With only the expired record the pruned set is empty and
	// writeTicketsLocked removes the file instead — a different branch, and
	// asserting on it here would conflate "it pruned" with "it deleted".
	live := hashTicket("a-live-one")
	plantIndentedTicketFile(t, s, map[string]persistedTicket{
		hashTicket("an-expired-one"): {Username: "admin", ExpiresAt: time.Now().Add(-time.Hour).UnixNano()},
		live:                         {Username: "admin", ExpiresAt: time.Now().Add(time.Hour).UnixNano()},
	})

	if _, err := s.RedeemLoginTicket("no-such-ticket"); err == nil {
		t.Fatal("a bogus ticket was accepted")
	}
	if !ticketFileWasRewritten(t, s) {
		t.Fatal("an expired record survived a redemption that should have pruned it")
	}
	var got map[string]persistedTicket
	b, err := os.ReadFile(s.ticketPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("on disk after the prune: %v, want only the live record", got)
	}
	if _, ok := got[live]; !ok {
		t.Error("the prune took the LIVE record too")
	}
}

// TestRedemptionRefusesARenamedAccount closes the window between mint and
// redeem. The two happen in different PROCESSES with up to LoginTicketTTL
// between them, and CreateSession validates nothing — so without the
// re-assertion a rename inside the window mints a fully-privileged session
// for a username the store no longer has.
func TestRedemptionRefusesARenamedAccount(t *testing.T) {
	s := ticketStore(t)
	raw, err := s.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.user.Username = "someone-else"
	s.mu.Unlock()

	if _, err := s.RedeemLoginTicket(raw); err == nil {
		t.Fatal("a ticket for a renamed account still redeemed")
	}
}
