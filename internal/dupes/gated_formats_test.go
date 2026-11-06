package dupes

// Suppression and the client format gate (backlog B489). dupe_suppressed is
// one flag for every client, while a WavPack or Monkey's Audio row reaches
// only a client that declares its format, so a gated copy never wins over a
// copy every client is served, and gated copies of two formats are never
// suppressed against each other.

import (
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
)

// codecRow is flacRow with another codec.
func codecRow(path, codec string, rate, bits int, size int64) Row {
	r := flacRow(path, rate, bits, 200, size)
	r.Codec = codec
	return r
}

// dsdRow is a DSD copy: a DSF, or a WavPack DSD file.
func dsdRow(path, codec string, rate int, size int64) Row {
	r := codecRow(path, codec, rate, 1, size)
	r.IsDSD = true
	return r
}

// servedTo is what a client with view sees of a group once plan is applied:
// the members it is served, sorted.
func servedTo(view formatgate.Set, members []Row, suppressed []string) []string {
	var out []string
	for _, m := range members {
		if view.Takes(m.Path) && !slices.Contains(suppressed, m.Path) {
			out = append(out, m.Path)
		}
	}
	slices.Sort(out)
	return out
}

func TestAGatedCopyNeverWinsOverACopyEveryClientIsServed(t *testing.T) {
	cases := []struct {
		name    string
		members []Row
		want    []string // suppressed under FilterHighestQuality
	}{
		{
			// The case the rule exists for: the WavPack copy has the better
			// geometry, and without the gate's term it wins and an app that
			// declares nothing is left no copy of the track.
			name: "a hi-res WavPack beside a CD FLAC",
			members: []Row{
				codecRow("A/x.flac", "FLAC", 44100, 16, 30_000_000),
				codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
			},
			want: []string{"B/x.wv"},
		},
		{
			name: "a hi-res Monkey's Audio copy beside a CD WAV",
			members: []Row{
				codecRow("A/x.wav", "WAV", 44100, 16, 50_000_000),
				codecRow("B/x.APE", "APE", 96000, 24, 80_000_000),
			},
			want: []string{"B/x.APE"},
		},
		{
			// DSD is its own domain, and within it the same rule holds: a
			// DSF is served to every client, a WavPack DSD file is not.
			name: "a DSD128 WavPack file beside a DSD64 DSF",
			members: []Row{
				dsdRow("A/x.dsf", "DSF", 2822400, 100),
				dsdRow("B/x.wv", "WAVPACK-DSD", 5644800, 200),
			},
			want: []string{"B/x.wv"},
		},
		{
			name: "both gated formats beside a FLAC",
			members: []Row{
				codecRow("A/x.flac", "FLAC", 44100, 16, 30_000_000),
				codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
				codecRow("C/x.ape", "APE", 192000, 24, 120_000_000),
			},
			want: []string{"B/x.wv", "C/x.ape"},
		},
		{
			// No copy every client is served: one unit per gated format, so
			// a .wv and an .ape are never suppressed against each other, and
			// a client that declares one of them alone keeps its copy.
			name: "a WavPack and a Monkey's Audio copy alone",
			members: []Row{
				codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
				codecRow("C/x.ape", "APE", 44100, 16, 40_000_000),
			},
			want: nil,
		},
		{
			// Within one gated format the order is the one it always was.
			name: "two WavPack copies and a Monkey's Audio one",
			members: []Row{
				codecRow("B/x.wv", "WAVPACK", 96000, 24, 90_000_000),
				codecRow("D/x.wv", "WAVPACK", 44100, 16, 30_000_000),
				codecRow("C/x.ape", "APE", 44100, 16, 40_000_000),
			},
			want: []string{"D/x.wv"},
		},
	}
	views := []formatgate.Set{
		{}, formatgate.SetOf(formatgate.WavPack), formatgate.SetOf(formatgate.APE), formatgate.All(),
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if tier := classify(c.members); tier != TierDifferentFormat {
				t.Fatalf("fixture should be different-format, got %s", tier)
			}
			got := planFor(t, c.members, FilterHighestQuality)
			if !slices.Equal(got, c.want) {
				t.Fatalf("suppressed %v, want %v", got, c.want)
			}
			// The property itself, in every view: a client served any copy
			// of the track before the plan is served one after it.
			for _, view := range views {
				if len(servedTo(view, c.members, nil)) > 0 && len(servedTo(view, c.members, got)) == 0 {
					t.Errorf("view %q: served a copy before suppression and none after (suppressed %v)",
						view.Key(), got)
				}
			}
			// Under same-format a different-format group is served whole,
			// gated copies included, as it always was.
			if got := planFor(t, c.members, FilterSameFormat); len(got) != 0 {
				t.Errorf("same-format suppressed %v, want nothing", got)
			}
		})
	}
}

// TestAGatedCopyIsSuppressedAgainstItsOwnFormatAsAnyCopyIs: two WavPack
// copies of one geometry are a same-format group and keep one winner, the
// larger, exactly as two FLAC copies do.
func TestAGatedCopyIsSuppressedAgainstItsOwnFormatAsAnyCopyIs(t *testing.T) {
	members := []Row{
		codecRow("A/x.wv", "WAVPACK", 44100, 16, 30_000_000),
		codecRow("B/x.wv", "WAVPACK", 44100, 16, 31_000_000),
	}
	if tier := classify(members); tier != TierSameFormat {
		t.Fatalf("fixture should be same-format, got %s", tier)
	}
	for _, mode := range []FilterMode{FilterSameFormat, FilterHighestQuality} {
		if got := planFor(t, members, mode); !slices.Equal(got, []string{"A/x.wv"}) {
			t.Errorf("mode %s: suppressed %v, want the smaller copy", mode, got)
		}
	}
}

// TestALossyHybridWavPackCopyIsLossy: WAVPACK-LOSSY is in the ranking's lossy
// set (the manifest side's lockstep test holds the set to IsLossyCodec), so a
// lossless WavPack copy outranks it within the format.
func TestALossyHybridWavPackCopyIsLossy(t *testing.T) {
	lossless := codecRow("A/x.wv", "WAVPACK", 44100, 16, 10)
	lossy := codecRow("B/x.wv", "WAVPACK-LOSSY", 44100, 16, 20)
	if !outranks(lossless, lossy) || outranks(lossy, lossless) {
		t.Fatal("a lossless WavPack copy must outrank a lossy hybrid one, whatever their sizes")
	}
	// And the gate's term comes before it: an MP3 beside a lossless WavPack
	// copy is the one every client is served.
	mp3 := codecRow("C/x.mp3", "MP3", 44100, 16, 5)
	if !outranks(mp3, lossless) {
		t.Fatal("a copy every client is served must outrank a gated one, lossless or not")
	}
}
