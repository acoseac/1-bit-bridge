package playback_test

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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

	// B's own write, from the read side published while B is bound.
	res := be.ChooseDSD(context.Background(), playback.ChoiceRequest{DACKey: d.Key, DACToken: d.Token,
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

// keyedBackend is a backend whose DSD is the effective policy over box and
// store, the test DAC bound at card 5 unless edit binds another, its DAC's
// tokens minted under key (Seams.DACTokenKey), its set published once.
func keyedBackend(t *testing.T, box *playbacktest.Box, store playback.DSDStore, key []byte, edit ...func(*playback.DSDPolicy)) *playback.Backend {
	t.Helper()
	p := playbacktest.Policy(5, box, store)
	p.Log = &choiceLog{}
	for _, e := range edit {
		e(p)
	}
	b, err := playback.New(playback.Config{Resolver: noContent{}, Locator: cardFive{}, DSD: p, Seams: &playback.Seams{DACTokenKey: key}})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	return b
}

// tokenKey is a token key of 32 bytes, each b.
func tokenKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// TestTheTokenIsTheReadingsOwn (B728 B9 PR 3): the token the read side
// publishes is the one the reading the set was made from minted, which the
// backend's own function makes of the reading's model key, maker and firmware
// (dacToken): two readings of one unit, by one backend or by two under one
// key, give one token; another model key, another maker's stamp, another
// firmware or another backend's key gives another; a firmware that is empty,
// or does not read, gives one token of a token's form, alike published and
// checked, a write carrying it written; and so does a unit with no
// manufacturer string (its stamp StampNone). New refuses a key that is not 32
// bytes.
func TestTheTokenIsTheReadingsOwn(t *testing.T) {
	type read struct {
		token playback.DACToken
		be    *playback.Backend
	}
	readOf := func(t *testing.T, m alsa.UnitModel, key []byte) read {
		t.Helper()
		box := playbacktest.NewBox(5)
		box.SetUnit(5, m)
		be := keyedBackend(t, box, playbacktest.NewMemStore(dsdstate.Defaults()), key)
		tok := tokenOf(t, be)
		if !tok.Valid() {
			t.Fatalf("the token %q is not of a token's form", tok)
		}
		k, err := dsdstate.KeyOf(m.USB, m.Product)
		if err != nil {
			t.Fatal(err)
		}
		if want := be.DACTokenForTest(policy.Identity{Key: k, Stamp: dsdstate.StampOf(m.Manufacturer), BCDDevice: m.BCDDevice}); tok != want {
			t.Fatalf("the read side publishes the token %s, want the reading's own %s", tok, want)
		}
		return read{token: tok, be: be}
	}
	base := playbacktest.DACUnit()
	first := readOf(t, base, tokenKey(1))
	first.be.RefreshDSD(true)
	if again := tokenOf(t, first.be); again != first.token {
		t.Errorf("a second reading of the unit publishes %s, want %s", again, first.token)
	}
	if again := readOf(t, base, tokenKey(1)).token; again != first.token {
		t.Errorf("a second backend under the key publishes %s for the unit, want %s", again, first.token)
	}
	for _, tc := range []struct {
		name string
		edit func(*alsa.UnitModel)
		key  []byte
	}{
		{"another model key", func(m *alsa.UnitModel) { m.Product = "Another DAC" }, tokenKey(1)},
		{"another maker", func(m *alsa.UnitModel) { m.Manufacturer = "Other Labs" }, tokenKey(1)},
		{"another firmware", func(m *alsa.UnitModel) { m.BCDDevice = "0200" }, tokenKey(1)},
		{"another backend's key", func(*alsa.UnitModel) {}, tokenKey(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := playbacktest.DACUnit()
			tc.edit(&m)
			if other := readOf(t, m, tc.key).token; other == first.token {
				t.Errorf("%s gives the token %s, the test DAC's own", tc.name, other)
			}
		})
	}
	var noFirmware playback.DACToken
	for _, tc := range []struct {
		name string
		edit func(*alsa.UnitModel)
	}{
		{"an empty firmware", func(m *alsa.UnitModel) { m.BCDDevice = "" }},
		{"a firmware that does not read", func(m *alsa.UnitModel) { m.BCDDevice = "zz" }},
		{"no manufacturer string", func(m *alsa.UnitModel) { m.Manufacturer = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := playbacktest.DACUnit()
			tc.edit(&m)
			r := readOf(t, m, tokenKey(1))
			if m.BCDDevice != playbacktest.DACBCD {
				if noFirmware == "" {
					noFirmware = r.token
				} else if r.token != noFirmware {
					t.Errorf("a firmware that does not read gives %s, an empty one %s: both are no firmware", r.token, noFirmware)
				}
			}
			wantChoice(t, r.be.ChooseDSD(bg, choiceOf(t, r.be, dsdstate.ModeConvert, 0)), playback.ChoiceWritten)
		})
	}
	t.Run("a key that is not 32 bytes", func(t *testing.T) {
		p := playbacktest.Policy(5, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults()))
		for _, k := range [][]byte{bytes.Repeat([]byte{1}, 31), bytes.Repeat([]byte{1}, 33), {}} {
			if _, err := playback.New(playback.Config{Resolver: noContent{}, Locator: cardFive{}, DSD: p, Seams: &playback.Seams{DACTokenKey: k}}); err == nil {
				t.Errorf("New took a token key of %d bytes", len(k))
			}
		}
	})
}

// TestAWriteCarriesTheTokenItsReadSidePublished (B728 B9 PR 3): a choice's
// write carries the token GET /v1/outputs published with the read side it was
// made from, and is written only where that token is its own reading's. The
// token the backend publishes is written. One not of a token's form is
// bad_request before the writes' turn, a write holding the turn: the write
// never queues for it, and its journal names no mode. The token of another
// unit than the one the write reads is dac_changed before the state's fence and
// its compare-and-set, nothing written, the answer carrying the token of the
// unit bound: a unit of another maker on the key, at revision 0 where neither
// unit has a choice, and at equal revisions; the bound unit on another
// firmware; another unit of the key the wire names, whose product string holds
// its serial, which the state keys apart and the wire does not; and the token
// of another boot of the backend, from a second backend over the same state,
// whose own token is then written.
func TestAWriteCarriesTheTokenItsReadSidePublished(t *testing.T) {
	a, b := makerUnit("Audio 1 Labs", "1", 2), makerUnit("Audio 2 Labs", "2", 3)
	stampA, stampB := dsdstate.StampOf(a.Manufacturer), dsdstate.StampOf(b.Manufacturer)
	// rig is a backend over st with first bound, and bind, which binds another
	// unit in its place and publishes its set.
	rig := func(t *testing.T, st dsdstate.State, first alsa.UnitModel) (*playback.Backend, *playbacktest.MemStore, *choiceLog, func(alsa.UnitModel)) {
		t.Helper()
		at := &boundUnit{}
		var box *playbacktest.Box
		be, store, log := choiceBackend(t, st, func(p *playback.DSDPolicy) {
			box = p.Box.(*playbacktest.Box)
			at.put(box, first)
			p.Bound = at.get
		})
		return be, store, log, func(m alsa.UnitModel) {
			at.put(box, m)
			be.RefreshDSD(true)
		}
	}
	write := func(be *playback.Backend, key string, token playback.DACToken, base uint64, mode string) playback.ChoiceResult {
		return be.ChooseDSD(bg, playback.ChoiceRequest{DACKey: key, DACToken: token, Choice: policy.Choice{BaseRevision: base, Mode: mode, By: dsdstate.ByApp}})
	}
	// refused fails t unless res is dac_changed, nothing written, its answer
	// the epoch published, its DAC the bound unit's.
	refused := func(t *testing.T, be *playback.Backend, store *playbacktest.MemStore, res playback.ChoiceResult, st dsdstate.State, writes int) {
		t.Helper()
		wantChoice(t, res, playback.ChoiceDACChanged)
		if n := store.Writes(); n != writes || !reflect.DeepEqual(store.State(), st) {
			t.Errorf("the refused write wrote the state (%d writes, was %d): %+v", n, writes, store.State())
		}
		if !res.Epoch || !reflect.DeepEqual(res.Policy, be.Policy()) {
			t.Errorf("the answer's epoch %+v (epoch %v), want the one published %+v", res.Policy.DSD, res.Epoch, be.Policy().DSD)
		}
		if got := dacOf(t, res.Policy).Token; got != tokenOf(t, be) {
			t.Errorf("the answer carries the token %s, want the bound unit's %s", got, tokenOf(t, be))
		}
	}

	t.Run("the token published is written", func(t *testing.T) {
		be, store, _, _ := rig(t, dsdstate.Defaults(), a)
		wantChoice(t, write(be, keyOf(t, be), tokenOf(t, be), 0, dsdstate.ModeConvert), playback.ChoiceWritten)
		if d, _ := store.State().DAC(playbacktest.DACKey(), stampA); d.Choice == nil || d.Choice.Mode != dsdstate.ModeConvert {
			t.Errorf("the bound unit's entry is %+v, want its write", d)
		}
	})

	t.Run("a token not of a token's form, before the turn", func(t *testing.T) {
		store := newHeldStore(dsdstate.Defaults())
		log := &choiceLog{}
		var queued atomic.Int32
		be, err := playback.New(playback.Config{Resolver: noContent{}, Locator: cardFive{}, DSD: func() *playback.DSDPolicy {
			p := playbacktest.Policy(5, playbacktest.NewBox(5), store)
			p.Log = log
			return p
		}(), Seams: &playback.Seams{ChoiceQueued: func() { queued.Add(1) }}})
		if err != nil {
			t.Fatal(err)
		}
		be.RefreshDSD(true)
		key := keyOf(t, be)
		held := make(chan playback.ChoiceResult, 1)
		store.armed.Store(true)
		t.Cleanup(store.let)
		go func() { held <- write(be, key, tokenOf(t, be), 0, dsdstate.ModeConvert) }()
		within(t, store.came, "the write holding the turn, inside the state's write")
		for _, token := range []playback.DACToken{"", "t1-", "t1-zz", playback.DACToken("t1-" + strings.Repeat("A", 32)), playback.DACToken("t1-" + strings.Repeat("a", 31)),
			playback.DACToken("t1-" + strings.Repeat("a", 33)), playback.DACToken("t2-" + strings.Repeat("a", 32)), playback.DACToken(stampA)} {
			answered := make(chan playback.ChoiceResult, 1)
			go func() { answered <- write(be, key, token, 0, dsdstate.ModeConvert) }()
			res := withinFor(t, answered, askWithin, "the answer of a write of no token's form, the turn held")
			wantChoice(t, res, playback.ChoiceBadRequest)
			if res.Epoch {
				t.Errorf("bad_request for %q carries an epoch: it is no write", token)
			}
		}
		if n := queued.Load(); n != 0 {
			t.Errorf("a write of no token's form queued for the turn %d times: its check comes before it", n)
		}
		store.let()
		wantChoice(t, within(t, held, "the held write's answer"), playback.ChoiceWritten)
		for _, line := range log.writes() {
			if line.attrs["outcome"] == string(playback.ChoiceBadRequest) && line.attrs["to"] != "" {
				t.Errorf("a bad_request's journal names the mode %q", line.attrs["to"])
			}
		}
	})

	for _, tc := range []struct {
		name string
		st   dsdstate.State
		base uint64
	}{
		{"a unit of another maker on the key, at revision 0", dsdstate.Defaults(), 0},
		{"a unit of another maker on the key, at equal revisions", dsdstate.State{DefaultMode: dsdstate.ModeAuto, DACs: []dsdstate.DAC{
			{Key: playbacktest.DACKey(), Stamp: stampA, Choice: &dsdstate.Choice{Mode: dsdstate.ModeConvert, Revision: 1, SetAt: playbacktest.T0, SetBy: dsdstate.ByApp}},
			{Key: playbacktest.DACKey(), Stamp: stampB, Choice: &dsdstate.Choice{Mode: dsdstate.ModeOff, Revision: 1, SetAt: playbacktest.T0, SetBy: dsdstate.ByApp}},
		}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be, store, _, bind := rig(t, tc.st, a)
			key, tokenA := keyOf(t, be), tokenOf(t, be)
			bind(b)
			if keyOf(t, be) != key || tokenOf(t, be) == tokenA {
				t.Fatalf("B bound publishes the key %s and the token %s: the case needs A's key and another token", keyOf(t, be), tokenOf(t, be))
			}
			refused(t, be, store, write(be, key, tokenA, tc.base, dsdstate.ModeConvert), tc.st, 0)
			wantChoice(t, write(be, key, tokenOf(t, be), tc.base, dsdstate.ModeConvert), playback.ChoiceWritten)
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

	t.Run("the bound unit on another firmware", func(t *testing.T) {
		be, store, _, bind := rig(t, dsdstate.Defaults(), a)
		key, token := keyOf(t, be), tokenOf(t, be)
		updated := a
		updated.BCDDevice = "0200"
		bind(updated)
		refused(t, be, store, write(be, key, token, 0, dsdstate.ModeConvert), dsdstate.Defaults(), 0)
	})

	t.Run("another unit of the key the wire names", func(t *testing.T) {
		withSerial := func(serial string) alsa.UnitModel {
			m := playbacktest.DACUnit()
			m.USB.Serial, m.Product = usbid.New(serial), "Test DAC "+serial
			return m
		}
		be, store, _, bind := rig(t, dsdstate.Defaults(), withSerial("SN-0001"))
		key, token := keyOf(t, be), tokenOf(t, be)
		bind(withSerial("SN-0002"))
		if keyOf(t, be) != key {
			t.Fatalf("the second unit publishes the key %s, the first %s: the case needs one key on the wire", keyOf(t, be), key)
		}
		refused(t, be, store, write(be, key, token, 0, dsdstate.ModeConvert), dsdstate.Defaults(), 0)
	})

	t.Run("a unit of another maker's token, before the fence", func(t *testing.T) {
		be, store, _, bind := rig(t, dsdstate.Defaults(), a)
		key, tokenA := keyOf(t, be), tokenOf(t, be)
		bind(b)
		store.Land(fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded))
		wantChoice(t, write(be, key, tokenOf(t, be), 0, dsdstate.ModeConvert), playback.ChoicePersistUncertain)
		was := store.State()
		refused(t, be, store, write(be, key, tokenA, 0, dsdstate.ModeOff), was, 0)
	})

	t.Run("a token of another boot of the backend", func(t *testing.T) {
		box, store := playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults())
		first := keyedBackend(t, box, store, tokenKey(1))
		key, earlier := keyOf(t, first), tokenOf(t, first)
		second := keyedBackend(t, box, store, tokenKey(2))
		if tokenOf(t, second) == earlier {
			t.Fatal("two boots publish one token: the case needs another")
		}
		refused(t, second, store, write(second, key, earlier, 0, dsdstate.ModeConvert), dsdstate.Defaults(), 0)
		wantChoice(t, write(second, key, tokenOf(t, second), 0, dsdstate.ModeConvert), playback.ChoiceWritten)
	})
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
