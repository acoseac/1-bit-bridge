package format

import "errors"

// PackDoP packs canonical planar stereo into left-aligned S32_LE DoP.
// It returns bytes written and the next frame marker; refusal changes neither.
// Input bit 7 is earliest. Chronological a,b occupy bits 23..16 and 15..8;
// both channels share one marker, alternating once per stereo carrier frame.
func PackDoP(dst, left, right []byte, marker byte) (int, byte, error) {
	if len(left) != len(right) {
		return 0, marker, errors.New("unequal DoP channels")
	}
	if len(left)%2 != 0 {
		return 0, marker, errors.New("incomplete DoP frame")
	}
	if len(left) > len(dst)/4 {
		return 0, marker, errors.New("short DoP destination")
	}
	if marker != 0x05 && marker != 0xfa {
		return 0, marker, errors.New("invalid DoP phase")
	}
	for i := 0; i < len(left); i += 2 {
		for ch, src := range [2][]byte{left, right} {
			a, b := src[i], src[i+1]
			out := i*4 + ch*4
			dst[out], dst[out+1], dst[out+2], dst[out+3] = 0, b, a, marker
		}
		marker ^= 0xff
	}
	return len(left) * 4, marker, nil
}
