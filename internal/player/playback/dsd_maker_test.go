package playback_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// Each unit's DSD is its own model's entry of its own maker (B883): a unit of
// another maker on the model's key, whatever it shares with the first, reads
// and writes none of the first's entry.

// makerUnit is the test DAC's model as a unit of maker's, its serial the one
// given, at enumeration devnum: one key and one firmware for every maker.
func makerUnit(maker, serial string, devnum int) alsa.UnitModel {
	m := playbacktest.DACUnit()
	m.USB.Serial, m.USB.DevNum, m.Manufacturer = usbid.New(serial), devnum, maker
	return m
}

// boundUnit is the DAC the binding resolves to, which a test moves.
type boundUnit struct {
	mu sync.Mutex
	bd playback.BoundDAC
}

func (b *boundUnit) get() (playback.BoundDAC, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bd, true
}

// put binds m at card 5 in box.
func (b *boundUnit) put(box *playbacktest.Box, m alsa.UnitModel) {
	box.SetUnit(5, m)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bd = playback.BoundDAC{Card: 5, Identity: m.USB, Enumeration: fmt.Sprintf("test/%d", m.USB.DevNum)}
}

// openStore opens the player's DSD state in dir, the real store, over a
// directory barrier and a medium of the test's.
func openStore(t *testing.T, dir string) *dsdstate.Store {
	t.Helper()
	s, err := dsdstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r := s.Report(); r.Outcome != dsdstate.Loaded && r.Outcome != dsdstate.Missing {
		t.Fatalf("the state opens as %+v", r)
	}
	return s
}

// durableSeams give the real store its durable writer over a barrier and a
// medium of the test's (process-global: never in parallel).
func durableSeams(t *testing.T) {
	t.Helper()
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
}

// TestAnotherMakersUnitOfTheModelReadsNoneOfTheFirstsEntry: a unit of maker A,
// migrated fresh from the legacy keys and an owner's record of it (its serial
// read, its manufacturer string "Audio 1 Labs"), plays native on its record's
// rows at the next start; a unit of maker B on the same key and firmware
// bound in its place ("Audio 2 Labs", serial 2: a redaction of each one's
// serial would make the two strings one) reads none of A's entry: the
// default's mode, no revision of its own, first use pending for both
// transports, DoP not confirmed, and DSD converted, never native or DoP; and
// B's own write is B's entry alone, A's left as it was, in the file too.
func TestAnotherMakersUnitOfTheModelReadsNoneOfTheFirstsEntry(t *testing.T) {
	durableSeams(t)
	a, b := makerUnit("Audio 1 Labs", "1", 2), makerUnit("Audio 2 Labs", "2", 3)
	dir := t.TempDir()
	boundA, err := dsdstate.BoundOf(alsa.Identity{Kind: alsa.IdentityUSB, USB: &a.USB}, alsa.QualificationIdentity{Manufacturer: a.Manufacturer,
		Product: a.Product, BCDDevice: a.BCDDevice, Port: a.USB.Port, DevNum: a.USB.DevNum, Interface: a.USB.Interface}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	allow := true
	if err := openStore(t, dir).Update(func(st *dsdstate.State) error {
		next, _, err := dsdstate.Migrate(*st, dsdstate.Input{
			Legacy: dsdstate.Legacy{DSDMode: dsdstate.ModeNative, AllowDoP: &allow, DSDQualification: "dsd-qualification.json"},
			Bound:  &boundA,
			Record: dsdstate.Record{Outcome: dsdstate.RecordTaken, SHA256: strings.Repeat("ab", 32), BCDDevice: playbacktest.DACBCD, QualifiedOn: "2026-10-01",
				Native: []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256}},
			Now: playbacktest.T0,
		})
		if err != nil {
			return err
		}
		*st = next
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The next start: the state from its file, A bound.
	store := openStore(t, dir)
	box, at := playbacktest.NewBox(5), &boundUnit{}
	at.put(box, a)
	be := policyBackend(t, box, store, func(p *playback.DSDPolicy) { p.Bound = at.get })
	be.RefreshDSD(true)
	if rates, transports := ratesOf(be.Policy()); !reflect.DeepEqual(rates, []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256}) ||
		!reflect.DeepEqual(transports, []string{"", "", ""}) {
		t.Fatalf("A plays %v (%q) on its record's rows, want native at every rate", rates, transports)
	}
	held := store.State()
	if len(held.DACs) != 1 || held.DACs[0].Choice == nil || held.DACs[0].Choice.Mode != dsdstate.ModeNative {
		t.Fatalf("the state holds %+v, want A's entry alone, its record's native", held.DACs)
	}
	entryA := held.DACs[0]

	// B in A's place.
	at.put(box, b)
	be.RefreshDSD(true)
	p := be.Policy()
	if len(p.Tuples) != 0 || len(p.Converted) != 3 {
		t.Fatalf("B plays %v, converted %v: want DSD converted at every rate, never native or DoP", p.Tuples, p.Converted)
	}
	d := p.DSD.DAC
	if d == nil || d.Choice != (playback.DSDChoice{Mode: dsdstate.ModeAuto}) {
		t.Fatalf("B's DAC reads %+v, want the default's auto, revision 0, DoP not confirmed", d)
	}
	for _, f := range d.FirstUse {
		if f.State != playback.FirstUsePending {
			t.Errorf("B's first use of %s is %s, want pending", f.Transport, f.State)
		}
	}
	if now, _ := store.State().DAC(playbacktest.DACKey(), entryA.Stamp); !reflect.DeepEqual(now, entryA) {
		t.Errorf("A's entry once B was read is %+v, want it as it was, %+v", now, entryA)
	}

	// B's own write.
	res := be.ChooseDSD(context.Background(), playback.ChoiceRequest{DACKey: d.Key, ObservedStamp: dsdstate.StampOf(b.Manufacturer),
		Choice: policy.Choice{BaseRevision: 0, Mode: dsdstate.ModeNative, FirstUseAcknowledged: true, By: dsdstate.ByApp}})
	if res.Outcome != playback.ChoiceWritten {
		t.Fatalf("B's write answered %s", res.Outcome)
	}
	for when, st := range map[string]dsdstate.State{"in the store": store.State(), "at the next start": openStore(t, dir).State()} {
		if now, _ := st.DAC(playbacktest.DACKey(), entryA.Stamp); !reflect.DeepEqual(now, entryA) {
			t.Errorf("%s A's entry once B wrote is %+v, want it as it was, %+v", when, now, entryA)
		}
		entryB, _ := st.DAC(playbacktest.DACKey(), dsdstate.StampOf(b.Manufacturer))
		if c := entryB.Choice; c == nil || c.Mode != dsdstate.ModeNative || c.Revision != 1 || len(entryB.FirstUse) != 1 || entryB.DoPConfirmed != nil {
			t.Errorf("%s B's entry is %+v, want its own native at revision 1 and its acknowledgement alone", when, entryB)
		}
	}
}

// TestAWriteMadeFromAnotherMakersReadSideIsRefused: a choice's write carries
// the stamp of the maker whose read side it was made from. One that is no
// stamp is bad_request before the configuration lease and anything after it,
// the journal naming no mode; one of another maker's unit than the one bound,
// on the same key, is dac_changed before the state's fence and its
// compare-and-set, at revision 0, where neither unit has a choice, and at
// equal revisions, nothing written; the bound unit's own is written.
func TestAWriteMadeFromAnotherMakersReadSideIsRefused(t *testing.T) {
	a, b := makerUnit("Audio 1 Labs", "1", 2), makerUnit("Audio 2 Labs", "2", 3)
	stampA, stampB := dsdstate.StampOf(a.Manufacturer), dsdstate.StampOf(b.Manufacturer)
	backendOf := func(t *testing.T, st dsdstate.State) (*playback.Backend, *playbacktest.MemStore, *choiceLog) {
		at := &boundUnit{}
		be, store, log := choiceBackend(t, st, func(p *playback.DSDPolicy) {
			at.put(p.Box.(*playbacktest.Box), b)
			p.Bound = at.get
		})
		return be, store, log
	}
	write := func(t *testing.T, be *playback.Backend, stamp dsdstate.Stamp, base uint64, mode string) playback.ChoiceOutcome {
		t.Helper()
		return be.ChooseDSD(context.Background(), playback.ChoiceRequest{DACKey: keyOf(t, be), ObservedStamp: stamp,
			Choice: policy.Choice{BaseRevision: base, Mode: mode, By: dsdstate.ByApp}}).Outcome
	}
	t.Run("a stamp that is no stamp, before the lease", func(t *testing.T) {
		for _, stamp := range []dsdstate.Stamp{"", "s1-zz", dsdstate.Stamp(b.Manufacturer)} {
			be, store, log := backendOf(t, dsdstate.Defaults())
			if err := be.Release(bg, 7); err != nil {
				t.Fatal(err)
			}
			if got := write(t, be, stamp, 0, dsdstate.ModeOff); got != playback.ChoiceBadRequest {
				t.Errorf("a write observed as %q, the player not stopped, answered %s; want bad_request", stamp, got)
			}
			if lines := log.writes(); len(lines) != 1 || lines[0].attrs["to"] != "" || lines[0].attrs["outcome"] != string(playback.ChoiceBadRequest) {
				t.Errorf("the write's journal %+v, want one bad_request line naming no mode", lines)
			}
			if n := store.Writes(); n != 0 {
				t.Errorf("a bad_request wrote the state %d times", n)
			}
		}
	})
	t.Run("another maker's stamp, before the fence", func(t *testing.T) {
		be, store, _ := backendOf(t, dsdstate.Defaults())
		store.Land(fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded))
		if got := write(t, be, stampB, 0, dsdstate.ModeConvert); got != playback.ChoicePersistUncertain {
			t.Fatalf("the write that landed answered %s; the case needs the fence", got)
		}
		before := store.State()
		if got := write(t, be, stampA, 0, dsdstate.ModeOff); got != playback.ChoiceDACChanged {
			t.Errorf("a write from A's read side with B bound, behind the fence, answered %s; want dac_changed", got)
		}
		if after := store.State(); !reflect.DeepEqual(after, before) {
			t.Errorf("the refused write changed the state: %+v", after)
		}
	})
	for _, tc := range []struct {
		name string
		st   dsdstate.State
		base uint64
	}{
		{"another maker's stamp at revision 0", dsdstate.Defaults(), 0},
		{"another maker's stamp at equal revisions", dsdstate.State{DefaultMode: dsdstate.ModeAuto, DACs: []dsdstate.DAC{
			{Key: playbacktest.DACKey(), Stamp: stampA, Choice: &dsdstate.Choice{Mode: dsdstate.ModeConvert, Revision: 1, SetAt: playbacktest.T0, SetBy: dsdstate.ByApp}},
			{Key: playbacktest.DACKey(), Stamp: stampB, Choice: &dsdstate.Choice{Mode: dsdstate.ModeOff, Revision: 1, SetAt: playbacktest.T0, SetBy: dsdstate.ByApp}},
		}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be, store, _ := backendOf(t, tc.st)
			if got := write(t, be, stampA, tc.base, dsdstate.ModeConvert); got != playback.ChoiceDACChanged {
				t.Errorf("a write from A's read side with B bound answered %s; want dac_changed", got)
			}
			if n := store.Writes(); n != 0 || !reflect.DeepEqual(store.State(), tc.st) {
				t.Errorf("the refused write wrote %d times: %+v", n, store.State())
			}
			if got := write(t, be, stampB, tc.base, dsdstate.ModeConvert); got != playback.ChoiceWritten {
				t.Fatalf("B's own write answered %s; want written", got)
			}
			if d, _ := store.State().DAC(playbacktest.DACKey(), stampB); d.Choice == nil || d.Choice.Mode != dsdstate.ModeConvert || d.Choice.Revision != tc.base+1 {
				t.Errorf("B's entry is %+v, want its write at revision %d", d, tc.base+1)
			}
			if want, ok := tc.st.DAC(playbacktest.DACKey(), stampA); ok {
				if d, _ := store.State().DAC(playbacktest.DACKey(), stampA); !reflect.DeepEqual(d, want) {
					t.Errorf("A's entry is %+v once B wrote, want it as it was", d)
				}
			}
		})
	}
}

// TestAMakerChangeAtTheSameKeyAndFirmwareIsProbedAgain: the probe's answers
// are kept under the unit's maker as well as its model, firmware, kernel and
// enumeration, so another maker's unit of the model in the bound place, at
// the same enumeration, is probed afresh: its answers are its own, the first
// maker's stay the first's, and what each accepted is written into its own
// entry.
func TestAMakerChangeAtTheSameKeyAndFirmwareIsProbedAgain(t *testing.T) {
	probe := newHeldProbe(dsd64Frames, dsd128Frames)
	r := newProbeRig(t, probe)
	ctx := context.Background()
	r.ask(t, ctx, ready)
	within(t, probe.came, "the first maker's probe")
	r.idle(t)
	other := playbacktest.DACUnit()
	other.Manufacturer = "Other Labs"
	r.box.SetUnit(5, other)
	r.ask(t, ctx, ready)
	r.idle(t)
	if calls, _ := probe.counts(); calls != 2 {
		t.Fatalf("the probe was called %d times; want the other maker's unit probed afresh", calls)
	}
	first := probedAt(2)
	second := first
	second.Stamp = string(dsdstate.StampOf(other.Manufacturer))
	if rates, _ := accepted(r.pol, first); !reflect.DeepEqual(rates, []int{dsd64Frames}) {
		t.Errorf("the first maker's answers are DSD_U32_BE at %v, want its own probe's", rates)
	}
	if rates, _ := accepted(r.pol, second); !reflect.DeepEqual(rates, []int{dsd128Frames}) {
		t.Errorf("the other maker's answers are DSD_U32_BE at %v, want its own probe's alone", rates)
	}
	st := r.store.State()
	if e := probeEvidence(st, playbacktest.DSD64); e == nil || probeEvidence(st, playbacktest.DSD128) != nil {
		t.Errorf("the first maker's entry holds DSD64 %+v and DSD128 %+v, want its own probe's DSD64 alone", e, probeEvidence(st, playbacktest.DSD128))
	}
	d, _ := st.DAC(playbacktest.DACKey(), dsdstate.StampOf(other.Manufacturer))
	if len(d.Evidence) != 1 || d.Evidence[0].BitRate != playbacktest.DSD128 || d.Evidence[0].DriverAcceptedAt.IsZero() {
		t.Errorf("the other maker's entry holds %+v, want its own probe's DSD128 alone", d.Evidence)
	}
}
