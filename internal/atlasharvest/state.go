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

// Snapshot returns a copy of the current state.
func (s *StateStore) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// SetCredential records a freshly-provisioned credential, leaving the cursor +
// last-submit untouched (a re-provision of the same library keeps its sync
// position). Resets LastSubmitAt to zero ONLY when the Atlas base URL changes —
// a different Atlas means a fresh library scope.
func (s *StateStore) SetCredential(token, baseURL string, expiresAt time.Time) error {
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

func (s *StateStore) persistLocked() error {
	b, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	return atomicwrite.WriteBytes(s.path, b, ".atlas-harvest-*.json.tmp")
}
