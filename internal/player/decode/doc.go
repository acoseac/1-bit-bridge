// Package decode is the player's lossless decoder seam.
//
// Content inspection chooses a decoder; file extensions and the library index
// never establish support. Open returns a Decoder for PCM content: integer
// FLAC, integer WAV (including validated WAVE_FORMAT_EXTENSIBLE PCM), and
// integer AIFF / AIFF-C with compression NONE or sowt. It returns a
// DSDDecoder for DSD content: stereo DSF (LSB- or MSB-first) and uncompressed
// stereo DFF, both into the canonical native DSD form (channel-separated
// bytes, bit 7 earliest). Everything else is refused with a typed,
// user-presentable reason, DST-compressed DFF included. ALAC lands in a
// later PR.
//
// Decoders are streaming, bounded and cancellable, and seek exactly: PCM by
// sample frame, DSD by byte per channel. They reconstruct source samples for
// playback only: they create no rendition and change no /v1/download bytes.
package decode
