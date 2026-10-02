package flactest

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestChecksumsAreTheFormatsOwn pins the fixtures' checksums to their
// published check values (CRC-8/SMBUS and CRC-16/BUYPASS over "123456789"):
// what makes a stream here one a real decoder would take.
func TestChecksumsAreTheFormatsOwn(t *testing.T) {
	if got := crc8([]byte("123456789")); got != 0xF4 {
		t.Errorf("crc8 = %#02x, want 0xF4", got)
	}
	if got := crc16([]byte("123456789")); got != 0xFEE8 {
		t.Errorf("crc16 = %#04x, want 0xFEE8", got)
	}
}

// TestAStreamDeclaresWhatItsFramesHold checks Stream's layout: STREAMINFO's
// fields, then one frame per Block (the last shorter), each ending in the
// CRC-16 of the bytes before it, and frames that follow one another to the
// end of the stream.
func TestAStreamDeclaresWhatItsFramesHold(t *testing.T) {
	const samples = 2*Block + 7
	b := Stream(176400, 2, 24, samples, 3)
	if !bytes.HasPrefix(b, []byte("fLaC")) {
		t.Fatal("no fLaC")
	}
	packed := binary.BigEndian.Uint64(b[18:26])
	if rate, ch, bits, n := packed>>44, packed>>41&7+1, packed>>36&0x1F+1, packed&(1<<36-1); rate != 176400 || ch != 2 || bits != 24 || n != samples {
		t.Errorf("STREAMINFO says %d Hz, %d channels, %d bit, %d samples", rate, ch, bits, n)
	}
	frames := b[42:]
	for i, n := range []int{Block, Block, 7} {
		f := Frame(uint64(i), n, 2, 24, 3, false)
		if !bytes.HasPrefix(frames, f) {
			t.Fatalf("frame %d is not where the one before it ends", i)
		}
		if c := binary.BigEndian.Uint16(f[len(f)-2:]); c != crc16(f[:len(f)-2]) {
			t.Errorf("frame %d: CRC-16 %#04x, want %#04x", i, c, crc16(f[:len(f)-2]))
		}
		frames = frames[len(f):]
	}
	if len(frames) != 0 {
		t.Errorf("%d bytes after the last frame", len(frames))
	}
}

// TestShPrintfEscapesEveryByte: an octal escape per byte, which printf(1)
// turns back into the byte, '%' and '\' included.
func TestShPrintfEscapesEveryByte(t *testing.T) {
	if got, want := ShPrintf([]byte{0, '%', '\\', 0xFF}), `\000\045\134\377`; got != want {
		t.Errorf("ShPrintf = %q, want %q", got, want)
	}
}
