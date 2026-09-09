package transcode

import (
	"path/filepath"
	"testing"
)

// countingLookPaths swaps both binary seams for counters and returns the
// tally. Restores on cleanup.
func countingLookPaths(t *testing.T) *int {
	t.Helper()
	n := 0
	origFF, origProbe := ffmpegLookPath, ffprobeLookPath
	t.Cleanup(func() { ffmpegLookPath, ffprobeLookPath = origFF, origProbe })
	ffmpegLookPath = func() (string, error) { n++; return filepath.FromSlash("/usr/bin/ffmpeg"), nil }
	ffprobeLookPath = func() (string, error) { n++; return filepath.FromSlash("/usr/bin/ffprobe"), nil }
	return &n
}

// TestCanDecodeViaSkipsTheProbeForSoxOnlySources pins the extension gate.
//
// FFmpegSnapshot re-checks binary presence LIVE on every call — two
// exec.LookPath PATH walks — even with a warm cache, by design, so that
// "ffmpeg vanished from PATH" is never masked. CanDecodeVia is called PER
// TRACK by four walks (the per-track enqueuer, both batch walks, the
// auto-optimize sweeper), and it consulted the snapshot for every source
// including the FLAC/WAV/MP3 majority whose decoder was never in question.
//
// Measured before the fix: 20,881 ns/op and 51 allocs for a .flac, against
// 23.71 ns/op and 0 allocs for needsDecodeRouting — ~880x, about a second of
// pure PATH-walking per 50k-track walk.
//
// Counting LookPath calls rather than timing anything: a benchmark would make
// this a flaky assertion about the host, and the property that matters is
// binary — a sox-only source must not reach the probe AT ALL.
func TestCanDecodeViaSkipsTheProbeForSoxOnlySources(t *testing.T) {
	probe := func() (SoxInfo, error) { return SoxInfo{}, nil }

	for _, ext := range []string{".flac", ".wav", ".aiff", ".mp3", ".ogg"} {
		t.Run("soxonly"+ext, func(t *testing.T) {
			resetFFmpegSnapshotForTest()
			n := countingLookPaths(t)
			_ = CanDecodeVia(probe, "/lib/Artist/Album/track"+ext)
			if *n != 0 {
				t.Errorf("%s consulted the ffmpeg probe %d times; a source whose decoder is not in question must not pay for it",
					ext, *n)
			}
		})
	}

	// The other side, and the reason the assertion above is not satisfied by
	// a CanDecodeVia that never probes at all: the two routable families MUST
	// still reach it, or a DSD source could not answer "no" on a host whose
	// ffmpeg lacks the dsd_* decoders.
	for _, ext := range []string{".m4a", ".mp4", ".dsf", ".dff"} {
		t.Run("routable"+ext, func(t *testing.T) {
			resetFFmpegSnapshotForTest()
			n := countingLookPaths(t)
			_ = CanDecodeVia(probe, "/lib/Artist/Album/track"+ext)
			if *n == 0 {
				t.Errorf("%s never consulted the ffmpeg probe; its decoder IS in question", ext)
			}
		})
	}
}

// TestCanDecodeViaGateChangesNoVerdict is the equivalence the gate rests on.
//
// The claim is that passing a zero FFmpegInfo on the gated path is inert,
// because decodeRouteFor reads `ff` only in the classDSD arm and in
// `ff.Available() && class == classMP4`. That is an argument about the code;
// this is the check. Every extension, both probe states, gated against
// ungated — if the arms ever change so that `ff` is read for a sox-only
// class, this fails rather than silently granting a route.
func TestCanDecodeViaGateChangesNoVerdict(t *testing.T) {
	sox := SoxInfo{FormatsKnown: true, Formats: []string{"flac", "wav", "aiff", "mp3"}}
	for _, ff := range []FFmpegInfo{
		{},
		{Path: "/usr/bin/ffmpeg"},
		{Path: "/usr/bin/ffmpeg", DecodersKnown: true, HasDSD: true},
		{MissingBinaries: []string{"ffprobe"}},
	} {
		for _, ext := range []string{".flac", ".wav", ".aiff", ".mp3", ".ogg", ".m4a", ".mp4", ".m4b", ".dsf", ".dff", ".xyz"} {
			p := "/lib/A/B/track" + ext
			ungated := decodeRouteFor(sox, ff, p)

			var gated FFmpegInfo
			if needsDecodeRouting(p) {
				gated = ff
			}
			if got := decodeRouteFor(sox, gated, p); got != ungated {
				t.Errorf("%s with ff=%+v: gated route %v, ungated %v — the gate is not inert for this class",
					ext, ff, got, ungated)
			}
		}
	}
}

// TestZeroFFmpegInfoGrantsNothing is the type's own docstring, asserted.
//
// MissingBinaries is nil on a zero value, so `len(...) == 0` answered TRUE for
// an FFmpegInfo nobody had probed — which is exactly the value the gate above
// passes on the sox-only path.
func TestZeroFFmpegInfoGrantsNothing(t *testing.T) {
	if (FFmpegInfo{}).Available() {
		t.Error("the zero FFmpegInfo reports Available() — its docstring says it grants nothing, and the extension gate passes exactly this value")
	}
	// The negative control: a probed value with both binaries present must
	// still answer true, or Available() would just be broken the other way.
	if !(FFmpegInfo{Path: "/usr/bin/ffmpeg"}).Available() {
		t.Error("a probed FFmpegInfo with no missing binaries reports unavailable")
	}
}
