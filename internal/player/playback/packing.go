package playback

import (
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// packings is what each depth of PCM may be packed as, B470's §1 table, in
// the order the backend offers them to a DAC: the container as wide as the
// samples first. The format package packs each without changing a value:
// S16_LE takes 16 bits; S24_3LE and S24_LE 24, the second in a four-byte
// container with the low 24 bits significant; S32_LE 32, or 24 shifted left
// eight bits with the low byte zero. Nothing else is ever sent: no
// resampling, no dither, no gain.
var packings = map[int][]format.ALSAFormat{
	16: {format.S16LE},
	24: {format.S243LE, format.S24LE, format.S32LE},
	32: {format.S32LE},
}

// packKey is one rate and depth, which the formats a DAC took are kept by.
type packKey struct {
	rate, bits int
}

// transportFor is the transport pcm plays as on the device: its own rate,
// channels and valid bits, packed as f.
func transportFor(rate, bits int, f format.ALSAFormat) alsapcm.Transport {
	return alsapcm.Transport{Kind: format.KindPCM, RateHz: rate, Channels: alsapcm.Channels, Format: f, ValidBits: bits}
}

// formatsFor is the order the backend offers a DAC the packings of bits at
// rate: the one the bound unit took last first (Discard forgets it), then
// the table's.
func (b *Backend) formatsFor(rate, bits int) []format.ALSAFormat {
	b.mu.Lock()
	took, ok := b.took[packKey{rate, bits}]
	b.mu.Unlock()
	out := make([]format.ALSAFormat, 0, len(packings[bits])+1)
	if ok {
		out = append(out, took)
	}
	for _, f := range packings[bits] {
		if !ok || f != took {
			out = append(out, f)
		}
	}
	return out
}

// preferred is the packing a transport of bits at rate is expected to take:
// the one the unit took last, or the table's first.
func (b *Backend) preferred(rate, bits int) format.ALSAFormat {
	return b.formatsFor(rate, bits)[0]
}

// remember records that the bound unit took f for bits at rate, and forget
// that it refused it.
func (b *Backend) remember(rate, bits int, f format.ALSAFormat) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.took[packKey{rate, bits}] = f
}

func (b *Backend) forget(rate, bits int, f format.ALSAFormat) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.took[packKey{rate, bits}] == f {
		delete(b.took, packKey{rate, bits})
	}
}
