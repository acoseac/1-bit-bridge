package decode

import (
	"context"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// Decoder is a streaming, bounded, cancellable PCM decoder with exact
// sample-frame seeking.
//
// ReadFrames writes interleaved, sign-extended int32 samples into dst. The
// length of dst must be a multiple of Channels. The returned n is the number
// of sample values written (channels × frames). io.EOF means the stream ended
// cleanly at a known endpoint; premature EOF against a declared length is a
// *Refusal with ReasonTruncated.
//
// A short mid-stream read may return n > 0 together with a non-nil error on
// the same call: the n samples are valid, and the error (typically a
// truncated Refusal, or a cancelled context) applies after them. Callers must
// not discard n when err != nil.
type Decoder interface {
	Format() format.PCM
	ReadFrames(ctx context.Context, dst []int32) (n int, err error)
	// Seek positions the decoder so the next ReadFrames returns sample frame
	// frame (0-based). Seeking past the end is an error.
	Seek(ctx context.Context, frame int64) error
	Close() error
}

// DSDDecoder is a streaming, bounded, cancellable DSD demuxer with exact
// byte-position seeking. It produces the canonical native DSD form:
// channel-separated bytes, bit 7 earliest in time.
//
// ReadDSD fills dst[c] with the next bytes of channel c. len(dst) must equal
// Channels and every dst[c] must have the same length; n is the number of
// bytes written to each channel. Positions are whole DSD bytes per channel
// (eight one-bit samples). io.EOF means the stream ended at its declared
// length; premature EOF is a *Refusal with ReasonTruncated, and as with
// Decoder, n > 0 may arrive together with that error.
type DSDDecoder interface {
	Format() format.NativeDSD
	ReadDSD(ctx context.Context, dst [][]byte) (n int, err error)
	// Seek positions the decoder so the next ReadDSD returns byte pos of
	// each channel (0-based). pos may equal the length; past it is an error.
	Seek(ctx context.Context, pos int64) error
	Close() error
}

// Source is what Open returns: a Decoder for PCM content or a DSDDecoder for
// DSD content. Callers type-switch on it.
type Source interface {
	Close() error
}

// Open inspects rs's content and returns a Decoder or a DSDDecoder, or a
// *Refusal. The caller retains ownership of rs and must keep it readable for
// the life of the Source; Close does not close rs.
func Open(rs io.ReadSeeker) (Source, error) {
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	res, err := sniff(rs)
	if err != nil {
		return nil, err
	}
	view := rs
	if res.start > 0 {
		os, err := newOffsetSeeker(rs, res.start)
		if err != nil {
			return nil, err
		}
		view = os
	} else if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	switch res.kind {
	case sniffFLAC:
		return openFLAC(view)
	case sniffWAV:
		return openWAV(view)
	case sniffAIFF:
		return openAIFF(view)
	case sniffALAC:
		return nil, refuse(ReasonNotYetSupported, "ALAC is not enabled in this release")
	case sniffDSF:
		return openDSF(view)
	case sniffDFF:
		return openDFF(view)
	case sniffMP3:
		return nil, refuse(ReasonLossy, "MP3 is refused; the bridge player is bit-exact lossless only")
	case sniffAAC:
		return nil, refuse(ReasonLossy, "AAC is refused; the bridge player is bit-exact lossless only")
	case sniffOgg:
		return nil, refuse(ReasonLossy, "Ogg/Opus/Vorbis is refused; the bridge player is bit-exact lossless only")
	default:
		return nil, refuse(ReasonUnknownContainer, "unrecognised audio container")
	}
}
