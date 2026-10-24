package dlna

import (
	"context"
	"testing"
	"time"
)

func TestSSDPNetworkTimingOverridesPreserveDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		network *SSDPNetwork
		want    time.Duration
	}{
		{"nil-network", nil, 17 * time.Millisecond},
		{"nil-delay", &SSDPNetwork{}, 17 * time.Millisecond},
		{"explicit-delay", &SSDPNetwork{Delay: func(time.Duration) time.Duration { return 23 * time.Millisecond }}, 23 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Today's production default is nil (uniform random in delay).
			// Seed a distinct default to test that an absent override keeps
			// it: the guard is hardening, not a present runtime defect.
			s := NewSSDPSubsystem(context.Background(), SSDPSubsystemOptions{})
			defer s.Close(context.Background())
			s.replyDelay = func(time.Duration) time.Duration { return 17 * time.Millisecond }
			s.configureNetwork(tc.network)
			if s.replyDelay == nil {
				t.Fatal("absent override cleared the default delay")
			}
			if got := s.delay(time.Second); got != tc.want {
				t.Fatalf("delay=%s want %s", got, tc.want)
			}
			if s.intervalFloor != time.Minute {
				t.Fatal("zero interval cleared the production floor")
			}
		})
	}
	t.Run("constructor", func(t *testing.T) {
		s := NewSSDPSubsystem(context.Background(), SSDPSubsystemOptions{Network: &SSDPNetwork{
			Delay: func(time.Duration) time.Duration { return 23 * time.Millisecond }, IntervalFloor: time.Millisecond,
		}})
		defer s.Close(context.Background())
		if s.delay(time.Second) != 23*time.Millisecond || s.intervalFloor != time.Millisecond {
			t.Fatal("constructor lost timing overrides")
		}
	})
}
