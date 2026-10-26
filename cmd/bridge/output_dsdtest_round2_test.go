package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
)

func TestNativeMatrixStopSkipsPCM(t *testing.T) {
	for _, key := range []byte{'s', 'q'} {
		t.Run(string(key), func(t *testing.T) {
			h := harnessRig(t)
			h.args[7] = "--scenario=matrix"
			r, w := io.Pipe()
			defer r.Close()
			defer w.Close()
			h.env.stdin = r
			h.env.tty = true
			var pcm atomic.Int32
			h.env.openPCM = func(context.Context, alsapcm.SinkConfig) (nativePCMSink, error) {
				pcm.Add(1)
				return nativePCMStub{}, nil
			}
			answer := make(chan int, 1)
			go func() { answer <- runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard) }()
			harnessEvent(t, h, "write")
			_, _ = w.Write([]byte{key})
			select {
			case code := <-answer:
				if code != 0 {
					t.Errorf("stop exit=%d", code)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stop held run")
			}
			if pcm.Load() != 1 {
				t.Errorf("later PCM opened after stop: %d", pcm.Load())
			}
			b, e := os.ReadFile(filepath.Join(h.dir, "report.json"))
			if e != nil {
				t.Fatal(e)
			}
			var report nativeReport
			if e = json.Unmarshal(b, &report); e != nil {
				t.Fatal(e)
			}
			want := "stopped"
			if key == 'q' {
				want = "quit"
			}
			if report.End != want || report.Contact != nil {
				t.Errorf("stop report End=%s contact=%v", report.End, report.Contact)
			}
		})
	}
}

func setNativeArg(args []string, name, value string) []string {
	out := append([]string(nil), args...)
	for i, a := range out {
		if strings.HasPrefix(a, "--"+name+"=") {
			out[i] = "--" + name + "=" + value
			return out
		}
	}
	return out
}
