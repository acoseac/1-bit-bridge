// Package format holds the player's canonical audio representations and the
// PCM packers that feed ALSA.
//
// A PCM stream is stereo signed integer with 16, 24 or 32 valid bits. There is
// no downmixing, truncation, resampling, floating intermediate, ReplayGain,
// normalization, dither or crossfade. Native DSD and DoP types are declared
// here for later PRs; they are not implemented in this package yet.
//
// Packing preserves every source bit under lossless widening: container width
// alone does not establish precision.
package format
