package decode_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

// Round-5 CodeRabbit: moov scans capped at 4 MiB must not discard codecs
// already found when a later box crosses the cut.

func TestReviewR5MoovCapKeepsALACBeforeCover(t *testing.T) {
	_, err := decode.Open(bytes.NewReader(moovPastCapWithCodec(t, stsdALAC())))
	r, ok := decode.AsRefusal(err)
	if !ok {
		t.Fatalf("want refusal, got %v", err)
	}
	if r.Reason != decode.ReasonNotYetSupported {
		t.Fatalf("got %q (%v), want not_yet_supported (ALAC before large covr)", r.Reason, err)
	}
}

func TestReviewR5MoovCapKeepsAACBeforeCover(t *testing.T) {
	_, err := decode.Open(bytes.NewReader(moovPastCapWithCodec(t, stsdAAC())))
	r, ok := decode.AsRefusal(err)
	if !ok {
		t.Fatalf("want refusal, got %v", err)
	}
	if r.Reason != decode.ReasonLossy {
		t.Fatalf("got %q (%v), want lossy (AAC before large covr)", r.Reason, err)
	}
}

func TestReviewR5MoovCapALACWhenSTBLCrossesCut(t *testing.T) {
	_, err := decode.Open(bytes.NewReader(moovSTBLCrossesCut(t, stsdALAC())))
	r, ok := decode.AsRefusal(err)
	if !ok {
		t.Fatalf("want refusal, got %v", err)
	}
	if r.Reason != decode.ReasonNotYetSupported {
		t.Fatalf("got %q (%v), want not_yet_supported (stsd before stbl cut)", r.Reason, err)
	}
}

func TestReviewR5CompleteMoovStillRefusesOversizeChild(t *testing.T) {
	// Under 4 MiB: truncated flag must stay clear, so a child past the end
	// of moov remains unsupported_layout (round-4 contract).
	child := make([]byte, 16)
	binary.BigEndian.PutUint32(child[0:4], 1)
	copy(child[4:8], "trak")
	binary.BigEndian.PutUint64(child[8:16], 0x8000000000000000)
	b := append(mp4Box("ftyp", []byte("isom\x00\x00\x00\x00")), mp4Box("moov", child)...)
	_, err := decode.Open(bytes.NewReader(b))
	r, ok := decode.AsRefusal(err)
	if !ok {
		t.Fatalf("want refusal, got %v", err)
	}
	if r.Reason != decode.ReasonUnsupportedLayout {
		t.Fatalf("got %q (%v), want unsupported_layout", r.Reason, err)
	}
}

// moovPastCapWithCodec builds ftyp+moov with a codec trak first, then a udta
// cover that pushes the moov payload past the 4 MiB scan cap.
func moovPastCapWithCodec(t *testing.T, stsd []byte) []byte {
	t.Helper()
	trak := mp4Box("trak",
		mp4Box("mdia",
			mp4Box("minf",
				mp4Box("stbl",
					mp4Box("stsd", stsd),
				),
			),
		),
	)
	// Cover art large enough that trak+udta exceeds maxMoovBytes (4 MiB).
	cover := (4 << 20) - len(trak) + 64
	if cover < 1024 {
		cover = 1024
	}
	covr := mp4Box("covr", make([]byte, cover))
	udta := mp4Box("udta", mp4FullBox("meta", 0, mp4Box("ilst", covr)))
	moovPayload := append(append([]byte{}, trak...), udta...)
	if len(moovPayload) <= 4<<20 {
		t.Fatalf("fixture moov payload %d not past 4 MiB cap", len(moovPayload))
	}
	return append(mp4Box("ftyp", []byte("M4A \x00\x00\x00\x00")), mp4Box("moov", moovPayload)...)
}

// moovSTBLCrossesCut puts stsd first inside an stbl whose declared size (and
// trailing free space) extends past the 4 MiB moov scan window.
func moovSTBLCrossesCut(t *testing.T, stsd []byte) []byte {
	t.Helper()
	stsdBox := mp4Box("stsd", stsd)
	// Pad so stbl header+stsd sits near the end of the 4 MiB window and the
	// declared stbl size reaches past it.
	pad := make([]byte, 4<<20) // oversized free payload; stbl size will claim it
	// Build stbl manually: size = 8 + len(stsdBox) + len(pad), but only
	// stsdBox+prefix of pad will sit inside the capped moov read.
	stblInner := append(append([]byte{}, stsdBox...), pad...)
	stbl := mp4Box("stbl", stblInner)
	trak := mp4Box("trak", mp4Box("mdia", mp4Box("minf", stbl)))
	// Prepend enough junk before trak so the cut lands inside stbl after stsd.
	// moov = filler + trak; filler sized so that within the first 4 MiB of
	// moov payload we get through stsd and then hit the cut mid-stbl.
	stsdEndInTrak := 8 + 8 + 8 + 8 + len(stsdBox) // trak/mdia/minf/stbl hdrs + stsd box
	// Want: fillerLen + stsdEndInTrak ≈ maxMoovBytes - 32 (cut shortly after stsd).
	fillerLen := (4 << 20) - stsdEndInTrak - 32
	if fillerLen < 16 {
		t.Fatalf("fillerLen %d", fillerLen)
	}
	filler := mp4Box("free", make([]byte, fillerLen-8))
	moovPayload := append(append([]byte{}, filler...), trak...)
	if len(moovPayload) <= 4<<20 {
		t.Fatalf("need moov past cap, got %d", len(moovPayload))
	}
	return append(mp4Box("ftyp", []byte("M4A \x00\x00\x00\x00")), mp4Box("moov", moovPayload)...)
}

func stsdAAC() []byte {
	entry := make([]byte, 16)
	binary.BigEndian.PutUint32(entry[0:4], 16)
	copy(entry[4:8], "mp4a")
	body := make([]byte, 8+len(entry))
	binary.BigEndian.PutUint32(body[4:8], 1)
	copy(body[8:], entry)
	return body
}
