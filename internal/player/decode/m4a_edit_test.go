package decode_test

import (
	"encoding/binary"
	"testing"
)

// The ALAC tests rewrite goldens box by box. An m4aNode is one box: a
// container the ALAC demuxer walks keeps its children, and any other box its
// payload, so stsd, udta and mdat stay opaque bytes. Sizes are written back
// as 32 bits, which every golden uses.
type m4aNode struct {
	typ      string
	payload  []byte
	children []*m4aNode
}

var m4aTestContainers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true,
	"stbl": true, "edts": true, "dinf": true,
}

func parseM4ANodes(t testing.TB, b []byte) []*m4aNode {
	t.Helper()
	var out []*m4aNode
	for len(b) > 0 {
		if len(b) < 8 {
			t.Fatalf("m4a test parse: %d trailing bytes", len(b))
		}
		size := int(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		if size < 8 || size > len(b) {
			t.Fatalf("m4a test parse: box %q of size %d in %d bytes", typ, size, len(b))
		}
		n := &m4aNode{typ: typ}
		if m4aTestContainers[typ] {
			n.children = parseM4ANodes(t, b[8:size])
		} else {
			n.payload = append([]byte(nil), b[8:size]...)
		}
		out = append(out, n)
		b = b[size:]
	}
	return out
}

func (n *m4aNode) encode() []byte {
	body := n.payload
	if n.children != nil {
		body = encodeM4A(n.children)
	}
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:8], n.typ)
	return append(out, body...)
}

func encodeM4A(nodes []*m4aNode) []byte {
	var out []byte
	for _, n := range nodes {
		out = append(out, n.encode()...)
	}
	return out
}

// m4aPath follows box types from a list of siblings, taking the first box of
// each type.
func m4aPath(t testing.TB, nodes []*m4aNode, path ...string) *m4aNode {
	t.Helper()
	var n *m4aNode
	for _, typ := range path {
		n = nil
		for _, c := range nodes {
			if c.typ == typ {
				n = c
				break
			}
		}
		if n == nil {
			t.Fatalf("m4a test: no %q box on the path %v", typ, path)
		}
		nodes = n.children
	}
	return n
}

// m4aSTBL is the golden's sample table: every golden has one track.
func m4aSTBL(t testing.TB, nodes []*m4aNode) *m4aNode {
	t.Helper()
	return m4aPath(t, nodes, "moov", "trak", "mdia", "minf", "stbl")
}

// m4aMoovLast lays raw out as ftyp, mdat, moov and moves every chunk offset
// with mdat's payload, so a test can grow or shrink moov without moving the
// audio its offsets point at.
func m4aMoovLast(t testing.TB, raw []byte) []*m4aNode {
	t.Helper()
	var ftyp, mdat, moov *m4aNode
	oldPayload, pos := 0, 0
	for _, n := range parseM4ANodes(t, raw) {
		switch n.typ {
		case "ftyp":
			ftyp = n
		case "mdat":
			if mdat != nil {
				t.Fatal("m4a test: two mdat boxes")
			}
			mdat, oldPayload = n, pos+8
		case "moov":
			moov = n
		}
		pos += len(n.encode())
	}
	if ftyp == nil || mdat == nil || moov == nil {
		t.Fatal("m4a test: want ftyp, mdat and moov")
	}
	nodes := []*m4aNode{ftyp, mdat, moov}
	m4aShiftChunkOffsets(t, nodes, int64(len(ftyp.encode())+8-oldPayload))
	return nodes
}

func m4aShiftChunkOffsets(t testing.TB, nodes []*m4aNode, delta int64) {
	t.Helper()
	for _, box := range m4aSTBL(t, nodes).children {
		switch box.typ {
		case "stco":
			n := int(binary.BigEndian.Uint32(box.payload[4:]))
			for i := 0; i < n; i++ {
				at := box.payload[8+4*i:]
				binary.BigEndian.PutUint32(at, uint32(int64(binary.BigEndian.Uint32(at))+delta))
			}
		case "co64":
			n := int(binary.BigEndian.Uint32(box.payload[4:]))
			for i := 0; i < n; i++ {
				at := box.payload[8+8*i:]
				binary.BigEndian.PutUint64(at, uint64(int64(binary.BigEndian.Uint64(at))+delta))
			}
		}
	}
}

// Offsets into an ALAC golden's stsd payload: the FullBox header and entry
// count (8), the sample entry's header (8), the AudioSampleEntry fields (28),
// then the alac box: its header (8), its version and flags (4) and the
// 24-byte ALACSpecificConfig.
const (
	stsdEntryCount    = 4
	stsdEntrySize     = 8
	stsdEntryType     = 12
	stsdDataRef       = 22
	stsdSoundVersion  = 24
	stsdChannels      = 32
	stsdAlacBoxSize   = 44
	stsdAlacBoxType   = 48
	stsdAlacFullBox   = 52
	stsdCookie        = 56
	cookieFrameLength = stsdCookie + 0
	cookieCompatible  = stsdCookie + 4
	cookieBitDepth    = stsdCookie + 5
	cookieKB          = stsdCookie + 8
	cookieChannels    = stsdCookie + 9
	cookieMaxFrame    = stsdCookie + 12
	cookieAvgBitRate  = stsdCookie + 16
	cookieSampleRate  = stsdCookie + 20
)

// m4aPacketSpans returns every packet's offset and size from the golden's
// tables (stsz with per-packet sizes, stsc, stco or co64): the test's own
// reading of the index, not the decoder's.
func m4aPacketSpans(t testing.TB, nodes []*m4aNode) (offs []int64, sizes []int) {
	t.Helper()
	stbl := m4aSTBL(t, nodes)
	stsz := m4aPath(t, stbl.children, "stsz").payload
	if binary.BigEndian.Uint32(stsz[4:]) != 0 {
		t.Fatal("m4a test: constant-size stsz")
	}
	for i, n := 0, int(binary.BigEndian.Uint32(stsz[8:])); i < n; i++ {
		sizes = append(sizes, int(binary.BigEndian.Uint32(stsz[12+4*i:])))
	}
	var chunks []int64
	for _, box := range stbl.children {
		switch box.typ {
		case "stco":
			for i, n := 0, int(binary.BigEndian.Uint32(box.payload[4:])); i < n; i++ {
				chunks = append(chunks, int64(binary.BigEndian.Uint32(box.payload[8+4*i:])))
			}
		case "co64":
			for i, n := 0, int(binary.BigEndian.Uint32(box.payload[4:])); i < n; i++ {
				chunks = append(chunks, int64(binary.BigEndian.Uint64(box.payload[8+8*i:])))
			}
		}
	}
	stsc := m4aPath(t, stbl.children, "stsc").payload
	runs := int(binary.BigEndian.Uint32(stsc[4:]))
	p := 0
	for c, off := range chunks {
		per := 0
		for r := 0; r < runs; r++ {
			if int(binary.BigEndian.Uint32(stsc[8+12*r:])) <= c+1 {
				per = int(binary.BigEndian.Uint32(stsc[12+12*r:]))
			}
		}
		for k := 0; k < per && p < len(sizes); k++ {
			offs = append(offs, off)
			off += int64(sizes[p])
			p++
		}
	}
	if len(offs) != len(sizes) {
		t.Fatalf("m4a test: %d packets placed of %d", len(offs), len(sizes))
	}
	return offs, sizes
}

// m4aReplacePacket rebuilds a single-chunk golden with packet idx replaced by
// pkt: the audio is laid out again in mdat, stsz takes the new size, and the
// chunk's offset is unchanged because moov is laid out after mdat.
func m4aReplacePacket(t testing.TB, raw []byte, idx int, pkt []byte) []byte {
	t.Helper()
	nodes := m4aMoovLast(t, raw)
	offs, sizes := m4aPacketSpans(t, nodes)
	stbl := m4aSTBL(t, nodes)
	for _, box := range stbl.children {
		if (box.typ == "stco" || box.typ == "co64") && binary.BigEndian.Uint32(box.payload[4:]) != 1 {
			t.Fatal("m4a test: m4aReplacePacket wants a single chunk")
		}
	}
	file := encodeM4A(nodes)
	if offs[0] != int64(len(nodes[0].encode())+8) {
		t.Fatal("m4a test: the first packet does not open mdat's payload")
	}
	var audio []byte
	for i := range sizes {
		if i == idx {
			audio = append(audio, pkt...)
			continue
		}
		audio = append(audio, file[offs[i]:offs[i]+int64(sizes[i])]...)
	}
	nodes[1].payload = audio
	binary.BigEndian.PutUint32(m4aPath(t, stbl.children, "stsz").payload[12+4*idx:], uint32(len(pkt)))
	return encodeM4A(nodes)
}
