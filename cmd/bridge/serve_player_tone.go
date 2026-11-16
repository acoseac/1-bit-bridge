package main

import (
	"context"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The console's test tone in serve's player (B601). The backend plays it, as
// the third holder of its device slot (playback.Backend.Tone); serve's side is
// the supervisor's half of the tone's gate, set from the output reports it
// hands the engine and from every snapshot of the watcher's it receives, its
// close first at the player's stop, and the console's view's Tone, which asks
// the backend only while the running player is stopped with its output
// available.

// toneBackend is the console's test tone in the player's backend
// (playback.Backend): a backend without it, a boot test's stand-in, plays
// none, and is asked nothing of it.
type toneBackend interface {
	Tone(context.Context, playback.ToneRequest) playback.ToneResult
	ToneOutput(open bool, why playback.ToneOutcome)
	CloseTones()
	ToneStatus() playback.ToneStatus
}

// tonesOf is be's test tone, nil for a backend that plays none.
func tonesOf(be playBackend) toneBackend {
	tb, _ := be.(toneBackend)
	return tb
}

// toneGate is the supervisor's half of the tone's gate
// (playback.Backend.ToneOutput), which the backend asks in the step that
// admits a tone to its device slot. It shuts before the engine hears a report
// that takes the output away (withheld for the live config's reason,
// output_unavailable for any other) or names a unit change (output_changed),
// and at a snapshot that is stale (snapshot_stale) while it is open; it opens
// once the engine has taken an available report and the latest snapshot is
// fresh, whichever of the two comes last, so a recovery at the same
// generation, which makes no report, opens it too. A report the engine did
// not take opens nothing. The backend's gate starts shut (output_unknown), as
// this does, and serve's stop shuts it for good (CloseTones). It is the
// supervisor's alone.
type toneGate struct {
	// avail is the last report the engine took was available: false at
	// first, made false before a report that takes the output away or names
	// a unit change reaches the engine, and when the engine does not take a
	// report, and true only once it has taken an available one.
	avail bool
	// fresh is the latest snapshot the subscription delivered was fresh
	// (probeConditions): false until one is.
	fresh bool
	// open is the gate as the supervisor last set it.
	open bool
}

// toneShutFor is whether rp shuts the tone's gate before the engine hears it,
// and with which word: a report that takes the output away is withheld for a
// reason of the live config's (the player turned off, another DAC named) and
// output_unavailable for any other; an available one that names a unit
// change is output_changed, which ends a tone of the old unit before the
// engine hears of the new one, the next tone locating the new unit.
func toneShutFor(rp engine.OutputReport) (playback.ToneOutcome, bool) {
	switch {
	case !rp.Available && (rp.Reason == string(binding.StateDisabled) || rp.Reason == reasonBindingChanged):
		return playback.ToneWithheld, true
	case !rp.Available:
		return playback.ToneOutputUnavailable, true
	case rp.UnitChanged:
		return playback.ToneOutputChanged, true
	}
	return "", false
}

// toneShut shuts the gate with why before a report reaches the engine: the
// output is not the tone's until the engine takes an available report again.
func (p *bridgePlayer) toneShut(why playback.ToneOutcome) {
	p.gate.avail = false
	p.toneSet(false, why)
}

// toneRefused is a report the engine did not take: what it holds of the
// output is not known here, so a gate that is open shuts (output_unknown),
// and opens again only once the engine takes an available report.
func (p *bridgePlayer) toneRefused() {
	p.gate.avail = false
	if p.gate.open {
		p.toneSet(false, playback.ToneOutputUnknown)
	}
}

// toneTaken is a report the engine took: an available one makes the output
// the tone's, and the gate opens while the latest snapshot is fresh.
func (p *bridgePlayer) toneTaken(rp engine.OutputReport) {
	if !rp.Available {
		return
	}
	p.gate.avail = true
	p.toneOpenIfDue()
}

// toneSnapshot takes the freshness of a snapshot the subscription delivered,
// before anything else is done with it: a stale one shuts the gate while it
// is open (snapshot_stale), and never takes the place of another shut's word
// (output_unknown before the first report, withheld, output_unavailable).
// Whether a fresh one opens the gate is asked once the snapshot is reported
// (toneOpenIfDue), against the report the engine took.
func (p *bridgePlayer) toneSnapshot(fresh bool) {
	p.gate.fresh = fresh
	if !fresh && p.gate.open {
		p.toneSet(false, playback.ToneSnapshotStale)
	}
}

// toneOpenIfDue opens the gate once the engine has taken an available report
// and the latest snapshot is fresh, unless it is open already.
func (p *bridgePlayer) toneOpenIfDue() {
	if p.gate.open || !p.gate.avail || !p.gate.fresh {
		return
	}
	p.toneSet(true, "")
}

// toneSet sets the backend's gate, and the supervisor's record of it.
func (p *bridgePlayer) toneSet(open bool, why playback.ToneOutcome) {
	p.gate.open = open
	if tb := tonesOf(p.run.be); tb != nil {
		tb.ToneOutput(open, why)
	}
}

// The words the console's view refuses a tone with before it asks the
// backend (consolePlayer.Tone), beside the backend's own (playback.ToneWords),
// four of which it says for the same cause: withheld, output_unknown,
// output_unavailable and release_unconfirmed.
const (
	// toneNotRunning is a player serve did not start, one not running yet,
	// one whose engine has stopped, or a backend that plays no tone.
	toneNotRunning = "player_not_running"
	// toneSounding is a player that plays, starts, or holds a pause.
	toneSounding = "player_sounding"
	// toneBusy is a player that is releasing the DAC, held for maintenance,
	// or still has a stream open.
	toneBusy = "player_busy"
)

// consoleToneWords is every word the console's view refuses a tone with.
func consoleToneWords() []string {
	return []string{toneNotRunning, toneSounding, toneBusy, string(playback.ToneWithheld), string(playback.ToneOutputUnknown),
		string(playback.ToneOutputUnavailable), string(playback.ToneReleaseUnconfirmed)}
}

// Tone plays the console's test tone through the running player's DAC
// (B601) when the player takes one now, judged in this order from one
// snapshot of its engine, live (the config the route loaded once) and one
// read of the backend's observation (Facts): a player serve started, running,
// with a backend that plays tones (player_not_running otherwise, an engine
// that has stopped included); no release unconfirmed (release_unconfirmed);
// no maintenance (player_busy); nothing sounding, starting or held paused
// (player_sounding); stopped with nothing pending (player_busy); nothing the
// live config withholds (withheld); its output known (output_unknown) and
// available (output_unavailable); and no stream open in the backend's
// observation (player_busy). Then it asks the backend's Tone on ctx, the
// request's, whose device slot and the supervisor's gate decide again, in one
// step: a play begun since this look holds a record that refuses the tone, or
// meets it in its hold and ends it. It writes nothing, and nothing it does
// makes Sounding true.
func (c *consolePlayer) Tone(ctx context.Context, live *config.Config, req admin.PlayerToneRequest) admin.PlayerToneResult {
	tb, why := c.toneAdmits(live)
	if why != "" {
		return admin.PlayerToneResult{Outcome: why}
	}
	res := tb.Tone(ctx, playback.ToneRequest{LevelDBFS: req.LevelDBFS, Seconds: req.Seconds})
	return admin.PlayerToneResult{Outcome: string(res.Outcome), LastError: res.LastError, Format: string(res.Format),
		RateHz: res.RateHz, ValidBits: res.ValidBits, Frames: res.Frames, Admitted: res.Admitted}
}

// toneAdmits is the running player's tone when one may be asked of it now, or
// the word Tone refuses with.
func (c *consolePlayer) toneAdmits(live *config.Config) (toneBackend, string) {
	eng := c.engine()
	if eng == nil {
		return nil, toneNotRunning
	}
	tb := tonesOf(c.p.be)
	if tb == nil {
		return nil, toneNotRunning
	}
	snap := eng.Snapshot()
	if signalled(eng.Done()) || snap.Closed() {
		return nil, toneNotRunning
	}
	if _, ok := snap.ReleaseUnconfirmed(); ok {
		return nil, string(playback.ToneReleaseUnconfirmed)
	}
	if _, held := snap.Maintenance(); held || snap.State() == engine.Maintenance {
		return nil, toneBusy
	}
	if sounding(snap) {
		return nil, toneSounding
	}
	if snap.State() != engine.Stopped || snap.Pending() != engine.IntentStop {
		return nil, toneBusy
	}
	if c.withheld(live) != "" {
		return nil, string(playback.ToneWithheld)
	}
	switch out := snap.Output(); {
	case !out.Known:
		return nil, string(playback.ToneOutputUnknown)
	case !out.Available:
		return nil, string(playback.ToneOutputUnavailable)
	}
	if streamOpen(c.p.be) {
		return nil, toneBusy
	}
	return tb, ""
}

// streamOpen reports whether be's observation of the occurrence it last
// installed shows its stream open on the DAC now, whatever its transport; a
// backend that observes nothing shows none.
func streamOpen(be playBackend) bool {
	f, ok := be.(interface {
		Facts() (playback.PlayerPolicy, *playback.PlaybackFacts)
	})
	if !ok {
		return false
	}
	_, live := f.Facts()
	return live != nil && live.State == "open"
}

// toneView is the console's test tone now (PlayerRunning.Tone), read from the
// running player's backend with no I/O (playback.Backend.ToneStatus): nil for
// a player serve did not start, one not running yet and one whose backend
// plays none, and idle while no tone plays. Its format, rate and valid bits
// are the tone in the slot's, or the last tone's while none has opened; its
// outcome and lastError the last tone's, cleared when the next takes the slot.
func (c *consolePlayer) toneView() *admin.PlayerTone {
	if c.engine() == nil {
		return nil
	}
	tb := tonesOf(c.p.be)
	if tb == nil {
		return nil
	}
	st := tb.ToneStatus()
	v := &admin.PlayerTone{State: string(st.State), LevelDBFS: st.Request.LevelDBFS, Seconds: st.Request.Seconds,
		Format: string(st.Format), RateHz: st.RateHz, ValidBits: st.ValidBits,
		Outcome: string(st.Last.Outcome), LastError: st.Last.LastError}
	if st.Format == "" {
		v.Format, v.RateHz, v.ValidBits = string(st.Last.Format), st.Last.RateHz, st.Last.ValidBits
	}
	return v
}
