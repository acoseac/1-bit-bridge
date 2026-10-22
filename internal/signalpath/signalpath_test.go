package signalpath

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

// volumioChain is the chain MPD's "volumio" device takes on the captured
// Pi: volumio (empty) -> volumioOutput (plug) -> volumioHw (hw).
var volumioChain = []string{"empty", "plug", "hw"}

// pcmOnTheGoBar is the capture pcm-44k1-16: a 16-bit 44.1 kHz FLAC that
// reaches the iFi GO bar as S16_LE through Volumio's chain, MPD set as
// Volumio sets it (no replay gain, no normalization, no mixer that works).
func pcmOnTheGoBar() Facts {
	return Facts{
		State:  "play",
		Audio:  "44100:16:2",
		Source: Source{Known: true, Codec: "FLAC", SampleRate: 44100, Bits: 16},
		MPD: MPD{
			ReplayGainMode:      "off",
			ConfigRead:          true,
			VolumeNormalization: "no",
			Outputs:             1,
			Output:              Output{MixerType: "hardware", Stages: volumioChain},
		},
		PCM: PCM{Open: true, State: "RUNNING", Format: "S16_LE", Rate: 44100, Channels: 2, Owner: "mpd"},
	}
}

// dsd256OnTheGoBar is the capture dsd256-native: DSD_U32_BE at 352800
// frames a second, which is DSD256, and MPD's audio: dsd256:2.
func dsd256OnTheGoBar() Facts {
	f := pcmOnTheGoBar()
	f.Audio = "dsd256:2"
	f.Source = Source{Known: true, Codec: "DSF", DSD: true, SampleRate: 11289600, Bits: 1, Channels: 2}
	f.PCM.Format, f.PCM.Rate = "DSD_U32_BE", 352800
	return f
}

// dsd64AsDoP is a DSD64 file an output with dop on sends as 24-bit PCM at
// 176.4 kHz, straight to hw.
func dsd64AsDoP() Facts {
	f := pcmOnTheGoBar()
	f.Audio = "dsd64:2"
	f.Source = Source{Known: true, Codec: "DFF", DSD: true, SampleRate: 2822400, Bits: 1, Channels: 2}
	f.MPD.Output = Output{MixerType: "none", DoP: true, Stages: []string{"hw"}}
	f.PCM.Format, f.PCM.Rate = "S24_3LE", 176400
	return f
}

func TestTheCapturedShapesAreBitExact(t *testing.T) {
	pcm96 := pcmOnTheGoBar()
	pcm96.Audio = "96000:24:2"
	pcm96.Source = Source{Known: true, Codec: "FLAC", SampleRate: 96000, Bits: 24}
	pcm96.PCM.Format, pcm96.PCM.Rate = "S24_3LE", 96000
	cases := map[string]struct {
		f    Facts
		want string
	}{
		"44.1/16 through Volumio's plug": {pcmOnTheGoBar(), BitExact},
		"96/24 through Volumio's plug":   {pcm96, BitExact},
		"native DSD256":                  {dsd256OnTheGoBar(), BitExact},
		"DSD64 as DoP":                   {dsd64AsDoP(), BitExactDoP},
	}
	for name, tc := range cases {
		got := Judge(tc.f)
		if got.Value != tc.want || len(got.Reasons) != 0 || got.Reasons == nil {
			t.Errorf("%s: %+v, want %s with reasons []", name, got, tc.want)
		}
	}
}

// TestEveryWithheldReason changes one fact of a bit-exact reading at a
// time and names the code, and only the codes, that change brings.
func TestEveryWithheldReason(t *testing.T) {
	type change struct {
		name  string
		base  func() Facts
		apply func(*Facts)
		want  []string
	}
	pcm, dsd, dop := pcmOnTheGoBar, dsd256OnTheGoBar, dsd64AsDoP
	direct := func(f *Facts) { f.MPD.Output.Stages = []string{"hw"} }
	cases := []change{
		{"paused", pcm, func(f *Facts) { f.State = "pause" }, []string{NotPlaying}},
		{"stopped", pcm, func(f *Facts) { f.State = "stop" }, []string{NotPlaying}},
		{"nothing holds the PCM", pcm, func(f *Facts) { f.PCM.Open = false }, []string{PCMClosed}},
		{"two substreams", pcm, func(f *Facts) { f.PCM.Ambiguous = true }, []string{PCMAmbiguous}},
		{"prepared, not running", pcm, func(f *Facts) { f.PCM.State = "PREPARED" }, []string{PCMNotRunning}},
		{"an xrun", pcm, func(f *Facts) { f.PCM.State = "XRUN" }, []string{PCMNotRunning}},
		{"an owner nobody can name", pcm, func(f *Facts) { f.PCM.Owner = "" }, []string{OwnerUnknown}},
		{"a foreign owner", pcm, func(f *Facts) { f.PCM.Owner = "aplay" }, []string{OwnerNotMPD}},
		{"another player", pcm, func(f *Facts) { f.PCM.Owner = "squeezelite" }, []string{OwnerNotMPD}},
		{"no format", pcm, func(f *Facts) { f.PCM.Format = "" }, []string{HWParamsUnknown}},
		{"no rate", pcm, func(f *Facts) { f.PCM.Rate = 0 }, []string{HWParamsUnknown}},
		{"no channels", pcm, func(f *Facts) { f.PCM.Channels = 0 }, []string{HWParamsUnknown}},

		{"no audio line", pcm, func(f *Facts) { f.Audio = "" }, []string{MPDAudioUnknown}},
		{"an audio line this does not read", pcm, func(f *Facts) { f.Audio = "44100:*:2" }, []string{MPDAudioUnknown}},

		{"not one of the bridge's rows", pcm, func(f *Facts) { f.Source = Source{} }, []string{SourceUnknown}},
		{"a row whose file changed", pcm, func(f *Facts) { f.Source.Known, f.Source.Changed = false, true }, []string{SourceChanged}},
		{"AAC", pcm, func(f *Facts) { f.Source.Codec, f.Source.Lossy = "AAC", true }, []string{SourceLossy}},
		{"MP3", pcm, func(f *Facts) { f.Source.Codec, f.Source.Lossy = "MP3", true }, []string{SourceLossy}},
		{"no codec", pcm, func(f *Facts) { f.Source.Codec = "" }, []string{SourceCodecUnknown}},
		{"an AIFF-C of unknown compression", pcm, func(f *Facts) { f.Source.Codec = "AIFC" }, []string{SourceCodecUnknown}},
		{"the file is 48 kHz", pcm, func(f *Facts) { f.Source.SampleRate = 48000 }, []string{SourceMismatch}},
		// S16_LE also holds fewer bits than the file.
		{"the file is 24-bit and MPD decodes 16", pcm, func(f *Facts) { f.Source.Bits = 24 }, []string{SourceMismatch, DepthReduced}},
		{"the file is mono", pcm, func(f *Facts) { f.Source.Channels = 1 }, []string{SourceMismatch}},
		{"a DSD row and a PCM stream", pcm, func(f *Facts) { f.Source.DSD = true }, []string{SourceMismatch}},

		{"replay gain track", pcm, func(f *Facts) { f.MPD.ReplayGainMode = "track" }, []string{MPDReplayGain}},
		{"replay gain auto", pcm, func(f *Facts) { f.MPD.ReplayGainMode = "auto" }, []string{MPDReplayGain}},
		{"no replay gain mode", pcm, func(f *Facts) { f.MPD.ReplayGainMode = "" }, []string{MPDReplayGain}},
		{"crossfade", pcm, func(f *Facts) { f.MPD.Crossfade = "5" }, []string{MPDCrossfade}},
		{"a crossfade this does not read", pcm, func(f *Facts) { f.MPD.Crossfade = "five" }, []string{MPDCrossfade}},
		{"mixramp", pcm, func(f *Facts) { f.MPD.MixRampDelay = "2.5" }, []string{MPDMixRamp}},
		{"a mixrampdelay this does not read", pcm, func(f *Facts) { f.MPD.MixRampDelay = "soon" }, []string{MPDMixRamp}},
		{"mpd.conf unread", pcm, func(f *Facts) { f.MPD.ConfigRead = false }, []string{MPDConfigUnread}},
		{"normalization yes", pcm, func(f *Facts) { f.MPD.VolumeNormalization = "yes" }, []string{MPDNormalization}},
		{"normalization true", pcm, func(f *Facts) { f.MPD.VolumeNormalization = "TRUE" }, []string{MPDNormalization}},
		{"normalization that is not a boolean", pcm, func(f *Facts) { f.MPD.VolumeNormalization = "sometimes" }, []string{MPDNormalization}},
		{"audio_output_format", pcm, func(f *Facts) { f.MPD.AudioOutputFormat = "48000:24:2" }, []string{MPDFormatOverride}},
		{"the output's format", pcm, func(f *Facts) { f.MPD.Output.Format = "44100:24:2" }, []string{MPDFormatOverride}},
		{"allowed_formats", pcm, func(f *Facts) { f.MPD.Output.AllowedFormats = "96000:16:*" }, []string{MPDFormatOverride}},
		{"software volume", pcm, func(f *Facts) { f.MPD.Output.MixerType = "software" }, []string{MPDSoftwareVolume}},
		{"a mixer MPD does not name", pcm, func(f *Facts) { f.MPD.Output.MixerType = "" }, []string{MPDSoftwareVolume}},
		{"filters", pcm, func(f *Facts) { f.MPD.Output.Filters = "eq" }, []string{MPDFilters}},
		{"no output reaches the card", pcm, func(f *Facts) { f.MPD.Outputs = 0 }, []string{ALSAChainUnproven}},
		{"two outputs reach the card", pcm, func(f *Facts) { f.MPD.Outputs = 2 }, []string{MPDOutputAmbiguous}},

		{"the plug pads 16 to 24", pcm, func(f *Facts) { f.PCM.Format = "S24_3LE" }, []string{ALSAPlugConverts}},
		{"the plug resamples", pcm, func(f *Facts) { f.PCM.Rate = 48000 }, []string{RateMismatch, ALSAPlugConverts}},
		{"the plug mixes down", pcm, func(f *Facts) { f.PCM.Channels = 1 }, []string{ChannelsMismatch, ALSAPlugConverts}},
		{"hw at another rate", pcm, func(f *Facts) { direct(f); f.PCM.Rate = 48000 }, []string{RateMismatch}},
		{"hw truncates 24 to 16", pcm, func(f *Facts) {
			direct(f)
			f.Audio, f.Source.Bits = "44100:24:2", 24
		}, []string{DepthReduced}},
		{"a float stream", pcm, func(f *Facts) { f.Audio = "44100:f:2" }, []string{StreamFloat}},
		{"a float card format", pcm, func(f *Facts) { direct(f); f.PCM.Format = "FLOAT_LE" }, []string{FormatMismatch}},
		{"an unsigned card format", pcm, func(f *Facts) { direct(f); f.PCM.Format = "U16_LE" }, []string{FormatMismatch}},
		{"a DSD card format for PCM", pcm, func(f *Facts) { direct(f); f.PCM.Format = "DSD_U32_BE" }, []string{FormatMismatch}},

		{"DSD at another rate", dsd, func(f *Facts) { f.PCM.Rate = 176400 }, []string{RateMismatch, ALSAPlugConverts}},
		{"MPD decodes DSD128 for a DSD256 file", dsd, func(f *Facts) { f.Audio = "dsd128:2" }, []string{SourceMismatch, RateMismatch, ALSAPlugConverts}},
		{"DSD on one channel", dsd, func(f *Facts) { direct(f); f.PCM.Channels = 1 }, []string{ChannelsMismatch}},
		{"a DSD width nobody defines", dsd, func(f *Facts) { f.PCM.Format = "DSD_U64_BE" }, []string{FormatMismatch}},
		{"DSD made PCM", dsd, func(f *Facts) { f.PCM.Format, f.PCM.Rate = "S32_LE", 352800 }, []string{DSDConvertedToPCM}},
		{"DSD to a float card", dsd, func(f *Facts) { f.PCM.Format = "FLOAT_LE" }, []string{FormatMismatch}},
		{"a PCM row and a DSD stream", dsd, func(f *Facts) { f.Source.DSD = false }, []string{SourceMismatch}},

		{"DoP off", dop, func(f *Facts) { f.MPD.Output.DoP = false }, []string{DSDConvertedToPCM}},
		{"DoP in 16 bits", dop, func(f *Facts) { f.PCM.Format = "S16_LE" }, []string{DoPContainerTooNarrow}},
		{"DoP at the wrong rate", dop, func(f *Facts) { f.PCM.Rate = 88200 }, []string{RateMismatch}},
		// A rate whose sixteenfold wraps to the DSD rate in an int: 2^60 +
		// 176400 on a 64-bit int, 2^28 + 176400 on a 32-bit one.
		{"DoP at a rate whose sixteenfold wraps to the DSD rate", dop, func(f *Facts) { f.PCM.Rate = math.MaxInt>>3 + 1 + 176400 }, []string{RateMismatch}},
		{"DoP for a DSD rate that is not a multiple of 16", dop, func(f *Facts) { f.Audio = "352801:dsd:2"; f.Source.SampleRate = 2822408 }, []string{RateMismatch}},
		{"DoP unsigned", dop, func(f *Facts) { f.PCM.Format = "U24_3LE" }, []string{FormatMismatch}},
		{"DoP through a plug into 32 bits", dop, func(f *Facts) {
			f.MPD.Output.Stages = volumioChain
			f.PCM.Format = "S32_LE"
		}, []string{ALSAPlugConverts}},
		{"DoP with software volume", dop, func(f *Facts) { f.MPD.Output.MixerType = "software" }, []string{MPDSoftwareVolume}},
	}
	for _, tc := range cases {
		f := tc.base()
		tc.apply(&f)
		got := Judge(f)
		if got.Value != Withheld || !slices.Equal(got.Reasons, Sorted(tc.want)) {
			t.Errorf("%s: %+v, want withheld %v", tc.name, got, Sorted(tc.want))
		}
	}
}

// TestWhatDoesNotChangeTheSamplesIsNotAReason pins the settings the
// decision leaves the verdict alone for: the DAC's own mixer, a padded
// container straight to hw, a decoder that widens, mixramp off, and
// crossfade 0.
func TestWhatDoesNotChangeTheSamplesIsNotAReason(t *testing.T) {
	cases := map[string]func(*Facts){
		"16 bits padded to 24 straight to hw": func(f *Facts) {
			f.MPD.Output.Stages = []string{"hw"}
			f.PCM.Format = "S24_3LE"
		},
		"16 bits padded to 32 straight to hw": func(f *Facts) {
			f.MPD.Output.Stages = []string{"hw"}
			f.PCM.Format = "S32_LE"
		},
		"24-bit ALAC that FFmpeg hands on as 32": func(f *Facts) {
			f.Audio = "96000:32:2"
			f.Source = Source{Known: true, Codec: "ALAC", SampleRate: 96000, Bits: 24, Channels: 2}
			f.PCM.Format, f.PCM.Rate = "S32_LE", 96000
		},
		"24-bit ALAC, 32 from FFmpeg, packed to 24 by MPD itself": func(f *Facts) {
			f.MPD.Output.Stages = []string{"hw"}
			f.Audio = "96000:32:2"
			f.Source = Source{Known: true, Codec: "ALAC", SampleRate: 96000, Bits: 24, Channels: 2}
			f.PCM.Format, f.PCM.Rate = "S24_3LE", 96000
		},
		"the DAC's mixer, through MPD's hardware mixer": func(f *Facts) { f.MPD.Output.MixerType = "hardware" },
		"the null mixer":                func(f *Facts) { f.MPD.Output.MixerType = "null" },
		"no mixer":                      func(f *Facts) { f.MPD.Output.MixerType = "none" },
		"crossfade 0":                   func(f *Facts) { f.MPD.Crossfade = "0" },
		"mixramp nan":                   func(f *Facts) { f.MPD.MixRampDelay = "nan" },
		"mixramp 0":                     func(f *Facts) { f.MPD.MixRampDelay = "0" },
		"mixramp below 0":               func(f *Facts) { f.MPD.MixRampDelay = "-1" },
		"normalization unset":           func(f *Facts) { f.MPD.VolumeNormalization = "" },
		"normalization 0":               func(f *Facts) { f.MPD.VolumeNormalization = "0" },
		"a row with no channels":        func(f *Facts) { f.Source.Channels = 0 },
		"a row with no rate":            func(f *Facts) { f.Source.SampleRate = 0 },
		"WavPack":                       func(f *Facts) { f.Source.Codec = "WV" },
		"Monkey's Audio":                func(f *Facts) { f.Source.Codec = "APE" },
		"a lower-case codec":            func(f *Facts) { f.Source.Codec = "flac" },
		"hw straight, no plug in front": func(f *Facts) { f.MPD.Output.Stages = []string{"hw"} },
	}
	for name, apply := range cases {
		f := pcmOnTheGoBar()
		apply(&f)
		if got := Judge(f); got.Value != BitExact {
			t.Errorf("%s: %+v, want bit_exact", name, got)
		}
	}
	direct := dsd64AsDoP()
	direct.PCM.Format = "S32_LE"
	if got := Judge(direct); got.Value != BitExactDoP {
		t.Errorf("DoP in 32 bits straight to hw: %+v", got)
	}
}

// TestAStreamThatIsNotMPDsIsJudgedNoFurther: once the stream is not MPD's
// running stream, the one code says so and nothing about MPD's settings
// is claimed for a stream they do not shape.
func TestAStreamThatIsNotMPDsIsJudgedNoFurther(t *testing.T) {
	f := pcmOnTheGoBar()
	f.MPD.ReplayGainMode = "track"
	f.MPD.Output.MixerType = "software"
	f.PCM.Owner = "aplay"
	if got := Judge(f); !slices.Equal(got.Reasons, []string{OwnerNotMPD}) {
		t.Fatalf("%+v", got)
	}
	f.PCM.Owner = "mpd"
	got := Judge(f)
	if !slices.Equal(got.Reasons, []string{MPDReplayGain, MPDSoftwareVolume}) {
		t.Fatalf("every reason of MPD's own stream: %+v", got)
	}
}

func TestParseAudio(t *testing.T) {
	cases := []struct {
		in   string
		want Audio
		ok   bool
	}{
		{"44100:16:2", Audio{Rate: 44100, Bits: 16, Channels: 2}, true},
		{"96000:24:2", Audio{Rate: 96000, Bits: 24, Channels: 2}, true},
		{"384000:32:2", Audio{Rate: 384000, Bits: 32, Channels: 2}, true},
		{"8000:8:1", Audio{Rate: 8000, Bits: 8, Channels: 1}, true},
		{"44100:f:2", Audio{Float: true, Rate: 44100, Channels: 2}, true},
		{"dsd64:2", Audio{DSD: true, Rate: 2822400, Channels: 2}, true},
		{"dsd256:2", Audio{DSD: true, Rate: 11289600, Channels: 2}, true},
		{"dsd512:2", Audio{DSD: true, Rate: 22579200, Channels: 2}, true},
		{"384000:dsd:2", Audio{DSD: true, Rate: 3072000, Channels: 2}, true},
		{" 44100:16:2 ", Audio{Rate: 44100, Bits: 16, Channels: 2}, true},
		{"44100:20:2", Audio{}, false},
		{"44100:*:2", Audio{}, false},
		{"0:16:2", Audio{}, false},
		{"44100:16:0", Audio{}, false},
		{"044100:16:2", Audio{}, false},
		{"dsd:2", Audio{}, false},
		{"dsd64", Audio{}, false},
		{"99999999999:16:2", Audio{}, false},
		{"dsd99999999:2", Audio{}, false},
		{"", Audio{}, false},
	}
	for _, tc := range cases {
		got, ok := ParseAudio(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEffectiveMixerTypeFollowsMPD(t *testing.T) {
	cases := []struct {
		output, enabled, global, want string
	}{
		{"", "", "", "hardware"},
		{"software", "", "", "software"},
		{"hardware", "no", "software", "hardware"},
		{"", "no", "software", "none"},
		{"", "false", "", "none"},
		{"", "yes", "software", "software"},
		{"", "", "null", "null"},
		{"disabled", "", "", "none"},
		{"none", "", "", "none"},
		{"Software", "", "", ""},
		{"", "maybe", "", ""},
		{"", "", "loud", ""},
	}
	for _, tc := range cases {
		if got := EffectiveMixerType(tc.output, tc.enabled, tc.global); got != tc.want {
			t.Errorf("(%q, %q, %q) = %q, want %q", tc.output, tc.enabled, tc.global, got, tc.want)
		}
	}
}

func TestSettling(t *testing.T) {
	play := func(song, elapsed string) Status { return Status{State: "play", SongID: song, Elapsed: elapsed} }
	cases := []struct {
		name          string
		before, after Status
		between       time.Duration
		want          []string
	}{
		{"steady", play("12", "4.997"), play("12", "5.003"), 6 * time.Millisecond, nil},
		{"steady across a slow read", play("12", "4.997"), play("12", "5.390"), 400 * time.Millisecond, nil},
		{"the song changed", play("12", "431.0"), play("13", "0.020"), 5 * time.Millisecond, []string{SongChanged}},
		{"paused between", play("12", "8.0"), Status{State: "pause", SongID: "12", Elapsed: "8.0"}, time.Millisecond, []string{StateChanged}},
		{"the song is starting", play("12", "0.2"), play("12", "0.21"), 10 * time.Millisecond, []string{SongStarting}},
		{"a seek forward", play("12", "10.0"), play("12", "70.0"), 5 * time.Millisecond, []string{Seeked}},
		{"a seek back", play("12", "70.0"), play("12", "10.0"), 5 * time.Millisecond, []string{Seeked}},
		{"a seek back to the start", play("12", "70.0"), play("12", "0.1"), 5 * time.Millisecond, []string{Seeked, SongStarting}},
		// Inside the half second either way, but backwards: MPD's elapsed
		// never goes back while it plays on.
		{"a small seek back", play("12", "4.8"), play("12", "4.7"), 5 * time.Millisecond, []string{Seeked}},
		{"elapsed that stands still", play("12", "4.8"), play("12", "4.8"), time.Millisecond, nil},
		// A song identity neither read gives cannot be shown to be one song.
		{"no song id while playing", Status{State: "play", Elapsed: "5.000"}, Status{State: "play", Elapsed: "5.006"}, 6 * time.Millisecond, []string{SongChanged}},
		{"a song id that does not read", play("x", "5.000"), play("x", "5.006"), 6 * time.Millisecond, []string{SongChanged}},
		{"no song id in the second read", play("12", "5.000"), Status{State: "play", Elapsed: "5.006"}, 6 * time.Millisecond, []string{SongChanged}},
		{"a pause and resume between", play("12", "10.0"), play("12", "10.0"), 900 * time.Millisecond, []string{Seeked}},
		{"no elapsed while playing", play("12", ""), play("12", "5"), time.Millisecond, []string{SongStarting}},
		{"an elapsed that does not read", play("12", "5"), play("12", "x"), time.Millisecond, []string{SongStarting}},
		{"stopped", Status{State: "stop"}, Status{State: "stop"}, time.Millisecond, nil},
		{"paused, same song", Status{State: "pause", SongID: "3", Elapsed: "1"}, Status{State: "pause", SongID: "3", Elapsed: "1"}, time.Second, nil},
	}
	for _, tc := range cases {
		got := Settling(tc.before, tc.after, tc.between)
		if !slices.Equal(got, Sorted(tc.want)) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCodesAreWords holds every code to the wire's rule: a lower-case
// word with underscores, never a sentence, each once.
func TestCodesAreWords(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Codes() {
		if seen[c] {
			t.Errorf("%s twice", c)
		}
		seen[c] = true
		if c == "" || strings.Trim(c, "abcdefghijklmnopqrstuvwxyz_") != "" || strings.HasPrefix(c, "_") || strings.HasSuffix(c, "_") {
			t.Errorf("%q is not a code", c)
		}
	}
	if got := Sorted([]string{Seeked, NotPlaying, Seeked, "zzz", MPDUnreachable}); !slices.Equal(got, []string{MPDUnreachable, Seeked, NotPlaying, "zzz"}) {
		t.Errorf("sorted %v", got)
	}
	if got := Sorted(nil); got == nil || len(got) != 0 {
		t.Errorf("sorted nil = %#v", got)
	}
}
