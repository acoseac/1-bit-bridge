package playback

import (
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"testing"
)

// TestProductionNegotiationCannotSelectNative models a device advertising all native formats.
func TestProductionNegotiationCannotSelectNative(t *testing.T) {
	advertised := map[format.ALSAFormat]bool{format.DSDU32BE: true, format.DSDU32LE: true, format.DSDU16LE: true}
	b := &Backend{}
	for _, bits := range []int{1, 16, 24, 32} {
		for _, f := range b.formatsFor(88200, bits) {
			if advertised[f] {
				t.Fatalf("production offers native %s at %d bits", f, bits)
			}
		}
	}
}
