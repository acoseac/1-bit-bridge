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
type Decoder interface {
	Format() format.PCM
	ReadFrames(ctx context.Context, dst []int32) (n int, err error)
	// Seek positions the decoder so the next ReadFrames returns sample frame
	// frame (0-based). Seeking past the end is an error.
	Seek(ctx context.Context, frame int64) error
	Close() error
}

// Open inspects rs's content and returns a decoder, or a *Refusal.
// The caller retains ownership of rs and must keep it readable for the life
// of the Decoder; Close does not close rs.
func Open(rs io.ReadSeeker) (Decoder, error) {
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
	case sniffDSF, sniffDFF:
		return nil, refuse(ReasonNotYetSupported, "DSD containers are not enabled in this release")
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
