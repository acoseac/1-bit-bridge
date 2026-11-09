package admin

import "github.com/acoseac/1-bit-bridge/internal/config"

// enrich.enabled and enrich.deezerEnabled as the console reads them (backlog
// B617): the settings rows, the enrichment card's Off and its Retry missing,
// the Jobs card, and what the two retry routes run.

// enrichSwitchValue is what the settings save writes for a switch the
// operator set, the enrichment switches and since B761 the update check's
// (update.checkEnabled): off is written as false, and on clears the key,
// since absent is on and a file without the key is one an older binary loads.
func enrichSwitchValue(on bool) *bool {
	if on {
		return nil
	}
	off := false
	return &off
}

// enrichmentState is what one config snapshot says of the switches the
// enrichment card and the retries answer to: whether the enricher runs (on),
// whether its Deezer portrait lookups do (deezerOn), and whether the Atlas
// harvest runs, the one retry facet that feeds no enricher. A retry runs what
// it says can run, and the card offers Retry missing only for that.
type enrichmentState struct {
	on, deezerOn, harvestActive bool
}

// enrichmentStateOf reads cfg's switches.
func enrichmentStateOf(cfg *config.Config) enrichmentState {
	if cfg == nil {
		return enrichmentState{on: true, deezerOn: true}
	}
	return enrichmentState{
		on:            cfg.Enrich.EffectiveEnabled(),
		deezerOn:      cfg.Enrich.EffectiveDeezerEnabled(),
		harvestActive: harvestActive(cfg),
	}
}

// harvestActive says whether the Atlas harvest runs on cfg. CONFIG-derived:
// the HarvestForceSubmit closure is wired on every bridge and answers false
// where the client does not run, so its presence is no signal.
func harvestActive(cfg *config.Config) bool {
	return cfg.Atlas.Enabled && cfg.Atlas.HarvestEnabled
}

// artistImagesOn says whether the retry's artist-image facet can do anything:
// it re-queues rows for the enricher to fetch their portraits from Deezer,
// so it needs both switches on. With Deezer off it would re-run each
// portrait-less row and stamp it again with no portrait.
func (st enrichmentState) artistImagesOn() bool { return st.on && st.deezerOn }

// errCodeEnrichmentOff is a retry's refusal while enrich.enabled is off and
// no facet that feeds no enricher can run (409).
const errCodeEnrichmentOff = "enrichment-off"

// errMsgEnrichmentOffRetry says why.
const errMsgEnrichmentOffRetry = "enrichment is off (enrich.enabled), so Retry missing has nothing to do here: " +
	"what it re-queues waits for the enricher, which takes no turn while it is off. " +
	"Turn enrichment on in Settings → Enrichment, then retry."
