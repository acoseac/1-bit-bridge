package main

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// GET /v1/outputs lists serve's player as an element of kind bridge (B470 PR
// 5b). The route asks it through api.BridgeOutput, which bridgeOutput is,
// over the boot plan and the bridgePlayer serve started: serve installs it
// before the API listeners start, whether or not the plan starts the player.
// Its words are a closed list, each named in PROTOCOL.md's bridge object
// (bridgeOutputStates, bridgeOutputReasons, bridgeOutputLastErrors).

// The element's states.
const (
	bridgeStateStopped     = "stopped"
	bridgeStatePlaying     = "playing"
	bridgeStatePaused      = "paused"
	bridgeStateUnavailable = api.BridgeStateUnavailable
)

// The element's reasons while it is unavailable, beside the plan's
// (refusedNotConfigured, refusedConfigInvalid) and the output's
// (outputReasons).
const (
	// reasonStarting is a player whose start has not finished.
	reasonStarting = "starting"
	// reasonStartFailed is a start that ended with no engine running: a
	// failure it recorded, a panic its supervisor recovered, or serve's stop
	// during it.
	reasonStartFailed = "start_failed"
	// reasonClosed is an engine whose shutdown has begun or whose Run has
	// ended.
	reasonClosed = string(engine.ReasonClosed)
	// reasonOutputUnknown is an output no report has reached yet.
	reasonOutputUnknown = string(engine.ReasonOutputUnknown)
	// reasonMaintenance is a maintenance hold.
	reasonMaintenance = string(engine.ReasonMaintenance)
	// reasonReleaseUnconfirmed is a release not confirmed by its deadline:
	// nothing opens the device until it is.
	reasonReleaseUnconfirmed = string(engine.ReasonReleaseUnconfirmed)
	// reasonOutputUnavailable is an output unavailable for a reason none of
	// outputReasons names: never sent today, the list's own bound.
	reasonOutputUnavailable = string(engine.ReasonOutputUnavailable)
)

// lastErrorDeviceBusy is the element's lastError for a play attempt that
// found the device held by another process (engine.Attempt.Busy).
const lastErrorDeviceBusy = "device_busy"

// bridgeOutputStates is every state the element can be in.
func bridgeOutputStates() []string {
	return []string{bridgeStateStopped, bridgeStatePlaying, bridgeStatePaused, bridgeStateUnavailable}
}

// outputReasons is every reason an output report gives the engine
// (engine.OutputState.Reason): each binding state but available
// (hotplug.Reporter's), and serve's binding_changed (playerOutput's);
// disabled is a binding state, which serve's live config withholds by too.
func outputReasons() []string {
	var out []string
	for _, s := range binding.States() {
		if s != binding.StateAvailable {
			out = append(out, string(s))
		}
	}
	return append(out, reasonBindingChanged)
}

// bridgeOutputReasons is every reason the element can carry: the plan's,
// the start's, the engine's and the output's, each once.
func bridgeOutputReasons() []string {
	out := []string{refusedNotConfigured, refusedConfigInvalid, reasonStarting, reasonStartFailed, reasonClosed, reasonOutputUnknown}
	out = append(out, outputReasons()...)
	out = append(out, reasonMaintenance, reasonReleaseUnconfirmed, reasonOutputUnavailable)
	var once []string
	for _, w := range out {
		if !slices.Contains(once, w) {
			once = append(once, w)
		}
	}
	return once
}

// bridgeOutputLastErrors is every lastError word.
func bridgeOutputLastErrors() []string { return []string{lastErrorDeviceBusy} }

// bridgeOutput is api.BridgeOutput over serve's boot plan and its player.
// Every request builds the facts afresh, from one snapshot of the engine,
// one of the backend's (playback.Backend.Facts) and at most one read of the
// identity file, which it never makes or writes; it reads ready before eng,
// never playerOutput (the supervisor's alone), and the surface's pointer
// under the player's mu. Its DAC read is bounded separately, with at most one
// in flight even after a timeout.
type bridgeOutput struct {
	plan    servePlayerPlan
	p       *bridgePlayer
	dataDir string
	// From the boot snapshot of output.player, which the player runs:
	// fixedVolume, and the legacy DSD keys as it holds them, which the
	// console's Player page alone still shows (B728's B13 replaces it); GET
	// /v1/outputs' dsdMode and allowDoP are the bound DAC's, from the
	// backend's snapshot (B8).
	fixedVolume  bool
	bootDSDMode  string
	bootAllowDoP bool
	// DAC reads cannot interrupt a kernel read; retain the flight until
	// it returns. Concurrent callers share it under their own wait bounds.
	dacMu     sync.Mutex
	dacFlight *playerDACFlight
	dacRead   func(context.Context) *api.OutputDAC
	dacLimit  time.Duration
}

// newBridgeOutput is the route's view of p, which plan decided and cfg, the
// boot snapshot, configures.
func newBridgeOutput(plan servePlayerPlan, p *bridgePlayer, cfg *config.Config) *bridgeOutput {
	player := cfg.Output.Player
	b := &bridgeOutput{plan: plan, p: p, dataDir: cfg.DataDir, dacLimit: 500 * time.Millisecond,
		fixedVolume: player.EffectiveMixer() == "none", bootDSDMode: player.EffectiveDSDMode(), bootAllowDoP: player.AllowsDoP()}
	if p != nil {
		b.dacRead = func(ctx context.Context) *api.OutputDAC { return readPlayerDAC(ctx, p.trees, p.device) }
	}
	return b
}

// Enabled is the player's gate: enabled on the boot snapshot, with serve's
// data directory's lock held. It reads nothing.
func (b *bridgeOutput) Enabled() bool { return b != nil && b.plan.lists }

// Facts is the player now: no element while the gate is off or the player
// has no identity.
func (b *bridgeOutput) Facts(ctx context.Context) api.BridgeOutputFacts {
	f, _ := b.facts(ctx)
	return f
}

// facts is Facts and the last play attempt of the one engine snapshot they
// were read from, the zero Attempt with no engine (B676): the console's view
// words its own lastError from that attempt (consoleLastError), which GET
// /v1/outputs' closed list does not name. The player's DSD comes from one
// read of the backend's facts, the set published and its read side (B728
// B8; binding note 7): the route opens no state file and waits on no lock a
// reading of the box holds (the backend's policyMu alone, held for copies,
// and a sink's lock, never held across a kernel call). dsdMode and allowDoP
// are that read's, and off and false with no read: the backend not ready,
// none, or the harness's, which publishes no read side.
func (b *bridgeOutput) facts(ctx context.Context) (api.BridgeOutputFacts, engine.Attempt) {
	if !b.Enabled() || b.p == nil {
		return api.BridgeOutputFacts{}, engine.Attempt{}
	}
	ready := signalled(b.p.ready)
	id, ok := b.identity(ready)
	if !ok {
		return api.BridgeOutputFacts{}, engine.Attempt{}
	}
	state, reason, attempt := b.state(ready)
	facts := api.BridgeOutputFacts{Listed: true, UUID: id, State: state, Reason: reason, LastError: playerLastError(attempt),
		DAC:      b.dac(ctx, state),
		OpenHome: b.openHome(id), Codecs: playedCodecs(), FixedVolume: b.fixedVolume, DSDMode: dsdstate.ModeOff,
		TimeSeekPCM: timeSeekWord(engine.TimeSeek(false)), TimeSeekDSD: timeSeekWord(engine.TimeSeek(true))}
	if ready {
		if be, ok := b.p.be.(*playback.Backend); ok {
			policy, live := be.Facts()
			facts.DSDPlayback, facts.DSDDAC, facts.Playback = bridgeDSDFacts(policy, live, b.p.device)
			if v := policy.DSD; v != nil {
				facts.DSDMode, facts.AllowDoP = v.Mode, v.AllowDoP
			}
			if policy.PlaysDSD() {
				// A tuple, or a rate the player converts to PCM (B728 B11).
				facts.Codecs = append(facts.Codecs, "dsf", "dff")
			}
		}
	}
	return facts, attempt
}

// bridgeDSDFacts is the element's dsdPlayback, dsdDAC and playback from one
// read of the backend's facts (playback.Backend.Facts): the published epoch,
// each tuple naming its transport (absent, native DSD) and its evidence, the
// rates converted with their plans and those refused with their words, the
// warnings, the bound DAC (nil while no DAC is available), and the current
// occurrence's observation, nil before one is installed. A backend with no
// read side (the harness's) publishes its epoch and tuples alone. The free
// text of the DAC's and the profile's (the model, the profile's name, its
// caveats' notes) goes out with the boot binding's serial and port taken out
// wherever they show (playerText), the backend having taken out the unit's
// own (B728 B8; binding note 11); every other string is a word of a closed
// list, a hash, a date or hexadecimal digits, which a serial is not.
func bridgeDSDFacts(policy playback.PlayerPolicy, live *playback.PlaybackFacts, device config.PlayerDevice) (*api.OutputDSDPlayback, *api.OutputDSDDAC, *api.OutputPlayback) {
	v := policy.DSD
	dsd := &api.OutputDSDPlayback{BootID: policy.BootID, PolicyGeneration: policy.Generation, Reason: policy.Reason, Tuples: []api.OutputDSDTuple{},
		Converted: []api.OutputDSDConverted{}, Refused: []api.OutputDSDRefused{}, Warnings: []api.OutputDSDWarning{},
		Model: playerText(policy.Model, device), BCDDevice: policy.BCDDevice}
	for i, p := range policy.Tuples {
		t := api.OutputDSDTuple{Format: p.Tuple.Format, BitRate: p.Tuple.BitRate, Channels: 2, PolicyID: p.PolicyID, Pause: p.Pause, TailMS: p.Tail.Milliseconds(), PrefillMS: p.Prefill.Milliseconds(),
			Transport: playback.TupleTransport(p.Tuple)}
		if v != nil && i < len(v.Evidence) {
			t.Evidence, t.EvidenceOn = v.Evidence[i].Rung, v.Evidence[i].On
		}
		dsd.Tuples = append(dsd.Tuples, t)
	}
	for _, r := range policy.Converted {
		dsd.Converted = append(dsd.Converted, api.OutputDSDConverted{BitRate: r.BitRate, RateHz: r.Plan.RateHz, ValidBits: r.Plan.ValidBits,
			PassbandHz: r.Plan.PassbandHz(), Dither: r.Plan.Dither(), Filter: r.Plan.Filter()})
	}
	var dac *api.OutputDSDDAC
	if v != nil {
		for _, r := range v.Refused {
			dsd.Refused = append(dsd.Refused, api.OutputDSDRefused{BitRate: r.BitRate, Reason: r.Reason})
		}
		for _, w := range v.Warnings {
			dsd.Warnings = append(dsd.Warnings, api.OutputDSDWarning{Code: w.Code, Reason: w.Reason})
		}
		dac = bridgeDSDDAC(v.DAC, device)
	}
	return dsd, dac, bridgePlayback(policy, live)
}

// bridgeDSDDAC is the element's dsdDAC from the backend's read side of the
// bound DAC, nil while there is none: its key, its profile, the choice that
// applies, each mode's option (native's and DoP's rates, [] for none; DoP's
// need of a confirmation and what blocks it; convert's plan, while it is
// offered) and each transport's first use.
func bridgeDSDDAC(d *playback.DSDDAC, device config.PlayerDevice) *api.OutputDSDDAC {
	if d == nil {
		return nil
	}
	out := &api.OutputDSDDAC{DACKey: d.Key, Options: []api.OutputDSDOption{}, FirstUse: []api.OutputDSDFirstUse{},
		Choice: api.OutputDSDChoice{Mode: d.Choice.Mode, MaxBitRate: d.Choice.MaxBitRate, Revision: d.Choice.Revision, DoPConfirmed: d.Choice.DoPConfirmed}}
	if p := d.Profile; p != nil {
		out.Profile = &api.OutputDSDProfile{ID: p.ID, Name: playerText(p.Name, device), Caveats: []api.OutputDSDCaveat{}}
		for _, c := range p.Caveats {
			out.Profile.Caveats = append(out.Profile.Caveats, api.OutputDSDCaveat{Code: c.Code, Transport: c.Transport, Note: playerText(c.Note, device)})
		}
	}
	for _, o := range d.Options {
		w := api.OutputDSDOption{Mode: o.Mode, Offered: o.Offered, Blocked: o.Blocked}
		if o.Rates != nil {
			rates := []api.OutputDSDOptionRate{}
			for _, r := range o.Rates {
				rates = append(rates, api.OutputDSDOptionRate{BitRate: r.BitRate, Evidence: r.Evidence.Rung, EvidenceOn: r.Evidence.On})
			}
			w.Rates = &rates
		}
		if o.Mode == dsdstate.ModeDoP {
			needs := o.NeedsConfirmation
			w.NeedsConfirmation = &needs
		}
		if p := o.Plan; p != nil {
			w.RateHz, w.ValidBits, w.PassbandHz, w.Dither = p.RateHz, p.ValidBits, p.PassbandHz(), p.Dither()
		}
		out.Options = append(out.Options, w)
	}
	for _, f := range d.FirstUse {
		out.FirstUse = append(out.FirstUse, api.OutputDSDFirstUse{Transport: f.Transport, State: f.State, BitRates: slices.Clone(f.BitRates)})
	}
	return out
}

// bridgePlayback is live, the current occurrence's observation, as the
// element sends it, nil before one is installed: a converted occurrence's
// format, rate and valid bits its PCM's and its plan its own, each read from
// the observation; a native or DoP one's evidence its tuple's in the set
// published now, by its policy ID, the rung that set computes as playback
// observes the tuple (an evidence step publishes no new set), while that set
// is the one it was admitted under, its boot and generation the
// observation's, else the one its admission gave it; PCM's and a converted
// one's none. A policy ID names a tuple's transport, format, rate and timing,
// never the DAC, its firmware or the set, so another DAC's tuple, or the same
// DAC's on another firmware, can have it (B728 B8 round 2: a retained
// observation took the next DAC's evidence); a generation names one set, and
// so the unit and the firmware it was computed for.
func bridgePlayback(policy playback.PlayerPolicy, live *playback.PlaybackFacts) *api.OutputPlayback {
	if live == nil {
		return nil
	}
	out := &api.OutputPlayback{BootID: live.BootID, PolicyGeneration: live.PolicyGeneration, Session: uint64(live.Session), Occurrence: uint64(live.Occurrence), State: live.State, Format: live.Format, RateHz: live.RateHz, LogicalRate: live.LogicalRate, Channels: live.Channels, ValidBits: live.ValidBits, PolicyID: live.PolicyID,
		Transport: live.Transport, PackingVersion: live.PackingVersion}
	ev := live.Evidence
	underItsSet := live.BootID == policy.BootID && live.PolicyGeneration == policy.Generation
	if v := policy.DSD; v != nil && underItsSet && live.Transport != playback.TransportConverted && live.PolicyID != "" {
		for i, p := range policy.Tuples {
			if p.PolicyID == live.PolicyID && i < len(v.Evidence) {
				ev = v.Evidence[i]
			}
		}
	}
	out.Evidence, out.EvidenceOn = ev.Rung, ev.On
	if c := live.Converted; c != nil {
		out.Converted = &api.OutputPlaybackConverted{FromBitRate: c.FromBitRate, Filter: c.Filter, PassbandHz: c.PassbandHz, Dither: c.Dither, GainDB: c.GainDB, ClippedSamples: c.ClippedSamples}
	}
	if !live.ObservedAt.IsZero() {
		at := live.ObservedAt.UTC()
		out.ObservedAt = &at
	}
	return out
}

// playerText takes the boot binding's serial and port out of s wherever
// they show, the port in each of its spellings (playback.PortForms), as the
// DAC's labels have them taken out (playerDACText): the free text of the
// player's DSD, which the backend took the unit's own serial and port out of
// already, and which a USB string may hold either in.
func playerText(s string, device config.PlayerDevice) string {
	if device.Serial.Present() {
		s = device.Serial.Redact(s)
	}
	for _, p := range playback.PortForms(device.Port) {
		s = strings.ReplaceAll(s, p, "[redacted]")
	}
	return s
}

// identity is the player's UUID: the started player's own once it runs,
// and before that, or with no engine, the identity file as it is now, read
// at every call and never cached, so a start that makes one and then
// removes it (a refused engine, ForgetPlayerUUID) lists nothing once it
// has. A file that is not there, or does not hold one UUID, is none.
func (b *bridgeOutput) identity(ready bool) (string, bool) {
	if ready && b.p.eng != nil {
		return b.p.id, b.p.id != ""
	}
	id, err := config.ReadPlayerUUID(b.dataDir)
	return id, err == nil
}

// state is the element's state and reason, and the last play attempt its
// lastError is read from: the plan's refusal first (ready may never close for
// it), then the start's outcome, then one snapshot of the engine, the attempt
// its own (the zero Attempt before an engine runs).
func (b *bridgeOutput) state(ready bool) (state, reason string, attempt engine.Attempt) {
	if !b.plan.start {
		why := b.plan.refused
		if why == "" {
			why = refusedConfigInvalid
		}
		return bridgeStateUnavailable, why, engine.Attempt{}
	}
	if !ready {
		return bridgeStateUnavailable, reasonStarting, engine.Attempt{}
	}
	eng := b.p.eng
	if eng == nil {
		return bridgeStateUnavailable, reasonStartFailed, engine.Attempt{}
	}
	snap := eng.Snapshot()
	closed := signalled(eng.Done()) || snap.Closed()
	state, reason = playerState(snap, closed)
	return state, reason, snap.LastAttempt()
}

// playerSnapshot is what the element reads of an engine snapshot
// (*engine.Snapshot); an interface so the table test reaches every state.
type playerSnapshot interface {
	State() engine.State
	Pending() engine.Intent
	Output() engine.OutputState
	Maintenance() (engine.MaintenanceInfo, bool)
	ReleaseUnconfirmed() (engine.ReleaseInfo, bool)
}

// playerState is the element's state and reason from a running engine's
// snapshot, in order: an engine closed; an output not known, or known and
// unavailable, by its report's reason; a maintenance hold; a release not
// confirmed; then the transport: playing (a session starting too), paused,
// stopped, and a release by what follows it (play playing, pause paused,
// stop or none stopped). This is not 5a3's OpenHome mapping, which has
// Buffering and reads maintenance as Stopped.
func playerState(s playerSnapshot, closed bool) (state, reason string) {
	if closed {
		return bridgeStateUnavailable, reasonClosed
	}
	switch out := s.Output(); {
	case !out.Known:
		return bridgeStateUnavailable, reasonOutputUnknown
	case !out.Available:
		return bridgeStateUnavailable, outputReason(out.Reason)
	}
	if _, held := s.Maintenance(); held || s.State() == engine.Maintenance {
		return bridgeStateUnavailable, reasonMaintenance
	}
	if _, ok := s.ReleaseUnconfirmed(); ok {
		return bridgeStateUnavailable, reasonReleaseUnconfirmed
	}
	switch s.State() {
	case engine.Playing:
		return bridgeStatePlaying, ""
	case engine.Paused:
		return bridgeStatePaused, ""
	case engine.Releasing:
		switch s.Pending() {
		case engine.IntentPlay:
			return bridgeStatePlaying, ""
		case engine.IntentPause:
			return bridgeStatePaused, ""
		}
	}
	return bridgeStateStopped, ""
}

// outputReason is an unavailable output's reason as the element gives it:
// the report's own word when it is one of outputReasons, and
// output_unavailable otherwise, so no word PROTOCOL.md does not name
// reaches the wire.
func outputReason(r string) string {
	if slices.Contains(outputReasons(), r) {
		return r
	}
	return reasonOutputUnavailable
}

// playerLastError is device_busy while the last play attempt a failed for a
// busy device, "" otherwise: the engine's record of the last attempt, never
// LastStop, which a start that succeeds does not replace. An attempt denied
// the device's node is "" here too: that cause is the console's alone
// (consoleLastError), never a word of PROTOCOL.md's closed list (B676).
func playerLastError(a engine.Attempt) string {
	if a.Failed && a.Busy {
		return lastErrorDeviceBusy
	}
	return ""
}

// openHome is the player's OpenHome Source while the plan started it and
// it listens on a LAN address: its UDN and the authority of each LOCATION it
// advertises (openhome.Status.Endpoints, from the function discovery builds
// LOCATION with). Nil otherwise; never read from the live config.
func (b *bridgeOutput) openHome(id string) *api.BridgeOpenHomeFacts {
	if !b.plan.openhome {
		return nil
	}
	b.p.mu.Lock()
	s := b.p.surface
	b.p.mu.Unlock()
	if s == nil {
		return nil
	}
	eps := s.Status().Endpoints()
	if len(eps) == 0 {
		return nil
	}
	return &api.BridgeOpenHomeFacts{UDN: openhome.UDN(id), Endpoints: eps}
}

// timeSeekWord is bridge.timeSeek's word for whether the player seeks within
// a track of a kind of content, as its engine says (engine.TimeSeek, which
// the seek's refusal reads; B674): what this build implements, the same in
// every state of the player, never whether a seek can be made now.
func timeSeekWord(supported bool) string {
	if supported {
		return api.TimeSeekSupported
	}
	return api.TimeSeekUnsupported
}

// playedCodecs is the containers the player plays in this release
// (playback.PlayedContainers), as the element lists them.
func playedCodecs() []string {
	var out []string
	for _, c := range playback.PlayedContainers() {
		out = append(out, string(c))
	}
	return out
}

// signalled reports whether ch is closed, without waiting.
func signalled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
