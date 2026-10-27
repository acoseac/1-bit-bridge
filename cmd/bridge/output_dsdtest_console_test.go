//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeConsoleCancellation(t *testing.T) {
	for _, prompt := range []bool{false, true} {
		name := "playback"
		if prompt {
			name = "prompt"
		}
		t.Run(name, func(t *testing.T) {
			r, w, e := os.Pipe()
			if e != nil {
				t.Fatal(e)
			}
			defer r.Close()
			defer w.Close()
			flags := func() int {
				rc, e := r.SyscallConn()
				if e != nil {
					t.Fatal(e)
				}
				var n int
				_ = rc.Control(func(fd uintptr) { n, e = unix.FcntlInt(fd, unix.F_GETFL, 0) })
				if e != nil {
					t.Fatal(e)
				}
				return n
			}
			before := flags()
			ctx, cancel := context.WithCancelCause(context.Background())
			c := newNativeConsole(ctx, r, cancel, prompt, nil)
			<-c.reading // the next read is an open, silent pipe
			cancel(context.Canceled)
			select {
			case <-c.done:
			case <-time.After(100 * time.Millisecond):
				t.Error("silent stdin reader survived cancellation")
				w.Close()
				<-c.done
			}
			if flags() != before {
				t.Error("stdin descriptor flags changed")
			}
			if _, e := r.Stat(); e != nil {
				t.Error("caller stdin closed", e)
			}
		})
	}
}
func TestNativeConsoleEditing(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	c := newNativeConsole(ctx, strings.NewReader("abc\x7f\bx\n"), cancel, true, nil)
	got, e := c.confirm(ctx, io.Discard, "type")
	if e != nil || got != "ax" {
		t.Fatalf("edited confirmation=%q err=%v", got, e)
	}
}

func TestNativeConsoleRawEcho(t *testing.T) {
	var out bytes.Buffer
	writer := &nativeConsoleOutput{out: &out, raw: true}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	if _, e := io.WriteString(writer, "type\n"); e != nil {
		t.Fatal(e)
	}
	c := newNativeConsole(ctx, strings.NewReader("abc\x7f\bx\n"), cancel, true, writer)
	got, e := c.confirm(ctx, io.Discard, "type")
	<-c.done
	if e != nil || got != "ax" {
		t.Fatal(got, e)
	}
	text := out.String()
	if !strings.Contains(text, "abc\b \b\b \bx\r\n") || !strings.Contains(text, "type\r\n") {
		t.Fatalf("raw prompt/echo=%q", text)
	}
}

func TestNativeConsoleRequiresInterruptibleInput(t *testing.T) {
	h := harnessRig(t)
	h.env.stdin = struct{ io.Reader }{strings.NewReader("")}
	calls := 0
	h.env.open = func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) {
		calls++
		return nil, errors.New("test stop")
	}
	code := runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard)
	if code != 1 || calls != 0 {
		t.Fatalf("unsupported input reached hardware: code=%d opens=%d", code, calls)
	}
}
