package alsa

import (
	"os"
	"path/filepath"
	"testing"
)

// capture is one of the trees captured on the Raspberry Pi 5 with the iFi
// GO bar (testdata/README.md), and what its card 5 files say. The values
// are the files' own; idle is the one with the DAC stopped.
type capture struct {
	name    string
	running bool
	// The stream0 Running block.
	altset     int
	packetSize int
	freqHz     int64
	freqRaw    string
	// hw_params.
	access, format       string
	rate, period, buffer int
	// status.
	triggerTime                            string
	delay, avail, availMax, hwPtr, applPtr int64
}

var captures = []capture{
	{name: "idle"},
	{name: "dsd256-native", running: true, altset: 4, packetSize: 400, freqHz: 355053, freqRaw: "0x2c.61b2",
		access: "RW_INTERLEAVED", format: "DSD_U32_BE", rate: 352800, period: 44100, buffer: 176400,
		triggerTime: "3507.604528195", delay: 172355, avail: 7377, availMax: 132300, hwPtr: 1418577, applPtr: 1587600},
	{name: "pcm-44k1-16", running: true, altset: 3, packetSize: 36, freqHz: 44101, freqRaw: "0x5.833b",
		access: "RW_INTERLEAVED", format: "S16_LE", rate: 44100, period: 5513, buffer: 22050,
		triggerTime: "3512.626651115", delay: 11891, avail: 10589, availMax: 16537, hwPtr: 175981, applPtr: 187442},
	{name: "pcm-96k-24", running: true, altset: 2, packetSize: 108, freqHz: 96010, freqRaw: "0xc.0051",
		access: "MMAP_INTERLEAVED", format: "S24_3LE", rate: 96000, period: 12000, buffer: 48000,
		triggerTime: "3517.637052632", delay: 38588, avail: 10420, availMax: 13152, hwPtr: 382420, applPtr: 420000},
	{name: "pcm-192k-24", running: true, altset: 2, packetSize: 216, freqHz: 192030, freqRaw: "0x18.00f5",
		access: "MMAP_INTERLEAVED", format: "S24_3LE", rate: 192000, period: 24000, buffer: 96000,
		triggerTime: "3522.643875847", delay: 77479, avail: 20345, availMax: 26304, hwPtr: 764345, applPtr: 840000},
	{name: "pcm-384k-24", running: true, altset: 2, packetSize: 300, freqHz: 384000, freqRaw: "0x30.0000",
		access: "MMAP_INTERLEAVED", format: "S24_3LE", rate: 384000, period: 48000, buffer: 192000,
		triggerTime: "3527.654497625", delay: 156382, avail: 39651, availMax: 52608, hwPtr: 1527651, applPtr: 1680000},
}

// capturedAltset is one entry of the altset list every capture's stream0
// carries: the iFi offers the same four in each.
type capturedAltset struct {
	altset  int
	formats []string
	bits    int
	dsdRaw  bool
}

var capturedAltsets = []capturedAltset{
	{1, []string{"S32_LE"}, 32, false},
	{2, []string{"S24_3LE"}, 24, false},
	{3, []string{"S16_LE"}, 16, false},
	{4, []string{"SPECIAL", "DSD_U32_BE"}, 32, true},
}

// capturedRates are the rates each of those altsets lists.
var capturedRates = []int{44100, 48000, 88200, 96000, 176400, 192000, 352800, 384000}

// syntheticCases are the trees testdata/README.md marks as made up.
var syntheticCases = []string{"i2s-and-hdmi", "garbled-stream0", "multi-format", "uac1-bits-less", "symlinked-pcm"}

// procRootOf is a test case's /proc tree.
func procRootOf(name string) string { return filepath.Join("testdata", name, "proc") }

// readFixture reads a file of a test case's /proc tree by its path in it.
func readFixture(t *testing.T, name, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(procRootOf(name), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// deref reads an optional number for a comparison; nil reads as -1, which
// no value in the fixtures is.
func deref[T int | int64](p *T) T {
	if p == nil {
		return -1
	}
	return *p
}
