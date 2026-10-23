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
//
// A caller that knows more than the snapshots say makes its reports through
// Withhold and Restate (serve's player, from the live config, which the
// watcher's readings may not have caught up with): they are numbered from
// the same count, so the engine's input stays monotonic whichever source a
// report comes from, and no generation carries two reports. Availability
// comes from a snapshot alone: Withhold reports the output unavailable, and
// Restate gives back the last snapshot's own verdict.
type Reporter struct {
	mu   sync.Mutex
	gen  uint64
	have bool
	last struct {
		instance   string
		generation uint64
		// available and reason are the last snapshot's own verdict, which
		// Restate gives back.
		available bool
		reason    string
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
	r.last.available, r.last.reason = rep.Available, rep.Reason
	return rep, true
}

// Withhold is a report the caller makes from what it knows beside the
// snapshots: the output unavailable with reason, numbered one more from the
// count Report numbers from. It is false, numbering nothing, until a
// snapshot has been reported: until then the engine holds the output
// unknown, which refuses Play. It changes nothing the Reporter tracks of the
// unit.
func (r *Reporter) Withhold(reason string) (engine.OutputReport, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.have {
		return engine.OutputReport{}, false
	}
	r.gen++
	return engine.OutputReport{Generation: r.gen, Reason: reason}, true
}

// Restate is the report of the last snapshot reported, its own verdict
// (available, or unavailable with the binding's state), again, numbered one
// more from the same count: how a caller that withheld the output gives it
// back, and the one way it can. It sets no UnitChanged, since no snapshot
// came between: the next one is flagged as Report flags it. False until a
// snapshot has been reported.
func (r *Reporter) Restate() (engine.OutputReport, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.have {
		return engine.OutputReport{}, false
	}
	r.gen++
	return engine.OutputReport{Generation: r.gen, Available: r.last.available, Reason: r.last.reason}, true
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
