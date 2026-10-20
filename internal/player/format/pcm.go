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
	// Later: ContainerALAC, ContainerDSF, ContainerDFF.
)

// Kind is the playback transport representation.
type Kind int

const (
	// KindPCM is interleaved signed-integer PCM.
	KindPCM Kind = iota
	// KindNativeDSD is channel-separated DSD bytes with bit 7 earliest.
	// Declared for later PRs; not produced by this package yet.
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

// NativeDSD is the shape later PRs fill for native ALSA DSD transport.
// Channel-separated bytes; bit 7 is earliest in time. DSF declared LSB-first
// is reversed into this form; MSB-first DSF and uncompressed DFF pass through.
type NativeDSD struct {
	// BitRateHz is the one-bit sample rate per channel (DSD64 = 2_822_400).
	BitRateHz int
	Channels  int
	// Frames is the number of one-bit sample frames when known.
	Frames int64
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
