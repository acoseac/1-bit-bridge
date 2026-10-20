package decode

import (
	"context"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
	flac "github.com/mewkiz/flac"
)

// flacDecoder wraps mewkiz/flac v1.0.14 integer subframe decoding.
//
// Seek inspection (PR 1a): Stream.Seek uses an embedded SEEKTABLE when
// present; otherwise makeSeekTable walks every frame once and builds a full
// point list. It does not bisect on unvalidated frame sync. For this PR's
// short goldens that is enough; a bounded lazy sparse index is deferred until
// measurement on Pi storage shows it is required. Exact sample-frame seeks
// discard samples inside the frame Seek returns to the start of.
type flacDecoder struct {
	rs       io.ReadSeeker
	stream   *flac.Stream
	pcm      format.PCM
	posFrame int64
	// pending holds interleaved samples already decoded but not yet returned.
	pending []int32
	// pendingFrame is the absolute frame index of pending[0]/channels].
	pendingFrame int64
	closed       bool
}

func openFLAC(rs io.ReadSeeker) (Decoder, error) {
	stream, err := flac.NewSeek(rs)
	if err != nil {
		if IsRefusal(err) {
			return nil, err
		}
		return nil, refuse(ReasonTruncated, "FLAC open failed: %v", err)
	}
	info := stream.Info
	if info == nil {
		return nil, refuse(ReasonTruncated, "FLAC missing STREAMINFO")
	}
	bits := int(info.BitsPerSample)
	// mewkiz/flac v1.0.14 still treats frame sample-size code 111 as reserved,
	// which modern flac(1) emits for 32-bit streams. Refuse up front rather
	// than fail mid-decode; 32-bit WAV/AIFF remain supported.
	if bits == 32 {
		return nil, refuse(ReasonNotYetSupported, "32-bit FLAC is not supported with mewkiz/flac v1.0.14 (frame sample-size code 111)")
	}
	if err := acceptPCMShape(int(info.NChannels), bits); err != nil {
		return nil, err
	}
	frames := int64(info.NSamples)
	return &flacDecoder{
		rs:     rs,
		stream: stream,
		pcm: format.PCM{
			RateHz:    int(info.SampleRate),
			Channels:  int(info.NChannels),
			ValidBits: int(info.BitsPerSample),
			Container: format.ContainerFLAC,
			Frames:    frames,
		},
	}, nil
}

func (d *flacDecoder) Format() format.PCM { return d.pcm }

func (d *flacDecoder) Close() error {
	d.closed = true
	d.pending = nil
	d.stream = nil
	return nil
}

func (d *flacDecoder) Seek(ctx context.Context, frame int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed {
		return refuse(ReasonTruncated, "FLAC decoder closed")
	}
	if frame < 0 || (d.pcm.Frames > 0 && frame > d.pcm.Frames) {
		return refuse(ReasonUnsupportedLayout, "FLAC seek frame %d out of range (0..%d)", frame, d.pcm.Frames)
	}
	if frame == d.pcm.Frames {
		d.pending = nil
		d.posFrame = frame
		return nil
	}
	// mewkiz Seek positions at the start of the frame containing sampleNum
	// and returns that frame's first sample number.
	first, err := d.stream.Seek(uint64(frame))
	if err != nil {
		return refuse(ReasonTruncated, "FLAC seek failed: %v", err)
	}
	d.pending = nil
	d.posFrame = int64(first)
	// Decode the frame and discard samples before the target.
	if err := d.fillPending(ctx); err != nil && err != io.EOF {
		return err
	}
	skip := int(frame - d.pendingFrame)
	if skip < 0 || skip*d.pcm.Channels > len(d.pending) {
		return refuse(ReasonTruncated, "FLAC seek landed outside decoded frame")
	}
	d.pending = d.pending[skip*d.pcm.Channels:]
	d.pendingFrame = frame
	d.posFrame = frame
	return nil
}

func (d *flacDecoder) ReadFrames(ctx context.Context, dst []int32) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if d.closed {
		return 0, refuse(ReasonTruncated, "FLAC decoder closed")
	}
	if err := checkReadBuf(dst, d.pcm.Channels); err != nil {
		return 0, err
	}
	if d.pcm.Frames > 0 && d.posFrame >= d.pcm.Frames {
		return 0, io.EOF
	}
	out := 0
	for out < len(dst) {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if len(d.pending) == 0 {
			if err := d.fillPending(ctx); err != nil {
				if err == io.EOF && out > 0 {
					return out, io.EOF
				}
				return out, err
			}
		}
		n := copy(dst[out:], d.pending)
		d.pending = d.pending[n:]
		d.pendingFrame += int64(n / d.pcm.Channels)
		out += n
		d.posFrame += int64(n / d.pcm.Channels)
	}
	if d.pcm.Frames > 0 && d.posFrame >= d.pcm.Frames {
		return out, io.EOF
	}
	return out, nil
}

func (d *flacDecoder) fillPending(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := d.stream.ParseNext()
	if err == io.EOF {
		return io.EOF
	}
	if err != nil {
		return refuse(ReasonTruncated, "FLAC frame decode failed: %v", err)
	}
	if len(f.Subframes) != d.pcm.Channels {
		return refuse(ReasonTruncated, "FLAC frame has %d subframes, want %d", len(f.Subframes), d.pcm.Channels)
	}
	n := len(f.Subframes[0].Samples)
	for _, sf := range f.Subframes {
		if len(sf.Samples) != n {
			return refuse(ReasonTruncated, "FLAC subframe length mismatch")
		}
	}
	interleaved := make([]int32, n*d.pcm.Channels)
	for i := 0; i < n; i++ {
		for c := 0; c < d.pcm.Channels; c++ {
			interleaved[i*d.pcm.Channels+c] = f.Subframes[c].Samples[i]
		}
	}
	d.pending = interleaved
	d.pendingFrame = int64(f.SampleNumber())
	return nil
}
