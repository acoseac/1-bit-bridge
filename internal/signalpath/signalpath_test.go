package signalpath

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
)

// pcmOnTheGoBar is the capture pcm-44k1-16: a 16-bit 44.1 kHz FLAC that
// reaches the iFi GO bar as S16_LE, MPD set as Volumio sets it (no replay
// gain, no normalization, a hardware mixer that does not work), held by
// MPD's output thread for its one alsa output, whose block mpd.conf has.
func pcmOnTheGoBar() Facts {
	return Facts{
		State:  "play",
		Audio:  "44100:16:2",
		Source: Source{Known: true, Codec: "FLAC", SampleRate: 44100, Bits: 16},
		MPD: MPD{
			ReplayGainMode:      "off",
			ConfRead:            true,
			VolumeNormalization: "no",
			Holders:             1,
			Output:              Output{Plugin: "alsa", Configured: true, MixerType: "hardware", AllowedFormatsLive: true},
		},
		PCM: PCM{Open: true, State: "RUNNING", Format: "S16_LE", Rate: 44100, Channels: 2, Owner: "mpd", Thread: "output:alsa"},
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
// 176.4 kHz, the stream watched opening with that setting.
func dsd64AsDoP() Facts {
	f := pcmOnTheGoBar()
	f.Audio = "dsd64:2"
	f.Source = Source{Known: true, Codec: "DFF", DSD: true, SampleRate: 2822400, Bits: 1, Channels: 2}
	f.MPD.Output = Output{Plugin: "alsa", Configured: true, MixerType: "none", DoP: true, DoPSettingsAtOpen: true}
	f.PCM.Format, f.PCM.Rate = "S24_3LE", 176400
	return f
}

// liveFormats sets the holder's allowed_formats as MPD reports it live.
func liveFormats(formats string) func(*Facts) {
	return func(f *Facts) { f.MPD.Output.AllowedFormats, f.MPD.Output.AllowedFormatsLive = formats, true }
}

// wantVerdict holds v to value with exactly reasons and warnings, and to
// bit-exactness not verifiable, which every verdict carries.
func wantVerdict(t *testing.T, name string, v Verdict, value string, reasons []string, warnings ...Warning) {
	t.Helper()
	if reasons == nil {
		reasons = []string{}
	}
	if warnings == nil {
		warnings = []Warning{}
	}
	if v.Value != value || !slices.Equal(v.Reasons, Sorted(reasons)) || v.Reasons == nil ||
		!slices.Equal(v.Warnings, warnings) || v.Warnings == nil || v.BitExactness != NotVerifiable {
		t.Errorf("%s: %+v, want %s, reasons %v, warnings %v, bit-exactness %s", name, v, value, Sorted(reasons), warnings, NotVerifiable)
	}
}

// TestTheCapturedShapesHaveTheirFormatPreserved: what the Pi measured
// reaches the DAC in the file's format, and the verdict says that and no
// more: format_preserved for PCM and native DSD, format_preserved_dop for
// DoP, each with bit-exactness not verifiable.
func TestTheCapturedShapesHaveTheirFormatPreserved(t *testing.T) {
	pcm96 := pcmOnTheGoBar()
	pcm96.Audio = "96000:24:2"
	pcm96.Source = Source{Known: true, Codec: "FLAC", SampleRate: 96000, Bits: 24}
	pcm96.PCM.Format, pcm96.PCM.Rate = "S24_3LE", 96000
	for name, tc := range map[string]struct {
		f    Facts
		want string
	}{
		"44.1/16":       {pcmOnTheGoBar(), FormatPreserved},
		"96/24":         {pcm96, FormatPreserved},
		"native DSD256": {dsd256OnTheGoBar(), FormatPreserved},
		"DSD64 as DoP":  {dsd64AsDoP(), FormatPreservedDoP},
	} {
		wantVerdict(t, name, Judge(tc.f), tc.want, nil)
	}
}

// TestNoVerdictIsBitExact: whatever the facts, an MPD output's verdict is
// one of the three values, and bit-exactness is not verifiable in every
// one of them, a withheld verdict and one for a reading that is not whole
// included.
func TestNoVerdictIsBitExact(t *testing.T) {
	stopped := pcmOnTheGoBar()
	stopped.State = "stop"
	resampled := pcmOnTheGoBar()
	resampled.PCM.Rate = 48000
	for _, v := range []Verdict{
		Judge(pcmOnTheGoBar()), Judge(dsd64AsDoP()), Judge(stopped), Judge(resampled), WithheldFor(ProcTimeout),
	} {
		if !slices.Contains([]string{FormatPreserved, FormatPreservedDoP, Withheld}, v.Value) || v.BitExactness != NotVerifiable {
			t.Errorf("%+v", v)
		}
	}
}

// TestEveryWithheldReason changes one fact of a reading whose format is
// preserved and names the code, and only the codes, that change brings.
// Each is what the kernel shows against the file, or what ties the stream
// to it.
func TestEveryWithheldReason(t *testing.T) {
	type change struct {
		name  string
		base  func() Facts
		apply func(*Facts)
		want  []string
	}
	pcm, dsd, dop := pcmOnTheGoBar, dsd256OnTheGoBar, dsd64AsDoP
	cases := []change{
		{"paused", pcm, func(f *Facts) { f.State = "pause" }, []string{NotPlaying}},
		{"stopped", pcm, func(f *Facts) { f.State = "stop" }, []string{NotPlaying}},
		{"nothing holds the PCM", pcm, func(f *Facts) { f.PCM.Open = false }, []string{PCMClosed}},
		{"two substreams", pcm, func(f *Facts) { f.PCM.Ambiguous = true }, []string{PCMAmbiguous}},
		{"prepared, not running", pcm, func(f *Facts) { f.PCM.State = "PREPARED" }, []string{PCMNotRunning}},
		{"an xrun", pcm, func(f *Facts) { f.PCM.State = "XRUN" }, []string{PCMNotRunning}},
		{"an owner nobody can name", pcm, func(f *Facts) { f.PCM.Owner = "" }, []string{OwnerUnknown}},
		{"a thread nobody can name", pcm, func(f *Facts) { f.PCM.Thread = "" }, []string{OwnerUnknown}},
		{"a foreign owner", pcm, func(f *Facts) { f.PCM.Owner = "aplay" }, []string{OwnerNotMPD}},
		{"another player", pcm, func(f *Facts) { f.PCM.Owner = "squeezelite" }, []string{OwnerNotMPD}},
		{"a thread that is no enabled output's", pcm, func(f *Facts) { f.MPD.Holders = 0 }, []string{OwnerNotOutput}},
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

		// Two enabled outputs whose names are alike past the 15 bytes a
		// thread name keeps: which one holds the card is not known.
		{"two outputs whose threads are named alike", pcm, func(f *Facts) { f.MPD.Holders = 2 }, []string{MPDOutputAmbiguous}},

		{"resampled", pcm, func(f *Facts) { f.PCM.Rate = 48000 }, []string{RateMismatch}},
		{"mixed down", pcm, func(f *Facts) { f.PCM.Channels = 1 }, []string{ChannelsMismatch}},
		{"24 truncated to 16", pcm, func(f *Facts) { f.Audio, f.Source.Bits = "44100:24:2", 24 }, []string{DepthReduced}},
		{"a float stream", pcm, func(f *Facts) { f.Audio = "44100:f:2" }, []string{StreamFloat}},
		{"a float card format", pcm, func(f *Facts) { f.PCM.Format = "FLOAT_LE" }, []string{FormatMismatch}},
		{"an unsigned card format", pcm, func(f *Facts) { f.PCM.Format = "U16_LE" }, []string{FormatMismatch}},
		{"a DSD card format for PCM", pcm, func(f *Facts) { f.PCM.Format = "DSD_U32_BE" }, []string{FormatMismatch}},

		{"DSD at another rate", dsd, func(f *Facts) { f.PCM.Rate = 176400 }, []string{RateMismatch}},
		{"MPD decodes DSD128 for a DSD256 file", dsd, func(f *Facts) { f.Audio = "dsd128:2" }, []string{SourceMismatch, RateMismatch}},
		{"DSD on one channel", dsd, func(f *Facts) { f.PCM.Channels = 1 }, []string{ChannelsMismatch}},
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
		{"DoP on one channel", dop, func(f *Facts) { f.PCM.Channels = 1 }, []string{ChannelsMismatch}},
		{"DoP from one of two outputs named alike", dop, func(f *Facts) { f.MPD.Holders = 2 }, []string{MPDOutputAmbiguous}},

		// With an allowed_formats MPD runs with, MPD 0.24's ALSA output takes
		// DoP from the first entry that matches the stream, by its "=dop",
		// and not from dop: with no such entry, the PCM at the card is MPD's
		// conversion of the DSD, of DoP's shape or not.
		{"dop on, and an allowed_formats that names the stream PCM", dop, liveFormats("176400:24:2"), []string{DSDConvertedToPCM}},
		{"dop on, and the stream's entry without =dop", dop, liveFormats("dsd64:*"), []string{DSDConvertedToPCM}},
		{"dop on, and the first entry that matches without =dop", dop, liveFormats("dsd64:2 dsd64:*=dop"), []string{DSDConvertedToPCM}},
		{"dop on, and =dop only for another DSD rate", dop, liveFormats("dsd128:*=dop"), []string{DSDConvertedToPCM}},
		{"dop on, and =dop only for another channel count", dop, liveFormats("dsd64:1=dop"), []string{DSDConvertedToPCM}},
		{"dop on, and an allowed_formats this does not read", dop, liveFormats("dsd64=dop"), []string{DSDConvertedToPCM}},

		// MPD reads dop and allowed_formats when it opens the output, and an
		// outputset since changes what it reports, not the stream it opened:
		// DoP is claimed only for settings the stream was watched opening
		// with. A report of conversion withholds either way.
		{"DoP settings the stream was not watched opening with", dop, func(f *Facts) { f.MPD.Output.DoPSettingsAtOpen = false }, []string{DoPUnconfirmed}},
		{"the stream's =dop entry, not watched at the open", dop, func(f *Facts) {
			f.MPD.Output.DoP, f.MPD.Output.DoPSettingsAtOpen = false, false
			liveFormats("dsd64:*=dop")(f)
		}, []string{DoPUnconfirmed}},
		{"DoP in 16 bits, not watched at the open", dop, func(f *Facts) {
			f.MPD.Output.DoPSettingsAtOpen, f.PCM.Format = false, "S16_LE"
		}, []string{DoPUnconfirmed, DoPContainerTooNarrow}},
		{"dop off, not watched at the open", dop, func(f *Facts) {
			f.MPD.Output.DoP, f.MPD.Output.DoPSettingsAtOpen = false, false
		}, []string{DSDConvertedToPCM}},
	}
	for _, tc := range cases {
		f := tc.base()
		tc.apply(&f)
		got := Judge(f)
		if got.Value != Withheld || !slices.Equal(got.Reasons, Sorted(tc.want)) || got.BitExactness != NotVerifiable {
			t.Errorf("%s: %+v, want withheld %v", tc.name, got, Sorted(tc.want))
		}
	}
}

// TestWhatDoesNotChangeTheSamplesIsNotAReason pins what leaves the format
// preserved and adds no warning: a container wider than the file, a decoder
// that widens, the DAC's own mixer, MPD's software volume at 100, mixramp
// and crossfade off, and what the row does not say.
func TestWhatDoesNotChangeTheSamplesIsNotAReason(t *testing.T) {
	cases := map[string]func(*Facts){
		"16 bits in a 24-bit format": func(f *Facts) { f.PCM.Format = "S24_3LE" },
		"16 bits in a 32-bit format": func(f *Facts) { f.PCM.Format = "S32_LE" },
		"24-bit ALAC that FFmpeg hands on as 32": func(f *Facts) {
			f.Audio = "96000:32:2"
			f.Source = Source{Known: true, Codec: "ALAC", SampleRate: 96000, Bits: 24, Channels: 2}
			f.PCM.Format, f.PCM.Rate = "S32_LE", 96000
		},
		"24-bit ALAC, 32 from FFmpeg, packed to 24": func(f *Facts) {
			f.Audio = "96000:32:2"
			f.Source = Source{Known: true, Codec: "ALAC", SampleRate: 96000, Bits: 24, Channels: 2}
			f.PCM.Format, f.PCM.Rate = "S24_3LE", 96000
		},
		"the DAC's mixer, through MPD's hardware mixer": func(f *Facts) { f.MPD.Volume = "62" },
		"the null mixer":                     func(f *Facts) { f.MPD.Output.MixerType, f.MPD.Volume = "null", "40" },
		"no mixer":                           func(f *Facts) { f.MPD.Output.MixerType = "none" },
		"software volume at 100":             func(f *Facts) { f.MPD.Output.MixerType, f.MPD.Volume = "software", "100" },
		"software volume with no volume":     func(f *Facts) { f.MPD.Output.MixerType = "software" },
		"crossfade 0":                        func(f *Facts) { f.MPD.Crossfade = "0" },
		"a crossfade this does not read":     func(f *Facts) { f.MPD.Crossfade = "five" },
		"mixramp nan":                        func(f *Facts) { f.MPD.MixRampDelay = "nan" },
		"mixramp 0":                          func(f *Facts) { f.MPD.MixRampDelay = "0" },
		"mixramp below 0":                    func(f *Facts) { f.MPD.MixRampDelay = "-1" },
		"no replay gain mode":                func(f *Facts) { f.MPD.ReplayGainMode = "" },
		"normalization unset":                func(f *Facts) { f.MPD.VolumeNormalization = "" },
		"normalization 0":                    func(f *Facts) { f.MPD.VolumeNormalization = "0" },
		"normalization that is not boolean":  func(f *Facts) { f.MPD.VolumeNormalization = "sometimes" },
		"an empty live allowed_formats":      func(f *Facts) { f.MPD.Output.AllowedFormats = " " },
		"a row with no channels":             func(f *Facts) { f.Source.Channels = 0 },
		"a row with no rate":                 func(f *Facts) { f.Source.SampleRate = 0 },
		"WavPack":                            func(f *Facts) { f.Source.Codec = "WV" },
		"Monkey's Audio":                     func(f *Facts) { f.Source.Codec = "APE" },
		"a lower-case codec":                 func(f *Facts) { f.Source.Codec = "flac" },
		"an output of another plugin":        func(f *Facts) { f.MPD.Output.Plugin = "ao" },
		"an output mpd.conf does not define": func(f *Facts) { f.MPD.Output = Output{Plugin: "alsa"} },
		"mpd.conf unread":                    func(f *Facts) { f.MPD.ConfRead, f.MPD.Output.Configured = false, false },
		"a stage type that changes nothing":  func(f *Facts) { f.MPD.Output.SampleStage = "plug" },
		"software volume at 100 at both status reads": func(f *Facts) {
			f.MPD.Output.MixerType, f.MPD.VolumeBefore, f.MPD.Volume = "software", "100", "100"
		},
		"the DAC's mixer below 100 before the /proc read": func(f *Facts) { f.MPD.VolumeBefore = "40" },
	}
	for name, apply := range cases {
		f := pcmOnTheGoBar()
		apply(&f)
		wantVerdict(t, name, Judge(f), FormatPreserved, nil)
	}
	direct := dsd64AsDoP()
	direct.PCM.Format = "S32_LE"
	wantVerdict(t, "DoP in 32 bits", Judge(direct), FormatPreservedDoP, nil)
}

// TestASettingThatChangesTheSamplesIsAWarning: a setting the reading read
// that would change the samples is a warning beside the verdict, labelled
// with where it was read, and never a reason: the format at the DAC is
// still the file's, and the verdict never claimed the samples.
func TestASettingThatChangesTheSamplesIsAWarning(t *testing.T) {
	fromMPD := func(code string) Warning { return Warning{Code: code, Source: FromMPD} }
	fromConf := func(code string) Warning { return Warning{Code: code, Source: FromMPDConf} }
	fromALSA := func(code string) Warning { return Warning{Code: code, Source: FromALSAConf} }
	cases := []struct {
		name  string
		apply func(*Facts)
		want  []Warning
	}{
		{"replay gain track", func(f *Facts) { f.MPD.ReplayGainMode = "track" }, []Warning{fromMPD(MPDReplayGain)}},
		{"replay gain auto", func(f *Facts) { f.MPD.ReplayGainMode = "auto" }, []Warning{fromMPD(MPDReplayGain)}},
		{"crossfade", func(f *Facts) { f.MPD.Crossfade = "5" }, []Warning{fromMPD(MPDCrossfade)}},
		{"mixramp", func(f *Facts) { f.MPD.MixRampDelay = "2.5" }, []Warning{fromMPD(MPDMixRamp)}},
		{"software volume below 100", func(f *Facts) { f.MPD.Output.MixerType, f.MPD.Volume = "software", "62" }, []Warning{fromConf(MPDSoftwareVolume)}},
		{"software volume at 0", func(f *Facts) { f.MPD.Output.MixerType, f.MPD.Volume = "software", "0" }, []Warning{fromConf(MPDSoftwareVolume)}},
		// The volume is read at both status reads: the card held samples
		// attenuated at either one.
		{"software volume below 100 before the /proc read", func(f *Facts) {
			f.MPD.Output.MixerType, f.MPD.VolumeBefore, f.MPD.Volume = "software", "60", "100"
		}, []Warning{fromConf(MPDSoftwareVolume)}},
		{"software volume below 100 after it", func(f *Facts) {
			f.MPD.Output.MixerType, f.MPD.VolumeBefore, f.MPD.Volume = "software", "100", "60"
		}, []Warning{fromConf(MPDSoftwareVolume)}},
		{"normalization yes", func(f *Facts) { f.MPD.VolumeNormalization = "yes" }, []Warning{fromConf(MPDNormalization)}},
		{"normalization TRUE", func(f *Facts) { f.MPD.VolumeNormalization = "TRUE" }, []Warning{fromConf(MPDNormalization)}},
		{"audio_output_format", func(f *Facts) { f.MPD.AudioOutputFormat = "48000:24:2" }, []Warning{fromConf(MPDFormatOverride)}},
		{"the output's format", func(f *Facts) { f.MPD.Output.Format = "44100:24:2" }, []Warning{fromConf(MPDFormatOverride)}},
		{"a live allowed_formats", func(f *Facts) { f.MPD.Output.AllowedFormats = "96000:16:*" }, []Warning{fromMPD(MPDFormatOverride)}},
		{"the file's allowed_formats, with none live", func(f *Facts) {
			f.MPD.Output.AllowedFormats, f.MPD.Output.AllowedFormatsLive = "96000:16:*", false
		}, []Warning{fromConf(MPDFormatOverride)}},
		{"filters", func(f *Facts) { f.MPD.Output.Filters = "eq" }, []Warning{fromConf(MPDFilters)}},
		{"a softvol stage", func(f *Facts) { f.MPD.Output.SampleStage = "softvol" }, []Warning{fromALSA(ALSASoftvol)}},
		{"a rate stage", func(f *Facts) { f.MPD.Output.SampleStage = "rate" }, []Warning{fromALSA(ALSARate)}},
		{"a dmix stage", func(f *Facts) { f.MPD.Output.SampleStage = "dmix" }, []Warning{fromALSA(ALSADmix)}},
		{"a route stage", func(f *Facts) { f.MPD.Output.SampleStage = "route" }, []Warning{fromALSA(ALSARoute)}},
		{"a LADSPA stage", func(f *Facts) { f.MPD.Output.SampleStage = "ladspa" }, []Warning{fromALSA(ALSALadspa)}},
		{"every one at once, in their order", func(f *Facts) {
			f.MPD.ReplayGainMode, f.MPD.Crossfade, f.MPD.MixRampDelay, f.MPD.Volume = "album", "3", "1", "50"
			f.MPD.VolumeNormalization, f.MPD.AudioOutputFormat = "yes", "*:24:*"
			f.MPD.Output.MixerType, f.MPD.Output.Format, f.MPD.Output.Filters = "software", "44100:16:2", "eq"
			f.MPD.Output.AllowedFormats, f.MPD.Output.SampleStage = "dsd64:2", "softvol"
		}, []Warning{
			fromMPD(MPDReplayGain), fromMPD(MPDCrossfade), fromMPD(MPDMixRamp), fromConf(MPDSoftwareVolume),
			fromConf(MPDNormalization), fromMPD(MPDFormatOverride), fromConf(MPDFormatOverride), fromConf(MPDFilters),
			fromALSA(ALSASoftvol),
		}},
	}
	for _, tc := range cases {
		f := pcmOnTheGoBar()
		tc.apply(&f)
		wantVerdict(t, tc.name, Judge(f), FormatPreserved, nil, tc.want...)
	}
	// Beside a verdict withheld for what the kernel shows, the warnings are
	// the same.
	f := pcmOnTheGoBar()
	f.PCM.Rate = 48000
	f.MPD.ReplayGainMode = "track"
	wantVerdict(t, "a resampled stream with replay gain", Judge(f), Withheld, []string{RateMismatch}, fromMPD(MPDReplayGain))
	f = dsd64AsDoP()
	f.MPD.Output.MixerType, f.MPD.Volume = "software", "80"
	wantVerdict(t, "DoP with software volume", Judge(f), FormatPreservedDoP, nil, fromConf(MPDSoftwareVolume))
}

// TestAWarningNeedsWhatItIsReadFrom: what only mpd.conf says is no warning
// when mpd.conf was not read or has no block for the output, and an
// output's settings are none when two outputs could be the one that holds
// the card. MPD's own reports still are.
func TestAWarningNeedsWhatItIsReadFrom(t *testing.T) {
	unread := pcmOnTheGoBar()
	unread.MPD.ConfRead, unread.MPD.Output.Configured = false, false
	unread.MPD.VolumeNormalization, unread.MPD.AudioOutputFormat = "yes", "48000:24:2"
	unread.MPD.Output.MixerType, unread.MPD.Volume, unread.MPD.Output.Filters = "software", "10", "eq"
	unread.MPD.ReplayGainMode = "track"
	wantVerdict(t, "mpd.conf unread", Judge(unread), FormatPreserved, nil, Warning{Code: MPDReplayGain, Source: FromMPD})

	ambiguous := pcmOnTheGoBar()
	ambiguous.MPD.Holders = 2
	ambiguous.MPD.Output.AllowedFormats, ambiguous.MPD.Output.SampleStage = "96000:16:*", "softvol"
	ambiguous.MPD.AudioOutputFormat = "48000:24:2"
	wantVerdict(t, "two outputs that could hold the card", Judge(ambiguous), Withheld, []string{MPDOutputAmbiguous},
		Warning{Code: MPDFormatOverride, Source: FromMPDConf})
}

// TestAStreamThatIsNotMPDsIsJudgedNoFurther: once the stream is not MPD's
// running stream, the one code says so, and no warning about MPD's
// settings is given for a stream they do not shape.
func TestAStreamThatIsNotMPDsIsJudgedNoFurther(t *testing.T) {
	f := pcmOnTheGoBar()
	f.MPD.ReplayGainMode = "track"
	f.MPD.Output.MixerType, f.MPD.Volume = "software", "50"
	f.PCM.Owner = "aplay"
	wantVerdict(t, "a foreign owner", Judge(f), Withheld, []string{OwnerNotMPD})
	// An mpd whose thread is no enabled output of the MPD the bridge reads:
	// another MPD, or an output the bridge cannot see.
	f.PCM.Owner = "mpd"
	f.MPD.Holders = 0
	wantVerdict(t, "an output MPD does not report", Judge(f), Withheld, []string{OwnerNotOutput})
	f.MPD.Holders = 1
	wantVerdict(t, "MPD's own stream", Judge(f), FormatPreserved, nil,
		Warning{Code: MPDReplayGain, Source: FromMPD}, Warning{Code: MPDSoftwareVolume, Source: FromMPDConf})
}

// TestDoPIsReadFromTheAllowedFormatsMPDRunsWith: with an allowed_formats MPD
// reports, MPD 0.24's ALSA output sends DoP when the first entry that
// matches the stream carries "=dop", whatever dop says, so the PCM at the
// card is DoP's shape from a DoP stream; with an empty one, dop decides. The
// file's allowed_formats, with none live, is what the bridge found, which
// MPD may not run with: a warning, and dop decides.
func TestDoPIsReadFromTheAllowedFormatsMPDRunsWith(t *testing.T) {
	live := Warning{Code: MPDFormatOverride, Source: FromMPD}
	for name, formats := range map[string]string{
		"the stream's entry with =dop":             "dsd64:*=dop",
		"MPD's own example list":                   "96000:16:* 192000:24:* dsd64:*=dop *:dsd:*",
		"the entry by its rate in bytes":           "352800:dsd:2=dop",
		"every DSD rate":                           "*:dsd:*=dop",
		"an entry for PCM before the stream's":     "176400:24:2 dsd64:*=dop",
		"spaces around and between the entries":    "  176400:24:2  dsd64:2=dop ",
		"the stream's entry, a leading zero in it": "dsd064:*=dop",
	} {
		f := dsd64AsDoP()
		f.MPD.Output.DoP = false
		liveFormats(formats)(&f)
		wantVerdict(t, name, Judge(f), FormatPreservedDoP, nil, live)
	}
	f := dsd64AsDoP()
	f.MPD.Output.AllowedFormats = "176400:24:2"
	wantVerdict(t, "the file's allowed_formats, with none live", Judge(f), FormatPreservedDoP, nil,
		Warning{Code: MPDFormatOverride, Source: FromMPDConf})
	f = dsd64AsDoP()
	liveFormats("")(&f)
	wantVerdict(t, "an empty live allowed_formats", Judge(f), FormatPreservedDoP, nil)
	f.MPD.Output.DoP = false
	wantVerdict(t, "an empty live allowed_formats and dop off", Judge(f), Withheld, []string{DSDConvertedToPCM})
}

// TestAllowedFormatsAreReadAsMPDReadsThem holds parseAllowedFormats to MPD
// 0.24's reading of the value (src/lib/alsa/AllowedFormat.cxx, and
// ParseAudioFormat with a mask): entries split on single spaces, each a
// rate (Hz, bytes a second for DSD, dsdN for N × 44100 one-bit samples),
// a sample format and channels, "*" for any, and "=dop" only on a DSD
// entry. A value with one entry MPD refuses is refused whole, as MPD
// refuses it: dsd64:=dop has no channel count (MPD writes dsd64:*=dop).
func TestAllowedFormatsAreReadAsMPDReadsThem(t *testing.T) {
	type af = allowedFormat
	for in, want := range map[string][]af{
		"":                 {},
		"   ":              {},
		"dsd64:*=dop":      {{rate: 352800, format: "dsd", dop: true}},
		"dsd512:2":         {{rate: 2822400, format: "dsd", channels: 2}},
		"dsd32:2":          {{rate: 176400, format: "dsd", channels: 2}},
		"dsd4096:*":        {{rate: 22579200, format: "dsd"}},
		"352800:dsd:2=dop": {{rate: 352800, format: "dsd", channels: 2, dop: true}},
		"*:*:*":            {{}},
		"44100:16:2":       {{rate: 44100, format: "16", channels: 2}},
		"96000:24_3:2":     {{rate: 96000, format: "24", channels: 2}},
		"96000:024_3:2":    {{rate: 96000, format: "24", channels: 2}},
		"044100:016:02":    {{rate: 44100, format: "16", channels: 2}},
		"192000:32:8":      {{rate: 192000, format: "32", channels: 8}},
		"48000:8:1":        {{rate: 48000, format: "8", channels: 1}},
		"44100:f:*":        {{rate: 44100, format: "f"}},
		"96000:16:* 192000:24:* dsd64:*=dop *:dsd:*": {
			{rate: 96000, format: "16"}, {rate: 192000, format: "24"},
			{rate: 352800, format: "dsd", dop: true}, {format: "dsd"},
		},
	} {
		got, ok := parseAllowedFormats(in)
		if !ok || !slices.Equal(got, want) {
			t.Errorf("%q = %+v, %v; want %+v", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"dsd64:=dop", "dsd64=dop", "dsd64:", "dsd64", "dsd63:2", "dsd30:2", "dsd4098:2", "dsd:2",
		"44100:16:2=dop", "*:*:*=dop", "44100:*:*=dop", "44100:16:2=DOP",
		"0:16:2", "1073741824:16:2", "44100:20:2", "44100:16:0", "44100:16:9", "44100:16", "44100:16:2:2",
		"44100:16:2\t", "44100::2", ":16:2", "44100:float:2", "f:16:2", "44100:16:2 dsd64:=dop",
		"44100:16_3:2", "44100:dsdx:2", "**:16:2", "44100:16:**", "DSD64:2",
		strings.Repeat("0", 54) + "44100:16:2",
	} {
		if got, ok := parseAllowedFormats(in); ok {
			t.Errorf("%q read as %+v, which MPD refuses", in, got)
		}
	}
	if got, ok := parseAllowedFormats(strings.Repeat("0", 53) + "44100:16:2"); !ok || len(got) != 1 || got[0].rate != 44100 {
		t.Errorf("an entry of 63 bytes: %+v, %v", got, ok)
	}
}

// TestEveryStageTheALSAReaderNamesIsAWarning: alsa.SampleStage names a
// plugin type from a set of its own, and each is a warning here.
func TestEveryStageTheALSAReaderNamesIsAWarning(t *testing.T) {
	types := alsa.SampleStageTypes()
	if len(types) == 0 {
		t.Fatal("no stage types")
	}
	for _, typ := range types {
		f := pcmOnTheGoBar()
		f.MPD.Output.SampleStage = typ
		if v := Judge(f); len(v.Warnings) != 1 || v.Warnings[0].Source != FromALSAConf || !slices.Contains(WarningCodes(), v.Warnings[0].Code) {
			t.Errorf("%s: %+v", typ, v.Warnings)
		}
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

// TestCodesAreWords holds every code, every warning's code and source, and
// the verdict's words to the wire's rule: a lower-case word with
// underscores, never a sentence, each once.
func TestCodesAreWords(t *testing.T) {
	seen := map[string]bool{}
	all := append(append(append(Codes(), WarningCodes()...), WarningSources()...),
		FormatPreserved, FormatPreservedDoP, Withheld, NotVerifiable)
	for _, c := range all {
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
