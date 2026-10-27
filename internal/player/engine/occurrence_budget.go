package engine

import "sync"

// OccurrenceBudget charges a slot until both engine and backend relinquish it.
type OccurrenceBudget struct {
	mu          sync.Mutex
	limit, used int
}

// OccurrenceReservation is the ownership shared by one hand-off's two layers.
type OccurrenceReservation struct {
	b               *OccurrenceBudget
	mu              sync.Mutex
	engine, backend bool
	charged         bool
}

// NewOccurrenceBudget creates bounded accounting; no occurrence is allocated yet.
func NewOccurrenceBudget(limit int) *OccurrenceBudget { return &OccurrenceBudget{limit: limit} }

// Reserve refuses before occurrence objects or channels can be allocated.
func (b *OccurrenceBudget) Reserve() *OccurrenceReservation {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.limit {
		return nil
	}
	b.used++
	return &OccurrenceReservation{b: b, charged: true}
}

// Used includes cancelled work whose owner has not finished relinquishing it.
func (b *OccurrenceBudget) Used() int { b.mu.Lock(); defer b.mu.Unlock(); return b.used }

// ReleaseEngine relinquishes the owner's reference, once.
func (r *OccurrenceReservation) ReleaseEngine() { r.release(true) }

// ReleaseBackend relinquishes openers, content, sink intervals and reporting.
func (r *OccurrenceReservation) ReleaseBackend() { r.release(false) }

func (r *OccurrenceReservation) release(engine bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if engine {
		if r.engine {
			return
		}
		r.engine = true
	} else {
		if r.backend {
			return
		}
		r.backend = true
	}
	if r.engine && r.backend && r.charged {
		r.b.mu.Lock()
		r.b.used--
		r.b.mu.Unlock()
	}
}

// ReservationBackend opts into ownership accounting at the engine hand-off.
type ReservationBackend interface{ OwnsOccurrenceReservations() bool }

func releaseOccurrences(os []*occ) {
	for _, o := range os {
		o.reservation.ReleaseEngine()
	}
}

// Unclassified carries ownership without imposing a native limit on PCM.
// PromoteNative must succeed before allocating native buffers.
func (b *OccurrenceBudget) Unclassified() *OccurrenceReservation {
	return &OccurrenceReservation{b: b}
}

// PromoteNative charges content identified as DSD; PCM never consumes a slot.
func (r *OccurrenceReservation) PromoteNative() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.charged {
		return true
	}
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	if r.b.used >= r.b.limit {
		return false
	}
	r.b.used++
	r.charged = true
	return true
}
