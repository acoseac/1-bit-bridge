package alsapcm

import (
	"context"
	"errors"
)

// openNativeBounded transfers ownership only through the unbuffered reply.
// If cancellation wins while open/admission is parked, the opening worker owns
// the eventual sink and its cleanup. The caller must not open a successor.
func openNativeBounded(ctx context.Context, c DSDConfig, h nativeSeams) (*DSDSink, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	type result struct {
		s *DSDSink
		e error
	}
	reply := make(chan result)
	go func() {
		s, e := openDSDSink(ctx, c, h)
		select {
		case reply <- result{s, e}:
		case <-ctx.Done():
			if s != nil {
				s.Revoke()
				<-s.Done()
			}
		}
	}()
	select {
	case r := <-reply:
		return r.s, r.e
	case <-ctx.Done():
		return nil, errors.Join(ErrReleaseUnconfirmed, ctx.Err())
	}
}
