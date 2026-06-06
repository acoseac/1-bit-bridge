package upnpingest

import (
	"sync"
	"time"
)

// memoryUpdateIDStore is the default in-memory backing for
// UpdateIDStore. It persists nothing across process restarts — fine for
// the initial wiring where the first scan tick simply walks regardless
// (the SystemUpdateID skip can't optimize the first tick anyway). The
// production wiring may swap in a SQLite-backed store for cross-restart
// state.
type memoryUpdateIDStore struct {
	mu      sync.Mutex
	entries map[string]memoryEntry
}

type memoryEntry struct {
	id           string
	lastWalkedAt time.Time
}

func newMemoryUpdateIDStore() *memoryUpdateIDStore {
	return &memoryUpdateIDStore{entries: make(map[string]memoryEntry)}
}

func (m *memoryUpdateIDStore) Get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return "", false
	}
	return e.id, true
}

func (m *memoryUpdateIDStore) Set(key, id string, lastWalkedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = memoryEntry{id: id, lastWalkedAt: lastWalkedAt}
}

func (m *memoryUpdateIDStore) LastWalkedAt(key string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return time.Time{}, false
	}
	return e.lastWalkedAt, true
}
