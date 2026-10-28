package main

import (
	"context"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

type reservationPlayBackend struct {
	*playStandIn
	owns   bool
	handed chan *engine.OccurrenceReservation
}

func (b *reservationPlayBackend) OwnsOccurrenceReservations() bool { return b.owns }
func (b *reservationPlayBackend) Start(ctx context.Context, r engine.StartRequest) error {
	b.handed <- r.First.Reservation
	defer r.First.Reservation.ReleaseBackend()
	return b.playStandIn.Start(ctx, r)
}
func (b *reservationPlayBackend) Prepare(ctx context.Context, s engine.SessionID, after engine.OccurrenceID, next *engine.Occurrence) error {
	if next != nil {
		defer next.Reservation.ReleaseBackend()
	}
	return b.playStandIn.Prepare(ctx, s, after, next)
}

func TestOutputPlayForwardsReservations(t *testing.T) {
	for _, owns := range []bool{true, false} {
		name := "opt_out"
		if owns {
			name = "opt_in"
		}
		t.Run(name, func(t *testing.T) {
			c := newPlayCase(t, goBarPlayer)
			env := c.env("linux")
			b := &reservationPlayBackend{playStandIn: c.standIn, owns: owns, handed: make(chan *engine.OccurrenceReservation, 1)}
			env.backend = func(cfg playback.Config) (playBackend, error) { b.notify = cfg.Notify; return b, nil }
			stdout, stderr := newPlayBuf(), newPlayBuf()
			code := runOutputPlay(context.Background(), append([]string{"--config", c.configPath}, c.files...), env, stdout, stderr)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			select {
			case r := <-b.handed:
				if (r != nil) != owns {
					t.Fatalf("reservation present=%v, backend opt-in=%v", r != nil, owns)
				}
			default:
				t.Fatal("backend never started")
			}
		})
	}
	t.Run("legacy", func(t *testing.T) {
		b, ok := any(playTee{playBackend: &playStandIn{}}).(engine.ReservationBackend)
		if ok && b.OwnsOccurrenceReservations() {
			t.Fatal("legacy backend opted in")
		}
	})
}
