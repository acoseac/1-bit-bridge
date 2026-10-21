package alsapcm

// ring is the sink's frame store, preallocated at open: the frames the
// producer has submitted and the writer has not written yet (the
// lookahead), and behind them the last keep frames written, so that a
// stream released while paused can be given again what the DAC had not
// played. Indexes count content frames from the sink's open, and a frame's
// bytes sit at its index modulo the capacity.
//
// The writer only reads frames from wr to sub, and the producer only
// writes frames from sub on, into the free part, so neither touches bytes
// the other uses; the indexes move under the sink's lock.
type ring struct {
	buf        []byte
	frameBytes int
	frames     int64 // the capacity
	keep       int64 // the frames written that the store keeps behind wr
	base       int64 // the oldest frame kept
	wr         int64 // the next frame to write to the kernel
	sub        int64 // the next frame the producer submits
}

// newRing allocates a store of frames frames.
func newRing(frames int64, frameBytes int, keep int64) ring {
	return ring{buf: make([]byte, frames*int64(frameBytes)), frameBytes: frameBytes, frames: frames, keep: keep}
}

// free is the frames the producer may submit now.
func (r *ring) free() int64 { return r.frames - (r.sub - r.base) }

// pending is the frames submitted and not yet written.
func (r *ring) pending() int64 { return r.sub - r.wr }

// span is the bytes of up to n frames from index i, as far as the end of
// the buffer.
func (r *ring) span(i, n int64) []byte {
	off := i % r.frames
	n = min(n, r.frames-off)
	fb := int64(r.frameBytes)
	return r.buf[off*fb : (off+n)*fb]
}

// put copies whole frames into the store from index i on, across the end
// of the buffer.
func (r *ring) put(i int64, b []byte) {
	for len(b) > 0 {
		n := copy(r.span(i, int64(len(b)/r.frameBytes)), b)
		i += int64(n / r.frameBytes)
		b = b[n:]
	}
}

// written records n frames written from wr, and lets go of the frames that
// fall out of those kept.
func (r *ring) written(n int64) {
	r.wr += n
	r.base = max(r.base, r.wr-r.keep)
}

// maxSegments bounds a run's record of content and silence. A pause by
// zeros adds a segment of silence and its resume one of content; a segment
// the kernel has played is folded away, so only the pauses within one
// hardware buffer count against it.
const maxSegments = 64

// segment is a stretch of frames written in one run: content or silence.
type segment struct {
	frames  int64
	silence bool
}

// run is what the kernel has been given since the stream was last
// prepared, which its pointers and its delay count from: content and
// silence in the order written. base is the content frame the run began
// with, so the content played is base plus the content among the frames
// the kernel has played. It is the writer's alone.
type run struct {
	base    int64
	written int64 // frames written in the run, content and silence
	content int64 // content frames written in the run
	segs    [maxSegments]segment
	n       int
	// foldedFrames and foldedContent count the segments folded away, every
	// frame of which the kernel had played.
	foldedFrames, foldedContent int64
}

// room is the segments still free.
func (r *run) room() int { return maxSegments - r.n }

// add records n frames written, content or silence, and reports false,
// recording nothing, when a new segment does not fit. A pause by zeros
// reserves the two segments its silence and its resume take, so a writer
// never meets that.
func (r *run) add(n int64, silence bool) bool {
	if n <= 0 {
		return true
	}
	if r.n > 0 && r.segs[r.n-1].silence == silence {
		r.segs[r.n-1].frames += n
	} else {
		if r.n == maxSegments {
			return false
		}
		r.segs[r.n] = segment{frames: n, silence: silence}
		r.n++
	}
	r.written += n
	if !silence {
		r.content += n
	}
	return true
}

// contentAmong is the content frames among the first p frames of the run.
func (r *run) contentAmong(p int64) int64 {
	c, at := r.foldedContent, r.foldedFrames
	for _, s := range r.segs[:r.n] {
		if at >= p {
			break
		}
		if !s.silence {
			c += min(s.frames, p-at)
		}
		at += s.frames
	}
	return c
}

// fold folds away the segments wholly among the first p frames, which the
// kernel has played.
func (r *run) fold(p int64) {
	i := 0
	for i < r.n && r.foldedFrames+r.segs[i].frames <= p {
		r.foldedFrames += r.segs[i].frames
		if !r.segs[i].silence {
			r.foldedContent += r.segs[i].frames
		}
		i++
	}
	if i > 0 {
		copy(r.segs[:], r.segs[i:r.n])
		r.n -= i
	}
}
