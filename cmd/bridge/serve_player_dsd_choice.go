package main

import (
	"context"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// PUT /v1/outputs/{id}/dsd through serve's view of its player (B728 B9; the
// design's section 7.2): bridgeOutput is the route's api.BridgeDSDChooser, the
// write the backend's (playback.Backend.ChooseDSD) and its answer the epoch
// the backend published once the write ended, read as GET /v1/outputs reads it
// (bridgeDSDFacts), so what a client reads back is what GET then says.

var _ api.BridgeDSDChooser = (*bridgeOutput)(nil)

// OwnsOutput reports whether id is the id of the player's element as GET
// /v1/outputs lists it now: bridge: and the player's identity, read as the
// element reads it (identity), never made.
func (b *bridgeOutput) OwnsOutput(id string) bool {
	if !b.Enabled() || b.p == nil {
		return false
	}
	uuid, ok := b.identity(signalled(b.p.ready))
	return ok && uuid != "" && id == api.BridgeOutputID(uuid)
}

// ChooseDSD writes the bound DAC's DSD choice, the app's, through the
// player's backend, and answers its outcome with the element's dsdDAC and
// dsdPlayback from the epoch published once it ended. A player not running
// yet, or whose backend is not the bridge's (the harness's), has no bound DAC
// to write for: dac_unavailable.
func (b *bridgeOutput) ChooseDSD(ctx context.Context, req api.DSDChoiceRequest) api.DSDChoiceResult {
	if b.p == nil || !signalled(b.p.ready) {
		return api.DSDChoiceResult{Outcome: string(playback.ChoiceDACUnavailable)}
	}
	be, ok := b.p.be.(*playback.Backend)
	if !ok {
		return api.DSDChoiceResult{Outcome: string(playback.ChoiceDACUnavailable)}
	}
	res := be.ChooseDSD(ctx, playback.ChoiceRequest{DACKey: req.DACKey, Interrupt: req.Interrupt, Choice: policy.Choice{
		BaseRevision: req.BaseRevision, Mode: req.Mode, MaxBitRate: req.MaxBitRate,
		ConfirmDoP: req.ConfirmDoP, FirstUseAcknowledged: req.FirstUseAcknowledged, By: dsdstate.ByApp}})
	dsd, dac, _ := bridgeDSDFacts(res.Policy, nil, b.p.device)
	return api.DSDChoiceResult{Outcome: string(res.Outcome), DSDDAC: dac, DSDPlayback: dsd}
}
