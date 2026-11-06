package manifest

// A hybrid WavPack file's correction file (.wvc) through the real scanner
// (backlog B489): the four observations a row records (confirmed absence, a
// matching file, one that does not match, an observation that did not
// complete), the skip gate's question about a correction file that changed
// beside an unchanged .wv, and the file on the wire. These tests count the
// audio files each scan opened (artFixture), the measure of what it re-read.

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The hybrid file the tests index, and its correction file's name.
const (
	hybridRel = "Artist/Album/01 Song.wv"
	hybridWVC = "Artist/Album/01 Song.wvc"
)

// hybridSpec is a hybrid stream two blocks long, so that its correction file
// can be cut at a block boundary.
func hybridSpec() wvSpec {
	return wvSpec{rate: 44100, bytes: 2, channels: 2, total: 200000, hybrid: true}
}

// writeHybrid writes a WavPack file of spec at rel in f's library, tagged,
// with the mtime t0.
func writeHybrid(t *testing.T, f *artFixture, rel string, spec wvSpec) {
	t.Helper()
	writeWavPack(t, f.path(rel), spec, []apeItem{apeText("Title", "Song")})
	setMTime(t, f.path(rel), t0)
}

// writeCorrection writes data as the file rel in f's library, with an mtime
// of its own, so a replacement never shares its predecessor's on a coarse
// clock.
func writeCorrection(t *testing.T, f *artFixture, rel string, data []byte, mtime time.Time) {
	t.Helper()
	writeFixtureFile(t, f.path(rel), data)
	setMTime(t, f.path(rel), mtime)
}

// correctionKeyOf returns the correction_key column of the row at rel.
func correctionKeyOf(t *testing.T, s *Store, rel string) string {
	t.Helper()
	var k string
	if err := s.db.QueryRow(`SELECT correction_key FROM tracks WHERE path = ?`, rel).Scan(&k); err != nil {
		t.Fatalf("correction_key(%s): %v", rel, err)
	}
	return k
}

// keyState is the state a correction key records: 0 for none.
func keyState(key string) byte {
	if key == "" {
		return 0
	}
	return key[0]
}

// requireCorrection asserts the row at rel is codec with bits (0: none), the
// sample_format column kind, and on the wire the correction file wire ("":
// none) at that file's size and mtime.
func requireCorrection(t *testing.T, f *artFixture, rel, codec string, bits int, kind, wire string) {
	t.Helper()
	tr := readTrack(t, f.store, rel)
	if tr.Codec != codec {
		t.Errorf("codec %q, want %q", tr.Codec, codec)
	}
	switch {
	case bits == 0 && tr.BitsPerSample != nil:
		t.Errorf("bitsPerSample %d, want none: a lossy stream states no depth", *tr.BitsPerSample)
	case bits != 0 && (tr.BitsPerSample == nil || *tr.BitsPerSample != bits):
		t.Errorf("bitsPerSample %v, want %d", tr.BitsPerSample, bits)
	}
	requireSampleFormats(t, f.store, map[string]string{rel: kind})
	if wire == "" {
		if tr.Correction != nil {
			t.Errorf("correction %+v on the wire, want none", *tr.Correction)
		}
		return
	}
	info, err := os.Stat(f.path(wire))
	if err != nil {
		t.Fatal(err)
	}
	want := CorrectionFile{Path: wire, Size: info.Size(), ModTime: info.ModTime().UTC()}
	if tr.Correction == nil || tr.Correction.Path != want.Path || tr.Correction.Size != want.Size ||
		!tr.Correction.ModTime.Equal(want.ModTime) {
		t.Errorf("correction %+v on the wire, want %+v", tr.Correction, want)
	}
}

// TestAHybridWavPacksCorrectionFileDecidesItsCodec pins each observation a
// first scan makes: a hybrid stream is WAVPACK, with its bits, its sample
// kind and its correction file on the wire, only beside its own correction
// file (the .wv's stem exactly, the extension in any case, as the walk names
// a track); beside none, or only something that is not a file under its
// name, it is WAVPACK-LOSSY with neither; beside a file that does not match
// it (another stream's, one cut short inside a block or at one's end, not
// WavPack, the hybrid file itself) it is WAVPACK-LOSSY too, and the scan says
// so once. A correction file beside a stream that is not hybrid is nothing to
// its row. Each row settles: the next scan reads nothing.
func TestAHybridWavPacksCorrectionFileDecidesItsCodec(t *testing.T) {
	spec := hybridSpec()
	other := spec
	other.total = 300000
	lossless := spec
	lossless.hybrid = false
	dsd := wvSpec{rate: 44100, bytes: 1, dsd: true, dsdPower: 3, channels: 2, total: 352800}
	full := wvcFile(spec)
	type want struct {
		codec  string
		bits   int
		kind   string
		wire   string
		key    byte
		warned bool
	}
	lossy := want{codec: CodecWavPackLossy, key: '-'}
	rejected := want{codec: CodecWavPackLossy, key: '!', warned: true}
	matched := want{CodecWavPack, 16, SampleFormatInteger, hybridWVC, '+', false}
	// Round 2: the reference decoder takes a correction block only with the
	// stream's own flags (match_wvc_header), so a correction file of other
	// bits or channels is another stream's, whatever its indexes say (Codex);
	// and where the .wv's header states no length, or its first block is past
	// the stream's first sample, the correction file must end where the .wv's
	// own last block does (Codex, Claude).
	bits24, mono := spec, spec
	bits24.bytes, mono.channels = 3, 1
	unknown := spec
	unknown.total = -1
	// late is spec's stream from its sample 4096 on: two blocks, the first
	// at index 4096, as a file that starts past the stream's first sample
	// holds; lateWVC its correction file, whole and cut after one block.
	late := func(correction bool, blocks int) []byte {
		var out []byte
		for i := 1; i <= blocks; i++ {
			b := wvFirstBlock(spec, correction)
			b.blockIndex = uint64(i) * 4096
			out = append(out, b.bytes()...)
		}
		return out
	}
	// emptyWVC is spec's correction file whose first block declares its
	// correction bitstream empty, which the reference decoder refuses
	// (init_wvc_bitstream).
	emptyWVC := func() []byte {
		first := wvFirstBlock(spec, true)
		n := len(first.bytes())
		first.subs = [][]byte{wvSub(fxIDWVCBitstream, nil)}
		return append(first.bytes(), full[n:]...)
	}()
	cases := []struct {
		name  string
		wv    wvSpec
		files map[string][]byte
		dir   string
		want  want
	}{
		{"another stream's of other bits", spec, map[string][]byte{hybridWVC: wvcFile(bits24)}, "", rejected},
		{"another stream's of other channels", spec, map[string][]byte{hybridWVC: wvcFile(mono)}, "", rejected},
		{"its own, the length unknown", unknown, map[string][]byte{hybridWVC: wvcFile(unknown)}, "", matched},
		{"cut at a block's end, the length unknown", unknown, map[string][]byte{hybridWVC: wvFirstBlock(unknown, true).bytes()}, "", rejected},
		{"its own, the stream from past its first sample", spec, map[string][]byte{hybridWVC: late(true, 2)}, "", matched},
		{"cut at a block's end, the stream from past its first sample", spec, map[string][]byte{hybridWVC: late(true, 1)}, "", rejected},
		{"its own, its correction bitstream empty", spec, map[string][]byte{hybridWVC: emptyWVC}, "", rejected},
		{"no correction file", spec, nil, "", lossy},
		{"its own", spec, map[string][]byte{hybridWVC: full}, "",
			want{CodecWavPack, 16, SampleFormatInteger, hybridWVC, '+', false}},
		{"its own, the extension in capitals", spec, map[string][]byte{"Artist/Album/01 Song.WVC": full}, "",
			want{CodecWavPack, 16, SampleFormatInteger, "Artist/Album/01 Song.WVC", '+', false}},
		{"another stream's", spec, map[string][]byte{hybridWVC: wvcFile(other)}, "", rejected},
		{"cut inside its last block", spec, map[string][]byte{hybridWVC: full[:len(full)-10]}, "", rejected},
		{"cut at a block's end", spec, map[string][]byte{hybridWVC: wvFirstBlock(spec, true).bytes()}, "", rejected},
		{"not WavPack", spec, map[string][]byte{hybridWVC: bytes.Repeat([]byte("not a correction file "), 20)}, "", rejected},
		{"the hybrid file itself", spec, map[string][]byte{hybridWVC: wvFile(spec)}, "", rejected},
		{"a directory of its name", spec, nil, hybridWVC, lossy},
		{"another file's", spec, map[string][]byte{"Artist/Album/02 Other.wvc": full}, "", lossy},
		{"its name in another case", spec, map[string][]byte{"Artist/Album/01 song.wvc": full}, "", lossy},
		{"beside a stream that is not hybrid", lossless, map[string][]byte{hybridWVC: full}, "",
			want{CodecWavPack, 16, SampleFormatInteger, "", 0, false}},
		{"beside a DSD stream", dsd, map[string][]byte{hybridWVC: full}, "",
			want{CodecWavPackDSD, 1, "", "", 0, false}},
	}
	// The .wv's audio where a case's spec cannot lay it out.
	wvData := map[string][]byte{
		"its own, the stream from past its first sample":              late(false, 2),
		"cut at a block's end, the stream from past its first sample": late(false, 2),
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newArtFixture(t)
			if data, ok := wvData[c.name]; ok {
				writeFixtureFile(t, f.path(hybridRel), append(append([]byte(nil), data...), apeTag([]apeItem{apeText("Title", "Song")}, true)...))
				setMTime(t, f.path(hybridRel), t0)
			} else {
				writeHybrid(t, f, hybridRel, c.wv)
			}
			for rel, data := range c.files {
				writeCorrection(t, f, rel, data, t0.Add(time.Minute))
			}
			if c.dir != "" {
				f.dir(t, c.dir)
			}
			rec := loggingtest.Record(t)
			if n := f.scan(t, "index"); n != 1 {
				t.Fatalf("the index read %d audio files, want 1", n)
			}
			requireCorrection(t, f, hybridRel, c.want.codec, c.want.bits, c.want.kind, c.want.wire)
			if got := keyState(correctionKeyOf(t, f.store, hybridRel)); got != c.want.key {
				t.Errorf("correction_key state %q, want %q", got, c.want.key)
			}
			f.requireSettled(t, hybridRel)
			wantLines := 0
			if c.want.warned {
				wantLines = 1
			}
			if lines := rec.Lines(msgCorrectionMismatch); len(lines) != wantLines {
				t.Errorf("mismatch lines %q, want %d", lines, wantLines)
			}
			for _, rel := range []string{hybridWVC, "Artist/Album/01 Song.WVC"} {
				if got, _ := f.store.GetTrack(context.Background(), rel); got != nil {
					t.Errorf("%s has a row: a correction file is never a track", rel)
				}
			}
		})
	}
}

// TestACorrectionFilesChangesReachItsRow pins the skip gate's question for an
// unchanged .wv: a correction file appearing, rewritten, replaced by one that
// does not match, changed again, and removed each re-reads the .wv once, and
// the row then settles. A change a client sees (the codec, the correction
// file's size or mtime on the wire) moves indexed_at; one it does not (a file
// that still does not match, at another stat; that file removed) is stamped
// without moving it, its new stat recorded so the gate asks no more. A file
// that does not match is said once for each of its stats. Removing the
// correction file of a lossless row clears its sample kind.
func TestACorrectionFilesChangesReachItsRow(t *testing.T) {
	f := newArtFixture(t)
	spec := hybridSpec()
	other := spec
	other.total = 300000
	writeHybrid(t, f, hybridRel, spec)
	rec := loggingtest.Record(t)
	f.scan(t, "index")
	requireCorrection(t, f, hybridRel, CodecWavPackLossy, 0, "", "")
	f.requireSettled(t, hybridRel)

	steps := []struct {
		name   string
		change func()
		codec  string
		bits   int
		kind   string
		wire   string
		moved  bool
		warned int
	}{
		{"its correction file added", func() { writeCorrection(t, f, hybridWVC, wvcFile(spec), t0.Add(time.Hour)) },
			CodecWavPack, 16, SampleFormatInteger, hybridWVC, true, 0},
		{"rewritten", func() { writeCorrection(t, f, hybridWVC, wvcFile(spec), t0.Add(2*time.Hour)) },
			CodecWavPack, 16, SampleFormatInteger, hybridWVC, true, 0},
		{"replaced by another stream's", func() { writeCorrection(t, f, hybridWVC, wvcFile(other), t0.Add(3*time.Hour)) },
			CodecWavPackLossy, 0, "", "", true, 1},
		{"that one changed again", func() { writeCorrection(t, f, hybridWVC, wvcFile(other)[:100], t0.Add(4*time.Hour)) },
			CodecWavPackLossy, 0, "", "", false, 2},
		{"removed", func() {
			if err := os.Remove(f.path(hybridWVC)); err != nil {
				t.Fatal(err)
			}
		}, CodecWavPackLossy, 0, "", "", false, 2},
		{"its own added back", func() { writeCorrection(t, f, hybridWVC, wvcFile(spec), t0.Add(5*time.Hour)) },
			CodecWavPack, 16, SampleFormatInteger, hybridWVC, true, 2},
		{"removed again", func() {
			if err := os.Remove(f.path(hybridWVC)); err != nil {
				t.Fatal(err)
			}
		}, CodecWavPackLossy, 0, "", "", true, 2},
	}
	for _, s := range steps {
		before := f.indexedAts(t, hybridRel)
		s.change()
		if n := f.scan(t, s.name); n != 1 {
			t.Errorf("%s: the scan read %d audio files, want the .wv once", s.name, n)
		}
		requireCorrection(t, f, hybridRel, s.codec, s.bits, s.kind, s.wire)
		if s.moved {
			f.requireMoved(t, before)
		} else {
			f.requireStill(t, before)
		}
		f.requireSettled(t, hybridRel)
		if lines := rec.Lines(msgCorrectionMismatch); len(lines) != s.warned {
			t.Errorf("%s: mismatch lines %q, want %d in all", s.name, lines, s.warned)
		}
	}
}

// failingCorrection is an opened correction file whose reads, or stats, fail
// with EIO, or whose stats after the first answer a size one larger: a file
// replaced while it was read.
type failingCorrection struct {
	*os.File
	failRead, failStat, replaced bool
	// readErr is what a failed read answers: EIO when 0.
	readErr syscall.Errno
	stats   int
}

func (c *failingCorrection) ReadAt(p []byte, off int64) (int, error) {
	if c.failRead {
		errno := c.readErr
		if errno == 0 {
			errno = syscall.EIO
		}
		return 0, &fs.PathError{Op: "read", Path: c.Name(), Err: errno}
	}
	return c.File.ReadAt(p, off)
}

func (c *failingCorrection) Stat() (fs.FileInfo, error) {
	if c.failStat {
		return nil, &fs.PathError{Op: "stat", Path: c.Name(), Err: syscall.EIO}
	}
	c.stats++
	info, err := c.File.Stat()
	if err == nil && c.replaced && c.stats > 1 {
		return grownInfo{info}, nil
	}
	return info, err
}

// grownInfo is a file's stat one byte larger.
type grownInfo struct{ fs.FileInfo }

func (g grownInfo) Size() int64 { return g.FileInfo.Size() + 1 }

// correctionFaults are the ways a look at a correction file does not
// complete, each a seam over the real file: the open fails, a read fails, a
// stat fails, or the file is replaced while it is read.
var correctionFaults = []struct {
	name string
	open func(abs string) (extractSource, error)
}{
	{"the open fails with EIO", func(abs string) (extractSource, error) {
		return nil, &fs.PathError{Op: "open", Path: abs, Err: syscall.EIO}
	}},
	{"a read fails with EIO", openFailingCorrection(func(c *failingCorrection) { c.failRead = true })},
	{"a stat fails with EIO", openFailingCorrection(func(c *failingCorrection) { c.failStat = true })},
	{"it is replaced while it is read", openFailingCorrection(func(c *failingCorrection) { c.replaced = true })},
}

func openFailingCorrection(set func(*failingCorrection)) func(abs string) (extractSource, error) {
	return func(abs string) (extractSource, error) {
		file, err := os.Open(abs)
		if err != nil {
			return nil, err
		}
		c := &failingCorrection{File: file}
		set(c)
		return c, nil
	}
}

// TestALookAtACorrectionFileThatDidNotCompleteConcludesNothing pins the
// fourth observation: when the correction file of a hybrid .wv could not be
// read whole, the .wv's extraction is a read that did not complete (backlog
// B134). A row keeps what it had, byte for byte and at its indexed_at, and a
// new file gets none, and the scan counts the file once in its line of
// unread files; the next scan that reads the correction file whole writes
// what it holds.
func TestALookAtACorrectionFileThatDidNotCompleteConcludesNothing(t *testing.T) {
	spec := hybridSpec()
	for _, fault := range correctionFaults {
		t.Run("a row/"+fault.name, func(t *testing.T) {
			f := newArtFixture(t)
			writeHybrid(t, f, hybridRel, spec)
			writeCorrection(t, f, hybridWVC, wvcFile(spec), t0.Add(time.Hour))
			f.scan(t, "index")
			requireCorrection(t, f, hybridRel, CodecWavPack, 16, SampleFormatInteger, hybridWVC)
			before, _ := storedRowAt(t, f.store, hybridRel)

			writeCorrection(t, f, hybridWVC, wvcFile(spec), t0.Add(2*time.Hour))
			f.sc.openCorrection = fault.open
			rec := loggingtest.Record(t)
			scanOnce(t, f.sc, "the fault")
			if after, _ := storedRowAt(t, f.store, hybridRel); after != before {
				t.Errorf("the row changed:\n got  %+v\n want %+v", after, before)
			}
			if lines := rec.Lines(msgUnreadAudio); len(lines) != 1 || !strings.Contains(lines[0], "count=1") {
				t.Errorf("unread lines %q, want one counting the .wv", lines)
			}

			f.sc.openCorrection = nil
			scanOnce(t, f.sc, "readable again")
			requireCorrection(t, f, hybridRel, CodecWavPack, 16, SampleFormatInteger, hybridWVC)
			f.requireSettled(t, hybridRel)
		})
		t.Run("a new file/"+fault.name, func(t *testing.T) {
			f := newArtFixture(t)
			writeHybrid(t, f, hybridRel, spec)
			writeCorrection(t, f, hybridWVC, wvcFile(spec), t0.Add(time.Hour))
			f.sc.openCorrection = fault.open
			scanOnce(t, f.sc, "the fault")
			if got, _ := f.store.GetTrack(context.Background(), hybridRel); got != nil {
				t.Errorf("the new file has a row (%s) from a look that did not complete", got.Codec)
			}
			f.sc.openCorrection = nil
			scanOnce(t, f.sc, "readable again")
			requireCorrection(t, f, hybridRel, CodecWavPack, 16, SampleFormatInteger, hybridWVC)
		})
	}
}

// TestACorrectionFileTheBridgeMayNotReadIsLossy: a correction file the bridge
// is refused leave to open or to read (on a NAS share, one of another owner,
// mode 0600) is an answer, never a look that did not complete (round 2,
// Claude's review): such a file serves no client either, so the .wv is
// indexed as WAVPACK-LOSSY with no correction on the wire, its key the state
// '?' and the file's stat. A scan that finds it still refused reads nothing
// again, and the first that may read it matches it.
func TestACorrectionFileTheBridgeMayNotReadIsLossy(t *testing.T) {
	spec := hybridSpec()
	for _, fault := range []struct {
		name string
		open func(abs string) (extractSource, error)
	}{
		{"its open refused", func(abs string) (extractSource, error) {
			return nil, &fs.PathError{Op: "open", Path: abs, Err: syscall.EACCES}
		}},
		{"its read refused", openFailingCorrection(func(c *failingCorrection) { c.failRead, c.readErr = true, syscall.EACCES })},
	} {
		t.Run(fault.name, func(t *testing.T) {
			f := newArtFixture(t)
			writeHybrid(t, f, hybridRel, spec)
			writeCorrection(t, f, hybridWVC, wvcFile(spec), t0.Add(time.Hour))
			f.sc.openCorrection = fault.open
			rec := loggingtest.Record(t)
			if n := f.scan(t, "index"); n != 1 {
				t.Fatalf("the index read %d audio files, want 1", n)
			}
			if got, _ := f.store.GetTrack(context.Background(), hybridRel); got == nil {
				t.Fatal("the .wv has no row: a correction file it may not read stopped its index")
			}
			requireCorrection(t, f, hybridRel, CodecWavPackLossy, 0, "", "")
			if got := keyState(correctionKeyOf(t, f.store, hybridRel)); got != '?' {
				t.Errorf("correction_key state %q, want '?'", got)
			}
			if lines := rec.Lines(msgUnreadAudio); len(lines) != 0 {
				t.Errorf("unread lines %q, want none: the file was read and its correction file answered", lines)
			}
			f.requireSettled(t, hybridRel)

			if n := f.scan(t, "still refused"); n != 0 {
				t.Errorf("the scan read %d audio files, want none while the correction file is still refused", n)
			}
			if lines := rec.Lines(msgCorrectionUnreadable); len(lines) != 1 {
				t.Errorf("unreadable lines %q, want one in all", lines)
			}

			f.sc.openCorrection = nil
			if n := f.scan(t, "readable"); n != 1 {
				t.Errorf("the scan read %d audio files, want the .wv once its correction file may be read", n)
			}
			requireCorrection(t, f, hybridRel, CodecWavPack, 16, SampleFormatInteger, hybridWVC)
			f.requireSettled(t, hybridRel)
		})
	}
}

// TestACorrectionFileGoneWhileItsWVCannotBeReadKeepsTheRow: the correction
// file removed while the .wv's own open fails leaves the row as it was, its
// correction file on the wire included, which a client that fetches it is
// answered 404 for (PROTOCOL.md: a refusal of the lossless decode); the scan
// that reads the .wv again writes it lossy.
func TestACorrectionFileGoneWhileItsWVCannotBeReadKeepsTheRow(t *testing.T) {
	f := newArtFixture(t)
	spec := hybridSpec()
	writeHybrid(t, f, hybridRel, spec)
	writeCorrection(t, f, hybridWVC, wvcFile(spec), t0.Add(time.Hour))
	f.scan(t, "index")
	before, _ := storedRowAt(t, f.store, hybridRel)

	if err := os.Remove(f.path(hybridWVC)); err != nil {
		t.Fatal(err)
	}
	counting := f.sc.openAudio
	f.sc.openAudio = func(abs string) (extractSource, error) {
		return nil, &fs.PathError{Op: "open", Path: abs, Err: syscall.EIO}
	}
	scanOnce(t, f.sc, "the .wv unreadable")
	if after, _ := storedRowAt(t, f.store, hybridRel); after != before {
		t.Errorf("the row changed:\n got  %+v\n want %+v", after, before)
	}
	f.sc.openAudio = counting
	if n := f.scan(t, "readable again"); n != 1 {
		t.Errorf("the scan read %d audio files, want the .wv once", n)
	}
	requireCorrection(t, f, hybridRel, CodecWavPackLossy, 0, "", "")
}

// TestTheCorrectionFileOnTheWire pins its shape: an object of path, size and
// mtime, the mtime encoded as the track's own, and no key at all on a row
// that has none.
func TestTheCorrectionFileOnTheWire(t *testing.T) {
	at := time.Date(2026, 11, 6, 10, 0, 0, 123456789, time.UTC)
	tr := Track{Path: "A/b.wv", Size: 10, ModTime: at,
		Correction: &CorrectionFile{Path: "A/b.wvc", Size: 20, ModTime: at}}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	c, ok := got["correction"].(map[string]any)
	if !ok {
		t.Fatalf("correction %#v, want an object", got["correction"])
	}
	if c["path"] != "A/b.wvc" || c["size"] != float64(20) || c["mtime"] != got["mtime"] || len(c) != 3 {
		t.Errorf("correction %v, want path, size and the track's own mtime encoding (%v)", c, got["mtime"])
	}
	tr.Correction = nil
	raw, err = json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got["correction"]; present {
		t.Error("a row with no correction file carries the key")
	}
}

// TestTheCorrectionKeyReadsBackWhatItRecords pins the key's grammar: each
// observation reads back as itself, a name holding the separator included,
// and a key this build never writes reads as one to re-read.
func TestTheCorrectionKeyReadsBackWhatItRecords(t *testing.T) {
	for _, o := range []correctionObservation{
		{},
		{state: correctionAbsent},
		{state: correctionMatched, name: "01 Song.wvc", size: 1234, mtimeNS: 1_700_000_000_123_456_789},
		{state: correctionRejected, name: "a:b:c.WVC", size: 0, mtimeNS: -5},
	} {
		got, ok := parseCorrectionKey(o.key())
		if !ok || got != o {
			t.Errorf("parseCorrectionKey(%q) = %v, %v; want %v", o.key(), got, ok, o)
		}
	}
	for _, k := range []string{"?", "+", "+12", "+1:2", "+1:2:", "!x:2:n", "+1:y:n"} {
		if _, ok := parseCorrectionKey(k); ok {
			t.Errorf("parseCorrectionKey(%q) took a key this build never writes", k)
		}
	}
}

// TestTheGateAsksNothingItCannotSee pins correctionDrifted's other answers: a
// row that depends on no correction file is never asked; a folder that cannot
// be listed answers no, so the row stays and the next scan asks again; a key
// this build never writes answers yes.
func TestTheGateAsksNothingItCannotSee(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone", "01 Song.wv")
	if correctionDrifted(missing, &TrackStat{CorrectionKey: ""}, &ExtractContext{}) {
		t.Error("a row that depends on no correction file was asked")
	}
	if correctionDrifted(missing, &TrackStat{CorrectionKey: "-"}, &ExtractContext{}) {
		t.Error("a folder that cannot be listed answered yes")
	}
	if !correctionDrifted(missing, &TrackStat{CorrectionKey: "?"}, &ExtractContext{}) {
		t.Error("a key this build never writes answered no")
	}
}

// TestTheCorrectionKeyRidesEveryTrackWrite pins the store half: both upserts
// and the version stamp write the key from the Track, and the skip gate's
// read (GetTrackStat) returns it.
func TestTheCorrectionKeyRidesEveryTrackWrite(t *testing.T) {
	ctx := context.Background()
	s := openRefusalStore(t)
	at := time.Unix(1_700_000_000, 0).UTC()
	if err := s.UpsertTrack(ctx, &Track{Path: "a/one.wv", Size: 1, ModTime: at, correctionKey: "-"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrackBatch(ctx, []*Track{{Path: "a/two.wv", Size: 1, ModTime: at, correctionKey: "+3:4:two.wvc"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.StampExtractorVersionBatch(ctx, []*Track{{Path: "a/one.wv", correctionKey: "!5:6:one.wvc"}}); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"a/one.wv": "!5:6:one.wvc", "a/two.wv": "+3:4:two.wvc"} {
		st, err := s.GetTrackStat(ctx, rel)
		if err != nil || st == nil || st.CorrectionKey != want {
			t.Errorf("%s: GetTrackStat %+v (%v), want key %q", rel, st, err, want)
		}
	}
}

// TestMigrationV61AddsTheCorrectionKeyIdempotently: the column exists after a
// fresh open, and a re-run of the migration (version rewound, column already
// there) neither fails nor touches a recorded key.
func TestMigrationV61AddsTheCorrectionKeyIdempotently(t *testing.T) {
	ctx := context.Background()
	s := openRefusalStore(t)
	exists, err := atlasColumnExists(s.db, "tracks", "correction_key")
	if err != nil || !exists {
		t.Fatalf("tracks.correction_key after a fresh open: exists %v (%v)", exists, err)
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	if err := s.UpsertTrack(ctx, &Track{Path: "a/one.wv", Size: 1, ModTime: at, correctionKey: "-"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version != 61 {
			continue
		}
		for run := 1; run <= 2; run++ {
			if err := m.post(s.db); err != nil {
				t.Fatalf("run %d of v61's post: %v", run, err)
			}
		}
	}
	if got := correctionKeyOf(t, s, "a/one.wv"); got != "-" {
		t.Errorf("correction_key = %q after a re-run, want the recorded %q", got, "-")
	}
}
