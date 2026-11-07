package playback

import "github.com/acoseac/1-bit-bridge/internal/player/policy"

// converts reports whether o was admitted to be converted to PCM.
func (o *occ) converts() bool {
	return o.admitted != nil && o.policyPlan.Outcome == policy.OutcomeConverted
}
