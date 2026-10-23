package alsa

import "strings"

// pcmFormat is one linear PCM format: the bits that carry the sample and
// whether it is signed.
type pcmFormat struct {
	bits   int
	signed bool
}

// pcmFormats is every linear integer PCM format the kernel names
// (sound/core/pcm.c, snd_pcm_format_names): the bits are the sample's
// significant bits, whatever the container, so S24_LE (24 bits in four
// bytes) and S24_3LE (24 bits in three) are both 24, and S20_LE is 20.
var pcmFormats = map[string]pcmFormat{
	"S8": {8, true}, "U8": {8, false},
	"S16_LE": {16, true}, "S16_BE": {16, true}, "U16_LE": {16, false}, "U16_BE": {16, false},
	"S18_3LE": {18, true}, "S18_3BE": {18, true}, "U18_3LE": {18, false}, "U18_3BE": {18, false},
	"S20_LE": {20, true}, "S20_BE": {20, true}, "U20_LE": {20, false}, "U20_BE": {20, false},
	"S20_3LE": {20, true}, "S20_3BE": {20, true}, "U20_3LE": {20, false}, "U20_3BE": {20, false},
	"S24_LE": {24, true}, "S24_BE": {24, true}, "U24_LE": {24, false}, "U24_BE": {24, false},
	"S24_3LE": {24, true}, "S24_3BE": {24, true}, "U24_3LE": {24, false}, "U24_3BE": {24, false},
	"S32_LE": {32, true}, "S32_BE": {32, true}, "U32_LE": {32, false}, "U32_BE": {32, false},
}

// PCMFormatBits reads a sample format as the kernel names it in hw_params:
// the bits that carry the sample and whether it is signed. S16_LE is 16,
// S24_3LE and S24_LE are 24, S32_LE is 32, and U16_LE is 16 and unsigned.
// ok is false for every other name: a DSD format, FLOAT, SPECIAL, an
// IEC958 subframe, µ-law, A-law, a compressed format, or a name the kernel
// does not print.
func PCMFormatBits(format string) (bits int, signed, ok bool) {
	f, ok := pcmFormats[format]
	if !ok {
		return 0, false, false
	}
	return f.bits, f.signed, true
}

// IsDSDFormat reports whether a sample format as the kernel names it is a
// native DSD one (DSD_U8, DSD_U16_LE, DSD_U16_BE, DSD_U32_LE, DSD_U32_BE).
// NativeDSDRate turns such a format's frame rate into the DSD rate.
func IsDSDFormat(format string) bool {
	return strings.HasPrefix(format, "DSD_")
}
