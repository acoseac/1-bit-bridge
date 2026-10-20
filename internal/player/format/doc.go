// Package format holds the player's canonical audio representations and the
// PCM packers that feed ALSA.
//
// A PCM stream is stereo signed integer with 16, 24 or 32 valid bits. There is
// no downmixing, truncation, resampling, floating intermediate, ReplayGain,
// normalization, dither or crossfade. A native DSD stream is stereo,
// channel-separated bytes with bit 7 earliest, which the DSF and DFF decoders
// produce. The native DSD and DoP packers are later PRs.
//
// Packing preserves every source bit under lossless widening: container width
// alone does not establish precision.
package format
