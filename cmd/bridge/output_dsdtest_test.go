package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestNativeProbeRequiresAcknowledgement pins every default-less input gate.
func TestNativeProbeRequiresAcknowledgement(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		h := harnessRig(t)
		calls := 0
		h.env.open = func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) {
			calls++
			return nil, errors.New("test opener")
		}
		runOutputDSDTest(context.Background(), h.args, h.env, io.Discard, io.Discard)
		if calls != 1 {
			t.Fatalf("valid opener-count=%d", calls)
		}
	})
	cases := []struct {
		flag   string
		values []string
	}{{"ack-level", []string{"MISSING", "", "invalid"}}, {"format", []string{"MISSING", "unknown"}}, {"bit-rate", []string{"MISSING", "123"}}, {"seconds", []string{"MISSING", "0", "601"}}, {"pause-method", []string{"MISSING", "auto"}}, {"tail-ms", []string{"MISSING", "-1", "101"}}, {"prefill-ms", []string{"MISSING", "-1", "501"}}}
	for _, tc := range cases {
		for _, v := range tc.values {
			label := v
			if label == "" {
				label = "empty"
			}
			t.Run(tc.flag+"/"+label, func(t *testing.T) {
				h := harnessRig(t)
				base := h.args[:len(h.args)-4]
				args := []string{}
				for _, a := range base {
					if !strings.HasPrefix(a, "--"+tc.flag+"=") {
						args = append(args, a)
					}
				}
				if v != "MISSING" {
					args = append(args, "--"+tc.flag+"="+v)
				}
				args = append(args, "dsd")
				calls := 0
				env := h.env
				env.at = func(time.Time) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
				env.open = func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) {
					calls++
					return nil, errors.New("fake refusal")
				}
				var out bytes.Buffer
				code := runOutputDSDTest(context.Background(), args, env, &out, &out)
				if calls != 0 {
					t.Fatalf("opener-count: missing/invalid %s reached opener %d times", tc.flag, calls)
				}
				if code == 0 || !strings.Contains(out.String(), "--"+tc.flag) {
					t.Fatalf("exit=%d opens=%d output=%q", code, calls, out.String())
				}
			})
		}
	}
}

// TestDoPOptions: explicit DoP parses, the backend-lifecycle scenario on its
// CLI candidate included; a transport other than native or dop, DoP without
// its noise acknowledgement, and DoP in a native format are each refused,
// naming the flag to fix.
func TestDoPOptions(t *testing.T) {
	for _, tc := range []struct {
		name, transport, format, ack, names string
		extra                               []string
	}{
		{"dop", "dop", "S32_LE", "--ack-dop-noise", "", nil},
		{"transport", "pcm", "S32_LE", "--ack-dop-noise", "--transport must be", nil},
		{"noise", "dop", "S32_LE", "", "--ack-dop-noise required", nil},
		{"format", "dop", "DSD_U32_BE", "--ack-dop-noise", "--format must be", nil},
		{"backend_lifecycle", "dop", "S32_LE", "--ack-dop-noise", "", []string{"--scenario=backend-lifecycle", "--provisional-record=candidate.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := harnessRig(t)
			args := append([]string{"--transport=" + tc.transport}, h.args[:len(h.args)-3]...)
			if tc.ack != "" {
				args = append([]string{tc.ack}, args...)
			}
			args = setNativeArg(args, "format", tc.format)
			for _, x := range tc.extra {
				name, value, _ := strings.Cut(strings.TrimPrefix(x, "--"), "=")
				if set := setNativeArg(args, name, value); !slices.Equal(set, args) {
					args = set
				} else {
					args = append([]string{x}, args...)
				}
			}
			_, e := parseNativeOptions(args, io.Discard)
			if tc.names == "" {
				if e != nil {
					t.Fatal("explicit DoP refused:", e)
				}
				return
			}
			if e == nil || !strings.Contains(e.Error(), tc.names) {
				t.Fatalf("refusal %v, want one naming %s", e, tc.names)
			}
		})
	}
}
