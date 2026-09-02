package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
	"github.com/dhowden/tag"
)

// extractedLyrics is the ONE document an extraction resolved for a file —
// what track_lyrics stores and /v1/lyrics serves.
type extractedLyrics struct {
	Format      string
	Synced      bool
	Body        string
	Language    string
	Source      string
	SidecarName string
	Tag         string
	// The LYRICS SOURCE's stat — the sidecar's when the row came from one,
	// else the audio file's — so the endpoint can answer 410 when THAT
	// file drifted (an edited .lrc under an unchanged FLAC).
	SourceMTimeNS int64
	SourceSize    int64
}

type sidecarStat struct {
	name    string
	mtimeNS int64
	size    int64
}

// sidecarLyricsExts in the order the skip gate and the extractor both
// prefer (lyrics.Source ranks make .ttml lose to LRC-shaped embedded
// sources; the ORDER here only decides which single file we read).
var sidecarLyricsExts = []string{".lrc", ".ttml", ".txt"}

type sidecarListing struct {
	once  sync.Once
	names map[string]string // lowercased file name → actual name
}

func loadSidecarNames(dir string) map[string]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".lrc", ".ttml", ".txt":
			m[strings.ToLower(e.Name())] = e.Name()
		}
	}
	return m
}

func sidecarNamesForDir(ec *ExtractContext, dir string) map[string]string {
	if ec == nil || ec.SidecarIndex == nil {
		return loadSidecarNames(dir)
	}
	pI, _ := ec.SidecarIndex.LoadOrStore(dir, &sidecarListing{})
	p := pI.(*sidecarListing)
	p.once.Do(func() { p.names = loadSidecarNames(dir) })
	return p.names
}

// sidecarLyricsFile finds THE sidecar for an audio file: same stem
// (case-folded), .lrc over .ttml over .txt. One answer shared by the
// extractor and the skip gate so they can never disagree.
func sidecarLyricsFile(absPath string, ec *ExtractContext) (name, abs string, ok bool) {
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)
	stem := strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
	names := sidecarNamesForDir(ec, dir)
	for _, ext := range sidecarLyricsExts {
		if actual, ok := names[stem+ext]; ok {
			return actual, filepath.Join(dir, actual), true
		}
	}
	return "", "", false
}

func sidecarSource(name string) lyrics.Source {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".lrc":
		return lyrics.SourceSidecarLRC
	case ".ttml":
		return lyrics.SourceSidecarTTML
	default:
		return lyrics.SourceSidecarText
	}
}

// sidecarLyricsDrifted is the skip gate's question for an UNCHANGED audio
// file: would re-extracting change the lyrics row? True when a sidecar
// that outranks the stored source appeared, the stored sidecar vanished or
// was renamed, or its size / mtime moved. False keeps the cheap skip.
func sidecarLyricsDrifted(absPath string, st *TrackStat, ec *ExtractContext) bool {
	name, abs, ok := sidecarLyricsFile(absPath, ec)
	stored := lyrics.Source(st.LyricsSource)
	storedIsSidecar := strings.HasPrefix(st.LyricsSource, "sidecar")
	if !ok {
		return storedIsSidecar
	}
	if !storedIsSidecar {
		// A sidecar beside an embedded-sourced row matters only if it
		// would win the pick; a .txt next to a SYLT never does.
		return sidecarSource(name).Rank() < stored.Rank()
	}
	if st.LyricsSidecarName != name {
		return true
	}
	info, err := os.Stat(abs)
	if err != nil {
		return true
	}
	return info.ModTime().UnixNano() != st.LyricsSourceMTimeNS || info.Size() != st.LyricsSourceSize
}

// decodeSidecarText accepts UTF-8 (with or without a BOM) and BOM-marked
// UTF-16. Legacy single/double-byte encodings (GB18030, Shift_JIS, …) are
// NOT guessed here: the phone's own sidecar tier reads the file over the
// bridge and runs its encoding ladder, so nothing is lost by serving none.
func decodeSidecarText(raw []byte) (string, bool) {
	if len(raw) >= 2 && ((raw[0] == 0xFF && raw[1] == 0xFE) || (raw[0] == 0xFE && raw[1] == 0xFF)) {
		be := raw[0] == 0xFE
		body := raw[2:]
		units := make([]uint16, 0, len(body)/2)
		for i := 0; i+1 < len(body); i += 2 {
			if be {
				units = append(units, uint16(body[i])<<8|uint16(body[i+1]))
			} else {
				units = append(units, uint16(body[i+1])<<8|uint16(body[i]))
			}
		}
		return string(utf16.Decode(units)), true
	}
	s := strings.TrimPrefix(string(raw), "\uFEFF")
	if !utf8.ValidString(s) {
		return "", false
	}
	return s, true
}

// applySidecarLyrics reads THE sidecar beside absPath (bounded) into a
// candidate and remembers its stat for the row's provenance columns.
func applySidecarLyrics(absPath string, t *Track, ec *ExtractContext) {
	name, abs, ok := sidecarLyricsFile(absPath, ec)
	if !ok {
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	// Remember the stat BEFORE any rejection below: a sidecar that yields
	// nothing (empty, oversized, legacy-encoded, whitespace) still gets a
	// `sidecar-rejected` provenance row, so the skip gate can compare its
	// stat instead of re-extracting the audio file on every scan
	// (CodeRabbit on bridge #840).
	t.lyricsSidecar = &sidecarStat{name: name, mtimeNS: info.ModTime().UnixNano(), size: info.Size()}
	if info.Size() <= 0 || info.Size() > lyrics.MaxBodyBytes {
		return
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return
	}
	text, ok := decodeSidecarText(raw)
	if !ok {
		return
	}
	body, ok := lyrics.Normalize(text)
	if !ok {
		return
	}
	var c lyrics.Candidate
	switch sidecarSource(name) {
	case lyrics.SourceSidecarLRC:
		if lyrics.LooksLikeLRC(body) {
			c = lyrics.Candidate{Source: lyrics.SourceSidecarLRC,
				Doc: lyrics.Doc{Format: lyrics.FormatLRC, Synced: true, Body: body}}
		} else {
			// A tagless .lrc is plain text and ranks as such.
			c = lyrics.Candidate{Source: lyrics.SourceSidecarText,
				Doc: lyrics.Doc{Format: lyrics.FormatText, Synced: false, Body: body}}
		}
	case lyrics.SourceSidecarTTML:
		c = lyrics.Candidate{Source: lyrics.SourceSidecarTTML,
			Doc: lyrics.Doc{Format: lyrics.FormatTTML, Synced: true, Body: body}}
	default:
		c = lyrics.Candidate{Source: lyrics.SourceSidecarText,
			Doc: lyrics.Doc{Format: lyrics.FormatText, Synced: false, Body: body}}
	}
	c.SidecarName = name
	t.lyricsCandidates = append(t.lyricsCandidates, c)
}

// iso639ToBCP47 maps the three-letter ID3 language code to the tag the
// wire carries; unknown / "XXX" / "und" → "" (no claim).
func iso639ToBCP47(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "eng":
		return "en"
	case "jpn":
		return "ja"
	case "zho", "chi":
		return "zh"
	case "kor":
		return "ko"
	case "deu", "ger":
		return "de"
	case "fra", "fre":
		return "fr"
	case "spa":
		return "es"
	case "ita":
		return "it"
	case "por":
		return "pt"
	case "rus":
		return "ru"
	case "nld", "dut":
		return "nl"
	case "swe":
		return "sv"
	case "nor":
		return "no"
	case "dan":
		return "da"
	case "fin":
		return "fi"
	case "pol":
		return "pl"
	case "ces", "cze":
		return "cs"
	case "hun":
		return "hu"
	case "tur":
		return "tr"
	case "ara":
		return "ar"
	case "heb":
		return "he"
	case "hin":
		return "hi"
	case "ell", "gre":
		return "el"
	case "ukr":
		return "uk"
	}
	return ""
}

// applyEmbeddedLyricsFromTag collects the embedded candidates dhowden/tag
// exposes: the library's own Lyrics() accessor (the FIRST USLT / MP4 ©lyr /
// Vorbis "lyrics"), every raw SYLT / SLT frame (rendered to LRC — the
// library leaves them as the raw frame body, header consumed; v2.2 keys
// "SLT", duplicates "SYLT_0"…), and every USLT / ULT *Comm for the
// multi-frame pick. Vorbis SYNCEDLYRICS et al. arrive through the FLAC raw
// walk (applyFLACMultiValueArtists) instead.
func applyEmbeddedLyricsFromTag(m tag.Metadata, t *Track) {
	if m == nil {
		return
	}
	if text := strings.TrimSpace(m.Lyrics()); text != "" {
		if c, ok := lyrics.TextCandidate(text, "", false, 0); ok {
			t.lyricsCandidates = append(t.lyricsCandidates, c)
		}
	}
	for key, v := range m.Raw() {
		var c lyrics.Candidate
		var ok bool
		switch rawFrameBase(key) {
		case "SYLT", "SLT":
			c, ok = syltCandidate(v)
		case "USLT", "ULT":
			c, ok = usltCandidate(v)
		}
		if ok {
			t.lyricsCandidates = append(t.lyricsCandidates, c)
		}
	}
}

// rawFrameBase folds dhowden's duplicate-frame keys ("SYLT_0", "USLT_2")
// onto the frame id.
func rawFrameBase(key string) string {
	up := strings.ToUpper(key)
	if i := strings.IndexByte(up, '_'); i > 0 {
		return up[:i]
	}
	return up
}

func syltCandidate(v any) (lyrics.Candidate, bool) {
	body, ok := v.([]byte)
	if !ok {
		return lyrics.Candidate{}, false
	}
	s, ok := lyrics.ParseSYLT(body)
	if !ok {
		return lyrics.Candidate{}, false
	}
	lrc, _ := lyrics.ToLRC(s)
	nb, ok := lyrics.Normalize(lrc)
	if !ok {
		return lyrics.Candidate{}, false
	}
	lang := iso639ToBCP47(s.Language)
	return lyrics.Candidate{
		Source:   lyrics.SourceSYLT,
		Doc:      lyrics.Doc{Format: lyrics.FormatLRC, Synced: true, Body: nb, Language: lang},
		Language: lang,
		Priority: lyrics.DescriptorPriority(s.Descriptor),
	}, true
}

func usltCandidate(v any) (lyrics.Candidate, bool) {
	c, ok := v.(*tag.Comm)
	if !ok || c == nil {
		return lyrics.Candidate{}, false
	}
	return lyrics.TextCandidate(c.Text, iso639ToBCP47(c.Language), false,
		lyrics.DescriptorPriority(c.Description))
}

// resolveLyrics picks ONE document from the candidates and stamps the
// provenance the store persists. Always clears the scratch fields.
func resolveLyrics(t *Track) {
	defer func() {
		t.lyricsCandidates = nil
		t.lyricsSidecar = nil
	}()
	pick, ok := lyrics.Pick(t.lyricsCandidates)
	if !ok {
		t.lyrics = rejectedSidecarRow(t.lyricsSidecar)
		return
	}
	body, ok := lyrics.Normalize(pick.Doc.Body)
	if !ok {
		t.lyrics = rejectedSidecarRow(t.lyricsSidecar)
		return
	}
	doc := lyrics.Doc{Format: pick.Doc.Format, Synced: pick.Doc.Synced, Body: body, Language: pick.Doc.Language}
	ex := &extractedLyrics{
		Format: doc.Format, Synced: doc.Synced, Body: body,
		Language: doc.Language, Source: string(pick.Source), Tag: lyrics.Tag(doc),
		SourceMTimeNS: t.ModTime.UnixNano(), SourceSize: t.Size,
	}
	if pick.SidecarName != "" && t.lyricsSidecar != nil && t.lyricsSidecar.name == pick.SidecarName {
		ex.SidecarName = pick.SidecarName
		ex.SourceMTimeNS = t.lyricsSidecar.mtimeNS
		ex.SourceSize = t.lyricsSidecar.size
	}
	t.lyrics = ex
}

// LyricsSourceSidecarRejected marks a track whose sidecar exists but yielded
// no document (empty, oversized, legacy-encoded, whitespace-only). The row
// carries the sidecar's stat and an EMPTY tag — invisible to the manifest
// (`lyricsTagSQL` NULLIFs it) and to /v1/lyrics (an empty body is 404) —
// so the skip gate can compare the sidecar's stat instead of re-extracting
// the audio file on every scan, and an edited sidecar still re-extracts.
const LyricsSourceSidecarRejected = "sidecar-rejected"

func rejectedSidecarRow(sc *sidecarStat) *extractedLyrics {
	if sc == nil {
		return nil
	}
	return &extractedLyrics{
		Format: "none", Source: LyricsSourceSidecarRejected, SidecarName: sc.name,
		SourceMTimeNS: sc.mtimeNS, SourceSize: sc.size,
	}
}
