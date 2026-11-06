package dsdconv

import (
	"context"
	"errors"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

var (
	// ErrChannels refuses a source that is not stereo.
	ErrChannels = errors.New("dsdconv: the converter takes stereo DSD only")
	// ErrSeek is Seek's answer to any frame but 0 before the first read:
	// converted DSD is not positioned in time (B728 §4.7).
	ErrSeek = errors.New("dsdconv: converted DSD is not positioned in time")
	// ErrWindow is ReadFrames' answer at a held occurrence's window point:
	// the frames before it are out, and the last Window frames wait for
	// Continue or End.
	ErrWindow = errors.New("dsdconv: the occurrence's last frames wait for what follows it")
	// ErrClosed is every call's answer after Close.
	ErrClosed = errors.New("dsdconv: the converter is closed")
	// ErrDetached is a successor's answer once its predecessor went back to
	// its window (Rewind): it follows nothing now, and plays no more.
	ErrDetached = errors.New("dsdconv: the occurrence before this one went back to its window")
	// ErrJoin refuses a Continue that is not a continuous join, or one asked
	// in a state that cannot take it.
	ErrJoin = errors.New("dsdconv: not a continuous join here")
)

var errUnbuilt = errors.New("dsdconv: not built yet")

// Options are a converter's own, fixed at New.
type Options struct {
	// Hold stops ReadFrames at the window point of an occurrence that can be
	// continued (its Frames a multiple of D, at least Window frames long):
	// ErrWindow, until Continue or End.
	Hold bool
	// First is the bytes of each channel already read from the source, which
	// the backend's open reads to prove the content gives bits: the
	// converter copies them at New and reads the source after them.
	First [][]byte
}

// Converter is one occurrence's conversion: a decode.Decoder over its
// source. It owns the source from New on and closes it once.
type Converter struct {
	src  decode.DSDDecoder
	plan Plan
}

// New makes a converter of src's DSD to plan's PCM. It refuses a source
// that is not stereo (ErrChannels), a rate but DSD64, DSD128 and DSD256
// (*RateError), a negative Frames and a plan that is not one of Plans, and
// allocates everything its reads use.
func New(src decode.DSDDecoder, plan Plan, opt Options) (*Converter, error) {
	return nil, errUnbuilt
}

// Format is the converted stream: the plan's rate and valid bits, stereo,
// ContainerConverted, and floor(Frames/D) frames.
func (c *Converter) Format() format.PCM { return format.PCM{} }

// Source is the DSD stream converted, as its source declares it.
func (c *Converter) Source() format.NativeDSD { return format.NativeDSD{} }

// Plan is the converter's plan.
func (c *Converter) Plan() Plan { return c.plan }

// Window is H at the source's rate under the plan.
func (c *Converter) Window() int { return 0 }

// ReadFrames converts the next frames into dst, per decode.Decoder.
func (c *Converter) ReadFrames(ctx context.Context, dst []int32) (int, error) {
	return 0, errUnbuilt
}

// Seek refuses every frame but 0 before the first read (ErrSeek).
func (c *Converter) Seek(ctx context.Context, frame int64) error { return errUnbuilt }

// Close closes the source, once.
func (c *Converter) Close() error { return errUnbuilt }

// Emitted is the frames of this occurrence ReadFrames has given.
func (c *Converter) Emitted() int64 { return -1 }

// Clipped is the samples of this occurrence saturated at full scale.
func (c *Converter) Clipped() int64 { return -1 }

// Holding reports whether the converter waits at its window point.
func (c *Converter) Holding() bool { return false }

// Continue continues a held occurrence into next, a continuous successor.
func (c *Converter) Continue(next *Converter) error { return errUnbuilt }

// End ends a held occurrence: its window is zero-padded.
func (c *Converter) End() error { return errUnbuilt }

// Rewind goes back to the window point's snapshot.
func (c *Converter) Rewind() error { return errUnbuilt }

// Continuous reports whether succ follows pred in a continuous join.
func Continuous(pred format.NativeDSD, predPlan Plan, succ format.NativeDSD, succPlan Plan) bool {
	return false
}
