package playback_test

import (
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// A write of the bound DAC's DSD choice that cannot be made (B728 B9 round 2,
// points 3 and 5): the state moving what it would publish in each of its
// rounds, or a reading of the box that never agrees with the set published.

// wantNotMade fails t unless log holds one line of the write, its outcome
// persist_failed, a warning that says the write was not made and not that it
// was not made durable, which says a write landed.
func wantNotMade(t *testing.T, log *choiceLog) {
	t.Helper()
	lines := log.writes()
	if len(lines) != 1 || lines[0].attrs["outcome"] != string(playback.ChoicePersistFailed) || lines[0].level != "warn" {
		t.Fatalf("the write's lines %+v, want one warning with outcome persist_failed", lines)
	}
	if msg := lines[0].msg; !strings.Contains(msg, "was not made") || strings.Contains(msg, "durable") {
		t.Errorf("the write's line says %q: want that it was not made, and nothing of durability", msg)
	}
}

// TestAWriteTheStateKeepsMovingUnderIsNotMade (B728 B9 round 2, point 3): a
// write whose state another writer moves in every round, in a way that moves
// what it would publish (a listener's rejection made and withdrawn, each
// published by its refresh), for maxChoiceRounds of them, is not made:
// persist_failed, nothing written, the client to write again; never stale,
// which says the client's revision is behind, which it is not.
func TestAWriteTheStateKeepsMovingUnderIsNotMade(t *testing.T) {
	b, store, log := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeAuto), func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) })
	var rounds atomic.Int32
	playback.SetSeamsForTest(b, &playback.Seams{ChoicePending: func() {
		n := rounds.Add(1)
		if err := store.Update(func(st *dsdstate.State) error {
			st.DACs[0].Listener = nil
			if n%2 == 1 {
				st.DACs[0].Listener = []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, Answer: dsdstate.AnswerNoise}}
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
		b.RefreshDSD(true)
	}})
	published := b.Policy()
	res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1, acknowledged))
	wantChoice(t, res, playback.ChoicePersistFailed)
	if got := rounds.Load(); got != 8 {
		t.Errorf("the write went round %d times, want 8", got)
	}
	if c := store.State().DACs[0].Choice; c.Mode != dsdstate.ModeAuto || c.Revision != 1 {
		t.Errorf("the choice %+v, want auto at revision 1, as it was", c)
	}
	if !reflect.DeepEqual(res.Policy.DSD.DAC.Choice, published.DSD.DAC.Choice) {
		t.Errorf("the answer's choice %+v, want the one published before, %+v", res.Policy.DSD.DAC.Choice, published.DSD.DAC.Choice)
	}
	wantNotMade(t, log)
}

// glitchingBox is the box whose card's controls, once armed, fail to read at
// every other read, the first one first: a write's readings, each one followed
// by a refresh's that reads them.
type glitchingBox struct {
	*playbacktest.Box
	armed atomic.Bool
	reads atomic.Int32
}

func (g *glitchingBox) ReadControls(card int) (alsactl.Snapshot, error) {
	if g.armed.Load() && g.reads.Add(1)%2 == 1 {
		return alsactl.Snapshot{}, errors.New("the control node did not answer")
	}
	return g.Box.ReadControls(card)
}

// TestAWriteWhoseReadingNeverAgreesWithTheSetPublishedIsNotMade (B728 B9 round
// 2, point 5): what a write would interrupt is judged against the set
// published, so where the write's reading of the box gives another set (DoP
// past a gain stage at unity, its controls unread in the write's readings
// alone), that set is read again, published and reconciled first (a refresh of
// the write's own), and the write reads again; one whose reading never agrees
// within maxChoiceRounds is not made: persist_failed, nothing written, DoP
// published as it was.
func TestAWriteWhoseReadingNeverAgreesWithTheSetPublishedIsNotMade(t *testing.T) {
	box := playbacktest.NewBox(5)
	unit := playbacktest.DACUnit()
	unit.Descriptors = playbacktest.VolumeDescriptors()
	box.SetUnit(5, unit)
	box.SetMixer(5, playbacktest.VolumeMixer())
	box.SetControls(5, playbacktest.VolumeControls(255)...)
	g := &glitchingBox{Box: box}
	b, store, log := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeDoP), func(p *playback.DSDPolicy) { p.Box = g })
	published := b.Policy()
	if rates, _ := ratesOf(published); len(rates) == 0 {
		t.Fatalf("DoP past a gain stage at unity publishes no tuple: %+v", published)
	}
	g.armed.Store(true)
	res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 1))
	wantChoice(t, res, playback.ChoicePersistFailed)
	if got := g.reads.Load(); got != 16 {
		t.Errorf("the box's controls were read %d times, want 16: each of 8 rounds' reading, and the refresh after it", got)
	}
	if c := store.State().DACs[0].Choice; c.Mode != dsdstate.ModeDoP || c.Revision != 1 {
		t.Errorf("the choice %+v, want DoP at revision 1, as it was", c)
	}
	if !reflect.DeepEqual(res.Policy.Tuples, published.Tuples) || res.Policy.Generation != published.Generation {
		t.Errorf("the set the write left %+v (generation %d), want DoP as published, %+v (generation %d)", res.Policy.Tuples, res.Policy.Generation, published.Tuples, published.Generation)
	}
	wantNotMade(t, log)
}
