package alsapcm_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// TestProductionNegotiationCannotSelectNative sends real PCM through Backend
// to a kernel advertising the literal native codes; no translation can yield 0.
func TestProductionNegotiationCannotSelectNative(t *testing.T) {
	for _, bits := range []int{16, 24, 32} {
		t.Run(strconv.Itoa(bits), func(t *testing.T) {
			attempts := 0
			d := newDirect(t, func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
				return func(ctx context.Context, c alsapcm.SinkConfig) (*alsapcm.Sink, error) {
					attempts++
					if c.Transport.Format == format.DSDU16LE || c.Transport.Format == format.DSDU32LE || c.Transport.Format == format.DSDU32BE {
						t.Error("production attempted a native format")
					}
					return open(ctx, c)
				}
			})
			d.k.HardwareFormats(49, 50, 52)
			d.files.add("pcm", wav(88200, bits, pattern(1, 1000, bits)))
			e := d.start(1, "pcm")
			var u *engine.Unplayable
			if !errors.As(e, &u) || u.Reason != playback.ReasonTransport {
				t.Errorf("production native-only negotiation: %v", e)
			}
			if attempts == 0 {
				t.Fatal("did not negotiate through Backend")
			}
			if v := d.k.View(); v.Writes != 0 || v.Committed != "" {
				t.Errorf("production committed native: %+v", v)
			}
			d.release(1)
			d.k.CheckClosedEach(t)
		})
	}
}
