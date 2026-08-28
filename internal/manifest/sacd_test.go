package manifest

// SACD TOC parser + expansion tests over a synthesized in-memory image —
// the Go sibling of the iOS repo's SACDTestImageBuilder (both mirror the
// SAME empirically-derived layout; neither reads the other's bytes). The
// builder writes raw bytes independently of the parser so a shared
// misunderstanding cannot self-validate.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- Fixture builder ---

type sacdFixtureTrack struct {
	startFrame int
	duration   int
	title      string
	performer  string
}

type sacdFixtureOptions struct {
	raw2064     bool // wrap sectors in the 12+4 raw framing
	plainDSD    bool // audio header byte WITHOUT the DST flag
	damageFirst bool // corrupt the master copy at 510 (fallback exercises 520)
	albumTitle  string
	albumArtist string
	year        int
	seq         int
}

const (
	fixAreaStart  = 540
	fixTOCSectors = 4 // header + TRL1 + TRL2 + TTxt
	fixAudioStart = 544
	fixAudioEnd   = 550
	fixAreaEnd    = 560 // the area's second TOC copy position
	fixTotal      = 600
)

func buildSACDImage(t *testing.T, tracks []sacdFixtureTrack, opt sacdFixtureOptions) []byte {
	t.Helper()
	if opt.albumTitle == "" {
		opt.albumTitle = "Test Album"
	}
	if opt.albumArtist == "" {
		opt.albumArtist = "Test Artist"
	}
	sectors := make([][]byte, fixTotal)
	for i := range sectors {
		sectors[i] = make([]byte, sacdSectorPayload)
	}
	putU16 := func(d []byte, off int, v uint16) { d[off] = byte(v >> 8); d[off+1] = byte(v) }
	putU32 := func(d []byte, off int, v uint32) {
		d[off] = byte(v >> 24)
		d[off+1] = byte(v >> 16)
		d[off+2] = byte(v >> 8)
		d[off+3] = byte(v)
	}
	tc := func(d []byte, off, frames int) {
		d[off] = byte(frames / (60 * sacdFramesPerSecond))
		d[off+1] = byte((frames / sacdFramesPerSecond) % 60)
		d[off+2] = byte(frames % sacdFramesPerSecond)
	}

	// Master TOC unit at 510/520/530 (identical copies).
	master := make([]byte, sacdSectorPayload)
	copy(master, "SACDMTOC")
	putU16(master, 18, uint16(opt.seq))
	putU32(master, 64, fixAreaStart)
	putU32(master, 68, fixAreaEnd)
	if opt.year > 0 {
		putU16(master, 120, uint16(opt.year))
		master[122] = 1
		master[123] = 1
	}
	text := make([]byte, sacdSectorPayload)
	copy(text, "SACDText")
	// Two NUL-terminated strings after the pointer table.
	off := 64
	putU16(text, 16, uint16(off))
	copy(text[off:], opt.albumTitle)
	off += len(opt.albumTitle) + 1
	putU16(text, 18, uint16(off))
	copy(text[off:], opt.albumArtist)
	for _, base := range []int{510, 520, 530} {
		copy(sectors[base], master)
		copy(sectors[base+1], text)
	}
	if opt.damageFirst {
		copy(sectors[510], "JUNKJUNK")
	}

	// Area TOC at fixAreaStart (a full copy at fixAreaEnd).
	writeArea := func(base int) {
		h := sectors[base]
		copy(h, "TWOCHTOC")
		putU16(h, 10, fixTOCSectors)
		h[32] = 2 // stereo
		tc(h, 64, tracks[len(tracks)-1].startFrame+tracks[len(tracks)-1].duration)
		h[68] = 0 // trackOffset
		h[69] = byte(len(tracks))
		putU32(h, 72, fixAudioStart)
		putU32(h, 76, fixAudioEnd)

		trl1 := sectors[base+1]
		copy(trl1, "SACDTRL1")
		for i := range tracks {
			putU32(trl1, 8+4*i, uint32(fixAudioStart+i)) // monotonic, in-bounds
			putU32(trl1, 8+4*255+4*i, 1)
		}
		trl2 := sectors[base+2]
		copy(trl2, "SACDTRL2")
		for i, tr := range tracks {
			tc(trl2, 8+4*i, tr.startFrame)
			tc(trl2, 8+1020+4*i, tr.duration)
		}
		ttxt := sectors[base+3]
		copy(ttxt, "SACDTTxt")
		cursor := 8 + 2*len(tracks)
		cursor = (cursor + 3) &^ 3
		for i, tr := range tracks {
			if tr.title == "" && tr.performer == "" {
				continue
			}
			putU16(ttxt, 8+2*i, uint16(cursor))
			items := 0
			if tr.title != "" {
				items++
			}
			if tr.performer != "" {
				items++
			}
			ttxt[cursor] = byte(items)
			c := cursor + 4
			write := func(typ byte, s string) {
				c = (c + 3) &^ 3
				ttxt[c] = typ
				copy(ttxt[c+1:], s)
				c += 1 + len(s) + 1 // type + text + NUL
			}
			if tr.title != "" {
				write(0x01, tr.title)
			}
			if tr.performer != "" {
				write(0x02, tr.performer)
			}
			cursor = (c + 3) &^ 3
		}
	}
	writeArea(fixAreaStart)
	writeArea(fixAreaEnd)

	// One audio sector header byte at the area start: bit 0 = DST.
	if opt.plainDSD {
		sectors[fixAudioStart][0] = 0x20 // packet count 1, no DST flag
	} else {
		sectors[fixAudioStart][0] = 0x21
	}

	var out bytes.Buffer
	for _, sec := range sectors {
		if opt.raw2064 {
			out.Write(make([]byte, 12))
			out.Write(sec)
			out.Write(make([]byte, 4))
		} else {
			out.Write(sec)
		}
	}
	return out.Bytes()
}

func writeSACDFixture(t *testing.T, dir, name string, tracks []sacdFixtureTrack, opt sacdFixtureOptions) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buildSACDImage(t, tracks, opt), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func twoFixtureTracks() []sacdFixtureTrack {
	return []sacdFixtureTrack{
		{startFrame: 0, duration: 150, title: "Opening", performer: "The Band"},
		{startFrame: 150, duration: 225, title: "Closing", performer: ""},
	}
}

// --- Path grammar (the pinned wire contract; iOS mirror: SACDVirtualPathTests) ---

func TestSACDVirtualPathGrammar(t *testing.T) {
	if got := SACDVirtualTrackPath("Music/A.iso", 1); got != "Music/A.iso/st/01.dff" {
		t.Fatalf("mint 1: %q", got)
	}
	if got := SACDVirtualTrackPath("Music/A.iso", 99); got != "Music/A.iso/st/99.dff" {
		t.Fatalf("mint 99: %q", got)
	}
	if got := SACDVirtualTrackPath("Music/A.iso", 100); got != "Music/A.iso/st/100.dff" {
		t.Fatalf("mint 100 stays unpadded: %q", got)
	}
	if got := SACDVirtualTrackPath("Music/A.iso", 256); got != "" {
		t.Fatalf("mint past the format ceiling must refuse: %q", got)
	}

	accepts := []string{
		"Music/A.iso/st/01.dff",
		"Music/A.iso/st/99.dff",
		"Music/A.iso/st/100.dff",
		"Music/A.iso/st/255.dff",
		"Music/A.iso/mc/03.dff", // reserved area, recognized never minted
		"Music/ALBUM.ISO/st/01.dff",
		"A.iso/st/01.dff", // root-level container
	}
	for _, p := range accepts {
		c, ok := SACDVirtualContainer(p)
		if !ok {
			t.Fatalf("must accept %q", p)
		}
		if !bytes.HasSuffix([]byte(p), []byte(".dff")) || c == "" {
			t.Fatalf("container of %q: %q", p, c)
		}
	}
	if c, _ := SACDVirtualContainer("Music/A.iso/st/01.dff"); c != "Music/A.iso" {
		t.Fatalf("container: %q", c)
	}

	rejects := []string{
		"Music/A.iso/st/1.dff",   // unpadded low index
		"Music/A.iso/st/001.dff", // over-padded
		"Music/A.iso/st/00.dff",  // zero
		"Music/A.iso/st/256.dff", // past ceiling
		"Music/A.iso/xx/01.dff",  // unknown area
		"Music/A.flac/st/01.dff", // non-iso container
		"Music/song.dff",         // ordinary file
		"Music/A.iso",            // the container itself
		"st/01.dff",              // no container at all
	}
	for _, p := range rejects {
		if IsSACDVirtualPath(p) {
			t.Fatalf("must reject %q", p)
		}
	}
}

// --- TOC parse + expansion ---

func TestSACDExpand_PlainGeometry(t *testing.T) {
	dir := t.TempDir()
	p := writeSACDFixture(t, dir, "Album.iso", twoFixtureTracks(),
		sacdFixtureOptions{year: 2004, seq: 2})
	mtime := time.Unix(1_700_000_000, 0).UTC()

	tracks, err := ExpandSACDISO(p, "Music/Album.iso", 1234, mtime)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("tracks: %d", len(tracks))
	}
	t0 := tracks[0]
	if t0.Path != "Music/Album.iso/st/01.dff" {
		t.Fatalf("path: %q", t0.Path)
	}
	if t0.Title != "Opening" || t0.Artist != "The Band" {
		t.Fatalf("tags: %q / %q", t0.Title, t0.Artist)
	}
	if t0.Album != "Test Album" || t0.AlbumArtist != "Test Artist" {
		t.Fatalf("album tags: %q / %q", t0.Album, t0.AlbumArtist)
	}
	if t0.Size != 1234 || !t0.ModTime.Equal(mtime) {
		t.Fatalf("container identity: size=%d mtime=%v", t0.Size, t0.ModTime)
	}
	if t0.Codec != "DFF" || t0.Compression != "DST" {
		t.Fatalf("codec: %q/%q", t0.Codec, t0.Compression)
	}
	if t0.SampleRate == nil || *t0.SampleRate != 2822400 {
		t.Fatalf("rate: %v", t0.SampleRate)
	}
	if t0.IsDSD == nil || !*t0.IsDSD || t0.BitsPerSample == nil || *t0.BitsPerSample != 1 {
		t.Fatalf("dsd typing: %v %v", t0.IsDSD, t0.BitsPerSample)
	}
	if t0.Duration == nil || *t0.Duration != 2.0 { // 150 frames / 75
		t.Fatalf("duration: %v", t0.Duration)
	}
	if t0.TrackNumber == nil || *t0.TrackNumber != 1 || t0.DiscNumber == nil || *t0.DiscNumber != 2 {
		t.Fatalf("numbers: %v %v", t0.TrackNumber, t0.DiscNumber)
	}
	if t0.Year == nil || *t0.Year != 2004 {
		t.Fatalf("year: %v", t0.Year)
	}
	// A performer-less track falls back to the album artist; duration 3 s.
	t1 := tracks[1]
	if t1.Path != "Music/Album.iso/st/02.dff" || t1.Artist != "Test Artist" {
		t.Fatalf("track 2: %q / %q", t1.Path, t1.Artist)
	}
	if t1.Duration == nil || *t1.Duration != 3.0 {
		t.Fatalf("track 2 duration: %v", t1.Duration)
	}
}

func TestSACDExpand_RawGeometryParsesIdentically(t *testing.T) {
	dir := t.TempDir()
	plain := writeSACDFixture(t, dir, "p.iso", twoFixtureTracks(), sacdFixtureOptions{})
	raw := writeSACDFixture(t, dir, "r.iso", twoFixtureTracks(), sacdFixtureOptions{raw2064: true})
	mt := time.Unix(1, 0)
	a, err := ExpandSACDISO(plain, "x.iso", 1, mt)
	if err != nil || len(a) != 2 {
		t.Fatalf("plain: %v %d", err, len(a))
	}
	b, err := ExpandSACDISO(raw, "x.iso", 1, mt)
	if err != nil || len(b) != 2 {
		t.Fatalf("raw: %v %d", err, len(b))
	}
	for i := range a {
		if a[i].Title != b[i].Title || *a[i].Duration != *b[i].Duration {
			t.Fatalf("geometry divergence at %d", i)
		}
	}
}

func TestSACDExpand_DamagedFirstMasterCopyFallsBack(t *testing.T) {
	dir := t.TempDir()
	p := writeSACDFixture(t, dir, "d.iso", twoFixtureTracks(),
		sacdFixtureOptions{damageFirst: true})
	tracks, err := ExpandSACDISO(p, "d.iso", 1, time.Unix(1, 0))
	if err != nil || len(tracks) != 2 {
		t.Fatalf("the doubled-TOC fallback must adopt the 520 copy: %v %d", err, len(tracks))
	}
}

func TestSACDExpand_NonSACDAndPlainDSDYieldNothing(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.iso")
	if err := os.WriteFile(junk, bytes.Repeat([]byte{0x5A}, 2_000_000), 0o644); err != nil {
		t.Fatal(err)
	}
	if tracks, err := ExpandSACDISO(junk, "junk.iso", 1, time.Unix(1, 0)); err != nil || tracks != nil {
		t.Fatalf("non-SACD must yield (nil, nil): %v %v", tracks, err)
	}
	tiny := filepath.Join(dir, "tiny.iso")
	if err := os.WriteFile(tiny, []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}
	if tracks, err := ExpandSACDISO(tiny, "tiny.iso", 1, time.Unix(1, 0)); err != nil || tracks != nil {
		t.Fatalf("tiny non-SACD must yield (nil, nil): %v %v", tracks, err)
	}
	plainDSD := writeSACDFixture(t, dir, "plain.iso", twoFixtureTracks(),
		sacdFixtureOptions{plainDSD: true})
	if tracks, err := ExpandSACDISO(plainDSD, "plain.iso", 1, time.Unix(1, 0)); err != nil || tracks != nil {
		t.Fatalf("plain-DSD area is outside the v1 envelope: %v %v", tracks, err)
	}
}

func TestSACDExpand_AlbumFallsBackToFilenameStem(t *testing.T) {
	dir := t.TempDir()
	p := writeSACDFixture(t, dir, "s.iso", twoFixtureTracks(),
		sacdFixtureOptions{albumTitle: " ", albumArtist: " "})
	tracks, err := ExpandSACDISO(p, "Music/Dark Side.iso", 1, time.Unix(1, 0))
	if err != nil || len(tracks) != 2 {
		t.Fatalf("%v %d", err, len(tracks))
	}
	if tracks[0].Album != "Dark Side" {
		t.Fatalf("album stem fallback: %q", tracks[0].Album)
	}
}

// Real-ISO validation — env-gated (the images never enter the repo).
// Run locally:
//
//	BRIDGE_SACD_ISO_PLAIN=/path/Genesis.iso \
//	BRIDGE_SACD_ISO_RAW="/path/Division Bell.ISO" \
//	go test ./internal/manifest/ -run RealISO -v
//
// Ground truth: the two discs' total frame counts as pinned by the iOS
// repo's spike harness (208,672 and 299,924 frames) — the Go parser must
// tile its per-track durations to the same totals.
func TestSACDExpand_RealISOs(t *testing.T) {
	plain := os.Getenv("BRIDGE_SACD_ISO_PLAIN")
	raw := os.Getenv("BRIDGE_SACD_ISO_RAW")
	if plain == "" || raw == "" {
		t.Skip("set BRIDGE_SACD_ISO_PLAIN and BRIDGE_SACD_ISO_RAW to run")
	}
	totals := map[int]bool{}
	for _, p := range []string{plain, raw} {
		f, err := os.Open(p)
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		toc, err := parseSACDTOC(f)
		f.Close()
		if err != nil || toc == nil || !toc.hasStereo || !toc.stereoIsDST {
			t.Fatalf("%s: %v %+v", p, err, toc)
		}
		// The iOS harness's tiling invariant: the LAST track's start +
		// normative duration equals the area's total frame count (the
		// per-track duration SUM legitimately undershoots it on discs
		// with lead-in/gap frames — Genesis carries 225 such frames).
		last := toc.stereoTracks[len(toc.stereoTracks)-1]
		areaFrames := last.startFrame + last.durationFrames
		for i, tr := range toc.stereoTracks {
			if tr.durationFrames <= 0 || tr.title == "" {
				t.Fatalf("%s track %d: %+v", p, i+1, tr)
			}
			if i > 0 {
				prev := toc.stereoTracks[i-1]
				if prev.startFrame+prev.durationFrames > tr.startFrame {
					t.Fatalf("%s track %d overlaps its successor", p, i)
				}
			}
		}
		t.Logf("%s: %d tracks, %d area frames, album=%q artist=%q",
			filepath.Base(p), len(toc.stereoTracks), areaFrames,
			toc.albumTitle, toc.albumArtist)
		totals[areaFrames] = true
	}
	if !totals[208672] || !totals[299924] {
		t.Fatalf("area frame totals %v do not match the spike ground truth {208672, 299924}", totals)
	}
}
