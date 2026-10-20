package decode

import "fmt"

// signExtend brings a bits-wide unsigned magnitude into a signed int32.
func signExtend(u uint32, bits int) int32 {
	shift := 32 - bits
	return int32(u<<shift) >> shift
}

func acceptPCMShape(channels, bits int) error {
	if channels != 2 {
		return refuse(ReasonUnsupportedChannels, "want stereo (2 channels), got %d", channels)
	}
	switch bits {
	case 16, 24, 32:
		return nil
	case 8, 12:
		return refuse(ReasonUnsupportedBits, "%d-bit PCM is refused; want 16, 24 or 32", bits)
	default:
		return refuse(ReasonUnsupportedBits, "%d-bit PCM is refused; want 16, 24 or 32", bits)
	}
}

func checkReadBuf(dst []int32, channels int) error {
	if channels <= 0 {
		return fmt.Errorf("player decode: invalid channel count %d", channels)
	}
	if len(dst)%channels != 0 {
		return fmt.Errorf("player decode: dst length %d is not a multiple of %d channels", len(dst), channels)
	}
	return nil
}
