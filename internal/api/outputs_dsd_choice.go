package api

import "context"

// PUT /v1/outputs/{id}/dsd writes the bound DAC's DSD choice for the
// bridge's own player (B728 B9; the design's section 7.2).

// BridgeDSDChooser is serve's player as PUT /v1/outputs/{id}/dsd asks it.
type BridgeDSDChooser interface {
	// OwnsOutput reports whether id names the player's element now.
	OwnsOutput(id string) bool
	// ChooseDSD writes the choice.
	ChooseDSD(ctx context.Context, req DSDChoiceRequest) DSDChoiceResult
}

// DSDChoiceRequest is a choice write's body, parsed.
type DSDChoiceRequest struct {
	DACKey               string
	BaseRevision         uint64
	Mode                 string
	MaxBitRate           int
	ConfirmDoP           bool
	FirstUseAcknowledged bool
	Interrupt            bool
}

// DSDChoiceResult is a choice write's outcome word and what the player
// published once it ended.
type DSDChoiceResult struct {
	Outcome     string
	DSDDAC      *OutputDSDDAC
	DSDPlayback *OutputDSDPlayback
}

// OutputDSDChoiceResponse is a written choice's answer.
type OutputDSDChoiceResponse struct {
	DSDDAC      *OutputDSDDAC      `json:"dsdDAC,omitempty"`
	DSDPlayback *OutputDSDPlayback `json:"dsdPlayback"`
}
