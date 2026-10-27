package format

import "fmt"

// Native ALSA layouts carry chronological bits in unsigned words.
const (
	DSDU32LE ALSAFormat = "DSD_U32_LE"
	DSDU32BE ALSAFormat = "DSD_U32_BE"
	DSDU16LE ALSAFormat = "DSD_U16_LE"
)

// DSDWordBytes returns the native word width, or zero for other layouts.
func DSDWordBytes(f ALSAFormat) int {
	switch f {
	case DSDU32LE, DSDU32BE:
		return 4
	case DSDU16LE:
		return 2
	}
	return 0
}

// PackDSD serializes whole canonical stereo words without changing their bits.
// Input is planar, bit 7 first; the oldest byte is most significant in a word.
func PackDSD(dst, left, right []byte, f ALSAFormat) (int, error) {
	w := DSDWordBytes(f)
	if w == 0 {
		return 0, fmt.Errorf("unknown native format %q", f)
	}
	if len(left) != len(right) {
		return 0, fmt.Errorf("unequal DSD channels")
	}
	if len(left)%w != 0 {
		return 0, fmt.Errorf("incomplete native word")
	}
	if len(left) > len(dst)/2 {
		return 0, fmt.Errorf("short native destination")
	}
	for off := 0; off < len(left); off += w {
		for ch, src := range [2][]byte{left, right} {
			for i := range w {
				j := i
				if f != DSDU32BE {
					j = w - 1 - i
				}
				dst[2*off+ch*w+i] = src[off+j]
			}
		}
	}
	return len(left) * 2, nil
}
