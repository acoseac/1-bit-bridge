package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"strings"
	"testing"
)

// TestNativeProbeRequiresAcknowledgement pins every default-less input gate.
func TestNativeProbeRequiresAcknowledgement(t *testing.T) {
	base := []string{"--ack-level=off-ear", "--format=DSD_U32_BE", "--bit-rate=2822400", "--seconds=5", "--pause-method=kernel", "--tail-ms=0", "--prefill-ms=0"}
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
				args := []string{}
				for _, a := range base {
					if !strings.HasPrefix(a, "--"+tc.flag+"=") {
						args = append(args, a)
					}
				}
				if v != "MISSING" {
					args = append(args, "--"+tc.flag+"="+v)
				}
				calls := 0
				env := outputDSDTestEnv{goos: "linux", open: func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) {
					calls++
					return nil, errors.New("fake refusal")
				}}
				var out bytes.Buffer
				code := runOutputDSDTest(context.Background(), args, env, &out, &out)
				if code == 0 || !strings.Contains(out.String(), "--"+tc.flag) || calls != 0 {
					t.Fatalf("exit=%d opens=%d output=%q", code, calls, out.String())
				}
			})
		}
	}
}
