package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
)

type nativeTerminalDiagnostic func([]byte) (int, error)

func (f nativeTerminalDiagnostic) Write(b []byte) (int, error) { return f(b) }

type nativeTerminalReader struct {
	entered, interrupted, returned chan struct{}
}

func (r nativeTerminalReader) Read([]byte) (int, error) {
	close(r.entered)
	<-r.interrupted
	close(r.returned)
	return 0, io.EOF
}

func (r nativeTerminalReader) Interrupt() { close(r.interrupted) }

// TestNativeTerminalLifetime drives the real harness with terminal state owned
// by the test. No host terminal or audio device is inspected or changed.
func TestNativeTerminalLifetime(t *testing.T) {
	for _, name := range []string{"complete", "nonterminal", "options", "assets", "report", "open_error", "input_error", "cancelled", "quit", "stop", "raw_error", "restore_error", "reader_join", "panic"} {
		t.Run(name, func(t *testing.T) {
			h := harnessRig(t)
			h.env.tty = name != "nonterminal"
			const original, raw = 17, 99
			var state atomic.Int32
			state.Store(original)
			entered, restored := 0, 0
			reader := nativeTerminalReader{make(chan struct{}), make(chan struct{}), make(chan struct{})}
			h.env.makeRaw = func() (func() error, error) {
				entered++
				if name == "raw_error" {
					return nil, errors.New("raw mode refused")
				}
				saved := state.Swap(raw)
				return func() error {
					restored++
					if name == "reader_join" {
						select {
						case <-reader.returned:
						default:
							t.Error("restored before the input reader returned")
						}
					}
					state.Store(saved)
					if name == "restore_error" {
						return errors.New("restore refused")
					}
					return nil
				}, nil
			}
			h.env.resolve = func(context.Context, string) (nativeEndpoint, func(alsapcm.NodeInfo) error, error) {
				if state.Load() != original {
					t.Error("raw mode during validation")
				}
				return nativeEndpoint{}, nil, nil
			}
			h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
				if name == "reader_join" {
					<-reader.entered
				}
				want := int32(raw)
				if name == "nonterminal" {
					want = original
				}
				if state.Load() != want {
					t.Error("console does not own the expected terminal state")
				}
				if name == "open_error" {
					return nil, errors.New("opener refused")
				}
				return &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: -1}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantEntered, wantRestored, wantCode := 1, 1, 0
			switch name {
			case "nonterminal":
				wantEntered, wantRestored = 0, 0
			case "options":
				h.args = nil
				wantEntered, wantRestored, wantCode = 0, 0, 2
			case "assets", "report":
				path := filepath.Join(h.dir, "dsd.dsf")
				if name == "report" {
					path = filepath.Join(h.dir, "report.json")
				}
				if e := os.WriteFile(path, []byte("invalid"), 0600); e != nil {
					t.Fatal(e)
				}
				wantEntered, wantRestored, wantCode = 0, 0, 1
			case "open_error", "restore_error":
				wantCode = 1
			case "input_error":
				h.env.stdin = io.LimitReader(strings.NewReader(""), 0)
				wantCode = 1
			case "cancelled":
				cancel()
				wantCode = 1
			case "quit":
				h.env.stdin = strings.NewReader("q")
			case "stop":
				h.env.stdin = strings.NewReader("s")
			case "raw_error":
				wantRestored, wantCode = 0, 1
			case "reader_join":
				h.env.stdin = reader
			case "panic":
				h.env.at = func(time.Time) (<-chan time.Time, func()) { panic("terminal test") }
			}
			errout := nativeTerminalDiagnostic(func(b []byte) (int, error) {
				if state.Load() != original {
					t.Error("diagnostic written before terminal restoration")
				}
				return len(b), nil
			})
			code := 0
			var caught any
			func() {
				defer func() { caught = recover() }()
				code = runOutputDSDTest(ctx, h.args, h.env, io.Discard, errout)
			}()
			if (name == "panic" && caught != "terminal test") || (name != "panic" && caught != nil) {
				t.Fatalf("unexpected panic: %v", caught)
			}
			if code != wantCode || entered != wantEntered || restored != wantRestored || state.Load() != original {
				t.Fatalf("exit=%d enters=%d restores=%d state=%d; want %d/%d/%d/%d", code, entered, restored, state.Load(), wantCode, wantEntered, wantRestored, original)
			}
		})
	}
}
