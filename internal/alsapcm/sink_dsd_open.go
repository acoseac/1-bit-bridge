package alsapcm

import (
	"context"
	"errors"
)

// nativeOpenPending retains the completion fence when cancellation wins before
// ownership transfers. Completion includes admission and eventual cleanup.
type nativeOpenPending struct {
	cause  error
	done   <-chan struct{}
	result *error
}

func (e *nativeOpenPending) Error() string {
	return errors.Join(ErrReleaseUnconfirmed, e.cause).Error()
}
func (e *nativeOpenPending) Unwrap() []error             { return []error{ErrReleaseUnconfirmed, e.cause} }
func (e *nativeOpenPending) Completion() <-chan struct{} { return e.done }
func (e *nativeOpenPending) CompletionResult() error     { <-e.done; return *e.result }

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
	done := make(chan struct{})
	var completed error
	go func() {
		defer close(done)
		var s *DSDSink
		var e, unhanded error
		sent := false
		defer func() {
			// A panic anywhere on the worker is its open's answer, never the
			// process's end (B728 B12 PR 0); a sink made and not delivered is
			// revoked, woken past a revocation the panic cut, and waited for.
			if r := recover(); r != nil {
				e = nativeCleanupError(nativePanicked(c.Log, "the native sink's opener", c.Endpoint, r), unhanded)
				if s != nil && !sent {
					s.Revoke()
					s.mu.Lock()
					s.interrupt()
					s.mu.Unlock()
					<-s.Done()
					e = errors.Join(e, s.result())
				}
				if completed = errors.Join(completed, e); !sent {
					select {
					case reply <- result{nil, e}:
					case <-ctx.Done():
					}
				}
			}
		}()
		h.unhanded = &unhanded
		s, e = openDSDSink(ctx, c, h)
		completed = e
		select {
		case reply <- result{s, e}:
			if sent = true; h.delivered != nil {
				h.delivered()
			}
		case <-ctx.Done():
			if s != nil {
				s.Revoke()
				<-s.Done()
				completed = errors.Join(completed, s.result())
			}
		}
	}()
	select {
	case r := <-reply:
		return r.s, r.e
	case <-ctx.Done():
		return nil, &nativeOpenPending{cause: ctx.Err(), done: done, result: &completed}
	}
}
