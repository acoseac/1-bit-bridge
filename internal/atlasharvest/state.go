// Package atlasharvest is the bridge side of the Phase-H bridge-driven bulk
// harvest. The iOS app provisions a bulk_harvest credential (an App-Attest-
// minted Atlas token + the Atlas base URL) over the authenticated bridge↔app
// channel; the client here submits the library's artist MB GIDs to Atlas,
// delta-syncs the harvested bios, and caches them in the artist_atlas overlay
// the bridge already serves to iOS. The open-source bridge never holds a
// long-lived Atlas secret of its own — the credential is the user's attested
// device's, revocable at Atlas, and persisted only locally (0600).
package atlasharvest

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// State is the persisted harvest state: the provisioned credential plus the
// delta-sync cursor and the last full-submit time. Stored as a 0600 JSON file
// in the data dir — NOT the manifest DB (it's a secret + small mutable state,
// like tokens.json).
type State struct {
	Token        string    `json:"token"`        // bulk_harvest bearer (secret)
	AtlasBaseURL string    `json:"atlasBaseUrl"` // e.g. https://atlas.ars.md
	ExpiresAt    time.Time `json:"expiresAt"`    // token expiry (zero = unknown)
	ResultCursor int64     `json:"resultCursor"` // delta-sync cursor
	LastSubmitAt time.Time `json:"lastSubmitAt"` // last full library submit
	// PendingCovers maps a release MBID Atlas reported "resolved" (a cover
	// reverse-resolve was enqueued) to the number of premium re-fetch attempts
	// that have come back CAA (premium not ready yet). The refresh sweep drains
	// this: a premium hit removes the entry; a capped miss count drops it (Tidal
	// likely lacks the release). Keyed map = natural dedup + idempotent re-adds.
	PendingCovers map[string]int `json:"pendingCovers,omitempty"`
}

// StateStore persists State atomically. All mutators serialize on mu and
// rewrite the whole file (it's tiny).
type StateStore struct {
	path string
	mu   sync.Mutex
	st   State
}

// OpenStateStore loads the state file, or starts empty when it's absent.
func OpenStateStore(path string) (*StateStore, error) {
	s := &StateStore{path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		// A corrupt state file is non-fatal: drop it and start fresh (the iOS
		// app re-provisions the credential; the cursor resets, which only costs
		// a re-sync of already-cached entities).
		s.st = State{}
	}
	return s, nil
}

// Snapshot returns a copy of the current state. The PendingCovers map is
// deep-copied under the lock — returning s.st by value still shares the
// map's underlying storage, so a caller reading/marshalling the snapshot
// concurrently with AddPendingCovers / SettlePendingCovers (which mutate
// the map under s.mu) would otherwise trip a fatal concurrent map access.
func (s *StateStore) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.st
	if s.st.PendingCovers != nil {
		snap.PendingCovers = make(map[string]int, len(s.st.PendingCovers))
		for k, v := range s.st.PendingCovers {
			snap.PendingCovers[k] = v
		}
	}
	return snap
}

// AtlasCredential returns the provisioned bulk_harvest bearer + Atlas base URL
// for authenticated premium-cover fetches (Phase B). ok=false when no
// credential is provisioned, or when the token is locally known to be expired
// (skip a guaranteed-401 request — the harvest client owns clearing it). The
// signature matches enrich.AtlasCredentialSource so *StateStore satisfies it
// without either package importing the other.
func (s *StateStore) AtlasCredential() (token, baseURL string, ok bool) {
	snap := s.Snapshot()
	if snap.Token == "" || snap.AtlasBaseURL == "" {
		return "", "", false
	}
	if !snap.ExpiresAt.IsZero() && time.Now().After(snap.ExpiresAt) {
		return "", "", false
	}
	return snap.Token, snap.AtlasBaseURL, true
}

// SetCredential records a freshly-provisioned credential, leaving the cursor +
// last-submit untouched (a re-provision of the same library keeps its sync
// position). Resets LastSubmitAt to zero ONLY when the Atlas base URL changes —
// a different Atlas means a fresh library scope.
func (s *StateStore) SetCredential(token, baseURL string, expiresAt time.Time) error {
	// Trim a trailing slash so https://atlas/ and https://atlas compare equal —
	// defensive; the API handler already canonicalizes to scheme://host.
	baseURL = strings.TrimRight(baseURL, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.AtlasBaseURL != baseURL {
		s.st.LastSubmitAt = time.Time{}
		s.st.ResultCursor = 0
	}
	s.st.Token = token
	s.st.AtlasBaseURL = baseURL
	s.st.ExpiresAt = expiresAt
	return s.persistLocked()
}

// SetCursor advances the delta-sync cursor.
func (s *StateStore) SetCursor(c int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c <= s.st.ResultCursor {
		return nil // never rewind
	}
	s.st.ResultCursor = c
	return s.persistLocked()
}

// SetLastSubmit records the time of the last full library submit.
func (s *StateStore) SetLastSubmit(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.LastSubmitAt = t
	return s.persistLocked()
}

// Clear wipes the credential (e.g. after Atlas rejects it as expired), so the
// client stops using a dead token until the app re-provisions. Sync position is
// preserved.
func (s *StateStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Token = ""
	s.st.ExpiresAt = time.Time{}
	return s.persistLocked()
}

// AddPendingCovers records release MBIDs Atlas reported resolved, so the
// refresh sweep re-fetches their (now premium) covers. New entries start at 0
// attempts; an already-pending MBID is left at its current attempt count (a
// re-report shouldn't reset its progress). One persist for the whole batch.
func (s *StateStore) AddPendingCovers(mbids []string) error {
	if len(mbids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.PendingCovers == nil {
		s.st.PendingCovers = make(map[string]int, len(mbids))
	}
	changed := false
	for _, m := range mbids {
		if m == "" {
			continue
		}
		if _, ok := s.st.PendingCovers[m]; !ok {
			s.st.PendingCovers[m] = 0
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.persistLocked()
}

// PendingCoversSnapshot returns a copy of the pending-cover attempt map.
func (s *StateStore) PendingCoversSnapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.st.PendingCovers))
	for k, v := range s.st.PendingCovers {
		out[k] = v
	}
	return out
}

// SettlePendingCovers finalizes a refresh-sweep pass: resolved MBIDs (premium
// fetched) are removed; missed MBIDs have their attempt count incremented and
// are dropped once they reach maxAttempts (Tidal likely lacks the release —
// stop re-fetching it forever). One persist for the whole batch.
func (s *StateStore) SettlePendingCovers(resolved, missed []string, maxAttempts int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.PendingCovers == nil {
		return nil
	}
	for _, m := range resolved {
		delete(s.st.PendingCovers, m)
	}
	for _, m := range missed {
		if _, ok := s.st.PendingCovers[m]; !ok {
			continue
		}
		s.st.PendingCovers[m]++
		if s.st.PendingCovers[m] >= maxAttempts {
			delete(s.st.PendingCovers, m)
		}
	}
	return s.persistLocked()
}

func (s *StateStore) persistLocked() error {
	b, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	return atomicwrite.WriteBytes(s.path, b, ".atlas-harvest-*.json.tmp")
}
