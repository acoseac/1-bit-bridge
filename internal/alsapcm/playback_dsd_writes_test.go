package alsapcm_test

import (
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// TestAnOpenHeldPastTheGateWritesNothingOfTheDSDState (B762 round 2, backlog
// B780): a session's native open the kernel holds past the player's stop
// records its driver_accepted once it lands (noteDriverAccepted), from a
// goroutine serve's player no longer waits for. Once the writes of the
// player's DSD state are closed (CloseDSDWrites, which serve's player calls
// once its engine's Run has returned and before it lets go of its share of
// the data directory's lock), that record is never written: the store takes
// no write, and the writes stay closed.
func TestAnOpenHeldPastTheGateWritesNothingOfTheDSDState(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
	d.files.add("a", dsd(playbacktest.DSD64, 0.5))
	d.insert("a")
	parked, release := d.k.ParkNextOpen()
	defer release()
	// The Play waits for the Start, which waits for the held open.
	played := make(chan error, 1)
	go func() { played <- d.eng.Play(bg) }()
	select {
	case <-parked:
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the session's native open was not held within %v", alsapcm.Watchdog())
	}
	gate := d.be.CloseDSDWrites()
	select {
	case <-gate:
	default:
		t.Fatal("a write of the DSD state runs before the held open has landed")
	}
	release()
	select {
	case err := <-played:
		if err != nil {
			t.Fatalf("play: %v", err)
		}
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the Play did not answer within %v of the held open's landing", alsapcm.Watchdog())
	}
	if got := d.openFormats(); len(got) != 1 {
		t.Fatalf("the opens asked %v, want the held one", got)
	}
	d.rig.stop()
	if n := d.store.Writes(); n != 0 {
		t.Errorf("the open held past the gate wrote the player's DSD state %d times: %+v", n, d.store.State())
	}
	select {
	case <-d.be.DSDWrites():
	default:
		t.Error("the writes read one under way once the rig stopped")
	}
}
