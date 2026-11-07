package playback

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestAWriteAfterTheGateIsNeverMade (B762 round 2, backlog B780): the gate
// (CloseDSDWrites) closes the writes of the player's DSD state to admission
// in one step under the lock that counts them: a write asked for after it
// never runs, is said once however many come, and leaves DSDWrites closed;
// one admitted before it counts, and the channel the gate returns closes only
// once that one has returned. Serve's player calls the gate once its
// engine's Run has returned, before it lets go of its share of the data
// directory's lock, so an abandoned session's goroutine that asks for a
// write later (an open the kernel held past the stop, an occurrence still
// playing to its observation) writes nothing into a data directory another
// serve may hold by then.
func TestAWriteAfterTheGateIsNeverMade(t *testing.T) {
	rec := loggingtest.NewRecorder()
	b, err := New(Config{Resolver: nopResolver{}, Locator: noUnit{}, Log: rec.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	held, started := make(chan struct{}), make(chan struct{})
	if !b.spawnWrite("a write before the gate", func() {
		close(started)
		<-held
	}) {
		t.Fatal("a write asked for before the gate was not admitted")
	}
	receiveWithin(t, started, "the write admitted before the gate")
	gate := b.CloseDSDWrites()
	select {
	case <-gate:
		t.Fatal("the gate's channel is closed while a write admitted before it runs")
	default:
	}
	var late atomic.Int32
	for range 2 {
		if b.spawnWrite("a write after the gate", func() { late.Add(1) }) {
			t.Error("a write asked for after the gate was admitted")
		}
	}
	close(held)
	receiveWithin(t, gate, "the gate's channel, once the write admitted before it returned")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if n := late.Load(); n != 0 {
		t.Errorf("%d writes asked for after the gate ran", n)
	}
	select {
	case <-b.DSDWrites():
	default:
		t.Error("the writes read one under way after the gate refused every write")
	}
	if lines := rec.Lines(dsdWriteRefused); len(lines) != 1 {
		t.Errorf("the refused writes were said %q, want once", lines)
	}
}
