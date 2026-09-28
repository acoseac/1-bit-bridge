package dlna

import (
	"testing"

	"tailscale.com/net/tsaddr"
)

// TestTailscaleULAIsTailscalesRange pins tailscaleULA to the range
// Tailscale's own code numbers its nodes from, so a copy that drifted (a
// typo, a /64 for the /48) fails here instead of letting a tailnet
// interface back into the LAN pickers.
func TestTailscaleULAIsTailscalesRange(t *testing.T) {
	if tailscaleULA != tsaddr.TailscaleULARange() {
		t.Errorf("tailscaleULA = %v, Tailscale's range is %v", tailscaleULA, tsaddr.TailscaleULARange())
	}
}
