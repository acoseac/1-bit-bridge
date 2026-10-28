package playback

import (
	"context"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// AdmitNativeForIntegration exists only in tests or the explicit overlay.
func AdmitNativeForIntegration(b *Backend, admit func(format.NativeDSD) (engine.NativePlan, error), open func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error)) {
	b.nativeAdmission = admit
	b.nativeOpen = open
}
