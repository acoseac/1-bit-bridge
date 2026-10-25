package format

import "fmt"

// Container names the source container a decoder reconstructed samples from.
// Indexing and file extensions never establish player support; content
// inspection does.
type Container string

const (
	ContainerFLAC Container = "flac"
	ContainerWAV  Container = "wav"
	ContainerAIFF Container = "aiff"
	ContainerDSF  Container = "dsf"
	ContainerDFF  Container = "dff"
	// ContainerALAC is ALAC packets in an MP4 (M4A) container.
	ContainerALAC Container = "alac"
)

// Containers is every container a decoder reconstructs samples from, in the
// order they are declared above (B470 PR 5b): what the player's codecs are
// filtered from (playback.PlayedContainers).
func Containers() []Container {
	return []Container{ContainerFLAC, ContainerWAV, ContainerAIFF, ContainerDSF, ContainerDFF, ContainerALAC}
}

// IsDSD reports whether c carries DSD, which its decoder gives as native DSD
// (NativeDSD), never as PCM: DSF and DFF.
func (c Container) IsDSD() bool { return c == ContainerDSF || c == ContainerDFF }

// Kind is the playback transport representation.
type Kind int

const (
	// KindPCM is interleaved signed-integer PCM.
	KindPCM Kind = iota
	// KindNativeDSD is channel-separated DSD bytes with bit 7 earliest.
	// The DSF and DFF decoders produce it; its ALSA packers are later PRs.
	KindNativeDSD
	// KindDoP is DSD-over-PCM with alternating 0x05/0xFA markers.
	// Declared for later PRs; not produced by this package yet.
	KindDoP
)

// String returns a stable name for logs and diagnostics.
func (k Kind) String() string {
	switch k {
	case KindPCM:
		return "pcm"
	case KindNativeDSD:
		return "native-dsd"
	case KindDoP:
		return "dop"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// PCM is a stereo signed-integer PCM stream description.
//
// Samples on the wire between decoder and packer are interleaved int32 values
// with the valid bits sign-extended into the int32. ValidBits is 16, 24 or 32.
type PCM struct {
	RateHz    int
	Channels  int
	ValidBits int
	Container Container
	// Frames is the total number of sample frames when known; 0 if the
	// container did not declare a length.
	Frames int64
}

// Validate reports whether p is a shape the 0.2.2 player accepts for PCM.
func (p PCM) Validate() error {
	if p.Channels != 2 {
		return fmt.Errorf("player format: want 2 channels, got %d", p.Channels)
	}
	switch p.ValidBits {
	case 16, 24, 32:
	default:
		return fmt.Errorf("player format: want 16, 24 or 32 valid bits, got %d", p.ValidBits)
	}
	if p.RateHz <= 0 {
		return fmt.Errorf("player format: invalid rate %d", p.RateHz)
	}
	if p.Frames < 0 {
		return fmt.Errorf("player format: negative frame count %d", p.Frames)
	}
	return nil
}

// DSD64RateHz is the one-bit rate of DSD64; DSD128..DSD512 are its
// multiples by 2, 4 and 8.
const DSD64RateHz = 2_822_400

// NativeDSD is the canonical DSD stream the DSF and DFF decoders produce, and
// the shape the native ALSA DSD packers (later PRs) consume.
// Channel-separated bytes; bit 7 is earliest in time. DSF declared LSB-first
// is reversed into this form; MSB-first DSF and uncompressed DFF pass through.
type NativeDSD struct {
	// BitRateHz is the one-bit sample rate per channel (DSD64 = 2_822_400).
	BitRateHz int
	Channels  int
	Container Container
	// Frames is the number of one-bit samples per channel. A decoder emits
	// whole bytes, so when Frames is not a multiple of eight the last byte's
	// trailing bits are the container's padding, unchanged; BytesPerChannel
	// counts that byte.
	Frames int64
}

// BytesPerChannel is the number of whole DSD bytes per channel a decoder
// emits: Frames rounded up to a byte. It divides before it rounds, because
// (Frames+7)/8 wraps negative for a Frames within 7 of math.MaxInt64, which a
// DSF header can declare. The result is bounded: 0 for a Frames of 0 or less
// (Validate refuses a negative one) and at most 2^60, so BytesPerChannel
// times the channel count, and any byte offset derived from it, fits in an
// int64.
func (d NativeDSD) BytesPerChannel() int64 {
	if d.Frames <= 0 {
		return 0
	}
	n := d.Frames / 8
	if d.Frames%8 != 0 {
		n++
	}
	return n
}

// Validate reports whether d is a shape the 0.2.2 player accepts for DSD:
// stereo, DSD64, DSD128, DSD256 or DSD512.
func (d NativeDSD) Validate() error {
	if d.Channels != 2 {
		return fmt.Errorf("player format: want 2 DSD channels, got %d", d.Channels)
	}
	switch d.BitRateHz {
	case DSD64RateHz, 2 * DSD64RateHz, 4 * DSD64RateHz, 8 * DSD64RateHz:
	default:
		return fmt.Errorf("player format: DSD bit rate %d Hz is not DSD64..DSD512", d.BitRateHz)
	}
	if d.Frames < 0 {
		return fmt.Errorf("player format: negative DSD sample count %d", d.Frames)
	}
	return nil
}

// DoP is the shape later PRs fill for DSD-over-PCM.
// Sixteen DSD bits per channel per carrier frame; markers alternate 0x05/0xFA.
type DoP struct {
	// CarrierRateHz is the PCM carrier rate (DSD64 → 176_400).
	CarrierRateHz int
	// DSDBitRateHz is the underlying one-bit rate.
	DSDBitRateHz int
	Channels     int
	// Frames is the number of DoP carrier frames when known.
	Frames int64
}
