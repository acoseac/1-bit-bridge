package manifest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhowden/tag"
)

// ExtractorVersion 15's three M4A repairs, pinned against REAL files
// (testdata/m4a — the same bytes the iOS app embeds in AVTagFixtures, so
// both indexing paths are held to one set of files) and against synthetic
// box trees for the shapes no tagger in the fixture set writes.
//
// The sixteen explicit-*.m4a files are a separate set, byte-identical to the
// iOS app's ExplicitAdvisoryFixtures. That copy is canonical; these are
// not regenerated from the AVTagFixtures recipe. The shared verdict rows
// are testdata/explicit-verdict-cases.tsv, the same file.

// extractFixture runs the production Extract over a testdata M4A, copied
// to a temp dir so the scan-time folder-art lookup sees nothing beside it.
func extractFixture(t *testing.T, name string) *Track {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "m4a", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return extractBytesAsM4A(t, raw)
}

func extractBytesAsM4A(t *testing.T, raw []byte) *Track {
	t.Helper()
	p := filepath.Join(t.TempDir(), "track.m4a")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	tr := &Track{Path: "track.m4a", Size: int64(len(raw)), ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return tr
}

// MARK: - The premises (fail when dhowden changes, not when we do)

// dhowden does not read `gnre`. If this ever fails, dhowden learned the atom
// and the fallback in extractMP4WithContext is redundant — delete it then,
// not before.
func TestDhowdenPremise_DoesNotReadTheITunesPredefinedGenre(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "m4a", "itunes_alac.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := tag.ReadFrom(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("tag.ReadFrom: %v", err)
	}
	if g := m.Genre(); g != "" {
		t.Fatalf("dhowden now reads gnre (%q): extractMP4PredefinedGenre can go", g)
	}
}

// dhowden keeps the data-atom locale on a freeform value. If this ever fails
// the strip is a no-op (it only touches values that START with the locale)
// and can be removed.
func TestDhowdenPremise_KeepsTheFreeformDataAtomLocale(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "m4a", "picard_alac.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := tag.ReadFrom(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("tag.ReadFrom: %v", err)
	}
	v, ok := m.Raw()["MusicBrainz Album Id"].(string)
	if !ok {
		t.Fatalf("premise: the freeform surfaces as a string; raw = %#v", m.Raw())
	}
	if !strings.HasPrefix(v, mp4DataLocale) {
		t.Fatalf("dhowden no longer keeps the locale (%q): stripMP4FreeformLocales can go", v)
	}
}

// MARK: - `gnre`, end to end on real files

// The user's report: a STANDARD genre written as `gnre` alone. Before v15
// the manifest carried no genre, so the file was missing from Rock.
func TestExtract_ITunesALAC_ReadsThePredefinedGenre(t *testing.T) {
	tr := extractFixture(t, "itunes_alac.m4a")
	if tr.Codec != "ALAC" {
		t.Fatalf("premise: the codec walk read the file; codec = %q", tr.Codec)
	}
	if tr.Genre != "Rock" {
		t.Errorf("Genre = %q, want Rock (gnre 18 is ID3v1 index 17)", tr.Genre)
	}
	// What the same file already carried, pinned so the v15 changes are
	// visibly additive.
	if tr.Year == nil || *tr.Year != 1975 {
		t.Errorf("Year = %v, want 1975", tr.Year)
	}
	if tr.TrackNumber == nil || *tr.TrackNumber != 11 {
		t.Errorf("TrackNumber = %v, want 11", tr.TrackNumber)
	}
	if tr.AlbumArtist != "Queen" || tr.Composer != "Freddie Mercury" {
		t.Errorf("AlbumArtist/Composer = %q/%q", tr.AlbumArtist, tr.Composer)
	}
}

// AAC takes the same path; Pop is the other genre the report named.
func TestExtract_ITunesAAC_ReadsThePredefinedPopGenre(t *testing.T) {
	tr := extractFixture(t, "itunes_aac_pop.m4a")
	if tr.Codec != "AAC" {
		t.Fatalf("premise: codec = %q, want AAC", tr.Codec)
	}
	if tr.Genre != "Pop" {
		t.Errorf("Genre = %q, want Pop (gnre 14 is ID3v1 index 13)", tr.Genre)
	}
}

// iTunes writes one atom or the other; a file carrying both chose its text.
func TestExtract_BothGenreAtoms_TheTextOneWins(t *testing.T) {
	tr := extractFixture(t, "both_genres_alac.m4a")
	if tr.Genre != "Classic Rock" {
		t.Errorf("Genre = %q, want the ©gen text \"Classic Rock\"", tr.Genre)
	}
}

// MARK: - The freeform locale, end to end

// Before v15 every one of these values reached the manifest prefixed with
// four NULs — the MusicBrainz id unusable for the Cover Art Archive, the
// original date unparseable.
func TestExtract_PicardFreeforms_ArriveWithoutTheLocale(t *testing.T) {
	tr := extractFixture(t, "picard_alac.m4a")
	if tr.MusicBrainzAlbumID != "4b8a2d4c-1c7a-4a9e-9d6f-3c2b1a0f9e8d" {
		t.Errorf("MusicBrainzAlbumID = %q", tr.MusicBrainzAlbumID)
	}
	if tr.OriginalYear == nil || *tr.OriginalYear != 1975 {
		t.Errorf("OriginalYear = %v, want 1975 (freeform ORIGINALDATE)", tr.OriginalYear)
	}
	if tr.Year == nil || *tr.Year != 2011 {
		t.Errorf("Year = %v, want 2011 (the pressing ©day)", tr.Year)
	}
	if tr.Genre != "Rock" {
		t.Errorf("Genre = %q, want the ©gen text", tr.Genre)
	}
}

// ReplayGain and the track id have no real fixture; the box tree carries the
// exact freeform shape mutagen / Picard write.
func TestExtract_FreeformReplayGainAndTrackID_Parse(t *testing.T) {
	raw := buildMP4WithILST(true,
		ilstText("\xa9nam", "Song"),
		ilstFreeform("com.apple.iTunes", "replaygain_track_gain", "-6.50 dB"),
		ilstFreeform("com.apple.iTunes", "replaygain_album_gain", "-7.25 dB"),
		ilstFreeform("com.apple.iTunes", "MusicBrainz Track Id", "6d3ddd5e-7a4a-4d13-9d4c-0bd2e9bda0f0"),
	)
	tr := extractBytesAsM4A(t, raw)
	if tr.Title != "Song" {
		t.Fatalf("premise: dhowden read the ilst; Title = %q", tr.Title)
	}
	if tr.ReplayGainTrackDB == nil || math.Abs(*tr.ReplayGainTrackDB-(-6.5)) > 1e-9 {
		t.Errorf("ReplayGainTrackDB = %v, want -6.5", tr.ReplayGainTrackDB)
	}
	if tr.ReplayGainAlbumDB == nil || math.Abs(*tr.ReplayGainAlbumDB-(-7.25)) > 1e-9 {
		t.Errorf("ReplayGainAlbumDB = %v, want -7.25", tr.ReplayGainAlbumDB)
	}
	if tr.MusicBrainzTrackID != "6d3ddd5e-7a4a-4d13-9d4c-0bd2e9bda0f0" {
		t.Errorf("MusicBrainzTrackID = %q", tr.MusicBrainzTrackID)
	}
}

func TestStripMP4FreeformLocales(t *testing.T) {
	loc := mp4DataLocale
	raw := map[string]any{
		"single":    loc + "value",
		"multi":     loc + "A;" + loc + "B",
		"empty":     loc,
		"semicolon": loc + "a;b", // one value that contains a ';'
		"plain":     "no locale here",
		"embedded":  "x" + loc + "y", // only a LEADING locale marks a freeform
		"number":    7,
		"list":      []string{loc + "untouched"},
	}
	stripMP4FreeformLocales(raw)
	want := map[string]any{
		"single":    "value",
		"multi":     "A;B",
		"empty":     "",
		"semicolon": "a;b",
		"plain":     "no locale here",
		"embedded":  "x" + loc + "y",
		"number":    7,
	}
	for k, w := range want {
		if raw[k] != w {
			t.Errorf("%s = %q, want %q", k, raw[k], w)
		}
	}
	if l, ok := raw["list"].([]string); !ok || l[0] != loc+"untouched" {
		t.Errorf("list = %#v: only string values are dhowden's freeform shape", raw["list"])
	}
}

// MARK: - The `gnre` walker, on synthetic trees

func TestExtractMP4PredefinedGenre_FullBoxMeta(t *testing.T) {
	got, err := extractMP4PredefinedGenre(bytes.NewReader(buildMP4WithILST(true, ilstGnre(18))))
	if err != nil || got != "Rock" {
		t.Fatalf("got %q, %v; want Rock", got, err)
	}
}

// QuickTime writes `meta` as a plain container: no version / flags before
// `hdlr`. Reading it as a FullBox would put every child 4 bytes out of phase.
func TestExtractMP4PredefinedGenre_PlainQuickTimeMeta(t *testing.T) {
	got, err := extractMP4PredefinedGenre(bytes.NewReader(buildMP4WithILST(false, ilstGnre(14))))
	if err != nil || got != "Pop" {
		t.Fatalf("got %q, %v; want Pop", got, err)
	}
}

func TestExtractMP4PredefinedGenre_AbsentShapesAreSilent(t *testing.T) {
	shortData := &bytes.Buffer{}
	writeAtom(shortData, "data", []byte{0, 0, 0, 0, 0, 0, 0, 0, 0x12}) // one value byte
	shortGnre := &bytes.Buffer{}
	writeAtom(shortGnre, "gnre", shortData.Bytes())

	cases := map[string][]byte{
		"value 0 (none)":        buildMP4WithILST(true, ilstGnre(0)),
		"past the table":        buildMP4WithILST(true, ilstGnre(uint16(len(id3v1Genres)+1))),
		"one value byte":        buildMP4WithILST(true, shortGnre.Bytes()),
		"ilst without gnre":     buildMP4WithILST(true, ilstText("\xa9nam", "Song")),
		"moov without udta":     buildMP4WithALACConfigRate(16, 44100),
		"no moov at all":        mp4Header(),
		"gnre with no data box": buildMP4WithILST(true, atomBytes("gnre", nil)),
	}
	for name, raw := range cases {
		got, err := extractMP4PredefinedGenre(bytes.NewReader(raw))
		if err != nil || got != "" {
			t.Errorf("%s: got %q, %v; want \"\", nil", name, got, err)
		}
	}
}

// A box declaring past its parent is malformed, not a genre: findAtom only
// checks the header sits inside the bound, so the walker checks the size.
func TestExtractMP4PredefinedGenre_ChildDeclaringPastItsParentIsAbsent(t *testing.T) {
	raw := buildMP4WithILST(true, ilstGnre(18))
	i := bytes.Index(raw, []byte("gnre"))
	if i < 4 {
		t.Fatal("fixture: no gnre")
	}
	binary.BigEndian.PutUint32(raw[i-4:i], 0x7FFFFFF0)
	got, err := extractMP4PredefinedGenre(bytes.NewReader(raw))
	if err != nil || got != "" {
		t.Fatalf("got %q, %v; want \"\", nil", got, err)
	}
}

func TestExtractMP4PredefinedGenre_PropagatesIOFailure(t *testing.T) {
	boom := errors.New("disk on fire")
	if _, err := extractMP4PredefinedGenre(failingReadSeeker{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reader's", err)
	}
}

// MARK: - The table

func TestID3v1GenreName(t *testing.T) {
	cases := map[uint16]string{
		1: "Blues", 14: "Pop", 18: "Rock", 142: "Christian Rock", 192: "Psybient",
		0: "", 193: "", math.MaxUint16: "",
	}
	for v, want := range cases {
		if got := id3v1GenreName(v); got != want {
			t.Errorf("id3v1GenreName(%d) = %q, want %q", v, got, want)
		}
	}
	if len(id3v1Genres) != 192 || id3v1Genres[141] != "Christian Rock " {
		t.Errorf("premise: dhowden's 192-entry table, index 141 verbatim; len %d, [141] %q",
			len(id3v1Genres), id3v1Genres[141])
	}
}

// Every index through dhowden's own numeric-genre expansion (the MP3 path)
// must name what our table names, so an M4A `gnre` and an MP3 "(17)" land on
// one genre — and a dhowden table change fails here instead of splitting a
// genre in two.
func TestID3v1Genres_MatchDhowdenExpansion(t *testing.T) {
	for n := range id3v1Genres {
		blob := buildID3v2_3(map[string]string{"genre": fmt.Sprintf("(%d)", n)})
		m, err := tag.ReadID3v2Tags(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("(%d): %v", n, err)
		}
		got := strings.TrimSpace(m.Genre())
		if want := id3v1GenreName(uint16(n + 1)); got != want {
			t.Errorf("(%d): dhowden %q, gnre %d names %q", n, got, n+1, want)
		}
	}
}

// MARK: - The moov search

// ffmpeg's default layout: the audio first, the moov after it. Before v15
// the search stopped at 4 MiB and this file had no codec, rate, bits or
// duration.
func TestExtract_MoovAfterALargeMdat_ReadsTheFormat(t *testing.T) {
	mvhd := atomBytes("mvhd", buildMVHDPayload(0, 1000, 125000))
	raw := withLeadingMdat(t,
		buildMP4WithMoovChildren("alac", buildALACSampleEntryPayloadRate(24, 96000), [][]byte{mvhd}),
		5<<20)
	tr := extractBytesAsM4A(t, raw)
	if tr.Codec != "ALAC" {
		t.Errorf("Codec = %q, want ALAC", tr.Codec)
	}
	if tr.BitsPerSample == nil || *tr.BitsPerSample != 24 {
		t.Errorf("BitsPerSample = %v, want 24", tr.BitsPerSample)
	}
	if tr.SampleRate == nil || *tr.SampleRate != 96000 {
		t.Errorf("SampleRate = %v, want 96000", tr.SampleRate)
	}
	if tr.Duration == nil || math.Abs(*tr.Duration-125) > 1e-9 {
		t.Errorf("Duration = %v, want 125", tr.Duration)
	}
}

// No moov anywhere in a large file is still honest absence, not a failure.
func TestFindMoov_NoMoovInALargeFileIsAbsent(t *testing.T) {
	raw := append(mp4Header(), atomBytes("mdat", make([]byte, 5<<20))...)
	_, _, _, err := findMoov(bytes.NewReader(raw))
	if !errors.Is(err, errMP4StructureNotFound) {
		t.Fatalf("err = %v, want errMP4StructureNotFound", err)
	}
}

// MARK: - Builders

// mp4Header is a lone `ftyp` in the shape dhowden detects as MP4.
func mp4Header() []byte { return atomBytes("ftyp", []byte("M4A mp42M4A ")) }

func atomBytes(typ string, payload []byte) []byte {
	b := &bytes.Buffer{}
	writeAtom(b, typ, payload)
	return b.Bytes()
}

// dataAtom is an ilst `data` atom: type indicator (version 0 + class), a
// zero locale, the value.
func dataAtom(class uint32, value []byte) []byte {
	p := &bytes.Buffer{}
	binary.Write(p, binary.BigEndian, class) // version 0 in the top byte
	p.Write(make([]byte, 4))                 // locale
	p.Write(value)
	return atomBytes("data", p.Bytes())
}

func ilstText(fourcc, text string) []byte {
	return atomBytes(fourcc, dataAtom(1, []byte(text)))
}

func ilstGnre(value uint16) []byte {
	v := make([]byte, 2)
	binary.BigEndian.PutUint16(v, value)
	return atomBytes("gnre", dataAtom(0, v))
}

// ilstFreeform is a `----` atom as iTunes and mutagen write it: `mean`,
// `name` (each a FullBox string), then one `data` atom per value.
func ilstFreeform(mean, name string, values ...string) []byte {
	p := &bytes.Buffer{}
	p.Write(atomBytes("mean", append([]byte{0, 0, 0, 0}, mean...)))
	p.Write(atomBytes("name", append([]byte{0, 0, 0, 0}, name...)))
	for _, v := range values {
		p.Write(dataAtom(1, []byte(v)))
	}
	return atomBytes("----", p.Bytes())
}

// buildMP4WithILST is an ALAC file (so the codec / rate walks resolve) whose
// moov carries udta/meta/{hdlr, ilst{items}}; fullBoxMeta picks between
// the iTunes / ISO `meta` and QuickTime's plain container.
func buildMP4WithILST(fullBoxMeta bool, items ...[]byte) []byte {
	hdlr := &bytes.Buffer{}
	hdlr.Write(make([]byte, 8)) // version + flags, pre_defined
	hdlr.WriteString("mdir")    // handler_type
	hdlr.Write(make([]byte, 12))
	hdlr.WriteByte(0) // empty name
	meta := &bytes.Buffer{}
	if fullBoxMeta {
		meta.Write(make([]byte, 4))
	}
	meta.Write(atomBytes("hdlr", hdlr.Bytes()))
	meta.Write(atomBytes("ilst", bytes.Join(items, nil)))
	udta := atomBytes("udta", atomBytes("meta", meta.Bytes()))
	return buildMP4WithMoovChildren("alac", buildALACSampleEntryPayloadRate(16, 44100), [][]byte{udta})
}

// withLeadingMdat moves the audio in front of the moov: ftyp, then an mdat
// of `size` zero bytes, then everything that followed the ftyp.
func withLeadingMdat(t *testing.T, mp4 []byte, size int) []byte {
	t.Helper()
	ftypSize := int(binary.BigEndian.Uint32(mp4[0:4]))
	if string(mp4[4:8]) != "ftyp" || ftypSize > len(mp4) {
		t.Fatalf("fixture: expected a leading ftyp")
	}
	out := append([]byte{}, mp4[:ftypSize]...)
	out = append(out, atomBytes("mdat", make([]byte, size))...)
	return append(out, mp4[ftypSize:]...)
}

// B428: use the actual four-byte atom name, never the five UTF-8 bytes of ©wrk.
func TestExtractMP4Work(t *testing.T) {
	work := func(v string) []byte { return ilstText("\xa9wrk", v) }
	free := func(v string) []byte { return ilstFreeform("com.apple.iTunes", "WORK", v) }
	long := strings.TrimSpace(strings.Repeat("Symphony in D minor — complete work; ", 12))
	cases := []struct {
		name  string
		items [][]byte
		want  string
	}{
		{"atom alone", [][]byte{work("  Requiem \n")}, "Requiem"},
		{"iTunes WORK alone", [][]byte{free(" Mass ")}, "Mass"},
		{"other mean WORK alone", [][]byte{ilstFreeform("org.example.tags", "WORK", " Mass ")}, "Mass"},
		{"blank atom beside WORK", [][]byte{work(" \t\n"), free("Mass")}, "Mass"},
		{"empty atom beside WORK", [][]byte{work(""), free("Mass")}, "Mass"},
		{"atom beside blank WORK", [][]byte{work("Requiem"), free(" \n")}, "Requiem"},
		{"atom beside empty WORK", [][]byte{work("Requiem"), free("")}, "Requiem"},
		{"equal atom first", [][]byte{work("Mass"), free("Mass")}, "Mass"},
		{"equal WORK first", [][]byte{free("Mass"), work("Mass")}, "Mass"},
		{"conflict atom first", [][]byte{work("Requiem"), free("Mass")}, "Mass"},
		{"conflict WORK first", [][]byte{free("Mass"), work("Requiem")}, "Mass"},
		{"other mean wins atom first", [][]byte{work("Requiem"), ilstFreeform("org.example.tags", "WORK", "Mass")}, "Mass"},
		{"other mean wins WORK first", [][]byte{ilstFreeform("org.example.tags", "WORK", "Mass"), work("Requiem")}, "Mass"},
		{"existing WORK beats other mean", [][]byte{free("Mass"), ilstFreeform("org.example.tags", "WORK", "New")}, "Mass"},
		{"longer than advisory limit", [][]byte{work(long)}, long},
		{"at work limit", [][]byte{work(strings.Repeat("w", 64<<10))}, strings.Repeat("w", 64<<10)},
		{"over work limit", [][]byte{work(strings.Repeat("w", (64<<10)+1))}, ""},
		{"over limit beside WORK", [][]byte{work(strings.Repeat("w", (64<<10)+1)), free("Mass")}, "Mass"},
		{"other mean over limit", [][]byte{ilstFreeform("org.example.tags", "WORK", strings.Repeat("w", (64<<10)+1)), work("Requiem")}, "Requiem"},
		{"non-text data", [][]byte{atomBytes("\xa9wrk", dataAtom(21, []byte("Mass")))}, ""},
		{"no data", [][]byte{atomBytes("\xa9wrk", nil)}, ""},
		{"neither", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := append([][]byte{ilstText("\xa9nam", "Movement")}, tc.items...)
			raw := buildMP4WithILST(true, items...)
			// Absence is part of the contract even when WORK would outrank
			// an erroneously present blank candidate at final selection.
			if tc.name == "blank atom beside WORK" || tc.name == "empty atom beside WORK" {
				tags, err := extractMP4WorkTags(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				if _, present := tags["©wrk"]; present {
					t.Error("blank ©wrk supplied a candidate beside WORK")
				}
			}
			tr := extractBytesAsM4A(t, raw)
			if tr.Title != "Movement" || tr.Codec != "ALAC" {
				t.Fatalf("fixture not parsed: title %q codec %q", tr.Title, tr.Codec)
			}
			if tr.Work != tc.want {
				t.Errorf("Work length %d = %.100q, want length %d = %.100q", len(tr.Work), tr.Work, len(tc.want), tc.want)
			}
		})
	}
}

func TestExtractMP4Work_TruncatedPayload(t *testing.T) {
	raw := buildMP4WithILST(true, ilstText("\xa9wrk", "Requiem"))
	// Keep every declared size; only the work's value is cut short.
	end := bytes.Index(raw, []byte("Requiem")) + len("Requiem")
	raw = raw[:end-2]
	if got := extractBytesAsM4A(t, raw).Work; got != "" {
		t.Fatalf("truncated work = %q, want absent", got)
	}
}

func TestExtractMP4Work_DhowdenFailureStillWalks(t *testing.T) {
	// A malformed title makes dhowden discard its metadata; the independent
	// walks still read structurally sound sibling fields.
	raw := buildMP4WithILST(true, ilstText("\xa9wrk", "Requiem"), ilstGnre(18), atomBytes("\xa9nam", dataAtom(99, []byte("bad"))))
	if _, err := tag.ReadFrom(bytes.NewReader(raw)); err == nil {
		t.Fatal("premise: dhowden accepted the malformed title")
	}
	tr := extractBytesAsM4A(t, raw)
	if tr.Work != "Requiem" || tr.Genre != "Rock" {
		t.Fatalf("Work/Genre = %q/%q, want Requiem/Rock despite dhowden failure", tr.Work, tr.Genre)
	}
}

// The v26 scan proof's two legs: changed rows join the delta, unchanged rows
// only stamp. Rewind the database alone so size and mtime cannot trigger a read.
func TestScanner_V33_MP4WorkReReadsV32Rows(t *testing.T) {
	root := t.TempDir()
	rows := []struct {
		name, old, want string
		items           [][]byte
	}{
		{"atom.m4a", "", "Requiem", [][]byte{ilstText("\xa9wrk", "Requiem")}},
		{"other.m4a", "", "Mass", [][]byte{ilstFreeform("org.example.tags", "WORK", "Mass")}},
		{"existing.m4a", "Mass", "Mass", [][]byte{ilstFreeform("com.apple.iTunes", "WORK", "Mass"), ilstText("\xa9wrk", "Requiem")}},
		{"plain.m4a", "", "", nil},
	}
	for _, row := range rows {
		if err := os.WriteFile(filepath.Join(root, row.name), buildMP4WithILST(true, row.items...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "initial")
	before := map[string]int64{}
	for _, row := range rows {
		if _, err := store.db.Exec(`UPDATE tracks SET extractor_version = 32, enriched_at = 1,
			tags_json = json_remove(tags_json, '$.work') WHERE path = ?`, row.name); err != nil {
			t.Fatal(err)
		}
		if row.old != "" {
			if _, err := store.db.Exec(`UPDATE tracks SET tags_json = json_set(tags_json, '$.work', ?) WHERE path = ?`, row.old, row.name); err != nil {
				t.Fatal(err)
			}
		}
		if got := storedTrack(t, store, row.name).Work; got != row.old {
			t.Fatalf("rewind %s: %q", row.name, got)
		}
		before[row.name] = trackIndexedAt(t, store, row.name)
	}
	scanOnce(t, sc, "v33")
	for _, row := range rows {
		if got := storedTrack(t, store, row.name).Work; got != row.want {
			t.Errorf("%s: Work = %q, want %q", row.name, got, row.want)
		}
		if got := trackColumn(t, store, row.name, "extractor_version"); got != int64(ExtractorVersion) {
			t.Errorf("%s: version = %d", row.name, got)
		}
		after := trackIndexedAt(t, store, row.name)
		enriched := trackColumn(t, store, row.name, "enriched_at")
		if row.old != row.want {
			if after <= before[row.name] || enriched != 0 {
				t.Errorf("%s: changed row did not join delta/reset enrichment: %d -> %d, enriched %d", row.name, before[row.name], after, enriched)
			}
		} else if after != before[row.name] || enriched != 1 {
			t.Errorf("%s: unchanged row was rewritten", row.name)
		}
	}
	scanOnce(t, sc, "unchanged")
	for _, row := range rows {
		if got := storedTrack(t, store, row.name).Work; got != row.want {
			t.Errorf("%s: later scan work = %q", row.name, got)
		}
	}
}

// mp4WorkReadFailure allows seeks to succeed and fails the value read itself.
type mp4WorkReadFailure struct {
	*bytes.Reader
	err error
}

func (r mp4WorkReadFailure) Read([]byte) (int, error) { return 0, r.err }

func TestReadMP4WorkText(t *testing.T) {
	raw := dataAtom(1, []byte("Requiem"))
	box := mp4Box{header: 8, size: uint64(len(raw))}
	boom := errors.New("work read failed")
	for _, tc := range []struct {
		name    string
		r       io.ReadSeeker
		wantErr error
	}{
		{"truncated", bytes.NewReader(raw[:len(raw)-1]), nil},
		{"EOF", bytes.NewReader(raw[:8]), nil},
		{"read failure", mp4WorkReadFailure{bytes.NewReader(raw), boom}, boom},
		{"seek failure", failingReadSeeker{err: boom}, boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, ok, err := readMP4WorkText(tc.r, box)
			if value != "" || ok || !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %q, %v, %v", value, ok, err)
			}
		})
	}
	for _, class := range []uint32{0, 2, 21} {
		raw := dataAtom(class, []byte("Mass"))
		if _, ok, err := readMP4WorkText(bytes.NewReader(raw), mp4Box{header: 8, size: uint64(len(raw))}); ok || err != nil {
			t.Errorf("class %d: %v, %v", class, ok, err)
		}
	}
	invalid := dataAtom(1, []byte{0xff})
	if _, ok, err := readMP4WorkText(bytes.NewReader(invalid), mp4Box{header: 8, size: uint64(len(invalid))}); ok || err != nil {
		t.Errorf("invalid UTF-8: %v, %v", ok, err)
	}
}

func TestExtractMP4WorkTags_PropagatesIOFailure(t *testing.T) {
	boom := errors.New("disk failure")
	if _, err := extractMP4WorkTags(failingReadSeeker{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("got %v, want %v", err, boom)
	}
}
