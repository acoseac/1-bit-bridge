package manifest

import (
	"path/filepath"
	"testing"
)

// TestTheScanIndexesAWavPackAndAnAPEFile is backlog B489's first case: a
// .wv and an .ape in a library are indexed, each from its own header and its
// APEv2 tag, under its codec token. Before B489 the walk passed both over
// (neither extension was in Ext), so neither had a row.
func TestTheScanIndexesAWavPackAndAnAPEFile(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	writeWavPack(t, filepath.Join(root, "Artist", "Album", "01 First.wv"), stdWavPack(),
		[]apeItem{apeText("Title", "One"), apeText("Artist", "Artist"), apeText("Album", "Album")})
	writeAPE(t, filepath.Join(root, "Artist", "Album", "02 Second.ape"), stdAPE(),
		[]apeItem{apeText("Title", "Two"), apeText("Artist", "Artist"), apeText("Album", "Album")})
	scanOnce(t, sc, "the library")

	for _, c := range []struct {
		rel, codec, title string
		duration          float64
	}{
		{"Artist/Album/01 First.wv", "WAVPACK", "One", 1},
		{"Artist/Album/02 Second.ape", "APE", "Two", float64(9*73728+1000) / 44100},
	} {
		tr := readTrack(t, store, c.rel)
		if tr.Codec != c.codec || tr.Title != c.title {
			t.Errorf("%s: codec %q, title %q; want %q, %q", c.rel, tr.Codec, tr.Title, c.codec, c.title)
		}
		if tr.SampleRate == nil || *tr.SampleRate != 44100 || tr.BitsPerSample == nil || *tr.BitsPerSample != 16 ||
			tr.Channels == nil || *tr.Channels != 2 {
			t.Errorf("%s: rate %v, bits %v, channels %v; want 44100, 16, 2", c.rel, tr.SampleRate, tr.BitsPerSample, tr.Channels)
		}
		if tr.Duration == nil || *tr.Duration != c.duration {
			t.Errorf("%s: duration %v, want %v", c.rel, tr.Duration, c.duration)
		}
	}
}
