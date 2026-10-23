package playback

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The reasons the backend refuses an occurrence before any of its content
// played, beside the decoders' own (decode.Reason, passed on as they are:
// float_pcm_refused, unknown_container, lossy_refused, unsupported_bit_depth
// and the rest) and engine.ReasonMissing for content the resolver could not
// open.
const (
	// ReasonDSD is DSD content, whatever the DSD mode: native DSD and DoP
	// are later releases (B470 PRs 2c and 2d), and DSD is never converted.
	ReasonDSD engine.Reason = "dsd_not_in_this_release"
	// ReasonRate is a rate the ALSA backend does not open
	// (alsapcm.MinRateHz to MaxRateHz), which the decoders take from 1 Hz.
	ReasonRate engine.Reason = "rate_not_supported"
	// ReasonTransport is content the DAC takes in none of the packings its
	// depth has, at its rate (packings): the DAC does not play it as it is.
	ReasonTransport engine.Reason = "transport_not_supported"
	// ReasonEmpty is content with no audio frames.
	ReasonEmpty engine.Reason = "no_audio_frames"
	// ReasonUnreadable is content that could not be read at its open for a
	// reason the decoders do not type (an I/O error).
	ReasonUnreadable engine.Reason = "unreadable"
)

// chunkFrames is the frames the producer decodes and hands the sink at a
// time.
const chunkFrames = 4096

// occ is one occurrence the engine handed the session, in play order.
type occ struct {
	id    engine.OccurrenceID
	entry uint32
	ref   engine.Reference
	pos   int // its place in the session's chain

	// The open. done is closed once the open has a decoder or a refusal;
	// until then the opener owns what it opened. cancel ends the open.
	done   chan struct{}
	cancel context.CancelFunc
	// Set by the open, read once done is closed.
	content Content
	dec     decode.Decoder
	pcm     format.PCM
	refused *engine.Unplayable
	// first is the first chunk, decoded at the open so content that cannot
	// give a frame is refused before any of it plays; firstErr is an error
	// that came with it, which applies after it (decode.Decoder's n > 0
	// with an error).
	first    []int32
	firstErr error

	// Under the session's lock from here.
	transport alsapcm.Transport // what it plays as
	join      engine.Join       // how it follows the occurrence before it
	out       *output           // the sink its frames went to
	start     int64             // its first frame's index in that sink's content frames
	written   int64             // its frames the sink took
	eof       bool              // every frame of it is in its sink
	dropped   bool              // cut, replaced or refused: never to be written again
	crossed   bool              // the played content has crossed its first frame
	closed    bool              // its decoder and content are closed

	// What the reporter has reported of it.
	repPrepared, repRefused, repSubmitted, repAudible bool
}

// opened reports whether the open has finished.
func (o *occ) opened() bool {
	select {
	case <-o.done:
		return true
	default:
		return false
	}
}

// open opens o's content and its decoder and decodes its first chunk, or
// refuses it: everything that is not stereo integer PCM at a rate the ALSA
// backend opens, with its kind's reason, before any of it plays. It owns
// what it opens until it returns, and leaves it on o.
func (b *Backend) open(ctx context.Context, o *occ) {
	refuse := func(reason engine.Reason, detail string) {
		o.refused = &engine.Unplayable{Reason: reason, Detail: detail}
	}
	c, err := b.cfg.Resolver.Open(ctx, o.ref)
	if err != nil {
		var u *engine.Unplayable
		if errors.As(err, &u) {
			o.refused = &engine.Unplayable{Reason: u.Reason, Detail: u.Detail}
			return
		}
		refuse(engine.ReasonMissing, err.Error())
		return
	}
	o.content = c
	if c.Data == nil || c.Size <= 0 {
		refuse(ReasonEmpty, fmt.Sprintf("the content is %d bytes", c.Size))
		return
	}
	src, err := decode.Open(io.NewSectionReader(c.Data, 0, c.Size))
	if err != nil {
		refuseDecode(o, err)
		return
	}
	d, ok := src.(decode.Decoder)
	if !ok {
		_ = src.Close()
		refuse(ReasonDSD, "native DSD and DoP are later releases, and DSD is never converted to PCM")
		return
	}
	o.dec = d
	pcm := d.Format()
	o.pcm = pcm
	switch {
	case pcm.Validate() != nil:
		refuse(engine.ReasonRefusedContent, pcm.Validate().Error())
		return
	case pcm.RateHz < alsapcm.MinRateHz || pcm.RateHz > alsapcm.MaxRateHz:
		refuse(ReasonRate, fmt.Sprintf("%d Hz is outside the %d to %d Hz the ALSA backend opens", pcm.RateHz, alsapcm.MinRateHz, alsapcm.MaxRateHz))
		return
	case len(packings[pcm.ValidBits]) == 0:
		refuse(engine.ReasonRefusedContent, fmt.Sprintf("%d valid bits has no packing", pcm.ValidBits))
		return
	}
	buf := make([]int32, chunkFrames*pcm.Channels)
	n, err := d.ReadFrames(ctx, buf)
	switch {
	case n > 0:
		o.first = buf[:n]
		if err != nil && !errors.Is(err, io.EOF) {
			o.firstErr = err
		} else if errors.Is(err, io.EOF) {
			o.firstErr = io.EOF
		}
	case errors.Is(err, io.EOF):
		refuse(ReasonEmpty, "the decoder found no audio frames")
	case err != nil && ctx.Err() != nil:
		refuse(engine.ReasonMissing, "the open was cancelled: "+err.Error())
	case err != nil:
		refuseDecode(o, err)
	default:
		refuse(ReasonEmpty, "the decoder gave no frames and no error")
	}
}

// refuseDecode refuses o with a decoder's refusal, by its own reason, or an
// I/O error as unreadable.
func refuseDecode(o *occ, err error) {
	if r, ok := decode.AsRefusal(err); ok {
		o.refused = &engine.Unplayable{Reason: engine.Reason(r.Reason), Detail: r.Message}
		return
	}
	o.refused = &engine.Unplayable{Reason: ReasonUnreadable, Detail: err.Error()}
}

// closeOcc closes o's decoder and content, once, and lets go of them and of
// the buffers they hold. The caller owns them: the opener, the producer
// writing o, or a session that holds no other owner of them (an occurrence
// opened and not taken, the teardown).
func closeOcc(o *occ) {
	if o.closed {
		return
	}
	o.closed = true
	if o.dec != nil {
		_ = o.dec.Close()
	}
	if o.content.Closer != nil {
		_ = o.content.Closer.Close()
	}
	o.dec, o.content, o.first = nil, Content{}, nil
}
