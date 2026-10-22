// Package signalpath judges what the box's DAC receives against the file
// the MPD beside the bridge is on: the verdict GET /v1/outputs/{id}/reading
// carries (B473 B3, decision 3 of the bridge-as-output plan). It reads
// nothing. The route gathers the facts (MPD's status around the /proc read,
// mpd.conf, the ALSA chain, the substream's hw_params and owner, the file's
// own row), and Judge decides from them alone, so every case is a row of a
// table.
//
// The wording is the plan's. The DAC receives; it never "plays", and
// nothing is said about what happens inside it. A verdict is bit_exact
// (PCM, or native DSD, every condition proven), bit_exact_dop (the file's
// DSD in DoP frames whose PCM path is proven untouched, which never claims
// that the DAC locked onto the DoP or decoded it), or withheld, with a code
// for every condition that is not proven. A code is a word the app maps to
// its own copy, never a sentence.
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
	BitExact    = "bit_exact"
	BitExactDoP = "bit_exact_dop"
	Withheld    = "withheld"
)

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
	// NoCard: this output names no sound card, so nothing says what a DAC
	// receives.
	NoCard = "no_card"
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
	// OwnerUnknown: the process that holds the substream could not be
	// named (a /proc mounted with hidepid, another pid namespace).
	OwnerUnknown = "owner_unknown"
	// OwnerNotMPD: a process other than mpd holds the substream.
	OwnerNotMPD = "owner_not_mpd"
	// HWParamsUnknown: the substream's hw_params did not say its format,
	// rate and channels.
	HWParamsUnknown = "hw_params_unknown"
	// SourceUnknown: the file is not one of this bridge's rows, so its own
	// format is not known here.
	SourceUnknown = "source_unknown"
	// SourceChanged: the file is one of this bridge's rows and has changed
	// on disk since the bridge read it.
	SourceChanged = "source_changed"
	// SourceLossy: the file is a lossy encode.
	SourceLossy = "source_lossy"
	// SourceCodecUnknown: the file's codec is not one the bridge knows to
	// be lossless.
	SourceCodecUnknown = "source_codec_unknown"
	// SourceMismatch: the file's own format and what MPD's decoder hands
	// on (status audio:) disagree.
	SourceMismatch = "source_mismatch"
	// MPDAudioUnknown: MPD's status gave no audio: format this reads.
	MPDAudioUnknown = "mpd_audio_unknown"
	// MPDConfigUnread: mpd.conf could not be read, so the settings only
	// it holds are not proven.
	MPDConfigUnread = "mpd_config_unread"
	// MPDOutputAmbiguous: more than one enabled alsa output of MPD's
	// reaches this card.
	MPDOutputAmbiguous = "mpd_output_ambiguous"
	// MPDReplayGain: replay gain is not off.
	MPDReplayGain = "mpd_replay_gain"
	// MPDSoftwareVolume: the output's mixer is MPD's software volume, or
	// one MPD's mixer types do not name.
	MPDSoftwareVolume = "mpd_software_volume"
	// MPDNormalization: volume_normalization is on.
	MPDNormalization = "mpd_normalization"
	// MPDFormatOverride: audio_output_format, the output's format, or its
	// allowed_formats is set.
	MPDFormatOverride = "mpd_format_override"
	// MPDCrossfade: crossfade (xfade) is not 0.
	MPDCrossfade = "mpd_crossfade"
	// MPDMixRamp: MixRamp is on (mixrampdelay above 0).
	MPDMixRamp = "mpd_mixramp"
	// MPDFilters: the output applies filters.
	MPDFilters = "mpd_filters"
	// ALSAChainUnproven: no enabled alsa output of MPD's reaches this card
	// through a chain the bridge parses: hw, or plug and empty stages to
	// hw, with no softvol, dmix, rate or route stage.
	ALSAChainUnproven = "alsa_chain_unproven"
	// ALSAPlugConverts: the chain has a plug stage and the card's format,
	// rate or channels are not the stream's, so the plug converted.
	ALSAPlugConverts = "alsa_plug_converts"
	// StreamFloat: MPD's decoder hands on floating point.
	StreamFloat = "stream_float"
	// FormatMismatch: the card's format cannot carry the stream as it is
	// (a float, unsigned or non-linear format, DSD for PCM, PCM for DSD
	// outside DoP).
	FormatMismatch = "format_mismatch"
	// RateMismatch: the card's rate is not the stream's (for DoP, not the
	// DSD rate over 16).
	RateMismatch = "rate_mismatch"
	// ChannelsMismatch: the card's channels are not the stream's.
	ChannelsMismatch = "channels_mismatch"
	// DepthReduced: the card's PCM format carries fewer bits than the file.
	DepthReduced = "depth_reduced"
	// DSDConvertedToPCM: a DSD file reaches the card as PCM, and not as
	// DoP.
	DSDConvertedToPCM = "dsd_converted_to_pcm"
	// DoPContainerTooNarrow: DoP in a PCM format of fewer than 24 bits.
	DoPContainerTooNarrow = "dop_container_too_narrow"
)

// codeOrder is every code in the order a list of them is given: the
// reading's codes first, then what stops a stream being judged, then the
// file, MPD, the ALSA chain and the formats.
var codeOrder = []string{
	MPDUnreachable, MPDTimeout, MPDRefused, ProcUnreadable, ProcTimeout, NoCard,
	StateChanged, SongChanged, Seeked, SongStarting,
	NotPlaying, PCMAmbiguous, PCMClosed, PCMNotRunning, OwnerUnknown, OwnerNotMPD, HWParamsUnknown,
	SourceUnknown, SourceChanged, SourceLossy, SourceCodecUnknown, SourceMismatch,
	MPDAudioUnknown, MPDConfigUnread, MPDOutputAmbiguous, MPDReplayGain, MPDSoftwareVolume,
	MPDNormalization, MPDFormatOverride, MPDCrossfade, MPDMixRamp, MPDFilters,
	ALSAChainUnproven, ALSAPlugConverts,
	StreamFloat, FormatMismatch, RateMismatch, ChannelsMismatch, DepthReduced,
	DSDConvertedToPCM, DoPContainerTooNarrow,
}

// Codes is every code a reading or a verdict can carry, in their order.
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
	slices.SortStableFunc(out, func(a, b string) int { return rank(a) - rank(b) })
	return out
}

func rank(code string) int {
	if i := slices.Index(codeOrder, code); i >= 0 {
		return i
	}
	return len(codeOrder)
}

// Verdict is Judge's answer. Reasons is empty, never nil, unless the
// value is Withheld.
type Verdict struct {
	Value   string
	Reasons []string
}

// WithheldFor is a verdict withheld for the given codes.
func WithheldFor(codes ...string) Verdict {
	return Verdict{Value: Withheld, Reasons: Sorted(codes)}
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

// MPD is how the MPD the bridge dialled is set.
type MPD struct {
	// ReplayGainMode is replay_gain_status's mode: off, track, album, auto.
	ReplayGainMode string
	// Crossfade is status xfade as MPD prints it; "" when absent.
	Crossfade string
	// MixRampDelay is status mixrampdelay as MPD prints it; "" when
	// absent, which is off, as is "nan".
	MixRampDelay string
	// ConfigRead is whether mpd.conf was read. What only the file says
	// (normalization, the format override, the mixer, the filters, the
	// outputs' devices) is proven only then.
	ConfigRead bool
	// VolumeNormalization and AudioOutputFormat are the file's top-level
	// settings, as written.
	VolumeNormalization string
	AudioOutputFormat   string
	// Outputs is how many enabled alsa outputs of MPD's reach this card
	// through a chain the bridge resolved (alsa.ResolveOutputChain).
	// Output is that output when there is exactly one.
	Outputs int
	Output  Output
}

// Output is the MPD audio_output that feeds the card.
type Output struct {
	// MixerType is the effective mixer: EffectiveMixerType's answer.
	MixerType      string
	Format         string
	AllowedFormats string
	Filters        string
	// DoP is whether the output sends DSD as DoP: the live dop attribute,
	// else the file's dop setting.
	DoP bool
	// Stages is the chain from the output's device to the card, as
	// alsa.Chain names it.
	Stages []string
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
}

// Judge decides the verdict for one whole reading. A stream that is not
// MPD's, or not running, stops the judging with the one code that says
// so; past that, every condition that is not proven adds its code, so the
// list is whole and the app can say each.
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
	case f.PCM.Owner == "":
		return WithheldFor(OwnerUnknown)
	case f.PCM.Owner != "mpd":
		return WithheldFor(OwnerNotMPD)
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

	if f.MPD.ReplayGainMode != "off" {
		add(MPDReplayGain)
	}
	if crossfadeOn(f.MPD.Crossfade) {
		add(MPDCrossfade)
	}
	if mixRampOn(f.MPD.MixRampDelay) {
		add(MPDMixRamp)
	}
	if !f.MPD.ConfigRead {
		add(MPDConfigUnread)
	} else {
		if on, ok := ParseBool(f.MPD.VolumeNormalization); f.MPD.VolumeNormalization != "" && (!ok || on) {
			add(MPDNormalization)
		}
		if strings.TrimSpace(f.MPD.AudioOutputFormat) != "" {
			add(MPDFormatOverride)
		}
	}
	out := f.MPD.Output
	switch {
	case f.MPD.Outputs == 0:
		add(ALSAChainUnproven)
	case f.MPD.Outputs > 1:
		add(MPDOutputAmbiguous)
	default:
		switch out.MixerType {
		case "hardware", "null", "none":
		default:
			add(MPDSoftwareVolume)
		}
		if strings.TrimSpace(out.Format) != "" || strings.TrimSpace(out.AllowedFormats) != "" {
			add(MPDFormatOverride)
		}
		if strings.TrimSpace(out.Filters) != "" {
			add(MPDFilters)
		}
	}
	oneOutput := f.MPD.Outputs == 1
	plug := oneOutput && slices.Contains(out.Stages, "plug")

	dop := false
	if audioOK {
		bits, signed, linear := alsa.PCMFormatBits(f.PCM.Format)
		dsdHW := alsa.IsDSDFormat(f.PCM.Format)
		switch {
		case audio.DSD && dsdHW:
			// Native DSD: the card's one-bit rate is its frame rate times
			// the format's width.
			hwRate, known := alsa.NativeDSDRate(f.PCM.Format, f.PCM.Rate)
			changed := false
			if !known {
				add(FormatMismatch)
			} else if hwRate != audio.Rate {
				add(RateMismatch)
				changed = true
			}
			if f.PCM.Channels != audio.Channels {
				add(ChannelsMismatch)
				changed = true
			}
			if plug && changed {
				add(ALSAPlugConverts)
			}
		case audio.DSD && linear:
			switch {
			case !oneOutput:
				// Which output, and so whether it sends DoP, is not known;
				// the chain's code already says why.
			case !out.DoP:
				add(DSDConvertedToPCM)
			default:
				// DoP: 16 one-bit samples a channel in each PCM frame, in
				// the top 24 bits of a sample.
				dop = true
				changed := false
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
					changed = true
				}
				if f.PCM.Channels != audio.Channels {
					add(ChannelsMismatch)
					changed = true
				}
				// MPD hands a plug DoP as 24-bit samples.
				if plug && (changed || bits != 24) {
					add(ALSAPlugConverts)
				}
			}
		case audio.DSD:
			add(FormatMismatch)
		case audio.Float:
			add(StreamFloat)
		case dsdHW || !linear || !signed:
			add(FormatMismatch)
		default:
			changed := false
			if f.PCM.Rate != audio.Rate {
				add(RateMismatch)
				changed = true
			}
			if f.PCM.Channels != audio.Channels {
				add(ChannelsMismatch)
				changed = true
			}
			// The container must hold the file's bits; when the file is not
			// known, the stream's stand in.
			floor := audio.Bits
			if f.Source.Known && !f.Source.DSD && f.Source.Bits > 0 {
				floor = f.Source.Bits
			}
			if bits < floor {
				add(DepthReduced)
			}
			if plug && (changed || bits != audio.Bits) {
				add(ALSAPlugConverts)
			}
		}
	}

	if len(codes) > 0 {
		return WithheldFor(codes...)
	}
	if dop {
		return Verdict{Value: BitExactDoP, Reasons: []string{}}
	}
	return Verdict{Value: BitExact, Reasons: []string{}}
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

// crossfadeOn reads status xfade: absent or 0 is off. MPD prints it only
// above 0 (lround of the seconds); a value that does not parse is not
// proven off.
func crossfadeOn(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	n, err := strconv.ParseFloat(v, 64)
	return err != nil || math.IsNaN(n) || n != 0
}

// mixRampOn reads status mixrampdelay: MPD prints it only above 0, and
// MixRamp overlaps songs only then; absent, nan, 0 and below are off. A
// value that does not parse is not proven off.
func mixRampOn(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "nan") {
		return false
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return true
	}
	return n > 0
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
// returned as "", which Judge counts as software volume not ruled out.
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
