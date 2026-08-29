package upload

import "sync"

// keyedLocks hands out one mutex per key and reaps entries when nobody holds
// them.
//
// The naive form — a map of mutexes that is never pruned — leaks one entry per
// uploaded file for the lifetime of the process, which a bulk session makes
// significant. The naive FIX is worse: deleting a key while another goroutine
// is between "look up the mutex" and "lock it" hands the two of them different
// mutexes for the same file, which is exactly the mutual exclusion the map
// exists to provide.
//
// So acquisition takes a reference under the map lock BEFORE taking the entry's
// own mutex, and release drops it under the map lock, deleting only at zero. A
// key is therefore never removed while anyone can still be about to lock it.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLockEntry
}

type keyedLockEntry struct {
	mu   sync.Mutex
	refs int
}

func newKeyedLocks() *keyedLocks {
	return &keyedLocks{m: make(map[string]*keyedLockEntry)}
}

// lock blocks until the caller holds the key, and returns the unlock func.
func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	e := k.m[key]
	if e == nil {
		e = &keyedLockEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Unlock()
			k.mu.Lock()
			e.refs--
			if e.refs == 0 {
				delete(k.m, key)
			}
			k.mu.Unlock()
		})
	}
}

// forgetPrefix drops every entry whose key starts with prefix. Called when a
// session's staging directory is removed, so an abandoned session cannot leave
// entries behind. Entries with live references are left alone — an in-flight
// write still owns its mutex, and release will reap it.
func (k *keyedLocks) forgetPrefix(prefix string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for key, e := range k.m {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix && e.refs == 0 {
			delete(k.m, key)
		}
	}
}

// size is the test seam for "no entry survived".
func (k *keyedLocks) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}
