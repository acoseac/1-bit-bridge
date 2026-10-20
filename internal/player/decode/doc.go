// Package decode is the player's lossless PCM decoder seam.
//
// Content inspection chooses a decoder; file extensions and the library index
// never establish support. Supported in this package: integer FLAC, integer
// WAV (including validated WAVE_FORMAT_EXTENSIBLE PCM), and integer AIFF /
// AIFF-C with compression NONE or sowt. Everything else is refused with a
// typed, user-presentable reason. ALAC, DSF and DFF land in a later PR.
//
// Decoders are streaming, bounded and cancellable, and seek by exact sample
// frame. They reconstruct source samples for playback only: they create no
// rendition and change no /v1/download bytes.
package decode
