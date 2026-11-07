package dsdconv

import (
	"context"
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// BenchmarkConvertDSD64, DSD128 and DSD256 convert a source that never
// ends, under each plan, and report the cost of an output frame (both
// channels) and the share of one core the plan's real time takes on the
// host that runs them (B728 §4.6's budget: at most 25 % of one Pi 4 core at
// DSD256), beside the allocations, which are none.
func BenchmarkConvertDSD64(b *testing.B)  { benchConvert(b, format.DSD64RateHz) }
func BenchmarkConvertDSD128(b *testing.B) { benchConvert(b, 2*format.DSD64RateHz) }
func BenchmarkConvertDSD256(b *testing.B) { benchConvert(b, 4*format.DSD64RateHz) }

func benchConvert(b *testing.B, rate int) {
	for _, p := range allPlans {
		b.Run(fmt.Sprintf("%dHz-%dbit", p.RateHz, p.ValidBits), func(b *testing.B) {
			c, err := New(newLoop(rate, 1), p, Options{})
			if err != nil {
				b.Fatal(err)
			}
			const frames = 4096
			buf := make([]int32, 2*frames)
			for i := 0; i < 4; i++ {
				if _, err := c.ReadFrames(context.Background(), buf); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := c.ReadFrames(context.Background(), buf); err != nil {
					b.Fatal(err)
				}
			}
			ns := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / frames
			b.ReportMetric(ns, "ns/frame")
			b.ReportMetric(ns*float64(p.RateHz)/1e7, "%core")
		})
	}
}
