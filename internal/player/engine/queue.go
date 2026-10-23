package engine

import (
	"fmt"
	"slices"
)

// The queue's bounds (B470 §8 and §9: TracksMax 1,000, 64 KiB of metadata
// an item and 16 MiB in all). Past any of them a command is refused and
// the queue is unchanged.
const (
	// MaxEntries is the most entries the queue holds.
	MaxEntries = 1000
	// MaxMetadataBytes is the most metadata one entry carries.
	MaxMetadataBytes = 64 << 10
	// MaxTotalMetadataBytes is the most metadata the queue holds in all.
	MaxTotalMetadataBytes = 16 << 20
	// MaxReferenceBytes is the longest reference.
	MaxReferenceBytes = 128
	// MaxReadList is the most IDs one ReadList answers.
	MaxReadList = 1000
)

// Reference is what an entry names for the backend to play: for 5a the
// /dlna/file/{hash} identity of one of the bridge's own indexed files. It
// is at most MaxReferenceBytes of printable ASCII (0x20 to 0x7e), never
// empty, and the engine never opens it, resolves it or reads it as a URI or
// a path: only the backend maps it to a file, and only a NewReference value
// reaches the queue.
type Reference struct{ s string }

// NewReference validates s as a Reference.
func NewReference(s string) (Reference, error) {
	if s == "" {
		return Reference{}, refuse("reference", ReasonBadReference, "empty")
	}
	if len(s) > MaxReferenceBytes {
		return Reference{}, refuse("reference", ReasonBadReference, fmt.Sprintf("%d bytes, more than %d", len(s), MaxReferenceBytes))
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e {
			return Reference{}, refuse("reference", ReasonBadReference, fmt.Sprintf("byte %d is 0x%02x, not printable ASCII", i, c))
		}
	}
	return Reference{s: s}, nil
}

// String is the reference as it was given.
func (r Reference) String() string { return r.s }

// IsZero reports whether r is the zero Reference, which names nothing.
func (r Reference) IsZero() bool { return r.s == "" }

// Entry is one queue entry as a reader sees it: a copy, so changing Meta
// changes nothing the engine holds.
type Entry struct {
	// ID is nonzero and never handed out twice, across runs and crashes.
	ID uint32
	// Ref is what the backend plays.
	Ref Reference
	// Meta is the controller's opaque metadata, at most MaxMetadataBytes.
	Meta []byte
}

// entry is the queue's own copy of an entry. Nothing writes to meta after
// the insert copies it, so snapshots and the writer share it.
type entry struct {
	id   uint32
	ref  Reference
	meta []byte
}

// public is a reader's copy of e.
func (e entry) public() Entry {
	return Entry{ID: e.id, Ref: e.ref, Meta: slices.Clone(e.meta)}
}

// queue is an immutable value: every edit builds a new one, so a snapshot
// that holds one never sees it change.
type queue struct {
	entries []entry
	index   map[uint32]int
	meta    int // the metadata bytes of every entry
}

// emptyQueue is the queue with nothing in it.
var emptyQueue = newQueue(nil)

// newQueue builds a queue over entries, which it keeps.
func newQueue(entries []entry) *queue {
	q := &queue{entries: entries, index: make(map[uint32]int, len(entries))}
	for i, e := range entries {
		q.index[e.id] = i
		q.meta += len(e.meta)
	}
	return q
}

func (q *queue) len() int { return len(q.entries) }

// has reports whether id is in the queue.
func (q *queue) has(id uint32) bool {
	_, ok := q.index[id]
	return ok
}

// pos is id's index.
func (q *queue) pos(id uint32) (int, bool) {
	i, ok := q.index[id]
	return i, ok
}

// get is id's entry.
func (q *queue) get(id uint32) (entry, bool) {
	i, ok := q.index[id]
	if !ok {
		return entry{}, false
	}
	return q.entries[i], true
}

// first is the first entry's ID, or 0.
func (q *queue) first() uint32 {
	if len(q.entries) == 0 {
		return 0
	}
	return q.entries[0].id
}

// ids is a copy of the IDs in order.
func (q *queue) ids() []uint32 {
	out := make([]uint32, len(q.entries))
	for i, e := range q.entries {
		out[i] = e.id
	}
	return out
}

// insertAfter is q with e after the entry after (0: at the head).
func (q *queue) insertAfter(after uint32, e entry) *queue {
	at := 0
	if after != 0 {
		at = q.index[after] + 1
	}
	entries := make([]entry, 0, len(q.entries)+1)
	entries = append(entries, q.entries[:at]...)
	entries = append(entries, e)
	entries = append(entries, q.entries[at:]...)
	return newQueue(entries)
}

// remove is q without id.
func (q *queue) remove(id uint32) *queue {
	i := q.index[id]
	entries := make([]entry, 0, len(q.entries)-1)
	entries = append(entries, q.entries[:i]...)
	entries = append(entries, q.entries[i+1:]...)
	return newQueue(entries)
}

// after is the entry that follows id in play order: the next one, the
// first after the last when repeat is on, or 0. An id of 0 (nothing
// selected) is followed by the first entry.
func (q *queue) after(id uint32, repeat bool) uint32 {
	if id == 0 {
		return q.first()
	}
	i, ok := q.index[id]
	switch {
	case !ok:
		return 0
	case i+1 < len(q.entries):
		return q.entries[i+1].id
	case repeat:
		return q.entries[0].id
	}
	return 0
}

// before is the entry before id in queue order, or id itself at the head
// (Previous restarts it there). An id of 0 is preceded by the first entry.
func (q *queue) before(id uint32) uint32 {
	if id == 0 {
		return q.first()
	}
	i, ok := q.index[id]
	switch {
	case !ok:
		return 0
	case i == 0:
		return id
	}
	return q.entries[i-1].id
}
