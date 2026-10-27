package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// One reader owns stdin throughout the run. While audio plays, stop keys cancel
// the run independently of its leg, including admission, Pause and TrackChange.
// At silent confirmation prompts s/q are entire lines, so their letters remain
// available in the required phrases. Ctrl-C is immediate in either mode.
type nativeConsole struct {
	err     error
	done    chan struct{}
	reading chan struct{}
	stop    context.CancelFunc
	echo    io.Writer
	prompt  atomic.Bool
	keys    chan byte
	lines   chan nativeLine
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

func newNativeConsole(ctx context.Context, source io.Reader, cancel context.CancelCauseFunc, prompt bool, echo io.Writer) *nativeConsole {
	ctx, stop := context.WithCancel(ctx)
	c := &nativeConsole{done: make(chan struct{}), reading: make(chan struct{}, 1), stop: stop, echo: echo, keys: make(chan byte, 16), lines: make(chan nativeLine, 1)}
	c.prompt.Store(prompt)
	var interrupt, release func()
	switch r := source.(type) {
	case *os.File:
		var ok bool
		source, interrupt, release, ok = pcmTestStoppableFile(r)
		if !ok {
			c.err = errors.New("stdin cannot be interrupted")
			c.lines <- nativeLine{err: c.err}
			close(c.keys)
			close(c.done)
			return c
		}
	case interface {
		io.Reader
		Interrupt()
	}:
		// An injected reader explicitly supplies its own cancellation contract.
		// The harness never closes a caller-owned input reader.
		interrupt = r.Interrupt
	case *strings.Reader, *bytes.Reader, *bytes.Buffer:
		// In-memory injected readers cannot park in Read.
	default:
		c.err = errors.New("stdin needs an interruptible reader")
		c.lines <- nativeLine{err: c.err}
		close(c.keys)
		close(c.done)
		return c
	}
	wakeDone := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		defer close(wakeDone)
		select {
		case <-ctx.Done():
			if interrupt != nil {
				interrupt()
			}
		case <-readDone:
		}
	}()
	in := bufio.NewReader(source)
	go func() {
		defer func() {
			close(readDone)
			<-wakeDone
			if release != nil {
				release()
			}
			close(c.keys)
			close(c.done)
		}()
		var line strings.Builder
		for {
			if ctx.Err() != nil {
				return
			}
			select {
			case c.reading <- struct{}{}:
			default:
			}
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
					if c.echo != nil {
						fmt.Fprint(c.echo, "\n")
					}
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
					if b == 0x7f || b == 0x08 {
						if text := line.String(); len(text) > 0 {
							line.Reset()
							line.WriteString(text[:len(text)-1])
							if c.echo != nil {
								fmt.Fprint(c.echo, "\b \b")
							}
						}
						continue
					}
					if b < 32 || b > 126 {
						continue
					}
					if c.echo != nil {
						fmt.Fprintf(c.echo, "%c", b)
					}
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

func (c *nativeConsole) close() { c.stop(); <-c.done }

// Serialize prompt and reader echo output; translate raw-terminal newlines.
type nativeConsoleOutput struct {
	mu  sync.Mutex
	out io.Writer
	raw bool
}

func (w *nativeConsoleOutput) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(b)
	if w.raw {
		b = bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	}
	_, e := w.out.Write(b)
	if e != nil {
		return 0, e
	}
	return original, nil
}
