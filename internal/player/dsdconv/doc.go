// Package dsdconv converts the bridge player's native DSD to PCM (B728 §4):
// a decode.Decoder over a decode.DSDDecoder, canonical DSD planes in and
// stereo int32 PCM out, for a DAC the effective policy converts for. It is
// the one transform in the player that changes samples, and its output is
// converted, never lossless or bit-exact.
//
// The plan (Plan) is the target the caller chooses from the DAC: 176.4,
// 88.2 or 44.1 kHz at 24 valid bits, or 16 where the DAC takes no 24-bit
// target. Its claims (rate, valid bits, passband, dither, filter, gain) are
// the plan's methods, and nothing else states them.
//
// The chain is linear phase at unity gain. Stage 1 takes the one-bit stream
// to 352.8 kHz through byte tables of partial sums, stage 2 is a half-band to
// 176.4 kHz, and stage 3 is the plan's low-pass: 30 kHz pass and 40 kHz
// stop, or 20 kHz and 22.05 kHz at 44.1 kHz, at least 100 dB down, evaluated
// at every output frame of the plan's rate. Output frame k is centred on
// input bit k·D, D the total ratio, and a stream of F bits gives floor(F/D)
// frames. The delay lines start at numeric zero, never at zero bits (which
// are -1.0, a full-scale offset), bits past the stream's Frames are never
// read, and the tail is flushed with numeric zeros as frames of the stream.
//
// The arithmetic is integer from the bits to the samples: the coefficients
// are designed once, in float64 with every product rounded on its own and no
// library function an architecture implements differently, then quantized,
// and every table, sum and shift after that is an integer one, so the output
// is the same on every architecture. A sample past full scale saturates at
// ±(2^23 - 1), or ±(2^15 - 1) at 16 bits, and is counted; 24-bit output
// rounds half to even with no dither, and 16-bit output takes TPDF dither of
// ±1 LSB from a fixed-seed xorshift re-seeded for each occurrence.
//
// A continuous join (Continuous) carries one chain's delay lines from an
// occurrence into its successor. The predecessor's last H frames (Window)
// depend on the successor's first bits, so a converter made with Hold stops
// before them (ErrWindow) with a snapshot of itself, and computes them only
// once its caller says what follows: the successor (Continue) or nothing
// (End). Rewind goes back to that snapshot, for a successor replaced or cut,
// as often as asked. The player's backend (internal/player/playback, B728
// B11) is its one caller: it converts where the effective set converts a
// rate, and hands the samples to its DAC's sink alone.
package dsdconv
