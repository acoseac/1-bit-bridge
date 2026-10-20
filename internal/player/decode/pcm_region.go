package decode

import (
	"context"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// linearRegion reads fixed-size interleaved frames from a payload confined to
// [regionStart, regionEnd). WAV, AIFF and DFF share it; each supplies what a
// frame becomes and its refusal wording.
type linearRegion struct {
	rs          io.ReadSeeker
	regionStart int64
	regionEnd   int64 // exclusive
	frameBytes  int
	// frames is the declared frame count; reads never pass it.
	frames    int64
	posFrame  int64
	byteBuf   []byte
	readStuck error
	// earlyEnd is ReasonTruncated text when the region ends before frames.
	earlyEnd string
	// midFrame is ReasonTruncated text when a read ends mid-frame.
	midFrame string
}

func (r *linearRegion) bytePos() int64 {
	return r.regionStart + r.posFrame*int64(r.frameBytes)
}

func (r *linearRegion) seekToPos() error {
	if _, err := r.rs.Seek(r.bytePos(), io.SeekStart); err != nil {
		r.readStuck = err
		return err
	}
	return nil
}

// seekFrame positions the reader at frame within the region.
// outOfRange and pastEnd are complete refusal messages (no printf verbs).
func (r *linearRegion) seekFrame(ctx context.Context, frame int64, outOfRange, pastEnd string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if frame < 0 || frame > r.frames {
		return refuse(ReasonUnsupportedLayout, "%s", outOfRange)
	}
	off := r.regionStart + frame*int64(r.frameBytes)
	if off > r.regionEnd {
		return refuse(ReasonUnsupportedLayout, "%s", pastEnd)
	}
	if _, err := r.rs.Seek(off, io.SeekStart); err != nil {
		return err
	}
	r.posFrame = frame
	r.readStuck = nil
	return nil
}

// readLinear reads up to framesWant frames and hands each committed run to
// emit (buf holds chunk whole frames; out is how many frames were emitted
// before it in this call). It returns the number of frames emitted.
func (r *linearRegion) readLinear(ctx context.Context, framesWant int, emit func(buf []byte, chunk, out int)) (int, error) {
	if r.readStuck != nil {
		return 0, r.readStuck
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	left := r.frames - r.posFrame
	if left <= 0 {
		return 0, io.EOF
	}
	if int64(framesWant) > left {
		framesWant = int(left)
	}
	out := 0
	for framesWant > 0 {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		chunk := framesPerRead(r.frameBytes, framesWant)
		regionLeft := r.regionEnd - r.bytePos()
		if regionLeft < 0 {
			regionLeft = 0
		}
		maxByRegion := int(regionLeft / int64(r.frameBytes))
		if chunk > maxByRegion {
			chunk = maxByRegion
		}
		if chunk == 0 {
			if r.posFrame < r.frames {
				return out, refuse(ReasonTruncated, "%s", r.earlyEnd)
			}
			return out, io.EOF
		}
		need := chunk * r.frameBytes
		if cap(r.byteBuf) < need {
			r.byteBuf = make([]byte, need)
		} else {
			r.byteBuf = r.byteBuf[:need]
		}
		n, err := io.ReadFull(r.rs, r.byteBuf)
		// Check cancellation after the read: a single chunk can cover the
		// whole dst, so a pre-read check alone would miss a cancel that
		// fires during ReadFull (Codex M8). Bytes already consumed must be
		// seeked back so posFrame and the reader stay aligned (R6).
		if cerr := ctx.Err(); cerr != nil {
			if e := r.seekToPos(); e != nil {
				return out, e
			}
			return out, cerr
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if n == 0 {
				if r.posFrame < r.frames {
					return out, refuse(ReasonTruncated, "%s", r.earlyEnd)
				}
				return out, io.EOF
			}
			whole := n / r.frameBytes
			if whole == 0 || n%r.frameBytes != 0 {
				if e := r.seekToPos(); e != nil {
					return out, e
				}
				return out, refuse(ReasonTruncated, "%s", r.midFrame)
			}
			r.byteBuf = r.byteBuf[:whole*r.frameBytes]
			chunk = whole
			err = refuse(ReasonTruncated, "%s", r.earlyEnd)
		} else if err != nil {
			if e := r.seekToPos(); e != nil {
				return out, e
			}
			return out, err
		} else {
			err = nil
		}
		emit(r.byteBuf, chunk, out)
		out += chunk
		r.posFrame += int64(chunk)
		framesWant -= chunk
		if err != nil {
			return out, err
		}
	}
	if r.posFrame >= r.frames {
		return out, io.EOF
	}
	return out, nil
}

// pcmRegion is a linearRegion whose frames are interleaved PCM samples.
// WAV and AIFF share this path; each supplies unpack and refusal wording.
type pcmRegion struct {
	linearRegion
	pcm format.PCM
	// unpack converts chunk interleaved frames from buf into dst at out
	// (a sample index) and returns the next sample index.
	unpack func(dst []int32, out int, buf []byte, chunk int) int
}

func (r *pcmRegion) Format() format.PCM { return r.pcm }

func (r *pcmRegion) readFrames(ctx context.Context, dst []int32) (int, error) {
	if r.readStuck != nil {
		return 0, r.readStuck
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := checkReadBuf(dst, r.pcm.Channels); err != nil {
		return 0, err
	}
	frames, err := r.readLinear(ctx, len(dst)/r.pcm.Channels, func(buf []byte, chunk, out int) {
		r.unpack(dst, out*r.pcm.Channels, buf, chunk)
	})
	return frames * r.pcm.Channels, err
}
