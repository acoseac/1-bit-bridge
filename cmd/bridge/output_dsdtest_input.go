package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"io"
	"strings"
	"sync/atomic"
)

// One reader owns stdin throughout the run. While audio plays, stop keys cancel
// the run independently of its leg, including admission, Pause and TrackChange.
// At silent confirmation prompts s/q are entire lines, so their letters remain
// available in the required phrases. Ctrl-C is immediate in either mode.
type nativeConsole struct {
	prompt atomic.Bool
	keys   chan byte
	lines  chan nativeLine
}
type nativeLine struct {
	text string
	err  error
}
type nativeStop struct{ key byte }

func (s *nativeStop) Error() string {
	if s.key == 'q' {
		return "quit"
	}
	return "stopped"
}

func newNativeConsole(ctx context.Context, in *bufio.Reader, cancel context.CancelCauseFunc, prompt bool) *nativeConsole {
	c := &nativeConsole{keys: make(chan byte, 16), lines: make(chan nativeLine, 1)}
	c.prompt.Store(prompt)
	go func() {
		defer close(c.keys)
		var line strings.Builder
		for {
			b, e := in.ReadByte()
			if e != nil {
				select {
				case c.lines <- nativeLine{err: e}:
				case <-ctx.Done():
				}
				return
			}
			if b == 3 {
				cancel(context.Canceled)
				return
			}
			if c.prompt.Load() {
				if b == '\r' || b == '\n' {
					v := strings.TrimSpace(line.String())
					line.Reset()
					if v == "s" || v == "q" {
						cancel(&nativeStop{v[0]})
						return
					}
					c.prompt.Store(false)
					select {
					case c.lines <- nativeLine{text: v}:
					case <-ctx.Done():
						return
					}
				} else {
					if line.Len() >= 4096 {
						cancel(errors.New("confirmation exceeds bound"))
						return
					}
					line.WriteByte(b)
				}
				continue
			}
			if b == 's' || b == 'q' {
				cancel(&nativeStop{b})
				return
			}
			if b == ' ' || b == 'n' || b == 'p' {
				select {
				case c.keys <- b:
				case <-ctx.Done():
					return
				default:
					cancel(errors.New("native command queue exceeded"))
					return
				}
			}
		}
	}()
	return c
}
func (c *nativeConsole) confirm(ctx context.Context, out io.Writer, prompt string) (string, error) {
	c.prompt.Store(true)
	fmt.Fprintln(out, prompt)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case v := <-c.lines:
		return v.text, v.err
	}
}

// withoutNativeCancellation removes only cancellation caused by a handled key;
// joined cleanup and device errors remain failures in the final report.
func withoutNativeCancellation(e error) error {
	if e == nil {
		return nil
	}
	if joined, ok := e.(interface{ Unwrap() []error }); ok {
		var kept []error
		for _, v := range joined.Unwrap() {
			kept = append(kept, withoutNativeCancellation(v))
		}
		return errors.Join(kept...)
	}
	if errors.Is(e, context.Canceled) && !errors.Is(e, alsapcm.ErrReleaseUnconfirmed) {
		return nil
	}
	return e
}
