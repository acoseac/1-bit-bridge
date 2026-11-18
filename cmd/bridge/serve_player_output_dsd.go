package main

import (
	"context"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// PUT /v1/outputs/{id}/dsd writes the DSD choice of the DAC serve's player
// is bound to (B728 B9 PR 3). The route asks it through api.BridgeDSDChoice,
// which bridgeOutput is, over the boot plan and the bridgePlayer GET
// /v1/outputs' view reads: the id is the element's as GET lists it, and the
// answer's members are projected as GET projects them (bridgeDSDFacts).

// dsdChooser is the player's backend as the write asks it:
// *playback.Backend, and the serve tests' stand-in.
type dsdChooser interface {
	ChooseDSD(ctx context.Context, req playback.ChoiceRequest) playback.ChoiceResult
	Policy() playback.PlayerPolicy
}

// Lists is whether id names the player's element as GET /v1/outputs lists it
// now: the player's gate holds and id is bridge: and the identity, read as
// facts reads it.
func (b *bridgeOutput) Lists(id string) bool {
	if !b.Enabled() || b.p == nil {
		return false
	}
	uuid, ok := b.identity(signalled(b.p.ready))
	return ok && uuid != "" && id == api.BridgeOutputID(uuid)
}

// ChooseDSD writes the bound DAC's DSD choice the route read, by the app
// (dsdstate.ByApp), in this order: the request's words, checked without the
// player (bad_request, no epoch); the player, read ready and only then its
// engine and its backend (dac_unavailable, no epoch, while it is not running
// or its backend makes no choice); the engine's configuration reservation,
// whose refusals answer with the epoch the backend publishes now
// (playback_not_stopped while the transport is not stopped or under
// maintenance; persist_failed with the engine's error for an engine closed or
// a request that ended first); then the backend's write, its outcome, its
// epoch and its cause as it answers them. The reservation is held until the
// backend's write has returned, whatever the request's context does: the
// write, once begun, runs to its end, and the reservation is let go after it
// (output.md). A panic in the player is contained here, logged without its
// value (logPlayerPanic), and answered internal with no epoch, the
// reservation let go.
func (b *bridgeOutput) ChooseDSD(ctx context.Context, req api.BridgeDSDChoiceRequest) (res api.BridgeDSDChoiceResult) {
	preq := playback.ChoiceRequest{DACKey: req.DACKey, DACToken: playback.DACToken(req.DACToken), Choice: policy.Choice{
		BaseRevision: req.BaseRevision, Mode: req.Mode, MaxBitRate: req.MaxBitRate,
		ConfirmDoP: req.ConfirmDoP, FirstUseAcknowledged: req.FirstUseAcknowledged, By: dsdstate.ByApp}}
	if preq.Check() != nil {
		return api.BridgeDSDChoiceResult{Outcome: api.DSDChoiceBadRequest}
	}
	eng, ch := b.chooser()
	if eng == nil || ch == nil {
		return api.BridgeDSDChoiceResult{Outcome: api.DSDChoiceDACUnavailable}
	}
	// A panic before the reservation is held, or in its release.
	defer containDSDChoice(&res)
	lease, err := eng.ReserveConfiguration(ctx)
	if err != nil {
		switch engine.ReasonOf(err) {
		case engine.ReasonNotStopped, engine.ReasonMaintenance:
			return b.dsdAnswer(api.DSDChoicePlaybackNotStopped, ch.Policy(), true, nil)
		default:
			return b.dsdAnswer(api.DSDChoicePersistFailed, ch.Policy(), true, err)
		}
	}
	defer eng.ReleaseConfiguration(lease)
	// A panic in the write, recovered before the reservation is let go.
	defer containDSDChoice(&res)
	cr := ch.ChooseDSD(ctx, preq)
	return b.dsdAnswer(string(cr.Outcome), cr.Policy, cr.Epoch, cr.Err)
}

// chooser is the engine and the backend the write goes through: ready read
// first, then the engine and the backend (cli-serve.md's view bullet); nil
// and nil while the player has not started, with no engine (a start that
// failed), or with a backend that makes no choice.
func (b *bridgeOutput) chooser() (*engine.Engine, dsdChooser) {
	if !b.Enabled() || b.p == nil || !signalled(b.p.ready) {
		return nil, nil
	}
	eng := b.p.eng
	if eng == nil {
		return nil, nil
	}
	ch, ok := b.p.be.(dsdChooser)
	if !ok {
		return nil, nil
	}
	return eng, ch
}

// dsdAnswer is an outcome as the route answers it: with an epoch, the policy
// it carries projected as GET /v1/outputs projects the backend's facts
// (bridgeDSDFacts, its dsdPlayback and dsdDAC; dsdMode and allowDoP the bound
// DAC's view's, off and false with none), and nothing of it without one.
func (b *bridgeOutput) dsdAnswer(outcome string, pol playback.PlayerPolicy, epoch bool, cause error) api.BridgeDSDChoiceResult {
	res := api.BridgeDSDChoiceResult{Outcome: outcome, Epoch: epoch, Cause: cause}
	if !epoch {
		return res
	}
	res.DSDPlayback, res.DSDDAC, _ = bridgeDSDFacts(pol, nil, b.p.device)
	res.DSDMode = dsdstate.ModeOff
	if v := pol.DSD; v != nil {
		res.DSDMode, res.AllowDoP = v.Mode, v.AllowDoP
	}
	return res
}

// containDSDChoice, deferred, recovers a panic in the write's path: logged
// without its value, and answered internal with no epoch.
func containDSDChoice(res *api.BridgeDSDChoiceResult) {
	if r := recover(); r != nil {
		logPlayerPanic("the DSD choice's write", r)
		*res = api.BridgeDSDChoiceResult{Outcome: api.DSDChoiceInternal}
	}
}
