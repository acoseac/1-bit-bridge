package main

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
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
// Every request builds the facts afresh, from one snapshot of the engine
// and at most one read of the identity file, which it never makes or
// writes; it reads ready before eng, never playerOutput (the supervisor's
// alone), and the surface's pointer under the player's mu. Its DAC read is
// bounded separately, with at most one in flight even after a timeout.
type bridgeOutput struct {
	plan    servePlayerPlan
	p       *bridgePlayer
	dataDir string
	// From the boot snapshot of output.player, which the player runs.
	fixedVolume bool
	dsdMode     string
	allowDoP    bool
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
		fixedVolume: player.EffectiveMixer() == "none", dsdMode: player.EffectiveDSDMode(), allowDoP: player.AllowsDoP()}
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
	if !b.Enabled() || b.p == nil {
		return api.BridgeOutputFacts{}
	}
	ready := signalled(b.p.ready)
	id, ok := b.identity(ready)
	if !ok {
		return api.BridgeOutputFacts{}
	}
	state, reason, lastErr := b.state(ready)
	facts := api.BridgeOutputFacts{Listed: true, UUID: id, State: state, Reason: reason, LastError: lastErr,
		DAC:      b.dac(ctx, state),
		OpenHome: b.openHome(id), Codecs: playedCodecs(), FixedVolume: b.fixedVolume, DSDMode: b.dsdMode, AllowDoP: b.allowDoP}
	if ready {
		if be, ok := b.p.be.(*playback.Backend); ok {
			policy, live := be.Facts()
			facts.DSDPlayback = &api.OutputDSDPlayback{BootID: policy.BootID, PolicyGeneration: policy.Generation, QualificationRevision: policy.Revision, Reason: policy.Reason, Tuples: []api.OutputDSDTuple{}}
			for _, p := range policy.Tuples {
				facts.DSDPlayback.Tuples = append(facts.DSDPlayback.Tuples, api.OutputDSDTuple{Format: p.Tuple.Format, BitRate: p.Tuple.BitRate, Channels: 2, PolicyID: p.PolicyID, Pause: p.Pause, TailMS: p.Tail.Milliseconds(), PrefillMS: p.Prefill.Milliseconds()})
			}
			if len(policy.Tuples) > 0 {
				facts.Codecs = append(facts.Codecs, "DSF", "DFF")
			}
			if live != nil {
				facts.Playback = &api.OutputPlayback{BootID: live.BootID, PolicyGeneration: live.PolicyGeneration, Session: uint64(live.Session), Occurrence: uint64(live.Occurrence), State: live.State, Format: live.Format, RateHz: live.RateHz, LogicalRate: live.LogicalRate, Channels: live.Channels, ValidBits: live.ValidBits, PolicyID: live.PolicyID}
			}
		}
	}
	return facts
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

// state is the element's state, reason and lastError: the plan's refusal
// first (ready may never close for it), then the start's outcome, then one
// snapshot of the engine.
func (b *bridgeOutput) state(ready bool) (state, reason, lastError string) {
	if !b.plan.start {
		why := b.plan.refused
		if why == "" {
			why = refusedConfigInvalid
		}
		return bridgeStateUnavailable, why, ""
	}
	if !ready {
		return bridgeStateUnavailable, reasonStarting, ""
	}
	eng := b.p.eng
	if eng == nil {
		return bridgeStateUnavailable, reasonStartFailed, ""
	}
	snap := eng.Snapshot()
	closed := signalled(eng.Done()) || snap.Closed()
	state, reason = playerState(snap, closed)
	return state, reason, playerLastError(snap)
}

// playerSnapshot is what the element reads of an engine snapshot
// (*engine.Snapshot); an interface so the table test reaches every state.
type playerSnapshot interface {
	State() engine.State
	Pending() engine.Intent
	Output() engine.OutputState
	Maintenance() (engine.MaintenanceInfo, bool)
	ReleaseUnconfirmed() (engine.ReleaseInfo, bool)
	LastAttempt() engine.Attempt
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

// playerLastError is device_busy while the last play attempt failed for a
// busy device, "" otherwise: the engine's record of the last attempt, never
// LastStop, which a start that succeeds does not replace.
func playerLastError(s playerSnapshot) string {
	if a := s.LastAttempt(); a.Failed && a.Busy {
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
