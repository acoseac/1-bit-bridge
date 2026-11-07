package acoustid

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
)

// A completed answer, re-admitted (backlog B617, round 3, point 3): a lookup
// whose headers the client's switch admitted, and whose body completes after
// it turns off, is no answer, never the no-match a sweep persists, whatever
// context the lookup carries.

// bodyBarrier is a base transport whose answers call before as their body's
// first Read or its Close begins, which the client's answer admission has
// passed by then.
type bodyBarrier struct {
	base   http.RoundTripper
	before func()
}

func (b bodyBarrier) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &barrierBody{ReadCloser: resp.Body, before: b.before}
	return resp, nil
}

// barrierBody calls before once, as its first Read or its Close begins: an
// error branch can close a body it never reads.
type barrierBody struct {
	io.ReadCloser
	once   sync.Once
	before func()
}

func (b *barrierBody) Read(p []byte) (int, error) {
	b.once.Do(b.before)
	return b.ReadCloser.Read(p)
}

func (b *barrierBody) Close() error {
	b.once.Do(b.before)
	return b.ReadCloser.Close()
}

// TestANoMatchCompletedAfterTheFlipIsNoAnswer: AcoustID's no-match, its body
// held at a barrier past the answer's admission while the switch turns off,
// comes back as ErrOff, never ErrNoMatch, on a pass's context and on one
// that carries no switch; the switch is asked once an hour, so no tick
// refuses it.
func TestANoMatchCompletedAfterTheFlipIsNoAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok","results":[]}`)
	}))
	t.Cleanup(srv.Close)
	for _, gated := range []bool{true, false} {
		kind := "a context that carries no switch"
		if gated {
			kind = "a pass's context"
		}
		t.Run(kind, func(t *testing.T) {
			var on atomic.Bool
			on.Store(true)
			tr := &http.Transport{}
			t.Cleanup(tr.CloseIdleConnections)
			hc := &http.Client{Timeout: 10 * time.Second, Transport: bodyBarrier{base: tr, before: func() { on.Store(false) }}}
			c := NewClient(srv.URL, testKey, "ua", hc, enrichgate.Enrichment(on.Load).Every(time.Hour))
			ctx := context.Background()
			if gated {
				var release context.CancelFunc
				ctx, release = enrichgate.CancelWhenOff(ctx, on.Load, time.Hour)
				defer release()
			}
			_, err := c.Lookup(ctx, testFP())
			if on.Load() {
				t.Fatal("the answer never reached its body: the case tests nothing")
			}
			if !errors.Is(err, enrichgate.ErrOff) || errors.Is(err, ErrNoMatch) {
				t.Errorf("Lookup = %v, want ErrOff: a no-match completed after the switch-off was taken", err)
			}
		})
	}
}

// TestAnErrorAnswerCompletedAfterTheFlipIsNoAnswer: an error status whose
// body the client reads after the switch turned off comes back as ErrOff,
// never the status's error, on a pass's context and on one that carries no
// switch (backlog B617, round 4).
func TestAnErrorAnswerCompletedAfterTheFlipIsNoAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "an error answer")
	}))
	t.Cleanup(srv.Close)
	for _, gated := range []bool{true, false} {
		kind := "a context that carries no switch"
		if gated {
			kind = "a pass's context"
		}
		t.Run(kind, func(t *testing.T) {
			var on atomic.Bool
			on.Store(true)
			tr := &http.Transport{}
			t.Cleanup(tr.CloseIdleConnections)
			hc := &http.Client{Timeout: 10 * time.Second, Transport: bodyBarrier{base: tr, before: func() { on.Store(false) }}}
			c := NewClient(srv.URL, testKey, "ua", hc, enrichgate.Enrichment(on.Load).Every(time.Hour))
			ctx := context.Background()
			if gated {
				var release context.CancelFunc
				ctx, release = enrichgate.CancelWhenOff(ctx, on.Load, time.Hour)
				defer release()
			}
			_, err := c.Lookup(ctx, testFP())
			if on.Load() {
				t.Fatal("the error answer's body was never read: the case tests nothing")
			}
			var herr *httpError
			if !errors.Is(err, enrichgate.ErrOff) || errors.As(err, &herr) {
				t.Errorf("Lookup = %v, want ErrOff: an error answer completed after the switch-off was taken", err)
			}
		})
	}
}
