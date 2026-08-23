package librarycat

import "testing"

// TestSortKeyAndBucket — cases from AlphabetBucket's own docstring, so
// the browser's A–Z index and the phone's scrubber file every name in
// the same place.
func TestSortKeyAndBucket(t *testing.T) {
	for _, tc := range []struct{ in, key, bucket string }{
		{"The Beatles", "BEATLES", "B"},
		{"The Cars", "CARS", "C"},
		{"Theremin", "THEREMIN", "T"},
		{"a-ha", "AHA", "A"},
		{"Adele", "ADELE", "A"},
		{"Air", "AIR", "A"},
		{"M83", "M83", "M"},
		{"2Pac", "2PAC", "#"},
		{"Éric Serra", "ERICSERRA", "E"},
		{"Sigur Rós", "SIGURROS", "S"},
		{"!!!", "", "#"},
		{"", "", "#"},
		{"   ", "", "#"},
		{"东方", "东方", "#"},
	} {
		gotKey := sortKey(tc.in)
		if gotKey != tc.key {
			t.Errorf("sortKey(%q) = %q, want %q", tc.in, gotKey, tc.key)
		}
		if gotBucket := bucket(gotKey); gotBucket != tc.bucket {
			t.Errorf("bucket(sortKey(%q)) = %q, want %q", tc.in, gotBucket, tc.bucket)
		}
	}
}

// TestSortKeyDocumentedGap pins the shared miss: ø ł đ æ are atomic
// code points with no NFD decomposition, so they do NOT fold. Pinned as
// behaviour because the phone has the identical gap — closing it here
// alone would split one artist across two buckets between clients.
func TestSortKeyDocumentedGap(t *testing.T) {
	if got := sortKey("Bjørnstad"); got == "BJORNSTAD" {
		t.Error("ø unexpectedly folded — if this is now desirable, the iOS twin must " +
			"change in the same commit or the two clients bucket the artist differently")
	}
}

// TestNaturalCompare — digit runs compare as numbers. "M2" before
// "M83" is the case the Swift docstring names.
func TestNaturalCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"M2", "M83", -1},
		{"M83", "M2", 1},
		{"M83", "M83", 0},
		{"DISC2", "DISC10", -1},
		{"DISC10", "DISC9", 1},
		{"A", "B", -1},
		{"A", "AA", -1},
		{"AA", "A", 1},
		{"", "A", -1},
		{"", "", 0},
		{"TRACK007", "TRACK7", 0}, // leading zeros are not a tie-break
		{"A1B", "A1C", -1},
		// Unbounded digit runs must not overflow into a wrong order.
		{"X99999999999999999999", "X100000000000000000000", -1},
	} {
		got := naturalCompare(tc.a, tc.b)
		if (got < 0) != (tc.want < 0) || (got > 0) != (tc.want > 0) {
			t.Errorf("naturalCompare(%q, %q) = %d, want sign of %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestClassify mirrors the iOS AlbumQualityFilter test names.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codec string
		rate  int
		bits  int
		dsd   bool
		want  QualityBucket
	}{
		{"cd flac", "FLAC", 44100, 16, false, QualityCD},
		{"hires by rate", "FLAC", 96000, 16, false, QualityHiRes},
		{"hires by depth", "FLAC", 44100, 24, false, QualityHiRes},
		{"hires both", "FLAC", 192000, 24, false, QualityHiRes},
		{"lossy mp3", "MP3", 44100, 0, false, QualityLossy},
		{"lossy aac", "AAC", 44100, 0, false, QualityLossy},
		// iOS's documented intentional gap: 48/16 lossless is neither
		// CD (which is exactly 44.1) nor hi-res. Preserved.
		{"48k 16bit lossless is in no bucket", "FLAC", 48000, 16, false, QualityUnknown},
		{"unknown codec", "", 44100, 16, false, QualityUnknown},
		{"unrecognised codec", "MYSTERY", 44100, 16, false, QualityUnknown},
		{"dsd64", "DSF", 2822400, 1, true, QualityDSD64},
		{"dsd128", "DSF", 5644800, 1, true, QualityDSD128},
		{"dsd256", "DSF", 11289600, 1, true, QualityDSD256Plus},
		{"dsd no rate", "DSF", 0, 0, true, QualityDSDUnknownRate},
	} {
		if got := Classify(tc.codec, tc.rate, tc.bits, tc.dsd); got != tc.want {
			t.Errorf("%s: Classify(%q,%d,%d,%v) = %v, want %v",
				tc.name, tc.codec, tc.rate, tc.bits, tc.dsd, got, tc.want)
		}
	}
}

// TestCDQualityTreatsAbsentBitDepthAsUnknown is the ONE deliberate
// divergence from iOS, and it is measured rather than tasteful: on the
// reference library bitsPerSample is present on 70 of 15,370 rows, so
// iOS's exact `bits == 16` rule selects 23 tracks while 9,807 are
// genuinely at CD rate. The sparsity is structural — a routed row's
// tags come from DIDL and carry no bit depth — so the bucket is dead on
// the phone too.
//
// Its negative control is the iOS rule itself: tighten the predicate to
// `bits == 16` and this goes red. That is the most convincing form the
// control can take, because the "fix" someone would reach for to restore
// parity is exactly what it forbids.
func TestCDQualityTreatsAbsentBitDepthAsUnknown(t *testing.T) {
	if got := Classify("FLAC", 44100, 0, false); got != QualityCD {
		t.Errorf("44.1kHz FLAC with ABSENT bit depth = %v, want cdQuality — absent is "+
			"unknown, not disqualifying", got)
	}
	// An explicit non-16 depth still disqualifies: absent is unknown,
	// present-and-wrong is a fact.
	if got := Classify("FLAC", 44100, 24, false); got != QualityHiRes {
		t.Errorf("44.1kHz/24 = %v, want hiresPCM", got)
	}
	if got := Classify("FLAC", 44100, 8, false); got == QualityCD {
		t.Error("an explicit 8-bit depth must not classify as CD quality")
	}
}

func TestQualityMask(t *testing.T) {
	var m QualityMask
	m.add(QualityCD)
	m.add(QualityLossy)
	m.add(QualityCD)
	if !m.Has(QualityCD) || !m.Has(QualityLossy) || m.Has(QualityHiRes) {
		t.Errorf("mask membership wrong: %b", m)
	}
	got := m.Buckets()
	if len(got) != 2 || got[0] != QualityLossy || got[1] != QualityCD {
		t.Errorf("Buckets() = %v, want tier order [lossy cdQuality]", got)
	}
}

// TestAlbumQualityPrefersHigherFidelityOnTies — a 1-track FLAC album
// with a 1-track MP3 bonus must read as the lossless tier rather than
// flipping on map order.
func TestAlbumQualityPrefersHigherFidelityOnTies(t *testing.T) {
	a := build(
		Row{Path: "A/X/1.flac", Album: "X", AlbumArtist: "A", Codec: "FLAC", SampleRate: 96000, BitsPerSample: 24},
		Row{Path: "A/X/2.mp3", Album: "X", AlbumArtist: "A", Codec: "MP3", SampleRate: 44100},
	).Albums[0]
	if a.Quality != QualityHiRes {
		t.Errorf("Quality = %v, want hiresPCM on a tie", a.Quality)
	}
	if !a.QualityMask.Has(QualityLossy) {
		t.Error("the mask must still record the lossy member so the UI can badge 'mixed'")
	}
}

func TestQualityBucketStrings(t *testing.T) {
	for q, want := range map[QualityBucket]string{
		QualityUnknown: "unknown", QualityLossy: "lossy", QualityCD: "cdQuality",
		QualityHiRes: "hiresPCM", QualityDSD64: "dsd64", QualityDSD128: "dsd128",
		QualityDSD256Plus: "dsd256Plus", QualityDSDUnknownRate: "dsdUnknownRate",
	} {
		if got := q.String(); got != want {
			t.Errorf("QualityBucket(%d).String() = %q, want %q", q, got, want)
		}
	}
	if !QualityDSDUnknownRate.IsDSD() || QualityCD.IsDSD() {
		t.Error("IsDSD misclassifies")
	}
}
