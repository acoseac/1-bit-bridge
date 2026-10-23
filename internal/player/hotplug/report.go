package hotplug

import (
	"context"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// Reporter turns the watcher's snapshots into the player engine's output
// input (engine.OutputReport, B470 PR 4b2), one report per generation a
// feed publishes, for the engine's whole life: its generation is its own,
// one more at each report, so a watcher replaced by another (a restart of
// the hotplug service, whose new feed numbers its generations from 1 again)
// can never freeze the input with a generation the engine has passed.
//
// A report is available when the binding resolves to its card
// (binding.StateAvailable), and otherwise unavailable with the binding's
// state as its reason (device_missing, device_ambiguous, disabled, …).
// UnitChanged is set when the binding resolves to another unit than the one
// it last resolved to, or to that unit at another enumeration: re-enumerated
// as the snapshot says, at another card number or USB device number, or back
// after a snapshot in which the binding resolved to no unit. A snapshot a
// feed republishes at its generation, its health alone changed (Stale, the
// source's mode), is no report: it says nothing new about the unit, its
// generation is acted on once, and a session's start resolves the binding
// itself on a whole reading (playback.BindingLocator), so a stale
// availability never starts playback on its own. Until the first report
// the engine holds the output unknown and refuses Play. The Reporter sends
// nothing but reports: the output's return never plays anything.
type Reporter struct {
	mu   sync.Mutex
	gen  uint64
	have bool
	last struct {
		instance   string
		generation uint64
	}
	// unit and card are what the binding last resolved to, nil before it
	// ever did; left is a report since in which it resolved to no unit.
	unit *alsa.USBIdentity
	card int
	left bool
}

// Report is the report snap makes, false when it makes none: an invalid
// snapshot, or the generation already reported.
func (r *Reporter) Report(snap Snapshot) (engine.OutputReport, bool) {
	if !snap.Valid() {
		return engine.OutputReport{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.have && r.last.instance == snap.Instance() && r.last.generation == snap.Generation() {
		return engine.OutputReport{}, false
	}
	r.have = true
	r.last.instance, r.last.generation = snap.Instance(), snap.Generation()
	res, ch := snap.Resolution(), snap.Change()
	r.gen++
	rep := engine.OutputReport{Generation: r.gen, Available: res.State == binding.StateAvailable}
	if !rep.Available {
		rep.Reason = string(res.State)
	}
	if resolved(res) {
		id := res.Identity
		if u := r.unit; u != nil {
			rep.UnitChanged = !binding.SameUnit(*u, id) || r.left || devnumChanged(*u, id) || r.card != res.Card ||
				ch.Unit || ch.ReEnumerated
		}
		r.unit, r.card, r.left = &id, res.Card, false
	} else if r.unit != nil {
		r.left = true
	}
	return rep, true
}

// Run reports each snapshot sub delivers through report (the engine's
// ReportOutput) until ctx ends, and returns then; or once report refuses
// with the engine's closed reason, the engine having shut down. A report
// the engine refuses otherwise is dropped: the next generation's goes.
func (r *Reporter) Run(ctx context.Context, sub *Subscription, report func(context.Context, engine.OutputReport) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case snap := <-sub.C():
			rep, ok := r.Report(snap)
			if !ok {
				continue
			}
			if err := report(ctx, rep); engine.ReasonOf(err) == engine.ReasonClosed {
				return
			}
		}
	}
}
