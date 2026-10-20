package decode

import (
	"context"
	"encoding/binary"
	"io"
	"math"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// maxSeekPointField is the largest SEEKTABLE sample/offset we will use.
// Larger uint64 values cannot be added to dataStart as int64 without wrap.
const maxSeekPointField = uint64(math.MaxInt64 / 2)

// flacDecoder decodes integer FLAC via mewkiz frame parsing, with its own
// metadata walk and seeking.
//
// mewkiz Stream.Seek is not used: its SampleNumber() multiplies the frame
// number by the *current* BlockSize (wrong for the short final block under
// fixed blocking), and NewSeek's makeSeekTable / PICTURE Parse are unbounded
// and uncancellable. Seeking here uses an optional bounded SEEKTABLE or a
// cancellable forward scan, and sample positions use STREAMINFO's nominal
// block size for fixed-blocksize streams.
type flacDecoder struct {
	rs         io.ReadSeeker
	info       *meta.StreamInfo
	dataStart  int64
	pcm        format.PCM
	seekPoints []meta.SeekPoint
	posFrame   int64
	// nextSample is the absolute sample index of the next frame at the
	// current rs offset. Positioning accumulates BlockSize rather than
	// trusting frame-header sample numbers: a short final block makes
	// STREAMINFO min≠max, and mewkiz SampleNumber() then multiplies the
	// frame number by that short BlockSize (Argon H4 / Codex H4/M9).
	nextSample   int64
	pending      []int32
	pendingFrame int64
	closed       bool
}

func openFLAC(rs io.ReadSeeker) (Decoder, error) {
	info, points, dataStart, err := parseFLACMetadata(rs)
	if err != nil {
		return nil, err
	}
	bits := int(info.BitsPerSample)
	if bits == 32 {
		return nil, refuse(ReasonNotYetSupported, "32-bit FLAC is not supported with mewkiz/flac v1.0.14 (frame sample-size code 111)")
	}
	if err := acceptPCMShape(int(info.NChannels), bits); err != nil {
		return nil, err
	}
	if err := acceptRate(int(info.SampleRate)); err != nil {
		return nil, err
	}
	d := &flacDecoder{
		rs:         rs,
		info:       info,
		dataStart:  dataStart,
		seekPoints: points,
		pcm: format.PCM{
			RateHz:    int(info.SampleRate),
			Channels:  int(info.NChannels),
			ValidBits: bits,
			Container: format.ContainerFLAC,
			Frames:    int64(info.NSamples), // 0 means unknown length
		},
	}
	if _, err := rs.Seek(dataStart, io.SeekStart); err != nil {
		return nil, err
	}
	return d, nil
}

// parseFLACMetadata walks metadata with size caps. PICTURE and other large
// bodies are skipped by seeking (no allocation of declared image data).
func parseFLACMetadata(rs io.ReadSeeker) (*meta.StreamInfo, []meta.SeekPoint, int64, error) {
	var sig [4]byte
	if _, err := io.ReadFull(rs, sig[:]); err != nil {
		return nil, nil, 0, refuse(ReasonTruncated, "FLAC header truncated")
	}
	if string(sig[:]) != "fLaC" {
		return nil, nil, 0, refuse(ReasonUnknownContainer, "invalid FLAC signature")
	}
	var (
		info    *meta.StreamInfo
		points  []meta.SeekPoint
		isLast  bool
		nBlocks int
	)
	for !isLast {
		nBlocks++
		if nBlocks > 256 {
			return nil, nil, 0, refuse(ReasonUnsupportedLayout, "FLAC metadata block count exceeds bound")
		}
		var hdr [4]byte
		if _, err := io.ReadFull(rs, hdr[:]); err != nil {
			return nil, nil, 0, refuse(ReasonTruncated, "FLAC metadata header truncated")
		}
		isLast = hdr[0]&0x80 != 0
		typ := meta.Type(hdr[0] & 0x7f)
		length := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		if length < 0 {
			return nil, nil, 0, refuse(ReasonTruncated, "FLAC metadata length invalid")
		}
		switch typ {
		case meta.TypeStreamInfo:
			if info != nil {
				return nil, nil, 0, refuse(ReasonUnsupportedLayout, "FLAC has more than one STREAMINFO")
			}
			if length != 34 {
				return nil, nil, 0, refuse(ReasonTruncated, "FLAC STREAMINFO length %d, want 34", length)
			}
			body := make([]byte, 34)
			if _, err := io.ReadFull(rs, body); err != nil {
				return nil, nil, 0, refuse(ReasonTruncated, "FLAC STREAMINFO truncated")
			}
			si, err := parseStreamInfoBody(body)
			if err != nil {
				return nil, nil, 0, err
			}
			info = si
		case meta.TypeSeekTable:
			if length%18 != 0 {
				return nil, nil, 0, refuse(ReasonTruncated, "FLAC SEEKTABLE length %d is not a multiple of 18", length)
			}
			if length > maxFLACMetaBody {
				return nil, nil, 0, refuse(ReasonUnsupportedLayout, "FLAC SEEKTABLE size %d exceeds bound", length)
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(rs, body); err != nil {
				return nil, nil, 0, refuse(ReasonTruncated, "FLAC SEEKTABLE truncated")
			}
			n := int(length / 18)
			points = make([]meta.SeekPoint, 0, n)
			for i := 0; i < n; i++ {
				off := i * 18
				sampleNum := binary.BigEndian.Uint64(body[off:])
				if sampleNum == meta.PlaceholderPoint {
					continue
				}
				offset := binary.BigEndian.Uint64(body[off+8:])
				// Ignore points that cannot be represented safely as int64
				// offsets from dataStart (Argon R3 H1).
				if sampleNum > maxSeekPointField || offset > maxSeekPointField {
					continue
				}
				points = append(points, meta.SeekPoint{
					SampleNum: sampleNum,
					Offset:    offset,
					NSamples:  binary.BigEndian.Uint16(body[off+16:]),
				})
			}
		default:
			// Skip without allocating the body (PICTURE may declare hundreds of MiB).
			if _, err := rs.Seek(length, io.SeekCurrent); err != nil {
				return nil, nil, 0, refuse(ReasonTruncated, "FLAC metadata skip failed: %v", err)
			}
		}
	}
	if info == nil {
		return nil, nil, 0, refuse(ReasonTruncated, "FLAC missing STREAMINFO")
	}
	dataStart, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, nil, 0, err
	}
	return info, points, dataStart, nil
}

func parseStreamInfoBody(b []byte) (*meta.StreamInfo, error) {
	if len(b) != 34 {
		return nil, refuse(ReasonTruncated, "FLAC STREAMINFO body length %d", len(b))
	}
	si := &meta.StreamInfo{}
	si.BlockSizeMin = binary.BigEndian.Uint16(b[0:2])
	si.BlockSizeMax = binary.BigEndian.Uint16(b[2:4])
	si.FrameSizeMin = uint32(b[4])<<16 | uint32(b[5])<<8 | uint32(b[6])
	si.FrameSizeMax = uint32(b[7])<<16 | uint32(b[8])<<8 | uint32(b[9])
	// bytes 10-17: 20-bit rate | 3-bit (channels-1) | 5-bit (bits-1) | 36-bit samples.
	v := binary.BigEndian.Uint64(b[10:18])
	si.SampleRate = uint32(v >> 44)
	si.NChannels = uint8((v>>41)&0x7) + 1
	si.BitsPerSample = uint8((v>>36)&0x1f) + 1
	si.NSamples = v & 0xFFFFFFFFF
	copy(si.MD5sum[:], b[18:34])
	if si.BlockSizeMin < 16 {
		return nil, refuse(ReasonTruncated, "FLAC invalid minimum block size %d", si.BlockSizeMin)
	}
	return si, nil
}

func (d *flacDecoder) Format() format.PCM { return d.pcm }

func (d *flacDecoder) Close() error {
	d.closed = true
	d.pending = nil
	return nil
}

func (d *flacDecoder) Seek(ctx context.Context, frame int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed {
		return refuse(ReasonTruncated, "FLAC decoder closed")
	}
	if frame < 0 {
		return refuse(ReasonUnsupportedLayout, "FLAC seek frame %d out of range", frame)
	}
	if d.pcm.Frames > 0 && frame > d.pcm.Frames {
		return refuse(ReasonUnsupportedLayout, "FLAC seek frame %d out of range (0..%d)", frame, d.pcm.Frames)
	}
	if d.pcm.Frames > 0 && frame == d.pcm.Frames {
		d.pending = nil
		d.posFrame = frame
		return nil
	}
	// Unknown length (Frames==0): Seek(0) must rewind; other seeks scan.
	if frame == 0 {
		if _, err := d.rs.Seek(d.dataStart, io.SeekStart); err != nil {
			return err
		}
		d.pending = nil
		d.pendingFrame = 0
		d.posFrame = 0
		d.nextSample = 0
		return nil
	}
	startOff := d.dataStart
	startSample := int64(0)
	if p, ok := seekPointBefore(d.seekPoints, uint64(frame)); ok {
		startOff = d.dataStart + int64(p.Offset)
		startSample = int64(p.SampleNum)
	}
	if _, err := d.rs.Seek(startOff, io.SeekStart); err != nil {
		return err
	}
	d.pending = nil
	d.nextSample = startSample
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		off, err := d.rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		first := d.nextSample
		f, err := d.parseFrame(ctx)
		if err == io.EOF {
			return refuse(ReasonTruncated, "FLAC seek past end of stream")
		}
		if err != nil {
			return err
		}
		next := first + int64(f.BlockSize)
		d.nextSample = next
		if next > frame {
			// Rewind to frame start and decode into pending from target.
			if _, err := d.rs.Seek(off, io.SeekStart); err != nil {
				return err
			}
			d.nextSample = first
			if err := d.fillPending(ctx); err != nil {
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
	}
}

// seekPointBefore returns the seek point with the greatest SampleNum that is
// still <= sample. Points after the target, overflow-sized fields, and empty
// tables yield ok=false so the caller scans from the stream start.
func seekPointBefore(points []meta.SeekPoint, sample uint64) (meta.SeekPoint, bool) {
	var best meta.SeekPoint
	found := false
	for _, p := range points {
		if p.SampleNum > sample {
			continue
		}
		if p.Offset > maxSeekPointField || p.SampleNum > maxSeekPointField {
			continue
		}
		if !found || p.SampleNum >= best.SampleNum {
			best = p
			found = true
		}
	}
	return best, found
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
		if d.pcm.Frames > 0 && d.posFrame >= d.pcm.Frames {
			return out, io.EOF
		}
		if len(d.pending) == 0 {
			if err := d.fillPending(ctx); err != nil {
				if err == io.EOF {
					if d.pcm.Frames > 0 && d.posFrame < d.pcm.Frames {
						if out > 0 {
							return out, refuse(ReasonTruncated, "FLAC ends before declared sample count")
						}
						return 0, refuse(ReasonTruncated, "FLAC ends before declared sample count")
					}
					if out > 0 {
						return out, io.EOF
					}
					return 0, io.EOF
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
	first := d.nextSample
	f, err := d.parseFrame(ctx)
	if err != nil {
		return err
	}
	n := len(f.Subframes[0].Samples)
	for _, sf := range f.Subframes {
		if len(sf.Samples) != n {
			return refuse(ReasonTruncated, "FLAC subframe length mismatch")
		}
	}
	if int(f.BlockSize) != n {
		return refuse(ReasonTruncated, "FLAC subframe length %d disagrees with block size %d", n, f.BlockSize)
	}
	if d.pcm.Frames > 0 && first+int64(n) > d.pcm.Frames {
		return refuse(ReasonUnsupportedLayout, "FLAC frame sample range exceeds STREAMINFO total")
	}
	// Reject samples outside the advertised valid-bit range.
	maxMag := int32(1) << (d.pcm.ValidBits - 1)
	interleaved := make([]int32, n*d.pcm.Channels)
	for i := 0; i < n; i++ {
		for c := 0; c < d.pcm.Channels; c++ {
			s := f.Subframes[c].Samples[i]
			if s < -maxMag || s >= maxMag {
				return refuse(ReasonTruncated, "FLAC sample exceeds STREAMINFO bit depth")
			}
			interleaved[i*d.pcm.Channels+c] = s
		}
	}
	d.pending = interleaved
	d.pendingFrame = first
	d.nextSample = first + int64(n)
	return nil
}

func (d *flacDecoder) parseFrame(ctx context.Context) (*frame.Frame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Bind the context into every byte read so mewkiz frame.New/Parse cannot
	// keep scanning after cancellation (Argon H4 / Codex M5).
	rs := &ctxReadSeeker{rs: d.rs, ctx: ctx}
	f, err := frame.New(rs)
	if err == io.EOF {
		return nil, io.EOF
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return nil, err
	}
	if err != nil {
		return nil, refuse(ReasonTruncated, "FLAC frame header failed: %v", err)
	}
	// Resolve inherited (zero) fields from STREAMINFO before subframe decode.
	if f.BitsPerSample == 0 {
		f.BitsPerSample = d.info.BitsPerSample
	}
	if f.SampleRate == 0 {
		f.SampleRate = d.info.SampleRate
	}
	if int(f.BitsPerSample) != d.pcm.ValidBits {
		return nil, refuse(ReasonUnsupportedLayout, "FLAC frame bit depth %d disagrees with STREAMINFO %d", f.BitsPerSample, d.pcm.ValidBits)
	}
	if int(f.SampleRate) != d.pcm.RateHz {
		return nil, refuse(ReasonUnsupportedLayout, "FLAC frame rate %d disagrees with STREAMINFO %d", f.SampleRate, d.pcm.RateHz)
	}
	if f.Channels.Count() != d.pcm.Channels {
		return nil, refuse(ReasonTruncated, "FLAC frame has %d channels, want %d", f.Channels.Count(), d.pcm.Channels)
	}
	if err := f.Parse(); err != nil {
		if err == context.Canceled || err == context.DeadlineExceeded {
			return nil, err
		}
		return nil, refuse(ReasonTruncated, "FLAC frame decode failed: %v", err)
	}
	if len(f.Subframes) != d.pcm.Channels {
		return nil, refuse(ReasonTruncated, "FLAC frame has %d subframes, want %d", len(f.Subframes), d.pcm.Channels)
	}
	return f, nil
}

// ctxReadSeeker forwards Seek to the underlying reader and aborts Read when
// ctx is done, so uncancellable third-party parsers stop promptly.
type ctxReadSeeker struct {
	rs  io.ReadSeeker
	ctx context.Context
}

func (c *ctxReadSeeker) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.rs.Read(p)
}

func (c *ctxReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return c.rs.Seek(offset, whence)
}
