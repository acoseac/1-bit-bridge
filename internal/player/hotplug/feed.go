package hotplug

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// Feed is where the watcher publishes: one per serve, so the inventory
// generation belongs to the serve and not to one watch. A watch stopped and
// started again (the player disabled and enabled) goes on from the feed's
// last generation, which therefore only ever grows, from 1 at the serve's
// first snapshot; the instance id is random per feed.
//
// Consumers subscribe: each subscription is a one-slot mailbox that keeps
// the newest snapshot, so a consumer that is slow or stuck loses the
// snapshots in between, never the newest, and never holds the watcher up.
type Feed struct {
	instance string

	mu   sync.Mutex
	gen  uint64
	last *snapshot
	// lastSession is the watch that published last, 0 for the disabled
	// snapshot; session is the watch allowed to publish now, 0 for none,
	// and sessions the last one handed out.
	lastSession, session, sessions uint64
	subs                           map[*Subscription]struct{}
}

// NewFeed is a feed with a random instance id and nothing published.
func NewFeed() *Feed {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return &Feed{instance: hex.EncodeToString(b[:]), subs: map[*Subscription]struct{}{}}
}

// Instance is the feed's random id, which every snapshot it publishes
// carries.
func (f *Feed) Instance() string { return f.instance }

// Latest is the newest snapshot, false before the first.
func (f *Feed) Latest() (Snapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		return Snapshot{}, false
	}
	return Snapshot{f.last}, true
}

// Subscription is one consumer's mailbox: C holds the newest snapshot not
// yet received, at most one.
type Subscription struct {
	f  *Feed
	ch chan Snapshot
}

// Subscribe opens a mailbox holding the newest snapshot, when there is one.
func (f *Feed) Subscribe() *Subscription {
	s := &Subscription{f: f, ch: make(chan Snapshot, 1)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[s] = struct{}{}
	if f.last != nil {
		s.ch <- Snapshot{f.last}
	}
	return s
}

// C delivers the newest snapshot each time one is published after the
// last receive.
func (s *Subscription) C() <-chan Snapshot { return s.ch }

// Close takes the mailbox off the feed and empties it: nothing more comes.
func (s *Subscription) Close() {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	delete(s.f.subs, s)
	select {
	case <-s.ch:
	default:
	}
}

// begin makes a new watch the one allowed to publish, and returns its id.
func (f *Feed) begin() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions++
	f.session = f.sessions
	return f.session
}

// end stops watch session from publishing: a reading it finishes after
// its stop, a read the stop abandoned, is discarded.
func (f *Feed) end(session uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.session == session {
		f.session = 0
	}
}

// lastDirs are the card directories of the newest snapshot.
func (f *Feed) lastDirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		return nil
	}
	return cardDirs(f.last.cards)
}

// reading is what one reconcile found, offered to the feed.
type reading struct {
	observed time.Time
	inv      binding.Inventory
	res      binding.Resolution
	device   config.PlayerDevice
	source   SourceStatus
	// removed are the card directories a removal the kernel announced
	// reached since the last publish (watch.noteRemoval).
	removed map[string]bool
}

// offered is what offer did with a reading.
type offered int

const (
	unchanged   offered = iota // nothing the snapshot says changed
	published                  // the next generation
	republished                // the last generation again, its health changed
	discarded                  // a watch no longer the feed's offered it
)

// offer publishes r as the next generation when it differs from the last
// snapshot in anything a snapshot says, and republishes the last at its
// generation when only its health changed (no longer stale, or another
// source mode). It logs each change of the binding's state once.
func (f *Feed) offer(session uint64, r reading, log *slog.Logger) offered {
	next := &snapshot{instance: f.instance, observed: r.observed, resolution: copyResolution(r.res), device: r.device,
		source: SourceStatus{Mode: r.source.Mode, Reason: r.source.Reason}}
	for _, c := range r.inv.Cards {
		next.cards = append(next.cards, Card{Card: copyCard(c)})
	}
	f.mu.Lock()
	if session == 0 || session != f.session {
		f.mu.Unlock()
		return discarded
	}
	ch, changed := compare(f.last, next, r.removed)
	if !changed {
		defer f.mu.Unlock()
		if f.last.stale == nil && f.last.source == next.source {
			return unchanged
		}
		again := *f.last
		again.stale, again.source = nil, next.source
		f.publishLocked(&again, session)
		return republished
	}
	f.gen++
	next.generation, next.change = f.gen, ch
	prev := f.last
	f.publishLocked(next, session)
	f.mu.Unlock()
	logChange(log, prev, next)
	return published
}

// markStale republishes the last snapshot at its generation, marked stale
// with failure, when session published it and it is not stale already.
func (f *Feed) markStale(session uint64, failure Failure) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if session == 0 || session != f.session || f.last == nil || f.lastSession != session || f.last.stale != nil {
		return false
	}
	again := *f.last
	again.stale = &failure
	f.publishLocked(&again, session)
	return true
}

// setSource republishes the last snapshot at its generation with the
// source's new mode, when session published it.
func (f *Feed) setSource(session uint64, st SourceStatus) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := SourceStatus{Mode: st.Mode, Reason: st.Reason}
	if session == 0 || session != f.session || f.last == nil || f.lastSession != session || f.last.source == src {
		return false
	}
	again := *f.last
	again.source = src
	f.publishLocked(&again, session)
	return true
}

// publishDisabled publishes the player turned off, once its watch has
// stopped: binding state disabled, no cards (none were read), the source
// off. Nothing is published when the last snapshot says so already.
func (f *Feed) publishDisabled(observed time.Time, set Settings, log *slog.Logger) bool {
	next := &snapshot{instance: f.instance, observed: observed, device: set.Device, source: SourceStatus{Mode: ModeOff},
		resolution: binding.Resolution{State: binding.StateDisabled, Reason: set.offReason(), Card: -1}}
	f.mu.Lock()
	ch, changed := compare(f.last, next, nil)
	if !changed {
		f.mu.Unlock()
		return false
	}
	f.gen++
	next.generation, next.change = f.gen, ch
	prev := f.last
	f.publishLocked(next, 0)
	f.mu.Unlock()
	logChange(log, prev, next)
	return true
}

// publishLocked makes n the last snapshot and puts it in every mailbox,
// taking out the one a consumer has not received: the send never waits,
// since only this, under the feed's lock, sends.
func (f *Feed) publishLocked(n *snapshot, session uint64) {
	f.last, f.lastSession = n, session
	for s := range f.subs {
		select {
		case <-s.ch:
		default:
		}
		s.ch <- Snapshot{n}
	}
}

// compare says what next changed since prev, marking next's cards whose
// unit re-enumerated, and whether anything a snapshot says changed.
func compare(prev, next *snapshot, removed map[string]bool) (Change, bool) {
	if prev == nil {
		return Change{First: true, Inventory: true, State: true}, true
	}
	c := Change{Previous: prev.resolution.State}
	for i := range next.cards {
		n := &next.cards[i]
		if n.State != binding.CardUSB || n.USB == nil {
			continue
		}
		for _, p := range prev.cards {
			if p.State == binding.CardUSB && p.USB != nil && binding.SameUnit(*p.USB, *n.USB) {
				n.ReEnumerated = p.Index != n.Index || devnumChanged(*p.USB, *n.USB) || removed[p.Dir] || removed[n.Dir]
				break
			}
		}
		c.Inventory = c.Inventory || n.ReEnumerated
	}
	c.Inventory = c.Inventory || !sameCards(prev.cards, next.cards)
	pr, nr := prev.resolution, next.resolution
	c.State = pr.State != nr.State
	if resolved(pr) && resolved(nr) {
		if !binding.SameUnit(pr.Identity, nr.Identity) {
			c.Unit, c.Swap = true, pr.Card == nr.Card
		} else {
			c.ReEnumerated = pr.Card != nr.Card || devnumChanged(pr.Identity, nr.Identity) ||
				removed[dirOf(prev.cards, pr.Card)] || removed[dirOf(next.cards, nr.Card)]
		}
	}
	c.Binding = !SameBinding(prev.device, next.device)
	changed := c.Inventory || c.State || c.Unit || c.ReEnumerated || c.Binding || pr.Card != nr.Card ||
		pr.Endpoint != nr.Endpoint || !slices.Equal(pr.Matches, nr.Matches)
	return c, changed
}

// sameCards reports whether two readings hold the same cards as far as a
// snapshot says: index, ALSA id and name, state, unit (one unit at one
// enumeration) and endpoints. What makes a card transient or unreadable,
// and where its directory is, are not compared.
func sameCards(a, b []Card) bool {
	return slices.EqualFunc(a, b, func(x, y Card) bool {
		if x.Index != y.Index || x.ID != y.ID || x.Name != y.Name || x.State != y.State || !slices.Equal(x.Endpoints, y.Endpoints) {
			return false
		}
		if x.USB == nil || y.USB == nil {
			return x.USB == nil && y.USB == nil
		}
		return binding.SameUnit(*x.USB, *y.USB) && x.USB.DevNum == y.USB.DevNum
	})
}

// SameBinding reports whether two bindings name the same thing: kind,
// model, unit, port and endpoint, ids compared as numbers. It is the
// watcher's one comparison of bindings (a changed binding is read at once,
// and is a change a snapshot names), and serve's player asks it too: a
// snapshot resolved from another binding than the one the player started
// with says nothing about the player's DAC.
func SameBinding(a, b config.PlayerDevice) bool {
	sameID := func(x, y string) bool {
		nx, ex := usbid.ParseHex16(x)
		ny, ey := usbid.ParseHex16(y)
		if ex == nil && ey == nil {
			return nx == ny
		}
		return x == y
	}
	sameSerial := a.Serial.Equal(b.Serial) || (!a.Serial.Present() && !b.Serial.Present())
	return a.Transport == b.Transport && a.Binding == b.Binding && sameID(a.VendorID, b.VendorID) && sameID(a.ProductID, b.ProductID) &&
		sameSerial && a.Port == b.Port && a.PCMDevice == b.PCMDevice && a.Subdevice == b.Subdevice
}

// dirOf is card n's directory in cards, "" when it is not there.
func dirOf(cards []Card, n int) string {
	for _, c := range cards {
		if c.Index == n {
			return c.Dir
		}
	}
	return ""
}

// cardDirs are the directories of cards that have one.
func cardDirs(cards []Card) []string {
	var out []string
	for _, c := range cards {
		if c.Dir != "" {
			out = append(out, c.Dir)
		}
	}
	return out
}

// logChange writes one INFO line for a new generation: "player DAC: …" when
// the binding's state changed or its unit did (another unit, the same one
// re-enumerated), "player hotplug: the inventory changed" for the rest,
// another DAC that came or went, so an operator sees each generation. It
// names the binding by its label, the unit by 4a's description and the
// cards by their models, never by a serial.
func logChange(log *slog.Logger, prev, next *snapshot) {
	r := next.resolution
	attrs := []any{"generation", next.generation, "binding", bindingLabel(next.device)}
	if prev != nil {
		attrs = append(attrs, "previous", string(prev.resolution.State))
	}
	if r.Card >= 0 {
		attrs = append(attrs, "card", r.Card)
	}
	if resolved(r) {
		attrs = append(attrs, "unit", binding.Describe(r.Identity))
	}
	attrs = append(attrs, "reason", r.Reason)
	switch c := next.change; {
	case c.First || c.State:
		log.Info("player DAC: "+string(r.State), attrs...)
	case c.Swap:
		log.Info("player DAC: another unit at the bound card", attrs...)
	case c.Unit:
		log.Info("player DAC: another unit", attrs...)
	case c.ReEnumerated:
		log.Info("player DAC: the bound unit re-enumerated", attrs...)
	default:
		log.Info("player hotplug: the inventory changed", append(attrs, "state", string(r.State), "cards", cardsLabel(next.cards))...)
	}
}

// cardsLabel lists cards for a log line: each index, its state, and a USB
// card's model ("5 usb 20b1:3009"), never a serial.
func cardsLabel(cards []Card) string {
	parts := make([]string, 0, len(cards))
	for _, c := range cards {
		p := strconv.Itoa(c.Index) + " " + string(c.State)
		if c.USB != nil {
			p += " " + c.USB.ID.String()
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// bindingLabel names a binding for a person: its kind and model, its
// serial's label, or its port. A field holding a value it does not take is
// "that value", as inspect shows it, since a serial pasted into the wrong
// field must not reach a log.
func bindingLabel(d config.PlayerDevice) string {
	id := func(s string) string {
		if n, err := usbid.ParseHex16(s); err == nil {
			return usbid.Hex16(n)
		}
		return "that value"
	}
	switch d.Binding {
	case config.BindingSerial:
		return "serial " + id(d.VendorID) + ":" + id(d.ProductID) + " (" + d.Serial.Label() + ")"
	case config.BindingUniqueModel:
		return "uniqueModel " + id(d.VendorID) + ":" + id(d.ProductID)
	case config.BindingPort:
		if _, err := usbid.ParsePort(d.Port); err != nil {
			return "port that value"
		}
		return "port " + d.Port
	case "":
		return "none"
	}
	return "that value"
}
