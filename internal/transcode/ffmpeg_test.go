package transcode

import (
	"errors"
	"strings"
	"testing"
)

// soxThatReads builds a SoxInfo whose format list is exactly `formats`, i.e. a
// build that CAN read those and nothing else.
func soxThatReads(formats ...string) SoxInfo {
	return SoxInfo{FormatsKnown: true, HasFLAC: true, Formats: formats}
}

func TestDecodeRouteFor(t *testing.T) {
	stock := soxThatReads("flac", "wav", "aiff", "mp3", "vorbis")
	withMP4 := soxThatReads("flac", "wav", "mp4", "m4a")

	cases := []struct {
		name   string
		info   SoxInfo
		ffmpeg bool
		path   string
		want   decodeRoute
		why    string
	}{
		{"flac goes direct even with ffmpeg present", stock, true, "/l/a.flac", routeSoxDirect,
			"the fallback widens coverage; it must never change how a working source is decoded"},
		{"alac routes to ffmpeg", stock, true, "/l/a.m4a", routeFFmpegPipe, ""},
		{"alac without ffmpeg is refused", stock, false, "/l/a.m4a", routeNone,
			"an honest refusal, the pre-fix behaviour"},
		{"a sox build WITH mp4 still goes direct", withMP4, true, "/l/a.m4a", routeSoxDirect,
			"sox-direct wins whenever sox can read it"},
		{"uppercase extension routes", stock, true, "/l/A.M4A", routeFFmpegPipe, ""},
		// An extension absent from soxFormatsForExt is CanDecode's OTHER
		// documented fail-open, so it reaches sox and gets sox's own
		// diagnostic. The fallback must not quietly claim it.
		{"an unlisted extension still fails open to sox", stock, true, "/l/a.wma", routeSoxDirect,
			"CanDecode fails open for extensions its map does not cover; the fallback does not change that"},
		// FormatsKnown=false is CanDecode's documented fail-open: an
		// unparseable `sox --help` must never disable a working install.
		{"unparseable sox help fails open to direct", SoxInfo{}, false, "/l/a.m4a", routeSoxDirect,
			"fail-open is the whole point of FormatsKnown=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeRouteFor(tc.info, tc.ffmpeg, tc.path); got != tc.want {
				t.Errorf("decodeRouteFor(%q) = %v, want %v\n%s", tc.path, got, tc.want, tc.why)
			}
		})
	}
}

// TestCanDecodeViaCoversTheFallback pins that the four eligibility call sites
// — all of which funnel through CanDecodeVia — gained ALAC coverage without
// each having to learn about ffmpeg.
func TestCanDecodeViaCoversTheFallback(t *testing.T) {
	stock := func() (SoxInfo, error) { return soxThatReads("flac", "wav"), nil }

	withFFmpeg(t, true)
	if !CanDecodeVia(stock, "/l/a.m4a") {
		t.Error("with ffmpeg present, an ALAC source must be decodable — the gate would " +
			"otherwise refuse work the pipeline can now do")
	}
	withFFmpeg(t, false)
	if CanDecodeVia(stock, "/l/a.m4a") {
		t.Error("without ffmpeg an ALAC source must still be refused honestly")
	}
	// The pre-existing contract is untouched in both directions.
	for _, present := range []bool{true, false} {
		withFFmpeg(t, present)
		if !CanDecodeVia(stock, "/l/a.flac") {
			t.Errorf("ffmpeg=%v: FLAC must stay decodable", present)
		}
	}
}

// withFFmpeg forces the toolchain probe present or absent for one test.
func withFFmpeg(t *testing.T, present bool) {
	t.Helper()
	oldFF, oldFP := ffmpegLookPath, ffprobeLookPath
	t.Cleanup(func() { ffmpegLookPath, ffprobeLookPath = oldFF, oldFP })
	if present {
		ffmpegLookPath = func() (string, error) { return "/usr/bin/ffmpeg", nil }
		ffprobeLookPath = func() (string, error) { return "/usr/bin/ffprobe", nil }
		return
	}
	ffmpegLookPath = func() (string, error) { return "", errors.New("not found") }
	ffprobeLookPath = func() (string, error) { return "", errors.New("not found") }
}

// TestFFmpegAvailableNeedsBothBinaries — ffprobe is not optional: without it
// the raw pipe cannot be told the source rate, and the completeness guard has
// nothing to compare against.
func TestFFmpegAvailableNeedsBothBinaries(t *testing.T) {
	missing := func() (string, error) { return "", errors.New("nope") }
	present := func() (string, error) { return "/usr/bin/x", nil }
	old1, old2 := ffmpegLookPath, ffprobeLookPath
	t.Cleanup(func() { ffmpegLookPath, ffprobeLookPath = old1, old2 })

	for _, tc := range []struct {
		name   string
		ff, fp func() (string, error)
		want   bool
	}{
		{"both present", present, present, true},
		{"ffmpeg only", present, missing, false},
		{"ffprobe only", missing, present, false},
		{"neither", missing, missing, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ffmpegLookPath, ffprobeLookPath = tc.ff, tc.fp
			if got := FFmpegAvailable(); got != tc.want {
				t.Errorf("FFmpegAvailable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecodeLengthDisagreesIsTwoSided is the load-bearing one.
//
// A LOWER-than-actual input rate makes the output LONGER, not shorter —
// measured: a 44.1 kHz source described to sox as 22050 produced exactly 2.0x
// the duration. A lower-bound-only guard (which is the natural thing to write,
// and what internal/analyze correctly uses for its own purpose) accepts that
// and commits a half-speed variant. Both bounds are required here.
func TestDecodeLengthDisagreesIsTwoSided(t *testing.T) {
	cases := []struct {
		name             string
		source, produced float64
		want             bool
	}{
		{"exact match", 8.0, 8.0, false},
		{"complete decode, sub-tolerance jitter", 8.0, 7.99, false},
		{"half-truncated faststart m4a (measured 0.488)", 8.0, 3.901, true},
		{"just inside the lower bound", 100, 98.5, false},
		{"just outside the lower bound", 100, 97.5, true},
		// The upper half. Without it these are ACCEPTED.
		{"input rate declared too low doubles the duration", 8.0, 16.0, true},
		{"just outside the upper bound", 100, 102.5, true},
		{"just inside the upper bound", 100, 101.5, false},
		// Unknown on either side is never a rejection.
		{"unknown source duration", 0, 4.0, false},
		{"unknown produced duration", 8.0, 0, false},
		{"both unknown", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeLengthDisagrees(tc.source, tc.produced); got != tc.want {
				t.Errorf("decodeLengthDisagrees(%v, %v) = %v, want %v", tc.source, tc.produced, got, tc.want)
			}
		})
	}
}

func TestParseProbeDuration(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"8.000000", 8},
		{" 12.5 \n", 12.5},
		{"N/A", 0},
		{"", 0},
		{"0", 0},
		{"-3", 0},
		{"inf", 0},
		{"NaN", 0},
	} {
		if got := parseProbeDuration(tc.in); got != tc.want {
			t.Errorf("parseProbeDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestSoxArgsFromSharesOneChain pins that the piped route differs from the
// direct one ONLY in its input description — so a sidecar produced through
// ffmpeg is the same transform applied to the same samples.
func TestSoxArgsFromSharesOneChain(t *testing.T) {
	j := JobSpec{
		SourceAbsPath: "/lib/a.m4a", SourceLibraryRel: "a.m4a",
		TargetSampleRate: 176400, TargetBits: 24, Quality: QualityVeryHigh,
		OutputDir: t.TempDir(), Kind: JobKindUpscale,
	}
	direct, _, _, _ := j.soxArgsFrom([]string{j.SourceAbsPath}, "sox")
	piped, _, _, _ := j.soxArgsFrom(soxStdinInputArgs(sourceGeometry{SampleRate: 44100, Channels: 2}), "ffmpeg+sox")

	// Everything from the FLAC output marker onward must be identical.
	// Cutting on the first "-b" would find the raw stream's bit depth on the
	// piped side, comparing different things — the failure that caught this.
	cut := func(a []string) []string {
		for i := range a {
			if a[i] == "-t" && i+1 < len(a) && a[i+1] == "flac" {
				return a[i:]
			}
		}
		t.Fatalf("no `-t flac` in %v", a)
		return nil
	}
	d, p := cut(direct), cut(piped)
	// The tmp token differs per call, so compare with it removed.
	strip := func(a []string) string {
		s := strings.Join(a, " ")
		if i := strings.Index(s, ".flac."); i >= 0 {
			if k := strings.Index(s[i:], sidecarTmpSuffix); k >= 0 {
				s = s[:i] + s[i+k:]
			}
		}
		return s
	}
	if strip(d) != strip(p) {
		t.Errorf("the effects chain must be identical on both routes\ndirect: %s\npiped:  %s", strip(d), strip(p))
	}
	// -b <targetBits> precedes the output marker on both routes.
	for _, a := range [][]string{direct, piped} {
		joined := strings.Join(a, " ")
		if !strings.Contains(joined, "-b 24 -t flac") {
			t.Errorf("target bit depth must precede the FLAC output on both routes: %s", joined)
		}
	}
	if got := direct[1]; got != j.SourceAbsPath {
		t.Errorf("direct route input = %q, want the source path", got)
	}
	if !strings.Contains(strings.Join(piped, " "), "-t raw -e float -b 32 -L -r 44100 -c 2 -") {
		t.Errorf("piped route must describe the headerless stream: %v", piped)
	}
}

// TestSettingsRecordTheDecoderThatRan — the settings blob is the forensic
// record of what produced a sidecar, so it must name the decoder the run
// actually used. It comes back FROM RunSox rather than being rebuilt by the
// persist site, which cannot know the route.
func TestSettingsRecordTheDecoderThatRan(t *testing.T) {
	j := JobSpec{SourceAbsPath: "/lib/a.m4a", TargetSampleRate: 176400, TargetBits: 24,
		Quality: QualityVeryHigh, OutputDir: t.TempDir(), Kind: JobKindUpscale}
	_, direct, _, _ := j.soxArgsFrom([]string{j.SourceAbsPath}, routeSoxDirect.String())
	_, piped, _, _ := j.soxArgsFrom(nil, routeFFmpegPipe.String())
	if !strings.Contains(direct, `"decoder":"sox"`) {
		t.Errorf("direct settings must name sox: %s", direct)
	}
	if !strings.Contains(piped, `"decoder":"ffmpeg+sox"`) {
		t.Errorf("piped settings must name the pipe: %s", piped)
	}
}

// TestFFmpegRoutableExtIsTheMP4FamilyOnly pins the allowlist directly. It is
// deliberately NOT "route whatever sox refused": the upstream gates already
// exclude lossy and DSD, so anything else reaching a refusal is a shape
// neither decoder was chosen for, and handing it to ffmpeg turns an honest
// "sox can't read this" into a mysterious mid-job failure.
func TestFFmpegRoutableExtIsTheMP4FamilyOnly(t *testing.T) {
	want := map[string]bool{".m4a": true, ".mp4": true, ".m4b": true, ".m4p": true}
	for ext := range ffmpegRoutableExt {
		if !want[ext] {
			t.Errorf("%q routes to ffmpeg but is not in the MP4 family — widening this set "+
				"is a deliberate act, not a side effect", ext)
		}
	}
	for ext := range want {
		if !ffmpegRoutableExt[ext] {
			t.Errorf("%q must route to ffmpeg: it is where ALAC lives", ext)
		}
	}
}
