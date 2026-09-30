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
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/baseurl"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("atlasharvest")

// errNotACredentialBase is SetCredential's refusal of a base it cannot hold a
// credential against. It names no part of the base: the value may carry user
// information, and net/url's own parse error quotes it whole.
var errNotACredentialBase = errors.New("atlasharvest: the Atlas base URL is not a plain https base URL naming a host (https://host[:port])")

// State is the persisted harvest state: the provisioned credential plus the
// delta-sync cursor and the last full-submit time. Stored as a 0600 JSON file
// in the data dir — NOT the manifest DB (it's a secret + small mutable state,
// like tokens.json).
type State struct {
	Token string `json:"token"` // bulk_harvest bearer (secret)
	// AtlasBaseURL is "" or baseurl.CredentialBase's scheme://host
	// (e.g. https://atlas.ars.md), whatever the file says: StateStore holds
	// no other form (backlog B97), and every request the harvest client, the
	// booklet fetch, the lyrics tier and the premium cover fetch make is
	// built from it.
	AtlasBaseURL string    `json:"atlasBaseUrl"`
	ExpiresAt    time.Time `json:"expiresAt"`    // token expiry (zero = unknown)
	ResultCursor int64     `json:"resultCursor"` // delta-sync cursor
	LastSubmitAt time.Time `json:"lastSubmitAt"` // last full library submit
	// LastBookletCheckAt is the last booklet availability-check cycle
	// (v1.8) — same slow re-check cadence as LastSubmitAt. Additive field:
	// pre-existing state files unmarshal it as zero (= check due).
	LastBookletCheckAt time.Time `json:"lastBookletCheckAt"`
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
//
// The Atlas base the file holds is held in the one form SetCredential
// stores, or not at all (holdOnlyACredentialBase): a hand edit can leave any
// string there, and every request of the harvest client, the booklet fetch,
// the lyrics tier and the premium cover fetch is built from it. A base with
// no such form is dropped with the credential held against it, and the drop
// is written back, so neither stays on disk; the app provisions a new
// credential, which is what a corrupt file already costs. When that write
// fails the open fails, naming no part of the base: answering as though the
// file no longer held the credential would let a harvest-off revoke
// (ClearStoredCredential) report gone a credential still on disk.
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
		return s, nil
	}
	if s.st.holdOnlyACredentialBase() {
		logger.Warn("atlasharvest.state.base_refused", "path", path,
			"detail", "the atlasBaseUrl this file holds is not a plain https base URL naming a host (https://host[:port]); "+
				"it is dropped with the credential held against it, and the app provisions a new one")
		// No lock: nothing else holds s yet.
		if err := s.persistLocked(); err != nil {
			return nil, fmt.Errorf("atlasharvest: %s holds an Atlas base URL no credential can be held against, "+
				"and could not be rewritten without it: %w", path, err)
		}
	}
	return s, nil
}

// holdOnlyACredentialBase reduces a loaded AtlasBaseURL to the form
// SetCredential stores (baseurl.CredentialBase), or, when it has none, drops
// it with the credential held against it (the token and its expiry) and
// reports true. The sync position stays, as Clear keeps it; a re-provision
// resets the cursor anyway, since its base differs from "".
//
// A base that reduces to the form (a trailing slash, the default port, an
// uppercase scheme, surrounding space) addresses the same endpoint, so it
// keeps its credential, and is written in the reduced form at the next
// write. A re-provision of the same host is then not taken for a new Atlas.
//
// Before backlog B97 the store kept whatever the file held, and the harvest
// client built every request URL from it: user information reached every
// transport error it logged, a path, a query or a fragment swallowed the
// path it appended, plain http carried the bearer token in the clear, and a
// port with no host was dialled on this machine (the stored half of backlog
// B49).
func (st *State) holdOnlyACredentialBase() (dropped bool) {
	if st.AtlasBaseURL == "" {
		return false
	}
	if base := baseurl.CredentialBase(st.AtlasBaseURL); base != "" {
		st.AtlasBaseURL = base
		return false
	}
	st.Token = ""
	st.AtlasBaseURL = ""
	st.ExpiresAt = time.Time{}
	return true
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
//
// The base is stored as baseurl.CredentialBase reduces it (scheme://host), so
// `https://atlas/` and `https://atlas:443` are the host `https://atlas` is,
// and a base with no such form is refused with an error naming none of it,
// before anything changes: no credential, cursor or file is touched. The
// credential endpoint refuses such a base on the wire first, so this is the
// store's own guarantee for every other caller (backlog B97).
func (s *StateStore) SetCredential(token, baseURL string, expiresAt time.Time) error {
	base := baseurl.CredentialBase(baseURL)
	if base == "" {
		return errNotACredentialBase
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.AtlasBaseURL != base {
		s.st.LastSubmitAt = time.Time{}
		s.st.ResultCursor = 0
	}
	s.st.Token = token
	s.st.AtlasBaseURL = base
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

// SetLastBookletCheck records the time of the last booklet availability
// check cycle.
func (s *StateStore) SetLastBookletCheck(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.LastBookletCheckAt = t
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

// ClearStoredCredential forgets the credential the state file at path holds,
// keeping the sync position, for a bridge that opens no StateStore because
// its harvest is off. The file outlives the switch: re-enabling the harvest
// reads it again, so a credential left there would come back into use though
// the app that provisioned it had asked for it to be revoked. A missing file,
// or one holding no credential, is left as it is: nothing is held, so nothing
// is written.
func ClearStoredCredential(path string) error {
	s, err := OpenStateStore(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Token == "" && s.st.ExpiresAt.IsZero() {
		return nil
	}
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
