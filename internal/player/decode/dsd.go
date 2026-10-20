package decode

import (
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

const (
	// dsdChannels is the only DSD channel count the 0.2.2 player accepts.
	dsdChannels = 2
	// maxDSDChunks bounds the chunk walk of a DSF file or a DFF FRM8 body,
	// and the sub-chunks of a DFF PROP.
	maxDSDChunks = 256
	// maxDFFPropBody is the largest DFF PROP chunk we load into memory. A
	// real one is about a hundred bytes.
	maxDFFPropBody = 64 << 10
)

// bitReverse maps a byte to the same bits in the opposite order: an
// LSB-first DSF byte to its canonical, bit-7-earliest form.
var bitReverse = func() (t [256]byte) {
	for i := range t {
		var r byte
		for b := 0; b < 8; b++ {
			if i&(1<<b) != 0 {
				r |= 0x80 >> b
			}
		}
		t[i] = r
	}
	return t
}()

func acceptDSDChannels(n uint64) error {
	if n != dsdChannels {
		return refuse(ReasonUnsupportedChannels, "want stereo DSD (2 channels), got %d", n)
	}
	return nil
}

// acceptDSDRate accepts DSD64, DSD128, DSD256 and DSD512 (2,822,400 Hz × 1,
// 2, 4, 8). The 48 kHz family and DSD1024 are refused.
func acceptDSDRate(container string, rate uint64) error {
	switch rate {
	case format.DSD64RateHz, 2 * format.DSD64RateHz, 4 * format.DSD64RateHz, 8 * format.DSD64RateHz:
		return nil
	}
	return refuse(ReasonUnsupportedLayout, "%s DSD rate %d Hz is not DSD64, DSD128, DSD256 or DSD512", container, rate)
}

// checkDSDBuf returns the per-channel length of dst, which must hold one
// slice per channel, all the same length.
func checkDSDBuf(dst [][]byte, channels int) (int, error) {
	if len(dst) != channels {
		return 0, fmt.Errorf("player decode: dst has %d channels, want %d", len(dst), channels)
	}
	n := len(dst[0])
	for c := 1; c < len(dst); c++ {
		if len(dst[c]) != n {
			return 0, fmt.Errorf("player decode: dst channel %d holds %d bytes, channel 0 holds %d", c, len(dst[c]), n)
		}
	}
	return n, nil
}
