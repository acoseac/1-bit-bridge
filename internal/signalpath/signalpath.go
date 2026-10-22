// Package signalpath judges what the box's DAC receives against the file
// the MPD beside the bridge is on: the verdict GET /v1/outputs/{id}/reading
// carries (B473 B3, and the owner's two levels of 2026-10-22). It reads
// nothing. The route gathers the facts (MPD's status around the /proc read,
// the substream's hw_params and owner, the file's own row, and what MPD
// reports and mpd.conf and the ALSA files say of its settings), and Judge
// decides from them alone, so every case is a row of a table.
//
// The verdict claims only what the kernel shows: the format the DAC
// receives, against the file's. It is format_preserved (PCM at the file's
// rate and channels in a format that holds every bit of its depth, or native
// DSD at the file's rate), format_preserved_dop (PCM at the rate and in the
// format DoP of the file's DSD rate takes, which is consistent with DoP and
// never a claim that the DAC locked onto it or decoded it), or withheld,
// with a code for every condition that does not hold. It never says
// bit-exact. From outside a player the bridge does not run it cannot show
// which configuration MPD runs with or what MPD does to the samples, so
// every verdict carries that bit-exactness is not verifiable for this
// player; bit-exact is the bridge's own player's claim (B470), which runs
// the whole path. A setting that would change the samples is a warning
// beside the verdict, labelled with where it was read: information, never
// a reason, and no warning is a proof that the samples are unchanged.
//
// The DAC receives; it never "plays", and nothing is said about what
// happens inside it. A code is a word the app maps to its own copy, never a
// sentence.
package signalpath

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
)

// The verdict's values.
const (
	FormatPreserved    = "format_preserved"
	FormatPreservedDoP = "format_preserved_dop"
	Withheld           = "withheld"
)

// NotVerifiable is every verdict's bit-exactness: the kernel shows the
// format a stream reaches the DAC in, never its samples, and what an MPD the
// bridge does not run does to them cannot be verified from outside it.
const NotVerifiable = "not_verifiable"

// Codes a reading's state carries: why it is unknown or settling. A
// verdict withheld for such a reading carries the same codes.
const (
	// MPDUnreachable: no session to the MPD the bridge reads (no endpoint,
	// a refused dial, a connection that closed or answered garbage).
	MPDUnreachable = "mpd_unreachable"
	// MPDTimeout: the dial or an MPD command ran past its deadline.
	MPDTimeout = "mpd_timeout"
	// MPDRefused: MPD answered a read command with ACK (a password it
	// requires, for one).
	MPDRefused = "mpd_refused"
	// ProcUnreadable: /proc/asound could not be read.
	ProcUnreadable = "proc_unreadable"
	// ProcTimeout: the /proc read ran past its deadline.
	ProcTimeout = "proc_timeout"
	// ALSAConfigUnread: the ALSA configuration, which says which card an
	// output's PCM names, was not had: its read ran past its deadline, one
	// from an earlier reading still ran, or it failed.
	ALSAConfigUnread = "alsa_config_unread"
	// NoCard: this output names no sound card, so nothing says what a DAC
	// receives.
	NoCard = "no_card"
	// ReadingFailed: the bridge could not take the reading (a step of its
	// own failed); a later reading is taken afresh.
	ReadingFailed = "reading_failed"
	// StateChanged: MPD's state differed between the status read before
	// the /proc read and the one after it.
	StateChanged = "state_changed"
	// SongChanged: MPD's songid differed between the two reads, currentsong
	// named another song, or one of them named no song id while MPD plays.
	SongChanged = "song_changed"
	// Seeked: elapsed went back, by however little, or moved forward by
	// more than the time between the two reads allows.
	Seeked = "seeked"
	// SongStarting: elapsed was under half a second, or not given while
	// playing.
	SongStarting = "song_starting"
	// SettingsChanged: what MPD reports of its outputs and settings (its
	// outputs and their attributes, replay gain, the audio format,
	// crossfade, MixRamp) differed between the reads before the /proc read
	// and after it.
	SettingsChanged = "settings_changed"
)

// Codes a withheld verdict carries for a whole reading.
const (
	// NotPlaying: MPD is paused or stopped.
	NotPlaying = "not_playing"
	// PCMAmbiguous: more than one substream of the card's PCM device is
	// open and not exactly one of them is MPD's.
	PCMAmbiguous = "pcm_ambiguous"
	// PCMClosed: MPD plays and nothing holds the card's PCM device open.
	PCMClosed = "pcm_closed"
	// PCMNotRunning: the substream is open and not RUNNING (prepared,
	// paused, an xrun, draining).
	PCMNotRunning = "pcm_not_running"
	// OwnerUnknown: the thread that holds the substream, or its process,
	// could not be named (a /proc mounted with hidepid, another pid
	// namespace).
	OwnerUnknown = "owner_unknown"
	// OwnerNotMPD: a process other than mpd holds the substream.
	OwnerNotMPD = "owner_not_mpd"
	// OwnerNotOutput: an mpd holds the substream from a thread that is no
	// enabled output's of the MPD the bridge reads (another MPD, or an
	// output it does not report).
	OwnerNotOutput = "owner_not_output"
	// HWParamsUnknown: the substream's hw_params did not say its format,
	// rate and channels.
	HWParamsUnknown = "hw_params_unknown"
	// SourceUnknown: the file is not one of this bridge's rows, so its own
	// format is not known here.
	SourceUnknown = "source_unknown"
	// SourceChanged: the file is one of this bridge's rows and has changed
	// on disk since the bridge read it.
	SourceChanged = "source_changed"
	// SourceLossy: the file is a lossy encode, which has no sample format of
	// its own to preserve.
	SourceLossy = "source_lossy"
	// SourceCodecUnknown: the file's codec is not one the bridge knows to
	// be lossless.
	SourceCodecUnknown = "source_codec_unknown"
	// SourceMismatch: the file's own format and what MPD's decoder hands
	// on (status audio:) disagree.
	SourceMismatch = "source_mismatch"
	// MPDAudioUnknown: MPD's status gave no audio: format this reads.
	MPDAudioUnknown = "mpd_audio_unknown"
	// MPDOutputAmbiguous: more than one enabled output of MPD's has an
	// output thread named as the one that holds the card is.
	MPDOutputAmbiguous = "mpd_output_ambiguous"
	// StreamFloat: MPD's decoder hands on floating point, not the file's
	// integer samples.
	StreamFloat = "stream_float"
	// FormatMismatch: the card's format cannot carry the stream as it is
	// (a float, unsigned or non-linear format, DSD for PCM, PCM for DSD
	// outside DoP).
	FormatMismatch = "format_mismatch"
	// RateMismatch: the card's rate is not the stream's (for DoP, not the
	// DSD rate over 16): the stream was resampled on its way.
	RateMismatch = "rate_mismatch"
	// ChannelsMismatch: the card's channels are not the stream's.
	ChannelsMismatch = "channels_mismatch"
	// DepthReduced: the card's PCM format carries fewer bits than the file.
	DepthReduced = "depth_reduced"
	// DSDConvertedToPCM: a DSD file reaches the card as PCM, and the output
	// does not send DoP.
	DSDConvertedToPCM = "dsd_converted_to_pcm"
	// DoPContainerTooNarrow: DoP in a PCM format of fewer than 24 bits.
	DoPContainerTooNarrow = "dop_container_too_narrow"
)

// codeOrder is every code in the order a list of them is given: the
// reading's codes first, then what stops a stream being judged, then the
// file, MPD's stream, the output and the formats.
var codeOrder = []string{
	MPDUnreachable, MPDTimeout, MPDRefused, ProcUnreadable, ProcTimeout, ALSAConfigUnread, NoCard, ReadingFailed,
	StateChanged, SongChanged, Seeked, SongStarting, SettingsChanged,
	NotPlaying, PCMAmbiguous, PCMClosed, PCMNotRunning, OwnerUnknown, OwnerNotMPD, OwnerNotOutput, HWParamsUnknown,
	SourceUnknown, SourceChanged, SourceLossy, SourceCodecUnknown, SourceMismatch,
	MPDAudioUnknown, MPDOutputAmbiguous,
	StreamFloat, FormatMismatch, RateMismatch, ChannelsMismatch, DepthReduced,
	DSDConvertedToPCM, DoPContainerTooNarrow,
}

// Codes is every code a reading's state or a verdict's reasons can carry,
// in their order.
func Codes() []string { return append([]string(nil), codeOrder...) }

// Sorted returns codes once each, in the order Codes gives them. A code
// this package does not define sorts last.
func Sorted(codes []string) []string {
	out := []string{}
	for _, c := range codes {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return rank(codeOrder, a) - rank(codeOrder, b) })
	return out
}

func rank(order []string, code string) int {
	if i := slices.Index(order, code); i >= 0 {
		return i
	}
	return len(order)
}

// Warnings: a setting the reading read that would change the samples on
// their way to the DAC. A warning never changes the verdict, which claims
// no sample, and its absence proves nothing.
const (
	// MPDReplayGain: MPD reports replay gain on (track, album or auto).
	MPDReplayGain = "mpd_replay_gain"
	// MPDCrossfade: MPD reports crossfade (xfade above 0), which mixes the
	// end of one song into the next.
	MPDCrossfade = "mpd_crossfade"
	// MPDMixRamp: MPD reports MixRamp on (mixrampdelay above 0).
	MPDMixRamp = "mpd_mixramp"
	// MPDSoftwareVolume: the output's mixer is MPD's software volume and
	// MPD reports a volume below 100.
	MPDSoftwareVolume = "mpd_software_volume"
	// MPDNormalization: volume_normalization is on.
	MPDNormalization = "mpd_normalization"
	// MPDFormatOverride: audio_output_format, the output's format, or its
	// allowed_formats is set.
	MPDFormatOverride = "mpd_format_override"
	// MPDFilters: the output applies filters.
	MPDFilters = "mpd_filters"
	// ALSASoftvol: the output's device goes through a softvol stage.
	ALSASoftvol = "alsa_softvol"
	// ALSARate: the output's device goes through a rate stage, a resampler.
	ALSARate = "alsa_rate"
	// ALSADmix: the output's device goes through a dmix stage, which mixes
	// streams at a format and rate of its own.
	ALSADmix = "alsa_dmix"
	// ALSARoute: the output's device goes through a route stage, which
	// routes and mixes channels.
	ALSARoute = "alsa_route"
	// ALSALadspa: the output's device goes through a LADSPA stage, a
	// filter of a plugin's.
	ALSALadspa = "alsa_ladspa"
)

// Where a warning's setting was read.
const (
	// FromMPD: reported by MPD over its protocol (status,
	// replay_gain_status, an outputs attribute).
	FromMPD = "mpd"
	// FromMPDConf: read from the mpd.conf the bridge finds, which MPD may
	// not have been started with.
	FromMPDConf = "mpd_conf"
	// FromALSAConf: read from the ALSA configuration files under /etc,
	// which may not be the ones MPD's ALSA loaded.
	FromALSAConf = "alsa_conf"
)

var (
	warningOrder = []string{
		MPDReplayGain, MPDCrossfade, MPDMixRamp, MPDSoftwareVolume, MPDNormalization, MPDFormatOverride, MPDFilters,
		ALSASoftvol, ALSARate, ALSADmix, ALSARoute, ALSALadspa,
	}
	sourceOrder = []string{FromMPD, FromMPDConf, FromALSAConf}
	// stageWarnings is the warning for each plugin type alsa.SampleStage
	// names.
	stageWarnings = map[string]string{
		"softvol": ALSASoftvol, "rate": ALSARate, "dmix": ALSADmix, "route": ALSARoute, "ladspa": ALSALadspa,
	}
)

// WarningCodes is every code a warning can carry, in their order.
func WarningCodes() []string { return append([]string(nil), warningOrder...) }

// WarningSources is every place a warning can say it was read, in their
// order.
func WarningSources() []string { return append([]string(nil), sourceOrder...) }

// Verdict is Judge's answer. Reasons is empty, never nil, unless the value
// is Withheld; Warnings is never nil.
type Verdict struct {
	Value   string
	Reasons []string
	// BitExactness is NotVerifiable, for every verdict.
	BitExactness string
	Warnings     []Warning
}

// Warning is one setting that would change the samples, and where the
// reading read it (FromMPD, FromMPDConf, FromALSAConf).
type Warning struct {
	Code   string
	Source string
}

// WithheldFor is a verdict withheld for the given codes, with no warning.
func WithheldFor(codes ...string) Verdict {
	return Verdict{Value: Withheld, Reasons: Sorted(codes), BitExactness: NotVerifiable, Warnings: []Warning{}}
}

// Facts is what one whole reading established. A fact the reading could
// not establish is its zero value.
type Facts struct {
	// State is MPD's playback state, the same at both status reads: play,
	// pause or stop.
	State string
	// Audio is MPD's status audio: line, the format its decoder hands the
	// outputs: "44100:16:2", "96000:24:2", "dsd64:2", "384000:dsd:2".
	Audio  string
	Source Source
	MPD    MPD
	PCM    PCM
}

// Source is the file MPD is on, as this bridge's manifest knows it.
type Source struct {
	// Known is true when the file is one of this bridge's rows and the row
	// is the file on disk now (the same size and modification time).
	Known bool
	// Changed is true when the file is one of this bridge's rows and its
	// size or modification time is no longer the row's.
	Changed bool
	// Codec is the manifest's codec string (FLAC, ALAC, WAV, AIFF, DSF,
	// DFF, AAC, MP3, …).
	Codec string
	// Lossy is the manifest's own verdict on Codec (manifest.IsLossyCodec).
	Lossy bool
	// DSD is a DSD file (DSF or DFF).
	DSD bool
	// SampleRate is in Hz, and for DSD the one-bit rate per channel:
	// 2822400 for DSD64. 0 when the row has none.
	SampleRate int
	// Bits is the file's sample bits, 0 when the row has none.
	Bits int
	// Channels is 0 when the row has none (a native FLAC's).
	Channels int
}

// MPD is the MPD the bridge dialled: which of its outputs holds the card,
// and what it reports and mpd.conf says of the settings the warnings name.
type MPD struct {
	// ReplayGainMode is replay_gain_status's mode: off, track, album, auto;
	// "" when it gave none.
	ReplayGainMode string
	// Crossfade is status xfade as MPD prints it; "" when absent.
	Crossfade string
	// MixRampDelay is status mixrampdelay as MPD prints it; "" when
	// absent, which is off, as is "nan".
	MixRampDelay string
	// Volume is status volume as MPD prints it; "" when absent.
	Volume string
	// ConfRead is whether mpd.conf was read at this reading.
	// VolumeNormalization and AudioOutputFormat are its top-level settings,
	// as written: what the file says, which is not shown to be what MPD runs
	// with (MPD reads its configuration when it starts).
	ConfRead            bool
	VolumeNormalization string
	AudioOutputFormat   string
	// Holders is how many enabled outputs of MPD's have an output thread
	// named as the thread that holds the card is: MPD names each output's
	// thread "output:" and its name, cut to the 15 bytes a thread name
	// keeps. Output is that output when there is exactly one.
	Holders int
	Output  Output
}

// Output is the MPD output whose thread holds the card.
type Output struct {
	// Plugin is its live plugin: alsa, ao, oss…
	Plugin string
	// Configured is whether mpd.conf was read and has its block (by name).
	// MixerType (EffectiveMixerType's answer), Format and Filters are that
	// block's.
	Configured bool
	MixerType  string
	Format     string
	Filters    string
	// AllowedFormats is the live allowed_formats attribute when MPD reports
	// one (AllowedFormatsLive), else its block's.
	AllowedFormats     string
	AllowedFormatsLive bool
	// DoP is whether the output sends DSD as DoP: the live dop attribute,
	// else its block's dop setting.
	DoP bool
	// SampleStage is the plugin type of the first stage that changes the
	// samples (softvol, rate, dmix, route, ladspa) on the way from its
	// device, as the ALSA files the bridge read define it
	// (alsa.PCMConfig.SampleStage); "" when the bridge met none.
	SampleStage string
}

// PCM is the card's playback substream the reading read.
type PCM struct {
	// Open is whether a substream of the card's PCM device was open with
	// parameters set.
	Open bool
	// Ambiguous is more than one such substream, not exactly one of them
	// held by MPD.
	Ambiguous bool
	// State is its status state: RUNNING, PREPARED, PAUSED, XRUN, …
	State string
	// Format, Rate and Channels are its hw_params; Rate counts frames, so
	// a DSD format's is its words per second.
	Format   string
	Rate     int
	Channels int
	// Owner is the name of the process that holds it (the owner thread's
	// Tgid); "" when it could not be named.
	Owner string
	// Thread is the name of the thread that holds it (owner_pid), as
	// /proc's status shows it; "" when it could not be named.
	Thread string
}

// Judge decides the verdict for one whole reading. A stream that is not
// MPD's, or not running, stops the judging with the one code that says
// so, and no warning; past that, every condition that does not hold adds
// its code, so the list is whole and the app can say each, and the
// warnings are every setting read that would change the samples.
func Judge(f Facts) Verdict {
	switch {
	case f.State != "play":
		return WithheldFor(NotPlaying)
	case f.PCM.Ambiguous:
		return WithheldFor(PCMAmbiguous)
	case !f.PCM.Open:
		return WithheldFor(PCMClosed)
	case f.PCM.State != "RUNNING":
		return WithheldFor(PCMNotRunning)
	case f.PCM.Owner == "" || f.PCM.Thread == "":
		return WithheldFor(OwnerUnknown)
	case f.PCM.Owner != "mpd":
		return WithheldFor(OwnerNotMPD)
	case f.MPD.Holders == 0:
		return WithheldFor(OwnerNotOutput)
	case f.PCM.Format == "" || f.PCM.Rate <= 0 || f.PCM.Channels <= 0:
		return WithheldFor(HWParamsUnknown)
	}

	var codes []string
	add := func(c string) { codes = append(codes, c) }

	audio, audioOK := ParseAudio(f.Audio)
	if !audioOK {
		add(MPDAudioUnknown)
	}

	switch {
	case f.Source.Changed:
		add(SourceChanged)
	case !f.Source.Known:
		add(SourceUnknown)
	default:
		if !losslessCodec(f.Source.Codec) {
			if f.Source.Lossy {
				add(SourceLossy)
			} else {
				add(SourceCodecUnknown)
			}
		}
		if audioOK && !sourceAgrees(f.Source, audio) {
			add(SourceMismatch)
		}
	}

	oneOutput := f.MPD.Holders == 1
	if !oneOutput {
		add(MPDOutputAmbiguous)
	}

	dop := false
	if audioOK {
		bits, signed, linear := alsa.PCMFormatBits(f.PCM.Format)
		dsdHW := alsa.IsDSDFormat(f.PCM.Format)
		switch {
		case audio.DSD && dsdHW:
			// Native DSD: the card's one-bit rate is its frame rate times
			// the format's width.
			if hwRate, known := alsa.NativeDSDRate(f.PCM.Format, f.PCM.Rate); !known {
				add(FormatMismatch)
			} else if hwRate != audio.Rate {
				add(RateMismatch)
			}
			if f.PCM.Channels != audio.Channels {
				add(ChannelsMismatch)
			}
		case audio.DSD && linear:
			switch {
			case !oneOutput:
				// Which output, and so whether it sends DoP, is not known;
				// mpd_output_ambiguous already says so.
			case !f.MPD.Output.DoP:
				add(DSDConvertedToPCM)
			default:
				// DoP: 16 one-bit samples a channel in each PCM frame, in
				// the top 24 bits of a sample. The kernel shows the frames'
				// format and rate, never the samples, so this is consistent
				// with DoP and no more.
				dop = true
				if !signed {
					add(FormatMismatch)
				}
				if bits < 24 {
					add(DoPContainerTooNarrow)
				}
				// The DSD rate over 16, compared by division: the card's
				// rate times 16 can wrap in an int to the DSD rate.
				if audio.Rate%16 != 0 || f.PCM.Rate != audio.Rate/16 {
					add(RateMismatch)
				}
				if f.PCM.Channels != audio.Channels {
					add(ChannelsMismatch)
				}
			}
		case audio.DSD:
			add(FormatMismatch)
		case audio.Float:
			add(StreamFloat)
		case dsdHW || !linear || !signed:
			add(FormatMismatch)
		default:
			if f.PCM.Rate != audio.Rate {
				add(RateMismatch)
			}
			if f.PCM.Channels != audio.Channels {
				add(ChannelsMismatch)
			}
			// The format must hold the file's bits; a wider one carries
			// them in a wider word. When the file's bits are not known, the
			// stream's stand in.
			floor := audio.Bits
			if f.Source.Known && !f.Source.DSD && f.Source.Bits > 0 {
				floor = f.Source.Bits
			}
			if bits < floor {
				add(DepthReduced)
			}
		}
	}

	v := Verdict{Value: FormatPreserved, Reasons: []string{}, BitExactness: NotVerifiable, Warnings: warnings(f)}
	switch {
	case len(codes) > 0:
		v.Value, v.Reasons = Withheld, Sorted(codes)
	case dop:
		v.Value = FormatPreservedDoP
	}
	return v
}

// warnings is every setting that would change the samples of MPD's own
// running stream that the reading read, once each, in WarningCodes order
// and then WarningSources order: what MPD reports (replay gain, crossfade,
// MixRamp, the output's live allowed_formats), what mpd.conf says
// (normalization, the format overrides, the holder's software volume while
// MPD reports a volume below 100, its filters), and the first sample stage
// the holder's device goes through as the ALSA files define it. A setting
// that does not read as on is no warning, and what was not read is none.
func warnings(f Facts) []Warning {
	out := []Warning{}
	add := func(code, source string) {
		if w := (Warning{Code: code, Source: source}); !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	m := f.MPD
	if mode := strings.TrimSpace(m.ReplayGainMode); mode != "" && mode != "off" {
		add(MPDReplayGain, FromMPD)
	}
	if above(m.Crossfade, 0) {
		add(MPDCrossfade, FromMPD)
	}
	if above(m.MixRampDelay, 0) {
		add(MPDMixRamp, FromMPD)
	}
	if m.ConfRead {
		if on, ok := ParseBool(m.VolumeNormalization); ok && on {
			add(MPDNormalization, FromMPDConf)
		}
		if strings.TrimSpace(m.AudioOutputFormat) != "" {
			add(MPDFormatOverride, FromMPDConf)
		}
	}
	if m.Holders == 1 {
		o := m.Output
		if strings.TrimSpace(o.AllowedFormats) != "" {
			source := FromMPDConf
			if o.AllowedFormatsLive {
				source = FromMPD
			}
			add(MPDFormatOverride, source)
		}
		if o.Configured {
			if o.MixerType == "software" && volumeBelowFull(m.Volume) {
				add(MPDSoftwareVolume, FromMPDConf)
			}
			if strings.TrimSpace(o.Format) != "" {
				add(MPDFormatOverride, FromMPDConf)
			}
			if strings.TrimSpace(o.Filters) != "" {
				add(MPDFilters, FromMPDConf)
			}
			if code, ok := stageWarnings[o.SampleStage]; ok {
				add(code, FromALSAConf)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b Warning) int {
		if d := rank(warningOrder, a.Code) - rank(warningOrder, b.Code); d != 0 {
			return d
		}
		return rank(sourceOrder, a.Source) - rank(sourceOrder, b.Source)
	})
	return out
}

// above reports whether v, a number as MPD prints it, reads as more than
// floor; absent, nan and a value that does not parse do not.
func above(v string, floor float64) bool {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return err == nil && !math.IsNaN(n) && n > floor
}

// volumeBelowFull reports whether status volume, as MPD prints it, is 0 to
// 99: MPD's software volume leaves a sample as it is only at 100.
func volumeBelowFull(v string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	return err == nil && n >= 0 && n < 100
}

// losslessCodec is the decision's list: flac, alac, wav, aiff, wv, ape,
// dsf, dff. The manifest names WAV and AIFF only for linear PCM, and a
// compressed AIFF-C or WAV by its encoding.
func losslessCodec(codec string) bool {
	switch strings.ToUpper(strings.TrimSpace(codec)) {
	case "FLAC", "ALAC", "WAV", "AIFF", "WV", "WAVPACK", "APE", "DSF", "DFF":
		return true
	}
	return false
}

// sourceAgrees reports whether what MPD's decoder hands on is the file:
// DSD for DSD, the same rate and channels where the row has them, and for
// PCM at least the file's bits (a decoder may widen, never narrow: FFmpeg
// hands 24-bit ALAC on as 32).
func sourceAgrees(src Source, a Audio) bool {
	if src.DSD != a.DSD {
		return false
	}
	if src.SampleRate > 0 && src.SampleRate != a.Rate {
		return false
	}
	if src.Channels > 0 && src.Channels != a.Channels {
		return false
	}
	if !a.DSD && !a.Float && src.Bits > 0 && a.Bits < src.Bits {
		return false
	}
	return true
}

// Audio is one MPD audio format: status audio: or currentsong Format:.
type Audio struct {
	// DSD is a DSD stream, Float a floating-point one; otherwise it is
	// integer PCM of Bits.
	DSD   bool
	Float bool
	// Rate is frames per second for PCM, and the one-bit rate per channel
	// for DSD (2822400 for dsd64).
	Rate     int
	Bits     int
	Channels int
}

var (
	audioDSDShort = regexp.MustCompile(`^dsd([1-9][0-9]*):([1-9][0-9]*)$`)
	audioDSDLong  = regexp.MustCompile(`^([1-9][0-9]*):dsd:([1-9][0-9]*)$`)
	audioPCM      = regexp.MustCompile(`^([1-9][0-9]*):(8|16|24|32|f):([1-9][0-9]*)$`)
)

// ParseAudio reads an audio format as MPD 0.24 prints it (src/pcm/
// AudioFormat.cxx, ToString): "44100:16:2", "44100:f:2", "dsd64:2" for a
// DSD rate that is a multiple of 44100 (64 × 44100 one-bit samples a
// second), and "384000:dsd:2" otherwise, whose rate is bytes a second, so
// the one-bit rate is eight times it. ok is false for anything else.
func ParseAudio(s string) (Audio, bool) {
	s = strings.TrimSpace(s)
	if m := audioDSDShort.FindStringSubmatch(s); m != nil {
		n, ok1 := atoiCapped(m[1])
		ch, ok2 := atoiCapped(m[2])
		if !ok1 || !ok2 || n > math.MaxInt32/44100 {
			return Audio{}, false
		}
		return Audio{DSD: true, Rate: n * 44100, Channels: ch}, true
	}
	if m := audioDSDLong.FindStringSubmatch(s); m != nil {
		rate, ok1 := atoiCapped(m[1])
		ch, ok2 := atoiCapped(m[2])
		if !ok1 || !ok2 || rate > math.MaxInt32/8 {
			return Audio{}, false
		}
		return Audio{DSD: true, Rate: rate * 8, Channels: ch}, true
	}
	if m := audioPCM.FindStringSubmatch(s); m != nil {
		rate, ok1 := atoiCapped(m[1])
		ch, ok2 := atoiCapped(m[3])
		if !ok1 || !ok2 {
			return Audio{}, false
		}
		if m[2] == "f" {
			return Audio{Float: true, Rate: rate, Channels: ch}, true
		}
		bits, _ := strconv.Atoi(m[2])
		return Audio{Rate: rate, Bits: bits, Channels: ch}, true
	}
	return Audio{}, false
}

// atoiCapped parses a decimal that fits in 32 bits.
func atoiCapped(s string) (int, bool) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n <= 0 {
		return 0, false
	}
	return int(n), true
}

// ParseBool reads a boolean setting the way MPD does (ParseBool in
// src/config: yes, true and 1 are true, no, false and 0 are false, any
// case). ok is false for anything else, which MPD refuses to start on.
func ParseBool(v string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1":
		return true, true
	case "no", "false", "0":
		return false, true
	}
	return false, false
}

// EffectiveMixerType is the mixer MPD gives an output (src/output/Init.cxx,
// audio_output_mixer_type): the output's mixer_type; else none when its
// mixer_enabled is false; else the top-level mixer_type; else hardware.
// The answer is hardware, software, null or none ("disabled" is none). A
// value MPD does not name, or a mixer_enabled that is not a boolean, is
// returned as "", which MPD refuses to start on.
func EffectiveMixerType(outputType, mixerEnabled, globalType string) string {
	if t := strings.TrimSpace(outputType); t != "" {
		return mixerName(t)
	}
	if e := strings.TrimSpace(mixerEnabled); e != "" {
		on, ok := ParseBool(e)
		if !ok {
			return ""
		}
		if !on {
			return "none"
		}
	}
	if t := strings.TrimSpace(globalType); t != "" {
		return mixerName(t)
	}
	return "hardware"
}

// mixerName is mixer_type_parse's set (src/mixer/MixerType.cxx): an exact
// match, as MPD compares it.
func mixerName(t string) string {
	switch t {
	case "none", "disabled":
		return "none"
	case "hardware", "software", "null":
		return t
	}
	return ""
}

// Status is the part of MPD's status reply the settling check reads, as
// MPD prints it: "" for a key it did not send.
type Status struct {
	State   string
	SongID  string
	Elapsed string
}

// settleBelow is the elapsed time under which a song is still starting:
// a format change can still be opening the device.
const settleBelow = 0.5

// seekSlack is how far elapsed may drift from the time between the two
// status reads before it counts as a seek: MPD moves elapsed a chunk at a
// time, tens of milliseconds, so half a second is a jump, not a step.
const seekSlack = 0.5

// Settling returns the codes that make a reading settle: the status read
// before the /proc read and the one after it, between apart, disagree on
// the state or the song, a read while MPD plays names no song id, the song
// is not half a second in, or elapsed went back or moved by more than
// between allows. Empty when the reading is steady.
func Settling(before, after Status, between time.Duration) []string {
	var codes []string
	if before.State != after.State {
		codes = append(codes, StateChanged)
	}
	if before.SongID != after.SongID {
		codes = append(codes, SongChanged)
	}
	if len(codes) > 0 || after.State != "play" {
		return Sorted(codes)
	}
	// While MPD plays, every status names the song by its id: two reads
	// that name none, or one that does not read, are not shown to be on one
	// song.
	if !ValidSongID(before.SongID) || !ValidSongID(after.SongID) {
		return []string{SongChanged}
	}
	e1, ok1 := elapsed(before.Elapsed)
	e2, ok2 := elapsed(after.Elapsed)
	if !ok1 || !ok2 {
		return []string{SongStarting}
	}
	if e1 < settleBelow || e2 < settleBelow {
		codes = append(codes, SongStarting)
	}
	// MPD's elapsed never goes back while it plays on, so any move back is
	// a seek, however small; forward, half a second off the time between
	// the reads is a jump.
	moved := e2 - e1
	took := between.Seconds()
	if moved < 0 || moved < took-seekSlack || moved > took+seekSlack {
		codes = append(codes, Seeked)
	}
	return Sorted(codes)
}

// ValidSongID reports whether id is a song id as MPD prints one: an
// unsigned decimal.
func ValidSongID(id string) bool {
	if id == "" {
		return false
	}
	_, err := strconv.ParseUint(id, 10, 32)
	return err == nil
}

func elapsed(v string) (float64, bool) {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, false
	}
	return n, true
}
