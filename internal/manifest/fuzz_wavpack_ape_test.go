package manifest

// Fuzz coverage for the WavPack and Monkey's Audio extractors and the APEv2
// tag reader (backlog B489), and the allocation property in bytes: a length
// read out of one of these files never sizes a buffer the file cannot back.
//
// The property is measured as runtime.MemStats.TotalAlloc before and after
// one extraction or one tag read (testing.AllocsPerRun counts allocations,
// not their bytes, so it cannot show this), and held to wvAPEAllocLimit: one
// mebibyte for the few-kilobyte files the fuzzer mostly makes, and eight
// bytes for each byte of the input beyond, for the copies a read of what is
// there makes (a tag's text, a front cover). These readers need no more: the
// WavPack search reads a fixed window at a time, the APE headers are read
// into fixed structures, and an APEv2 value is read only within its cap and
// what the file holds. The same bound holds for a few-kilobyte file whose
// every length claims 0xFFFFFFFF (TestAFewKilobytesClaimingFourGibibytesAllocateUnderAMebibyte).
//
// Each target drives the real entry point through real files on disk, as
// the extractor targets beside them do (fuzz_extractors_test.go), and checks
// what an extraction that succeeds may say. Without -fuzz they run their
// seeds as ordinary tests.

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

// totalAllocated is runtime.MemStats.TotalAlloc: the bytes the heap has
// handed out since the process started.
func totalAllocated() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

// wvAPEAllocLimit is the most one extraction or tag read of an n-byte file
// may allocate (the file's docblock).
func wvAPEAllocLimit(n int) uint64 { return 1<<20 + 8*uint64(n) }

// requireWVAPEAllocation fails t when allocated exceeds what an n-byte input
// accounts for.
func requireWVAPEAllocation(t *testing.T, what string, n int, allocated uint64) {
	t.Helper()
	if limit := wvAPEAllocLimit(n); allocated > limit {
		t.Fatalf("%s of %d bytes allocated %d bytes (limit %d): a length read from the file sized a buffer it cannot back",
			what, n, allocated, limit)
	}
}

// claimAll is b with every 32-bit field at the offsets given set to
// 0xFFFFFFFF, the largest length a field can claim.
func claimAll(b []byte, offsets ...int) []byte {
	out := append([]byte(nil), b...)
	for _, off := range offsets {
		binary.LittleEndian.PutUint32(out[off:], 0xFFFFFFFF)
	}
	return out
}

// claimingTag is an APEv2 tag one item long whose footer claims a tag and an
// item count of 0xFFFFFFFF and whose item claims a value of 0xFFFFFFFF bytes.
func claimingTag() []byte {
	footer := len(claimingItemTag()) - 32
	return claimAll(claimingItemTag(), footer+12, footer+16)
}

// claimingItemTag is an APEv2 tag whose footer is true and whose one item, a
// text item, claims a value of 0xFFFFFFFF bytes: what the item's own bounds
// (the bytes before the footer, and a text value's cap) are for, where
// claimingTag is stopped by the footer's.
func claimingItemTag() []byte {
	tag := apeTag([]apeItem{apeText("Title", "claims everything")}, true)
	// The item's value length is the first field after the header.
	binary.LittleEndian.PutUint32(tag[32:], 0xFFFFFFFF)
	return tag
}

// wvClaims are WavPack files of a few kilobytes whose lengths claim
// 0xFFFFFFFF: the block's ckSize, its total and its block samples, a
// metadata sub-block's length (ID_LARGE with 0xFFFFFF words), and the APEv2
// tag after it.
func wvClaims() map[string][]byte {
	base := wvFile(stdWavPack())
	pad := bytes.Repeat([]byte{0x5A}, 4096)
	large := wvBlock{blockSamples: 4096, totalSamples: 44100, flags: 0x1800 | 1 | 9<<23, crc: 1,
		subs: [][]byte{{0x80 | 0x21, 0xFF, 0xFF, 0xFF}, bytes.Repeat([]byte{0}, 64)}}
	return map[string][]byte{
		"ckSize":         append(claimAll(base, 4), pad...),
		"total":          append(claimAll(base, 12), pad...),
		"blockSamples":   append(claimAll(base, 20), pad...),
		"a sub-block":    append(large.bytes(), pad...),
		"its tag":        append(append(base, pad...), claimingTag()...),
		"its tag's item": append(append(base, pad...), claimingItemTag()...),
	}
}

// apeClaims are Monkey's Audio files of a few kilobytes whose lengths claim
// 0xFFFFFFFF: the descriptor's own length, the header's, the seek table's,
// the WAV header's and the frames', the header's blocks per frame and total
// frames, the old layout's total frames, and the APEv2 tag after it.
func apeClaims() map[string][]byte {
	pad := bytes.Repeat([]byte{0x3C}, 4096)
	newer := apeFile(stdAPE())
	old := stdAPE()
	old.version = 3950
	old.formatFlags = fxAPEFlagSeekElems | fxAPEFlagPeakLevel
	older := apeFile(old)
	return map[string][]byte{
		"the descriptor's lengths": append(claimAll(newer, 8, 12, 16, 20, 24), pad...),
		"the header's counts":      append(claimAll(newer, 52+4, 52+8, 52+12), pad...),
		"the old header's frames":  append(claimAll(older, 24, 28, 32+4), pad...),
		"its tag":                  append(append(newer, pad...), claimingTag()...),
		"its tag's item":           append(append(newer, pad...), claimingItemTag()...),
	}
}

// TestAFewKilobytesClaimingFourGibibytesAllocateUnderAMebibyte: a WavPack or
// Monkey's Audio file of a few kilobytes whose header, sub-blocks or APEv2
// tag claim 0xFFFFFFFF allocates under a mebibyte to extract, measured in
// bytes (runtime.MemStats.TotalAlloc). The least of three extractions is
// taken, so a goroutine another test left allocating cannot fail it.
func TestAFewKilobytesClaimingFourGibibytesAllocateUnderAMebibyte(t *testing.T) {
	dir := t.TempDir()
	check := func(t *testing.T, ext string, data []byte) {
		p := filepath.Join(dir, "claims"+ext)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		least := uint64(1 << 63)
		for range 3 {
			var tr Track
			before := totalAllocated()
			_ = ExtractWithContext(p, &tr, &ExtractContext{})
			least = min(least, totalAllocated()-before)
		}
		if least >= 1<<20 {
			t.Errorf("extracting %d bytes allocated %d bytes, want under a mebibyte", len(data), least)
		}
		t.Logf("%d bytes extracted, %d allocated", len(data), least)
		if len(data) > 16<<10 {
			t.Fatalf("the fixture is %d bytes, not a few kilobytes", len(data))
		}
	}
	for name, data := range wvClaims() {
		t.Run("wv "+name, func(t *testing.T) { check(t, ".wv", data) })
	}
	for name, data := range apeClaims() {
		t.Run("ape "+name, func(t *testing.T) { check(t, ".ape", data) })
	}
	for name, tag := range map[string][]byte{"its footer": claimingTag(), "its item": claimingItemTag()} {
		t.Run("the APEv2 reader alone, "+name, func(t *testing.T) {
			data := append(bytes.Repeat([]byte{0}, 4096), tag...)
			least := uint64(1 << 63)
			for range 3 {
				before := totalAllocated()
				_, _, _ = readAPEv2(bytes.NewReader(data), int64(len(data)), "claims")
				least = min(least, totalAllocated()-before)
			}
			if least >= 1<<20 {
				t.Errorf("reading the tag allocated %d bytes, want under a mebibyte", least)
			}
			t.Logf("%d bytes read, %d allocated", len(data), least)
		})
	}
}

// FuzzExtractWavPack extracts a WavPack file and, when withWVC is set, a
// correction file beside it. An extraction that succeeds names one of
// WavPack's tokens, and says what the token allows: a lossy hybrid has no
// depth and no sample kind, a DSD stream one bit and no kind, a correction
// file only for a lossless hybrid that has one beside it.
func FuzzExtractWavPack(f *testing.F) {
	hybrid := wvSpec{rate: 44100, bytes: 2, channels: 2, hybrid: true, total: 300000, samples: 4096}
	for _, spec := range []wvSpec{
		stdWavPack(),
		{rate: 96000, bytes: 3, channels: 2, total: 96000},
		{rate: 48000, bytes: 4, channels: 2, float: true, total: 48000},
		{rate: 44100, bytes: 2, channels: 1, total: 44100},
		{rate: 48000, bytes: 3, channels: 6, total: 48000},
		{rate: 37800, bytes: 2, channels: 2, total: 37800, customRate: true},
		{rate: 44100, bytes: 1, channels: 2, dsd: true, dsdPower: 3, total: 352800 * 2},
		{rate: 48000, bytes: 1, channels: 2, dsd: true, dsdPower: 4, total: 768000},
		{rate: 44100, bytes: 2, channels: 2, total: -1},
	} {
		f.Add(wvFile(spec), []byte(nil), false)
	}
	f.Add(wvFile(hybrid), wvcFile(hybrid), true)
	f.Add(wvFile(hybrid), []byte(nil), false)
	f.Add(wvFile(hybrid), wvcFile(stdWavPack()), true)
	f.Add(wvFile(hybrid), wvcFile(hybrid)[:100], true)
	tagged := append(wvFile(stdWavPack()), apeTag([]apeItem{
		apeText("Title", "t"), apeText("Artist", "a\x00b"), apeText("Track", "3/12"),
		{key: "Cover Art (Front)", value: append([]byte("cover.jpg\x00"), 0xFF, 0xD8, 0xFF, 0xE0), flags: 2},
	}, true)...)
	f.Add(tagged, []byte(nil), false)
	f.Add(append(wvFile(stdWavPack()), id3v1Tag("t", "a")...), []byte(nil), false)
	for _, data := range wvClaims() {
		f.Add(data, []byte(nil), false)
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, wv, wvc []byte, withWVC bool) {
		if len(wv)+len(wvc) > 1<<20 {
			t.Skip()
		}
		p := filepath.Join(dir, "fuzz.wv")
		pc := filepath.Join(dir, "fuzz.wvc")
		if err := os.WriteFile(p, wv, 0o600); err != nil {
			t.Skip()
		}
		if withWVC {
			if err := os.WriteFile(pc, wvc, 0o600); err != nil {
				t.Skip()
			}
		} else if err := os.Remove(pc); err != nil && !os.IsNotExist(err) {
			t.Skip()
		}
		var tr Track
		before := totalAllocated()
		err := ExtractWithContext(p, &tr, &ExtractContext{})
		requireWVAPEAllocation(t, "extracting a WavPack file and its correction file", len(wv)+len(wvc), totalAllocated()-before)
		if err != nil {
			return
		}
		switch tr.Codec {
		case CodecWavPack:
		case CodecWavPackLossy:
			if tr.BitsPerSample != nil || tr.SampleFormat != "" {
				t.Fatalf("a lossy hybrid carries a depth or a kind: %+v", tr)
			}
		case CodecWavPackDSD:
			if tr.IsDSD == nil || !*tr.IsDSD || tr.BitsPerSample == nil || *tr.BitsPerSample != 1 || tr.SampleFormat != "" {
				t.Fatalf("a DSD stream is not one-bit DSD with no kind: %+v", tr)
			}
		default:
			t.Fatalf("a WavPack file extracted as %q", tr.Codec)
		}
		if tr.Correction != nil && (tr.Codec != CodecWavPack || !withWVC) {
			t.Fatalf("a correction file on a %s row (one beside it: %v)", tr.Codec, withWVC)
		}
		if tr.SampleFormat != "" && tr.SampleFormat != "integer" && tr.SampleFormat != "float" {
			t.Fatalf("sample kind %q", tr.SampleFormat)
		}
		if tr.Duration != nil && !plausibleDuration(*tr.Duration) {
			t.Fatalf("an implausible duration %v", *tr.Duration)
		}
	})
}

// FuzzExtractAPE extracts a Monkey's Audio file, either layout, with or
// without a tag. One that succeeds is APE with a positive rate and a depth
// it can have, and an integer kind where it has one.
func FuzzExtractAPE(f *testing.F) {
	for _, level := range []uint16{fxAPEFast, fxAPENormal, fxAPEHigh, fxAPEExtraHigh, fxAPEInsane} {
		spec := stdAPE()
		spec.compression = level
		f.Add(apeFile(spec))
		old := stdAPE()
		old.version = 3950
		old.compression = level
		old.formatFlags = fxAPEFlagSeekElems | fxAPEFlagPeakLevel
		f.Add(apeFile(old))
	}
	for _, flags := range []uint16{fxAPEFlag8Bit, fxAPEFlag24Bit, 0} {
		old := stdAPE()
		old.version = 3800
		old.formatFlags = flags
		f.Add(apeFile(old))
	}
	junk := stdAPE()
	junk.junk = append([]byte("ID3\x04\x00\x00\x00\x00\x00\x0a"), make([]byte, 10)...)
	f.Add(apeFile(junk))
	f.Add(append(apeFile(stdAPE()), apeTag([]apeItem{apeText("Album", "x"), apeText("Year", "2001")}, false)...))
	for _, data := range apeClaims() {
		f.Add(data)
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		p := filepath.Join(dir, "fuzz.ape")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Skip()
		}
		var tr Track
		before := totalAllocated()
		err := ExtractWithContext(p, &tr, &ExtractContext{})
		requireWVAPEAllocation(t, "extracting a Monkey's Audio file", len(data), totalAllocated()-before)
		if err != nil {
			return
		}
		if tr.Codec != CodecAPE || tr.SampleRate == nil || *tr.SampleRate <= 0 {
			t.Fatalf("a Monkey's Audio file extracted as %q at %v", tr.Codec, tr.SampleRate)
		}
		if tr.BitsPerSample != nil && (*tr.BitsPerSample < 1 || *tr.BitsPerSample > 32) {
			t.Fatalf("a depth of %d", *tr.BitsPerSample)
		}
		if tr.SampleFormat != "" && tr.SampleFormat != "integer" {
			t.Fatalf("sample kind %q", tr.SampleFormat)
		}
		if tr.Duration != nil && !plausibleDuration(*tr.Duration) {
			t.Fatalf("an implausible duration %v", *tr.Duration)
		}
	})
}

// FuzzAPEv2 reads an APEv2 tag at the end of arbitrary bytes. What it keeps
// is what the format allows within the reader's caps: at most apeTagMaxItems
// items, each key 2 to 255 printable ASCII characters, each value no longer
// than its cap, a cover no longer than the artwork cap, and nothing longer
// than the bytes it came from.
func FuzzAPEv2(f *testing.F) {
	items := []apeItem{
		apeText("Title", "t"), apeText("ARTIST", "a\x00b\x00"), apeText("Album Artist", "aa"),
		apeText("Track", "3/12"), apeText("Disc", "1/2"), apeText("Year", "1999"),
		apeText("MUSICBRAINZ_ALBUMID", "11111111-1111-1111-1111-111111111111"),
		apeText("REPLAYGAIN_TRACK_GAIN", "-6.5 dB"), apeText("Lyrics", "la la"),
		{key: "Cover Art (Front)", value: append([]byte("c.jpg\x00"), 0xFF, 0xD8, 0xFF), flags: 2},
		{key: "Related", value: []byte("http://example.invalid/"), flags: 4},
	}
	f.Add(apeTag(items, true))
	f.Add(apeTag(items, false))
	f.Add(append(apeTag(items, true), id3v1Tag("t", "a")...))
	f.Add(claimingTag())
	f.Add(claimingItemTag())
	v1 := apeTag(items[:2], false)
	binary.LittleEndian.PutUint32(v1[len(v1)-32+8:], 1000)
	f.Add(v1)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		before := totalAllocated()
		tag, found, err := readAPEv2(bytes.NewReader(data), int64(len(data)), "fuzz")
		requireWVAPEAllocation(t, "reading an APEv2 tag", len(data), totalAllocated()-before)
		if err != nil {
			t.Fatalf("a read of bytes in memory failed: %v", err)
		}
		if !found {
			if len(tag.items) != 0 || tag.cover != nil {
				t.Fatalf("no tag, and items %v", tag.items)
			}
			return
		}
		if len(tag.items) > apeTagMaxItems {
			t.Fatalf("%d items", len(tag.items))
		}
		total := 0
		for _, it := range tag.items {
			if len(it.key) < 2 || len(it.key) > 255 {
				t.Fatalf("a key of %d bytes", len(it.key))
			}
			for _, c := range []byte(it.key) {
				if c < 0x20 || c > 0x7e {
					t.Fatalf("a key holding %#x", c)
				}
			}
			limit := apeTextValueMax
			if apeLyricsKey(it.key) != "" {
				limit = lyrics.MaxBodyBytes * 2
			}
			for _, v := range it.values {
				total += len(v)
				if len(v) > limit {
					t.Fatalf("a value of %d bytes under %q, past its cap of %d", len(v), it.key, limit)
				}
			}
		}
		if len(tag.cover) > maxArtworkBytes+apeCoverNameMax || total+len(tag.cover) > len(data) {
			t.Fatalf("a cover of %d bytes and %d of text from %d bytes", len(tag.cover), total, len(data))
		}
	})
}
