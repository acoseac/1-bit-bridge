package playback_test

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The effective DSD policy's selection and publication (B728 B7), without a
// device: what a DSD occurrence is admitted under and what GET /v1/outputs
// publishes are one set, and each rate's refusal is the policy's word.

// noContent is a resolver with nothing to open: these tests open nothing.
type noContent struct{}

func (noContent) Open(context.Context, engine.Reference) (playback.Content, error) {
	return playback.Content{}, errors.New("no content here")
}

// cardFive is the locator of the box's card.
type cardFive struct{}

func (cardFive) Locate(context.Context) (playback.Unit, error) {
	return playback.Unit{Endpoint: alsapcm.Endpoint{Card: 5}}, nil
}

// policyBackend is a backend whose DSD is the effective policy over box and
// store, the test DAC bound at card 5, edit applied to the policy first.
func policyBackend(t *testing.T, box *playbacktest.Box, store playback.DSDStore, edit ...func(*playback.DSDPolicy)) *playback.Backend {
	t.Helper()
	p := playbacktest.Policy(5, box, store)
	for _, e := range edit {
		e(p)
	}
	b, err := playback.New(playback.Config{Resolver: noContent{}, Locator: cardFive{}, DSD: p})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dsdAt is stereo DSD content at bitRate.
func dsdAt(bitRate int) format.NativeDSD { return format.NativeDSD{BitRateHz: bitRate, Channels: 2} }

// reasonOf is err's refusal word, "" for none.
func reasonOf(err error) engine.Reason {
	var u *engine.Unplayable
	if errors.As(err, &u) {
		return u.Reason
	}
	return ""
}

// ratesOf are the rates p publishes a tuple at, and each tuple's transport.
func ratesOf(p playback.PlayerPolicy) (rates []int, transports []string) {
	for _, t := range p.Tuples {
		rates = append(rates, t.Tuple.BitRate)
		transports = append(transports, t.Tuple.Transport)
	}
	return rates, transports
}

// TestSelectionAndPublicationReadOneSet (B728 B7; R10, R11): a DSD
// occurrence is admitted under the set published now, never under the state
// read again beside it: a choice written without a refresh changes neither
// what a successor is admitted under nor what is published; the next
// session's first occurrence reads the policy afresh, which publishes the new
// set at the next generation, and is admitted under that very set. Under a
// race of refreshes and admissions, each admission's plan is the tuple the
// publication of its generation holds.
func TestSelectionAndPublicationReadOneSet(t *testing.T) {
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	b := policyBackend(t, playbacktest.NewBox(5), store)
	b.RefreshDSD(true)
	first := b.Policy()
	if rates, transports := ratesOf(first); !slices.Equal(rates, []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256}) || slices.ContainsFunc(transports, func(s string) bool { return s != "" }) {
		t.Fatalf("the native choice published %+v, want native at the three rates", first.Tuples)
	}
	store.Set(playbacktest.ChoiceState(dsdstate.ModeDoP))
	plan, gen, err := b.AdmitDSDForTest(dsdAt(playbacktest.DSD64), false)
	if err != nil || gen != first.Generation || plan.Tuple.Transport != "" || plan.PolicyID != first.Tuples[0].PolicyID {
		t.Errorf("a successor admitted %+v under generation %d (%v); want the published native tuple under %d", plan, gen, err, first.Generation)
	}
	if p := b.Policy(); p.Generation != first.Generation || p.Tuples[0].PolicyID != first.Tuples[0].PolicyID {
		t.Errorf("a write with no refresh moved the publication: %+v", p)
	}
	plan, gen, err = b.AdmitDSDForTest(dsdAt(playbacktest.DSD64), true)
	p := b.Policy()
	if err != nil || plan.Tuple.Transport != playback.TransportDoP || gen != first.Generation+1 || p.Generation != gen || p.Tuples[0].PolicyID != plan.PolicyID {
		t.Errorf("a session's first occurrence admitted %+v under generation %d (%v), the publication %+v; want DoP under the next, published", plan, gen, err, p)
	}

	// The race: a writer alternates the choice and refreshes, while a reader
	// reads the publication and an admitter admits successors, each on a
	// goroutine of its own and without pause, through the writer's next write
	// and refresh too; every admission's plan is the tuple its generation was
	// published with. The writer goes on to its next write only once the reader
	// and the admitter have both seen the generation its refresh published (a
	// condition, never a time), so every generation is read and admitted under,
	// whatever the scheduler runs first: left to it, one processor ran the
	// reader's loop to its end before the writer's first refresh, and the race
	// compared nothing (macOS's CI, and -cpu 1 here, round 2).
	const rounds = 100
	var (
		mu        sync.Mutex
		seen      = sync.NewCond(&mu)
		published = map[uint64]string{}
		admitted  []struct {
			gen uint64
			id  string
		}
		read, under uint64 // the latest generation the reader read, and the admitter admitted under
		done        bool
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range rounds {
			mode := dsdstate.ModeNative
			if i%2 == 1 {
				mode = dsdstate.ModeDoP
			}
			store.Set(playbacktest.ChoiceState(mode))
			b.RefreshDSD(true)
			gen := b.Policy().Generation
			mu.Lock()
			for read < gen || under < gen {
				seen.Wait()
			}
			mu.Unlock()
		}
		mu.Lock()
		done = true
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		for {
			p := b.Policy()
			mu.Lock()
			published[p.Generation] = p.Tuples[0].PolicyID
			read = max(read, p.Generation)
			seen.Broadcast()
			stop := done
			mu.Unlock()
			if stop {
				return
			}
			// On one processor the writer runs only when this loop yields.
			runtime.Gosched()
		}
	}()
	go func() {
		defer wg.Done()
		for {
			plan, gen, err := b.AdmitDSDForTest(dsdAt(playbacktest.DSD64), false)
			mu.Lock()
			if err != nil {
				// The writer waits for no admission more.
				under = ^uint64(0)
				seen.Broadcast()
				mu.Unlock()
				t.Errorf("a successor refused under generation %d: %v", gen, err)
				return
			}
			admitted = append(admitted, struct {
				gen uint64
				id  string
			}{gen, plan.PolicyID})
			under = max(under, gen)
			seen.Broadcast()
			stop := done
			mu.Unlock()
			if stop {
				return
			}
			runtime.Gosched()
		}
	}()
	wg.Wait()
	checked := map[uint64]bool{}
	for _, a := range admitted {
		if id, ok := published[a.gen]; ok {
			checked[a.gen] = true
			if id != a.id {
				t.Errorf("generation %d was published with %s and admitted %s", a.gen, id, a.id)
			}
		}
	}
	if len(checked) < rounds {
		t.Errorf("the race compared %d generations, want every one of the %d the writer published", len(checked), rounds)
	}
}

// TestAConvertedRateIsRefusedUntilThePlayerConverts (B728 B7, until B11): a
// rate the effective set converts to PCM is refused at its admission as
// dsd_not_in_this_release, and published with no tuple, that word the
// aggregate reason, since no rate plays.
func TestAConvertedRateIsRefusedUntilThePlayerConverts(t *testing.T) {
	b := policyBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeConvert)))
	for _, rate := range []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256} {
		if _, _, err := b.AdmitDSDForTest(dsdAt(rate), true); reasonOf(err) != playback.ReasonDSD {
			t.Errorf("DSD at %d under a conversion: %v, want %s", rate, err, playback.ReasonDSD)
		}
	}
	if p := b.Policy(); len(p.Tuples) != 0 || p.Reason != string(playback.ReasonDSD) {
		t.Errorf("a converting set published %+v (%q), want no tuple and %s", p.Tuples, p.Reason, playback.ReasonDSD)
	}
}

// TestAnUnknownDACsDSDStaysRefused (B728 B7; binding note 1): a DAC the
// profile database does not know, with no choice of its own, is converted
// at every rate under the bridge's auto, which refuses it until B11; a
// migrated explicit choice for it plays at the rates its record approved.
func TestAnUnknownDACsDSDStaysRefused(t *testing.T) {
	b := policyBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults()))
	for _, rate := range []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256} {
		if _, _, err := b.AdmitDSDForTest(dsdAt(rate), true); reasonOf(err) != playback.ReasonDSD {
			t.Errorf("an unknown DAC's DSD at %d: %v, want %s", rate, err, playback.ReasonDSD)
		}
	}
	if p := b.Policy(); len(p.Tuples) != 0 || p.Reason != string(playback.ReasonDSD) {
		t.Errorf("an unknown DAC published %+v (%q), want nothing playing", p.Tuples, p.Reason)
	}
	migrated := policyBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.MigratedState(dsdstate.ModeNative, false, []int{playbacktest.DSD64}, nil)))
	if plan, _, err := migrated.AdmitDSDForTest(dsdAt(playbacktest.DSD64), true); err != nil || plan.Tuple.Format != string(format.DSDU32BE) {
		t.Errorf("an unknown DAC's migrated native choice at DSD64: %+v, %v; want native", plan, err)
	}
}

// TestAMigratedChoiceRefusesARateItsRecordDidNotApprove (B728 B7; the
// design's section 3.6): a migrated native choice plays the rates the
// owner's record approved, with no new acknowledgement, and refuses every
// other as first_use_pending; the publication lists the approved rates, its
// reason empty while a rate plays.
func TestAMigratedChoiceRefusesARateItsRecordDidNotApprove(t *testing.T) {
	b := policyBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.MigratedState(dsdstate.ModeNative, false, []int{playbacktest.DSD64}, nil)))
	if plan, _, err := b.AdmitDSDForTest(dsdAt(playbacktest.DSD64), true); err != nil || plan.Tuple.Transport != "" {
		t.Errorf("the approved rate: %+v, %v; want it native", plan, err)
	}
	for _, rate := range []int{playbacktest.DSD128, playbacktest.DSD256} {
		if _, _, err := b.AdmitDSDForTest(dsdAt(rate), false); reasonOf(err) != "first_use_pending" {
			t.Errorf("DSD at %d, which the record did not approve: %v, want first_use_pending", rate, err)
		}
	}
	p := b.Policy()
	if rates, _ := ratesOf(p); !slices.Equal(rates, []int{playbacktest.DSD64}) || p.Reason != "" {
		t.Errorf("the publication %+v (%q), want DSD64 alone and no reason", p.Tuples, p.Reason)
	}
	if p.Model != "Test Test DAC" || p.BCDDevice != playbacktest.DACBCD {
		t.Errorf("the publication's model %q, firmware %q; want the unit's", p.Model, p.BCDDevice)
	}
}

// TestAMigratedDoPSetupWithUnverifiableGainRefuses (B728 B7; binding note 1,
// the gain safety exception): a migrated DoP setup plays as before only
// where the DoP gain check passes: with a feature unit on the playback path
// and the kernel's table of its controls not read, every rate is refused as
// dop_gain_unverifiable, the aggregate reason; with the table read and the
// volume at 0 dB it plays DoP.
func TestAMigratedDoPSetupWithUnverifiableGainRefuses(t *testing.T) {
	st := playbacktest.MigratedState(dsdstate.ModeDoP, true, nil, []int{playbacktest.DSD64, playbacktest.DSD128})
	box := playbacktest.NewBox(5)
	unit := playbacktest.DACUnit()
	unit.Descriptors = playbacktest.VolumeDescriptors()
	box.SetUnit(5, unit)
	box.SetControls(5, playbacktest.VolumeControls(255)...)
	box.SetMixer(5, []byte("this is not the kernel's table"))
	b := policyBackend(t, box, playbacktest.NewMemStore(st))
	for _, rate := range []int{playbacktest.DSD64, playbacktest.DSD128} {
		if _, _, err := b.AdmitDSDForTest(dsdAt(rate), true); reasonOf(err) != "dop_gain_unverifiable" {
			t.Errorf("DoP at %d with the gain unverifiable: %v, want dop_gain_unverifiable", rate, err)
		}
	}
	if p := b.Policy(); len(p.Tuples) != 0 || p.Reason != "dop_gain_unverifiable" {
		t.Errorf("the publication %+v (%q), want none and dop_gain_unverifiable", p.Tuples, p.Reason)
	}
	box.SetMixer(5, playbacktest.VolumeMixer())
	if plan, _, err := b.AdmitDSDForTest(dsdAt(playbacktest.DSD64), true); err != nil || plan.Tuple.Transport != playback.TransportDoP {
		t.Errorf("DoP with the volume visible at 0 dB: %+v, %v; want DoP", plan, err)
	}
}

// TestThePublishedReasonIsTheLowestRatesWordOnlyWhenNoRatePlays (B728 B7;
// binding note 8): within today's wire shape the aggregate reason is empty
// while any rate plays, the tuples saying which; with none playing it is
// the lowest rate's word.
func TestThePublishedReasonIsTheLowestRatesWordOnlyWhenNoRatePlays(t *testing.T) {
	box := playbacktest.NewBox(5)
	native := playbacktest.NativeAltset()
	native.Rates = []int{176400, 352800}
	box.SetAltsets(5, 0, playbacktest.S32Altset(), native)
	b := policyBackend(t, box, playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)))
	b.RefreshDSD(true)
	if p := b.Policy(); p.Reason != "" || len(p.Tuples) != 2 || p.Tuples[0].Tuple.BitRate != playbacktest.DSD128 {
		t.Errorf("a set playing DSD128 and DSD256: %+v (%q), want their tuples and no reason", p.Tuples, p.Reason)
	}
	if _, _, err := b.AdmitDSDForTest(dsdAt(playbacktest.DSD64), false); reasonOf(err) != "native_not_offered" {
		t.Errorf("DSD64, which the DAC offers no native format at: %v", err)
	}
	box.SetAltsets(5, 0, playbacktest.S32Altset())
	b.RefreshDSD(true)
	if p := b.Policy(); p.Reason != "native_not_offered" || len(p.Tuples) != 0 {
		t.Errorf("a set playing nothing: %+v (%q), want DSD64's word, native_not_offered", p.Tuples, p.Reason)
	}
}

// TestAStateThatDoesNotReadRefusesEveryRate (B728 B7; the design's section
// 2.5): with no state (one that did not open), every rate is refused as
// dsd_state_unreadable, before any choice or capability is looked at.
func TestAStateThatDoesNotReadRefusesEveryRate(t *testing.T) {
	b := policyBackend(t, playbacktest.NewBox(5), nil, func(p *playback.DSDPolicy) { p.Store = nil })
	for _, rate := range []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256} {
		if _, _, err := b.AdmitDSDForTest(dsdAt(rate), true); reasonOf(err) != dsdstate.RefusalStateUnreadable {
			t.Errorf("DSD at %d with no state: %v, want %s", rate, err, dsdstate.RefusalStateUnreadable)
		}
	}
	if p := b.Policy(); p.Reason != dsdstate.RefusalStateUnreadable {
		t.Errorf("the publication's reason %q, want %s", p.Reason, dsdstate.RefusalStateUnreadable)
	}
}

// TestNoSetComesFromAReadingThatIsNotFresh (B728 B7; the design's section
// 3.1): while the box's reading of the bound DAC is stale or names no unit,
// no set is computed, the box is not read, and DSD is refused as
// dsd_dac_unread, the aggregate reason.
func TestNoSetComesFromAReadingThatIsNotFresh(t *testing.T) {
	box := playbacktest.NewBox(5)
	b := policyBackend(t, box, playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)), func(p *playback.DSDPolicy) {
		p.Bound = func() (playback.BoundDAC, bool) { return playback.BoundDAC{}, false }
	})
	if _, _, err := b.AdmitDSDForTest(dsdAt(playbacktest.DSD64), true); reasonOf(err) != playback.ReasonDACUnread {
		t.Errorf("DSD with no fresh reading: %v, want %s", err, playback.ReasonDACUnread)
	}
	if p := b.Policy(); p.Reason != string(playback.ReasonDACUnread) || len(p.Tuples) != 0 {
		t.Errorf("the publication %+v (%q), want none and %s", p.Tuples, p.Reason, playback.ReasonDACUnread)
	}
	if n := box.Reads(); n != 0 {
		t.Errorf("the box was read %d times with no fresh reading", n)
	}
}
