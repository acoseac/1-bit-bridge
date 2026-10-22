package hotplug

import (
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
)

// Snapshot is one published view of the box: the inventory generation it
// belongs to, when the box was read, every card's identity as 4a defines
// it, how the configured binding resolved, what changed since the previous
// generation, the event source's mode, and whether readings have failed
// since. It is immutable in fact: its fields are unexported and every
// accessor returns a copy, so what one consumer does with what it was
// given never reaches another. The zero Snapshot is none.
//
// Nothing in it starts playback, and nothing in it means "play": it says
// what the hardware is (B470 PR 4b decides what playback does about it).
type Snapshot struct{ d *snapshot }

type snapshot struct {
	instance   string
	generation uint64
	observed   time.Time
	cards      []Card
	resolution binding.Resolution
	device     config.PlayerDevice
	change     Change
	source     SourceStatus
	stale      *Failure
}

// Card is one card as the snapshot holds it: 4a's binding.Card, and
// whether its unit is the one the previous generation held at a new
// enumeration (a new devnum, a new card number, or a removal the watcher
// saw).
type Card struct {
	binding.Card
	ReEnumerated bool
}

// Change is what a generation changed, so a consumer can act on it without
// comparing snapshots. A snapshot republished at its generation, because
// only its health changed (Stale, Source), carries its generation's Change
// again: act on a generation once.
type Change struct {
	// First is the serve's first snapshot.
	First bool
	// Inventory is a card or a unit that came, went or changed (its
	// identity, port, endpoints or card number), or a unit that
	// re-enumerated.
	Inventory bool
	// State is the binding's state differing from the previous
	// generation's, which Previous holds.
	State    bool
	Previous binding.State
	// Unit is the binding resolved, before and now, to two different
	// units; Swap is that, at the same card number.
	Unit, Swap bool
	// ReEnumerated is the binding resolved, before and now, to the same
	// unit at a new enumeration: a node opened before is dead, and what
	// was learned of the unit is stale.
	ReEnumerated bool
	// Binding is the configured binding differing from the previous
	// generation's (another kind, unit, model, port or endpoint).
	Binding bool
}

// Failure is a run of failed readings: since when, and the first one's
// error. It never quotes a serial (the readers' problems never do).
type Failure struct {
	Since time.Time
	Err   string
}

// Valid reports whether s is a snapshot at all.
func (s Snapshot) Valid() bool { return s.d != nil }

// Instance is the serve's random id: a generation is unique within it,
// and one from an earlier run never matches by coincidence.
func (s Snapshot) Instance() string {
	if s.d == nil {
		return ""
	}
	return s.d.instance
}

// Generation is the inventory generation: 1 for the serve's first
// snapshot, and one more for each snapshot that changed anything
// (Change); never less than an earlier snapshot's.
func (s Snapshot) Generation() uint64 {
	if s.d == nil {
		return 0
	}
	return s.d.generation
}

// Observed is when the box was read for this generation.
func (s Snapshot) Observed() time.Time {
	if s.d == nil {
		return time.Time{}
	}
	return s.d.observed
}

// Cards is every card either tree listed, by index, as a copy.
func (s Snapshot) Cards() []Card {
	if s.d == nil {
		return nil
	}
	out := make([]Card, len(s.d.cards))
	for i, c := range s.d.cards {
		out[i] = Card{Card: copyCard(c.Card), ReEnumerated: c.ReEnumerated}
	}
	return out
}

// Resolution is how the binding resolved (binding.Settled), as a copy:
// its state, the card and the unit's identity, the endpoint, and the cards
// an ambiguous binding names.
func (s Snapshot) Resolution() binding.Resolution {
	if s.d == nil {
		return binding.Resolution{Card: -1}
	}
	return copyResolution(s.d.resolution)
}

// Binding is the configured binding the resolution was made from. Its
// serial is a usbid.Serial, which prints as a redaction.
func (s Snapshot) Binding() config.PlayerDevice {
	if s.d == nil {
		return config.PlayerDevice{}
	}
	return s.d.device
}

// Change is what this generation changed.
func (s Snapshot) Change() Change {
	if s.d == nil {
		return Change{}
	}
	return s.d.change
}

// Source is the event source's mode and why, as it was when this snapshot
// was published (its counts are the service's Status's).
func (s Snapshot) Source() SourceStatus {
	if s.d == nil {
		return SourceStatus{}
	}
	return s.d.source
}

// Stale reports whether the readings since this generation have failed,
// and since when: the snapshot is the last good reading, kept, not one the
// box was found in. A stale snapshot is never grounds to start playback.
func (s Snapshot) Stale() (Failure, bool) {
	if s.d == nil || s.d.stale == nil {
		return Failure{}, false
	}
	return *s.d.stale, true
}

// copyCard is c with nothing shared: its endpoints and the identities it
// points at copied.
func copyCard(c binding.Card) binding.Card {
	c.Endpoints = slices.Clone(c.Endpoints)
	if c.USB != nil {
		u := *c.USB
		c.USB = &u
	}
	if c.ProcUSBID != nil {
		id := *c.ProcUSBID
		c.ProcUSBID = &id
	}
	if c.SysfsUSB != nil {
		u := *c.SysfsUSB
		c.SysfsUSB = &u
	}
	return c
}

// copyResolution is r with its matches copied: an alsa.USBIdentity is a
// value, its serial immutable.
func copyResolution(r binding.Resolution) binding.Resolution {
	r.Matches = slices.Clone(r.Matches)
	return r
}

// resolved reports whether r names a unit: available, or the unit there
// without the bound endpoint.
func resolved(r binding.Resolution) bool {
	return r.State == binding.StateAvailable || r.State == binding.StateEndpointMissing
}

// devnumChanged reports whether two readings of one unit give two
// enumerations: both devnums known and different.
func devnumChanged(a, b alsa.USBIdentity) bool {
	return a.DevNum != 0 && b.DevNum != 0 && a.DevNum != b.DevNum
}
