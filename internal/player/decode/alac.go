package decode

import (
	"context"
	"encoding/binary"
	"io"
	"math/bits"
	"sort"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
	alac "github.com/mycophonic/saprobe-alac"
)

// ALAC in M4A: the bounded demuxer (m4a.go) finds the one audio track and
// reads its tables into a packet index; the magic cookie and every packet's
// element header are checked here; and only then does saprobe-alac's packet
// decoder see a packet. Its container path, NewDecoder, is never used: it
// panics on malformed configurations, allocates from untrusted table counts
// and ignores edit lists (Codex's B470 evaluation).

// alacPackets decodes one ALAC packet into interleaved little-endian PCM
// bytes. saprobe-alac's PacketDecoder is the implementation Open uses; a
// test stands in for it to prove what a panic becomes.
type alacPackets interface {
	DecodePacket(packet []byte) ([]byte, error)
}

func newALACLibrary(cfg alac.PacketConfig) (alacPackets, error) {
	d, err := alac.NewPacketDecoder(cfg)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// alacRun is one stts run, the entries of one duration side by side merged:
// count packets of delta frames each, the first of them packet firstPacket,
// starting at media frame firstFrame.
type alacRun struct {
	firstPacket int
	firstFrame  int64
	count       int
	delta       int
}

type alacDecoder struct {
	rs         io.ReadSeeker
	pcm        format.PCM
	cfg        alacConfig
	newPackets func(alac.PacketConfig) (alacPackets, error)
	packets    alacPackets
	offsets    []int64
	sizes      []uint32
	runs       []alacRun
	// start and end are the media frames presented, [start, end): the edit
	// list's window, or the whole media.
	start, end int64
	pos        int64 // media frame of the next frame ReadFrames returns
	next       int   // the packet decoded next
	skip       int   // frames of it before pos
	pend       []int32
	samples    []int32
	pkt        []byte
	readStuck  error
	closed     bool
	// libraryPanics counts packets the library panicked on: the fuzz
	// targets read it, because the refusal a panic becomes is otherwise
	// the same as any malformed packet's.
	libraryPanics int
}

func openALAC(rs io.ReadSeeker) (Decoder, error) {
	return openALACWith(rs, newALACLibrary)
}

// openALACWith opens rs with newPackets as the packet decoder's constructor,
// which it calls only after the container, the cookie and every packet's
// extent have been validated.
func openALACWith(rs io.ReadSeeker, newPackets func(alac.PacketConfig) (alacPackets, error)) (Decoder, error) {
	size, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	w := &m4aWalk{rs: rs, size: size}
	moov, mdats, err := w.topLevel()
	if err != nil {
		return nil, err
	}
	tr, err := w.alacTrack(moov)
	if err != nil {
		return nil, err
	}
	d := &alacDecoder{rs: rs, cfg: tr.cfg, newPackets: newPackets}
	if err := d.index(w, tr, mdats); err != nil {
		return nil, err
	}
	if err := d.window(tr); err != nil {
		return nil, err
	}
	d.pcm = format.PCM{
		RateHz:    int(tr.cfg.sampleRate),
		Channels:  int(tr.cfg.channels),
		ValidBits: int(tr.cfg.bitDepth),
		Container: format.ContainerALAC,
		Frames:    d.end - d.start,
	}
	if d.packets, err = newPackets(tr.cfg.library()); err != nil {
		return nil, refuse(ReasonUnsupportedLayout, "ALAC decoder refused the configuration: %v", err)
	}
	d.samples = make([]int32, 2*int(tr.cfg.frameLength))
	d.pos = d.start
	d.next, d.skip = d.packetAt(d.start)
	return d, nil
}

// topLevel walks the file's boxes: ftyp first, one moov anywhere, and every
// mdat, whose payloads are where packets may lie. A fragmented file is
// refused.
func (w *m4aWalk) topLevel() (moov m4aBox, mdats []m4aBox, err error) {
	haveMoov, first := false, true
	err = w.children("file", 0, w.size, true, func(b m4aBox) error {
		if first && b.typ != "ftyp" {
			return refuse(ReasonUnknownContainer, "M4A does not open with ftyp")
		}
		first = false
		switch b.typ {
		case "moov":
			if haveMoov {
				return refuse(ReasonUnsupportedLayout, "M4A has two moov boxes")
			}
			moov, haveMoov = b, true
		case "mdat":
			mdats = append(mdats, b)
		case "moof", "mfra":
			return refuse(ReasonUnsupportedLayout, "fragmented MP4 (%s) is not played", b.typ)
		}
		return nil
	})
	if err == nil && !haveMoov {
		err = refuse(ReasonTruncated, "M4A has no moov box")
	}
	return moov, mdats, err
}

// alacTrackBoxes is the audio track's validated configuration and the boxes
// its index is read from.
type alacTrackBoxes struct {
	cfg            alacConfig
	mediaDuration  uint64
	movieTimescale uint32 // 0 when moov has no mvhd
	// hasEdit is set when the track has an edit list, whose one edit
	// presents the media from editMediaTime (media timescale) for
	// editDuration (movie timescale).
	hasEdit       bool
	editMediaTime int64
	editDuration  uint64
	stts, stsc    m4aBox
	stsz, chunks  m4aBox
}

// alacTrack finds moov's one audio track and validates everything about it
// that is not a table: its handler, data reference, sample description and
// cookie, media header and edit boxes.
func (w *m4aWalk) alacTrack(moov m4aBox) (alacTrackBoxes, error) {
	var tr alacTrackBoxes
	var traks []m4aBox
	var mvhd *m4aBox
	err := w.children("moov", moov.start, moov.end, false, func(b m4aBox) error {
		switch b.typ {
		case "trak":
			traks = append(traks, b)
		case "mvhd":
			if mvhd != nil {
				return refuse(ReasonUnsupportedLayout, "M4A moov holds two mvhd boxes")
			}
			mvhd = &b
		case "mvex":
			return refuse(ReasonUnsupportedLayout, "fragmented MP4 (mvex) is not played")
		}
		return nil
	})
	if err != nil {
		return tr, err
	}
	if mvhd != nil {
		if tr.movieTimescale, err = w.movieTimescale(*mvhd); err != nil {
			return tr, err
		}
	}
	var sound []m4aBox
	for _, trak := range traks {
		ok, err := w.isSoundTrack(trak)
		if err != nil {
			return tr, err
		}
		if ok {
			sound = append(sound, trak)
		}
	}
	switch len(sound) {
	case 0:
		return tr, refuse(ReasonUnsupportedLayout, "M4A has no audio track")
	case 1:
	default:
		return tr, refuse(ReasonUnsupportedLayout, "M4A has %d audio tracks; one is played", len(sound))
	}
	return w.soundTrack(sound[0], tr)
}

// isSoundTrack reports whether trak's handler is soun. A trak without mdia
// or hdlr is no audio track.
func (w *m4aWalk) isSoundTrack(trak m4aBox) (bool, error) {
	t, err := w.collect(trak, "mdia")
	if err != nil {
		return false, err
	}
	mdia, ok := t["mdia"]
	if !ok {
		return false, nil
	}
	m, err := w.collect(mdia, "hdlr")
	if err != nil {
		return false, err
	}
	hdlr, ok := m["hdlr"]
	if !ok {
		return false, nil
	}
	p, err := w.payload(hdlr, maxM4ASmallBox)
	if err != nil {
		return false, err
	}
	if len(p) < 12 {
		return false, refuse(ReasonTruncated, "M4A hdlr box of %d bytes", len(p))
	}
	return string(p[8:12]) == "soun", nil
}

func (w *m4aWalk) soundTrack(trak m4aBox, tr alacTrackBoxes) (alacTrackBoxes, error) {
	t, err := w.collect(trak, "mdia", "edts")
	if err != nil {
		return tr, err
	}
	if edts, ok := t["edts"]; ok {
		e, err := w.collect(edts, "elst")
		if err != nil {
			return tr, err
		}
		elst, ok := e["elst"]
		if !ok {
			return tr, refuse(ReasonUnsupportedLayout, "M4A edts holds no elst")
		}
		p, err := w.payload(elst, maxM4ASmallBox)
		if err != nil {
			return tr, err
		}
		if tr.editMediaTime, tr.editDuration, err = parseEdit(p); err != nil {
			return tr, err
		}
		tr.hasEdit = true
	}
	m, err := w.collect(t["mdia"], "mdhd", "minf")
	if err != nil {
		return tr, err
	}
	mdhd, ok := m["mdhd"]
	if !ok {
		return tr, refuse(ReasonUnsupportedLayout, "M4A audio track has no mdhd")
	}
	minf, ok := m["minf"]
	if !ok {
		return tr, refuse(ReasonUnsupportedLayout, "M4A audio track has no minf")
	}
	n, err := w.collect(minf, "dinf", "stbl")
	if err != nil {
		return tr, err
	}
	if err := w.selfContained(n["dinf"]); err != nil {
		return tr, err
	}
	stbl, ok := n["stbl"]
	if !ok {
		return tr, refuse(ReasonUnsupportedLayout, "M4A audio track has no stbl")
	}
	s, err := w.collect(stbl, "stsd", "stts", "stsc", "stsz", "stco", "co64", "stz2", "ctts")
	if err != nil {
		return tr, err
	}
	for _, unmodelled := range []string{"stz2", "ctts"} {
		if _, ok := s[unmodelled]; ok {
			return tr, refuse(ReasonUnsupportedLayout, "M4A sample table has %s, which is not read", unmodelled)
		}
	}
	for _, need := range []string{"stsd", "stts", "stsc", "stsz"} {
		if _, ok := s[need]; !ok {
			return tr, refuse(ReasonUnsupportedLayout, "M4A sample table has no %s", need)
		}
	}
	stco, haveStco := s["stco"]
	co64, haveCo64 := s["co64"]
	switch {
	case haveStco && haveCo64:
		return tr, refuse(ReasonUnsupportedLayout, "M4A sample table has both stco and co64")
	case haveStco:
		tr.chunks = stco
	case haveCo64:
		tr.chunks = co64
	default:
		return tr, refuse(ReasonUnsupportedLayout, "M4A sample table has no chunk offsets")
	}
	tr.stts, tr.stsc, tr.stsz = s["stts"], s["stsc"], s["stsz"]

	desc, err := w.payload(s["stsd"], maxSTSDBytes)
	if err != nil {
		return tr, err
	}
	if tr.cfg, err = parseALACSampleDescription(desc); err != nil {
		return tr, err
	}
	timescale, duration, err := w.mediaHeader(mdhd)
	if err != nil {
		return tr, err
	}
	if timescale != tr.cfg.sampleRate {
		return tr, refuse(ReasonUnsupportedLayout, "M4A media timescale %d is not the ALAC sample rate %d", timescale, tr.cfg.sampleRate)
	}
	tr.mediaDuration = duration
	return tr, nil
}

// selfContained requires a data reference that says the samples are in this
// file (ISO/IEC 14496-12 §8.7.2: one url or urn entry with flag 1). The
// sample entry's data_reference_index, checked with the cookie, is 1.
func (w *m4aWalk) selfContained(dinf m4aBox) error {
	if dinf.typ == "" {
		return refuse(ReasonUnsupportedLayout, "M4A audio track has no data reference (dinf)")
	}
	d, err := w.collect(dinf, "dref")
	if err != nil {
		return err
	}
	dref, ok := d["dref"]
	if !ok {
		return refuse(ReasonUnsupportedLayout, "M4A audio track has no data reference (dref)")
	}
	p, err := w.payload(dref, maxM4ASmallBox)
	if err != nil {
		return err
	}
	if len(p) < 8 || p[0] != 0 {
		return refuse(ReasonUnsupportedLayout, "M4A data reference box is not a version 0 dref")
	}
	if n := binary.BigEndian.Uint32(p[4:8]); n != 1 {
		return refuse(ReasonUnsupportedLayout, "M4A data reference lists %d entries; one is read", n)
	}
	e := p[8:]
	if len(e) < 12 || uint64(binary.BigEndian.Uint32(e[0:4])) < 12 || uint64(binary.BigEndian.Uint32(e[0:4])) > uint64(len(e)) {
		return refuse(ReasonTruncated, "M4A data reference entry does not fit its dref")
	}
	if typ := string(e[4:8]); typ != "url " && typ != "urn " {
		return refuse(ReasonUnsupportedLayout, "M4A data reference %q is not a url or urn entry", typ)
	}
	if e[11]&1 == 0 {
		return refuse(ReasonUnsupportedLayout, "M4A data reference points to another file; only self-contained audio is played")
	}
	return nil
}

func (w *m4aWalk) movieTimescale(mvhd m4aBox) (uint32, error) {
	p, err := w.payload(mvhd, maxM4ASmallBox)
	if err != nil {
		return 0, err
	}
	switch {
	case len(p) >= 20 && p[0] == 0:
		return binary.BigEndian.Uint32(p[12:16]), nil
	case len(p) >= 32 && p[0] == 1:
		return binary.BigEndian.Uint32(p[20:24]), nil
	case len(p) > 0 && p[0] > 1:
		return 0, refuse(ReasonUnsupportedLayout, "M4A mvhd version %d", p[0])
	}
	return 0, refuse(ReasonTruncated, "M4A mvhd box of %d bytes", len(p))
}

func (w *m4aWalk) mediaHeader(mdhd m4aBox) (timescale uint32, duration uint64, err error) {
	p, err := w.payload(mdhd, maxM4ASmallBox)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case len(p) >= 20 && p[0] == 0:
		return binary.BigEndian.Uint32(p[12:16]), uint64(binary.BigEndian.Uint32(p[16:20])), nil
	case len(p) >= 32 && p[0] == 1:
		return binary.BigEndian.Uint32(p[20:24]), binary.BigEndian.Uint64(p[24:32]), nil
	case len(p) > 0 && p[0] > 1:
		return 0, 0, refuse(ReasonUnsupportedLayout, "M4A mdhd version %d", p[0])
	}
	return 0, 0, refuse(ReasonTruncated, "M4A mdhd box of %d bytes", len(p))
}

// index reads stsz, stts, stsc and the chunk offsets into one offset and one
// size per packet and the stts runs, after checking that each count fits
// its box and the tables agree: stts and stsc place exactly the packets stsz
// sizes, every packet lies inside an mdat's payload, and stts's frames are
// mdhd's duration.
func (d *alacDecoder) index(w *m4aWalk, tr alacTrackBoxes, mdats []m4aBox) error {
	h, err := w.fullBox(tr.stsz, 12)
	if err != nil {
		return err
	}
	constSize, n := binary.BigEndian.Uint32(h[4:8]), uint64(binary.BigEndian.Uint32(h[8:12]))
	if n == 0 {
		return refuse(ReasonTruncated, "M4A ALAC track has no packets")
	}
	if constSize == 0 {
		// The count must fit the box before the cap is asked, so a count
		// the box cannot hold is truncation whatever its size.
		if err := fits(tr.stsz, 12, n, 4); err != nil {
			return err
		}
	}
	if n > maxALACPackets {
		return refuse(ReasonUnsupportedLayout, "M4A ALAC track has %d packets; more than %d are not played", n, maxALACPackets)
	}
	if constSize != 0 {
		// No table bytes back a constant size's count: the packets must
		// fit the mdat bytes the file holds before the index is allocated.
		if hi, lo := bits.Mul64(n, uint64(constSize)); hi != 0 || lo > mdatBytesHeld(mdats, w.size) {
			return refuse(ReasonTruncated, "M4A stsz's %d packets of %d bytes do not fit mdat", n, constSize)
		}
	}
	d.sizes = make([]uint32, n)
	if constSize != 0 {
		for i := range d.sizes {
			d.sizes[i] = constSize
		}
	} else if err := w.table(tr.stsz, 12, n, 4, func(i int, e []byte) error {
		d.sizes[i] = binary.BigEndian.Uint32(e)
		return nil
	}); err != nil {
		return err
	}
	frames, err := d.readSTTS(w, tr, n)
	if err != nil {
		return err
	}
	if tr.mediaDuration != uint64(frames) {
		return refuse(ReasonTruncated, "M4A mdhd duration %d disagrees with stts's %d frames", tr.mediaDuration, frames)
	}
	return d.placePackets(w, tr, n, mdats)
}

// readSTTS reads the stts runs: every entry at least one packet, every packet
// 1 to frameLength frames (the decoder's buffers hold one frame), and the
// entries covering exactly the n packets stsz sizes. Entries of one duration
// side by side are one run, and more than maxALACRuns runs are refused, so
// a table of an entry a packet costs one run. It returns the media's frames,
// at most n*frameLength.
func (d *alacDecoder) readSTTS(w *m4aWalk, tr alacTrackBoxes, n uint64) (int64, error) {
	h, err := w.fullBox(tr.stts, 8)
	if err != nil {
		return 0, err
	}
	count := uint64(binary.BigEndian.Uint32(h[4:8]))
	if count == 0 {
		return 0, refuse(ReasonTruncated, "M4A stts has no entries")
	}
	if err := fits(tr.stts, 8, count, 8); err != nil {
		return 0, err
	}
	if count > n {
		return 0, refuse(ReasonTruncated, "M4A stts lists %d runs for %d packets", count, n)
	}
	d.runs = make([]alacRun, 0, min(count, maxALACRuns))
	var packets uint64
	var frames int64
	err = w.table(tr.stts, 8, count, 8, func(_ int, e []byte) error {
		c, delta := uint64(binary.BigEndian.Uint32(e[0:4])), binary.BigEndian.Uint32(e[4:8])
		if c == 0 {
			return refuse(ReasonTruncated, "M4A stts has a run of 0 packets")
		}
		if delta == 0 || delta > d.cfg.frameLength {
			return refuse(ReasonTruncated, "M4A stts gives a packet %d frames; ALAC packets hold 1 to %d", delta, d.cfg.frameLength)
		}
		if c > n-packets {
			return refuse(ReasonTruncated, "M4A stts covers more than stsz's %d packets", n)
		}
		switch last := len(d.runs) - 1; {
		case last >= 0 && d.runs[last].delta == int(delta):
			d.runs[last].count += int(c)
		case len(d.runs) == maxALACRuns:
			return refuse(ReasonUnsupportedLayout, "M4A stts has more than %d runs of packets of one duration", maxALACRuns)
		default:
			d.runs = append(d.runs, alacRun{firstPacket: int(packets), firstFrame: frames, count: int(c), delta: int(delta)})
		}
		packets += c
		frames += int64(c) * int64(delta)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if packets != n {
		return 0, refuse(ReasonTruncated, "M4A stts covers %d packets; stsz has %d", packets, n)
	}
	return frames, nil
}

// placePackets reads the chunk offsets and, in step with them, stsc, and
// gives every packet its offset: chunk by chunk, each packet after the one
// before it, every packet at least one byte, no larger than the cookie and
// the format allow, and inside one mdat's payload. stsc must place exactly
// the n packets stsz sizes.
func (d *alacDecoder) placePackets(w *m4aWalk, tr alacTrackBoxes, n uint64, mdats []m4aBox) error {
	width := 4
	if tr.chunks.typ == "co64" {
		width = 8
	}
	h, err := w.fullBox(tr.chunks, 8)
	if err != nil {
		return err
	}
	chunks := uint64(binary.BigEndian.Uint32(h[4:8]))
	if chunks == 0 {
		return refuse(ReasonTruncated, "M4A %s lists no chunks", tr.chunks.typ)
	}
	if err := fits(tr.chunks, 8, chunks, width); err != nil {
		return err
	}
	if chunks > n {
		return refuse(ReasonTruncated, "M4A %s lists %d chunks for %d packets", tr.chunks.typ, chunks, n)
	}
	stsc, err := readSTSC(w, tr, chunks)
	if err != nil {
		return err
	}
	maxPacket := d.cfg.maxPacketBytes()
	d.offsets = make([]int64, n)
	var p uint64
	err = w.table(tr.chunks, 8, chunks, width, func(c int, e []byte) error {
		per, err := stsc.packets(uint64(c) + 1)
		if err != nil {
			return err
		}
		if uint64(per) > n-p {
			return refuse(ReasonTruncated, "M4A stsc places more than stsz's %d packets", n)
		}
		var at uint64
		if width == 8 {
			at = binary.BigEndian.Uint64(e)
		} else {
			at = uint64(binary.BigEndian.Uint32(e))
		}
		m, ok := mdatHolding(mdats, at)
		if !ok {
			return refuse(ReasonTruncated, "M4A chunk %d at byte %d is outside mdat", c, at)
		}
		off := int64(at)
		for k := uint32(0); k < per; k++ {
			size := d.sizes[p]
			switch {
			case size == 0:
				return refuse(ReasonTruncated, "ALAC packet %d is empty", p)
			case size > maxPacket:
				if d.cfg.maxFrameBytes != 0 {
					return refuse(ReasonTruncated, "ALAC packet %d of %d bytes exceeds the cookie's max frame bytes %d", p, size, d.cfg.maxFrameBytes)
				}
				return refuse(ReasonTruncated, "ALAC packet %d of %d bytes exceeds the %d a packet can be", p, size, maxPacket)
			case int64(size) > m.end-off:
				return refuse(ReasonTruncated, "ALAC packet %d runs past the end of its mdat", p)
			}
			d.offsets[p] = off
			off += int64(size)
			p++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if p != n {
		return refuse(ReasonTruncated, "M4A stsc places %d packets; stsz has %d", p, n)
	}
	return nil
}

// mdatBytesHeld counts the mdat payload bytes the file holds: an mdat that
// runs past the end of a cut file counts only to that end.
func mdatBytesHeld(mdats []m4aBox, fileSize int64) uint64 {
	var held uint64
	for _, m := range mdats {
		if end := min(m.end, fileSize); end > m.start {
			held += uint64(end - m.start)
		}
	}
	return held
}

// mdatHolding returns the mdat whose payload holds byte at.
func mdatHolding(mdats []m4aBox, at uint64) (m4aBox, bool) {
	for _, m := range mdats {
		if at >= uint64(m.start) && at < uint64(m.end) {
			return m, true
		}
	}
	return m4aBox{}, false
}

// stscRuns reads the sample-to-chunk runs in step with the chunk offsets,
// so they are never held: the first starts at chunk 1, each after the one
// before it and at most at the last chunk, and each gives its chunks one or
// more packets of sample description 1.
type stscRuns struct {
	cur     *tableCursor
	chunks  uint64
	read    int    // runs read
	per     uint32 // the packets of each chunk of the run reached last
	next    uint64 // the chunk the next run starts at, 0 after the last
	nextPer uint32
}

// readSTSC checks stsc's count against its box and the chunks and reads its
// first run.
func readSTSC(w *m4aWalk, tr alacTrackBoxes, chunks uint64) (*stscRuns, error) {
	h, err := w.fullBox(tr.stsc, 8)
	if err != nil {
		return nil, err
	}
	count := uint64(binary.BigEndian.Uint32(h[4:8]))
	if count == 0 {
		return nil, refuse(ReasonTruncated, "M4A stsc has no entries")
	}
	if err := fits(tr.stsc, 8, count, 12); err != nil {
		return nil, err
	}
	if count > chunks {
		return nil, refuse(ReasonTruncated, "M4A stsc lists %d runs for %d chunks", count, chunks)
	}
	s := &stscRuns{cur: w.cursor(tr.stsc, 8, count, 12), chunks: chunks}
	if err := s.advance(0); err != nil {
		return nil, err
	}
	return s, nil
}

// packets returns the packets of chunk c, counted from 1. The caller asks
// for the chunks in order.
func (s *stscRuns) packets(c uint64) (uint32, error) {
	if c == s.next {
		s.per = s.nextPer
		if err := s.advance(c); err != nil {
			return 0, err
		}
	}
	return s.per, nil
}

// advance reads the run after the one that starts at chunk at (0 before the
// first run) into next and nextPer.
func (s *stscRuns) advance(at uint64) error {
	e, err := s.cur.next()
	if e == nil || err != nil {
		s.next = 0
		return err
	}
	i := s.read
	s.read++
	first, per, desc := uint64(binary.BigEndian.Uint32(e[0:4])), binary.BigEndian.Uint32(e[4:8]), binary.BigEndian.Uint32(e[8:12])
	switch {
	case i == 0 && first != 1:
		return refuse(ReasonTruncated, "M4A stsc's first run starts at chunk %d", first)
	case i > 0 && (first <= at || first > s.chunks):
		return refuse(ReasonTruncated, "M4A stsc run %d starts at chunk %d, out of order", i, first)
	case per == 0:
		return refuse(ReasonTruncated, "M4A stsc gives chunks of 0 packets")
	case desc != 1:
		return refuse(ReasonUnsupportedLayout, "M4A stsc names sample description %d; one description is read", desc)
	}
	s.next, s.nextPer = first, per
	return nil
}

// window applies the edit list, if there is one, to the media's frames
// [0, total): presentation starts at the edit's media time and lasts the
// frames its duration covers whole, never past the media's end. One edit of
// rate 1 is played; an empty edit, a dwell, several edits and other rates
// are refused.
func (d *alacDecoder) window(tr alacTrackBoxes) error {
	last := d.runs[len(d.runs)-1]
	total := last.firstFrame + int64(last.count)*int64(last.delta)
	d.start, d.end = 0, total
	if !tr.hasEdit {
		return nil
	}
	mediaTime, duration := tr.editMediaTime, tr.editDuration
	if tr.movieTimescale == 0 {
		return refuse(ReasonUnsupportedLayout, "M4A has an edit list and no mvhd timescale for its duration")
	}
	if mediaTime >= total {
		return refuse(ReasonUnsupportedLayout, "M4A edit's media time %d is at or past the media's end %d", mediaTime, total)
	}
	// floor(duration × rate ÷ movie timescale) in 128 bits: a quotient that
	// would not fit is far past the media's end.
	frames := uint64(total)
	if hi, lo := bits.Mul64(duration, uint64(d.cfg.sampleRate)); hi < uint64(tr.movieTimescale) {
		frames, _ = bits.Div64(hi, lo, uint64(tr.movieTimescale))
	}
	if frames == 0 {
		return refuse(ReasonUnsupportedLayout, "M4A edit of %d at a movie timescale of %d covers no whole frame", duration, tr.movieTimescale)
	}
	d.start = mediaTime
	if frames < uint64(total-mediaTime) {
		d.end = mediaTime + int64(frames)
	}
	return nil
}

// parseEdit reads an elst's one edit, version 0 or 1 (ISO/IEC 14496-12
// §8.6.6): its segment duration in the movie timescale and its media time.
// An empty edit (media time -1), a rate other than 1 (a dwell is rate 0) and
// a zero duration are refused, and so are several edits.
func parseEdit(p []byte) (mediaTime int64, duration uint64, err error) {
	if len(p) < 8 {
		return 0, 0, refuse(ReasonTruncated, "M4A elst box of %d bytes", len(p))
	}
	width := 12
	switch p[0] {
	case 0:
	case 1:
		width = 20
	default:
		return 0, 0, refuse(ReasonUnsupportedLayout, "M4A elst version %d", p[0])
	}
	if n := binary.BigEndian.Uint32(p[4:8]); n != 1 {
		return 0, 0, refuse(ReasonUnsupportedLayout, "M4A edit list has %d edits; one is played", n)
	}
	e := p[8:]
	if len(e) < width {
		return 0, 0, refuse(ReasonTruncated, "M4A elst box of %d bytes holds no whole edit", len(p))
	}
	var rate []byte
	if width == 12 {
		duration = uint64(binary.BigEndian.Uint32(e[0:4]))
		mediaTime = int64(int32(binary.BigEndian.Uint32(e[4:8])))
		rate = e[8:12]
	} else {
		duration = binary.BigEndian.Uint64(e[0:8])
		mediaTime = int64(binary.BigEndian.Uint64(e[8:16]))
		rate = e[16:20]
	}
	integer, fraction := binary.BigEndian.Uint16(rate[0:2]), binary.BigEndian.Uint16(rate[2:4])
	switch {
	case mediaTime == -1:
		return 0, 0, refuse(ReasonUnsupportedLayout, "M4A empty edit (media time -1) is not played")
	case mediaTime < 0:
		return 0, 0, refuse(ReasonTruncated, "M4A edit media time %d", mediaTime)
	case integer != 1 || fraction != 0:
		return 0, 0, refuse(ReasonUnsupportedLayout, "M4A edit rate %d+%d/65536; only rate 1 is played", int16(integer), fraction)
	case duration == 0:
		return 0, 0, refuse(ReasonUnsupportedLayout, "M4A edit of zero duration")
	}
	return mediaTime, duration, nil
}

func (cfg alacConfig) library() alac.PacketConfig {
	return alac.PacketConfig{
		FrameLength:   cfg.frameLength,
		BitDepth:      cfg.bitDepth,
		NumChannels:   cfg.channels,
		PB:            cfg.pb,
		MB:            cfg.mb,
		KB:            cfg.kb,
		MaxRun:        cfg.maxRun,
		MaxFrameBytes: cfg.maxFrameBytes,
		AvgBitRate:    cfg.avgBitRate,
		SampleRate:    cfg.sampleRate,
	}
}

func (d *alacDecoder) Format() format.PCM { return d.pcm }

func (d *alacDecoder) Close() error {
	d.closed = true
	d.packets, d.offsets, d.sizes, d.pend, d.samples, d.pkt = nil, nil, nil, nil, nil, nil
	return nil
}

// packetAt returns the packet that holds media frame m, and m's frame within
// it.
func (d *alacDecoder) packetAt(m int64) (packet, skip int) {
	r := d.runs[sort.Search(len(d.runs), func(i int) bool {
		run := d.runs[i]
		return run.firstFrame+int64(run.count)*int64(run.delta) > m
	})]
	k := (m - r.firstFrame) / int64(r.delta)
	return r.firstPacket + int(k), int(m - r.firstFrame - k*int64(r.delta))
}

// frames returns stts's frames for packet p.
func (d *alacDecoder) frames(p int) int {
	return d.runs[sort.Search(len(d.runs), func(i int) bool {
		return d.runs[i].firstPacket+d.runs[i].count > p
	})].delta
}

// Seek moves to presentation frame frame. Every packet read seeks to its own
// offset, so there is nothing to do here but check and record the position.
func (d *alacDecoder) Seek(ctx context.Context, frame int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed {
		return refuse(ReasonTruncated, "ALAC decoder closed")
	}
	if frame < 0 || frame > d.pcm.Frames {
		return refuse(ReasonUnsupportedLayout, "ALAC seek frame %d out of range (0..%d)", frame, d.pcm.Frames)
	}
	d.pos, d.pend, d.readStuck = d.start+frame, nil, nil
	if d.pos < d.end {
		d.next, d.skip = d.packetAt(d.pos)
	}
	return nil
}

// ReadFrames decodes a packet at a time. A packet is read from its own
// offset, so a cancel or an I/O error during it leaves nothing to undo: the
// position moves only for frames handed to dst, and the next read reads the
// packet again. A failed seek sticks until a successful Seek.
func (d *alacDecoder) ReadFrames(ctx context.Context, dst []int32) (int, error) {
	if d.readStuck != nil {
		return 0, d.readStuck
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if d.closed {
		return 0, refuse(ReasonTruncated, "ALAC decoder closed")
	}
	if err := checkReadBuf(dst, d.pcm.Channels); err != nil {
		return 0, err
	}
	out := 0
	for out < len(dst) {
		if d.pos >= d.end {
			return out, io.EOF
		}
		if len(d.pend) == 0 {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			s, err := d.decode(ctx, d.next)
			if err != nil {
				return out, err
			}
			d.pend, d.skip = s[2*d.skip:], 0
			d.next++
		}
		n := min(len(d.pend), len(dst)-out) / 2
		if left := d.end - d.pos; int64(n) > left {
			n = int(left)
		}
		copy(dst[out:], d.pend[:2*n])
		d.pend = d.pend[2*n:]
		out += 2 * n
		d.pos += int64(n)
	}
	if d.pos >= d.end {
		return out, io.EOF
	}
	return out, nil
}

// decode reads packet p, checks its element header against stts and the
// cookie, decodes it and returns its interleaved samples.
func (d *alacDecoder) decode(ctx context.Context, p int) ([]int32, error) {
	size := int(d.sizes[p])
	if _, err := d.rs.Seek(d.offsets[p], io.SeekStart); err != nil {
		d.readStuck = err
		return nil, err
	}
	if cap(d.pkt) < size {
		d.pkt = make([]byte, size)
	}
	pkt := d.pkt[:size]
	got, err := io.ReadFull(d.rs, pkt)
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		return nil, refuse(ReasonTruncated, "ALAC packet %d is cut short: %d of its %d bytes", p, got, size)
	case err != nil:
		return nil, err
	}
	frames := d.frames(p)
	if err := checkALACPacket(p, pkt, frames, d.cfg); err != nil {
		return nil, err
	}
	raw, err := d.decodePacket(p, pkt)
	if err != nil {
		return nil, err
	}
	bps := int(d.cfg.bitDepth) / 8
	if len(raw) != frames*2*bps {
		return nil, refuse(ReasonTruncated, "ALAC packet %d decoded %d bytes; its %d frames are %d", p, len(raw), frames, frames*2*bps)
	}
	out := d.samples[:2*frames]
	switch bps {
	case 2:
		for i := range out {
			out[i] = int32(int16(binary.LittleEndian.Uint16(raw[2*i:])))
		}
	case 3:
		for i := range out {
			b := raw[3*i : 3*i+3]
			out[i] = signExtend(uint32(b[0])|uint32(b[1])<<8|uint32(b[2])<<16, 24)
		}
	default:
		for i := range out {
			out[i] = int32(binary.LittleEndian.Uint32(raw[4*i:]))
		}
	}
	return out, nil
}

// decodePacket runs the library's packet decoder on a packet that passed
// every check before it. A panic in the library is the last resort's case:
// the packet is refused as malformed, nothing the library wrote is returned,
// and the library's decoder is discarded for a fresh one, since a panic can
// leave it half-written.
func (d *alacDecoder) decodePacket(p int, pkt []byte) (raw []byte, err error) {
	if d.packets == nil {
		if d.packets, err = d.newPackets(d.cfg.library()); err != nil {
			return nil, refuse(ReasonUnsupportedLayout, "ALAC decoder refused the configuration: %v", err)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			d.packets = nil
			d.libraryPanics++
			raw, err = nil, refuse(ReasonTruncated, "ALAC packet %d is malformed: the packet decoder failed on it (%v)", p, r)
		}
	}()
	raw, err = d.packets.DecodePacket(pkt)
	if err != nil {
		return nil, refuse(ReasonTruncated, "ALAC packet %d does not decode: %v", p, err)
	}
	return raw, nil
}
