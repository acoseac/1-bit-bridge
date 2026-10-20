package decode_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

// Round-4 CodeRabbit findings on the R3 commits.

func TestReviewR4MP4ExtendedSizeNoPanic(t *testing.T) {
	// ISO-BMFF extended size 0x8000000000000000: converting to int before the
	// len(buf) check makes the comparison succeed and buf[headerLen:size] panic.
	child := make([]byte, 16)
	binary.BigEndian.PutUint32(child[0:4], 1) // size==1 → 64-bit largesize follows
	copy(child[4:8], "trak")
	binary.BigEndian.PutUint64(child[8:16], 0x8000000000000000)
	b := append(mp4Box("ftyp", []byte("isom\x00\x00\x00\x00")), mp4Box("moov", child)...)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Open panicked on extended box size: %v", r)
		}
	}()
	_, err := decode.Open(bytes.NewReader(b))
	if !decode.IsRefusal(err) {
		t.Fatalf("want typed refusal, got %v", err)
	}
}
