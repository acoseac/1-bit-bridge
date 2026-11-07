package playback_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// A write of the bound DAC's DSD choice through the backend (B728 B9; the
// design's section 7.2), without a session: its outcomes, what each refusal
// leaves as it was, the compare-and-set between writers, the window between
// the state's write and the publish, a write whose state lands and is not
// known to be durable, and the probe a write makes due.

// choiceLog records the policy's lines, each with its attributes.
type choiceLog struct {
	mu    sync.Mutex
	lines []choiceLine
}

type choiceLine struct {
	level, msg string
	attrs      map[string]string
}

func (l *choiceLog) add(level, msg string, args []any) {
	attrs := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		attrs[fmt.Sprint(args[i])] = fmt.Sprint(args[i+1])
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, choiceLine{level: level, msg: msg, attrs: attrs})
}

func (l *choiceLog) Info(msg string, args ...any) { l.add("info", msg, args) }
func (l *choiceLog) Warn(msg string, args ...any) { l.add("warn", msg, args) }

// writes are the lines a choice write logged: each one with its outcome.
func (l *choiceLog) writes() []choiceLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []choiceLine
	for _, line := range l.lines {
		if _, ok := line.attrs["outcome"]; ok {
			out = append(out, line)
		}
	}
	return out
}

// everything is every string the log holds.
func (l *choiceLog) everything() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, line := range l.lines {
		b.WriteString(line.msg)
		for k, v := range line.attrs {
			b.WriteString(" " + k + "=" + v)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// choiceBackend is a backend whose DSD is the effective policy over a box
// with the test DAC at card 5 and st, its lines in its log, edit applied to
// the policy first, its set published once.
func choiceBackend(t *testing.T, st dsdstate.State, edit ...func(*playback.DSDPolicy)) (*playback.Backend, *playbacktest.MemStore, *choiceLog) {
	t.Helper()
	store := playbacktest.NewMemStore(st)
	log := &choiceLog{}
	b := policyBackend(t, playbacktest.NewBox(5), store, append([]func(*playback.DSDPolicy){func(p *playback.DSDPolicy) { p.Log = log }}, edit...)...)
	b.RefreshDSD(true)
	return b, store, log
}

// keyOf is the bound DAC's dacKey as b publishes it.
func keyOf(t *testing.T, b *playback.Backend) string {
	t.Helper()
	v := b.Policy().DSD
	if v == nil || v.DAC == nil {
		t.Fatalf("the backend publishes no DAC: %+v", v)
	}
	return v.DAC.Key
}

// revisionOf is the bound DAC's choice's revision as p publishes it.
func revisionOf(t *testing.T, p playback.PlayerPolicy) uint64 {
	t.Helper()
	if p.DSD == nil || p.DSD.DAC == nil {
		t.Fatalf("the epoch publishes no DAC: %+v", p.DSD)
	}
	return p.DSD.DAC.Choice.Revision
}

// choiceOf is a write of mode over base for b's DAC.
func choiceOf(t *testing.T, b *playback.Backend, mode string, base uint64, edit ...func(*playback.ChoiceRequest)) playback.ChoiceRequest {
	t.Helper()
	r := playback.ChoiceRequest{DACKey: keyOf(t, b), Choice: policy.Choice{BaseRevision: base, Mode: mode, By: dsdstate.ByApp}}
	for _, e := range edit {
		e(&r)
	}
	return r
}

func acknowledged(r *playback.ChoiceRequest) { r.Choice.FirstUseAcknowledged = true }
func confirmed(r *playback.ChoiceRequest)    { r.Choice.ConfirmDoP = true }
func interrupting(r *playback.ChoiceRequest) { r.Interrupt = true }

// wantOutcome fails t unless res's outcome is want.
func wantChoice(t *testing.T, res playback.ChoiceResult, want playback.ChoiceOutcome) {
	t.Helper()
	if res.Outcome != want {
		t.Fatalf("the write's outcome is %s, want %s", res.Outcome, want)
	}
}

// TestAChoiceIsWrittenAndAnsweredFromTheEpochItPublished (the design's
// section 7.2): a write lands in the state, moves the choice's revision, and
// is answered with the epoch the backend published once it was written, which
// is what the backend publishes afterwards: a choice that leaves what plays as
// it was (auto converting an unknown DAC's DSD, then convert) at the same
// generation, and one that changes it (native) at the next.
func TestAChoiceIsWrittenAndAnsweredFromTheEpochItPublished(t *testing.T) {
	b, store, _ := choiceBackend(t, dsdstate.Defaults())
	before := b.Policy()
	res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 0))
	wantChoice(t, res, playback.ChoiceWritten)
	if !reflect.DeepEqual(res.Policy, b.Policy()) {
		t.Errorf("the answer's epoch %+v is not the one published after it %+v", res.Policy, b.Policy())
	}
	if res.Policy.Generation != before.Generation {
		t.Errorf("convert after auto, which converted the same rates, published generation %d after %d: a change that leaves what plays as it was keeps the generation", res.Policy.Generation, before.Generation)
	}
	if c := res.Policy.DSD.DAC.Choice; c.Mode != dsdstate.ModeConvert || c.Revision != 1 {
		t.Errorf("the answer's choice %+v, want convert at revision 1", c)
	}
	if mode, _, own := store.State().ChoiceFor(playbacktest.DACKey()); !own || mode != dsdstate.ModeConvert {
		t.Errorf("the state holds %s (own %v), want convert", mode, own)
	}
	res = b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1, acknowledged))
	wantChoice(t, res, playback.ChoiceWritten)
	if res.Policy.Generation != before.Generation+1 {
		t.Errorf("native after convert published generation %d, want %d", res.Policy.Generation, before.Generation+1)
	}
	if rates, _ := ratesOf(res.Policy); !slices.Equal(rates, []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256}) {
		t.Errorf("native published tuples at %v, want the three rates", rates)
	}
}

// TestEveryRefusalChangesNothing (binding note 1; the design's section 7.2):
// each check refuses with its own word, and a refused write writes nothing
// and publishes nothing: the state, the epoch and the probe's bookkeeping are
// as they were, and one line says the write and its outcome.
func TestEveryRefusalChangesNothing(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	for _, tc := range []struct {
		name  string
		st    dsdstate.State
		edit  func(*playback.DSDPolicy)
		store func(*playbacktest.MemStore)
		req   func(*testing.T, *playback.Backend) playback.ChoiceRequest
		want  playback.ChoiceOutcome
	}{
		{"a word the write does not know", native, nil, nil, func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
			return choiceOf(t, b, "loud", 1)
		}, playback.ChoiceBadRequest},
		{"no DAC available", native, func(p *playback.DSDPolicy) {
			p.Bound = func() (playback.BoundDAC, bool) { return playback.BoundDAC{}, false }
		}, nil,
			func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
				return playback.ChoiceRequest{DACKey: "d1-0123456789abcdef", Choice: policy.Choice{BaseRevision: 1, Mode: dsdstate.ModeOff, By: dsdstate.ByApp}}
			}, playback.ChoiceDACUnavailable},
		{"another DAC's key", native, nil, nil, func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
			return choiceOf(t, b, dsdstate.ModeOff, 1, func(r *playback.ChoiceRequest) { r.DACKey = "d1-0000000000000000" })
		}, playback.ChoiceDACChanged},
		{"a stale revision", native, nil, nil, func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
			return choiceOf(t, b, dsdstate.ModeOff, 0)
		}, playback.ChoiceStale},
		{"a mode not offered", dsdstate.Defaults(), func(p *playback.DSDPolicy) {
			p.Box.(*playbacktest.Box).SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
		}, nil, func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
			return choiceOf(t, b, dsdstate.ModeNative, 0, acknowledged)
		}, playback.ChoiceModeNotOffered},
		{"DoP unconfirmed", dsdstate.Defaults(), nil, nil, func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
			return choiceOf(t, b, dsdstate.ModeDoP, 0, acknowledged)
		}, playback.ChoiceConfirmationRequired},
		{"first use unacknowledged", dsdstate.Defaults(), nil, nil, func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
			return choiceOf(t, b, dsdstate.ModeNative, 0)
		}, playback.ChoiceFirstUseUnacknowledged},
		{"a write that fails before it lands", native, nil, func(m *playbacktest.MemStore) { m.Fail(errors.New("no space left on device")) },
			func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
				return choiceOf(t, b, dsdstate.ModeOff, 1)
			}, playback.ChoicePersistFailed},
		{"a file refused in memory", native, nil, func(m *playbacktest.MemStore) {
			m.Fail(fmt.Errorf("%w: the entry is still there", dsdstate.ErrNotWritable))
		}, func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
			return choiceOf(t, b, dsdstate.ModeOff, 1)
		}, playback.ChoiceStateUnreadable},
		{"the fence of an earlier landed write", native, nil, func(m *playbacktest.MemStore) { m.Fail(dsdstate.ErrLandedNotHeld) },
			func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
				return choiceOf(t, b, dsdstate.ModeOff, 1)
			}, playback.ChoicePersistUncertain},
		{"a write too large for its file", native, nil, func(m *playbacktest.MemStore) { m.Fail(dsdstate.ErrTooLarge) },
			func(t *testing.T, b *playback.Backend) playback.ChoiceRequest {
				return choiceOf(t, b, dsdstate.ModeOff, 1)
			}, playback.ChoicePersistFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var edit []func(*playback.DSDPolicy)
			if tc.edit != nil {
				edit = append(edit, tc.edit)
			}
			b, store, log := choiceBackend(t, tc.st, edit...)
			req := tc.req(t, b)
			if tc.store != nil {
				tc.store(store)
			}
			state, writes, published := store.State(), store.Writes(), b.Policy()
			res := b.ChooseDSD(bg, req)
			wantChoice(t, res, tc.want)
			if store.Writes() != writes || !reflect.DeepEqual(store.State(), state) {
				t.Errorf("a refused write changed the state: %+v, was %+v", store.State(), state)
			}
			if !reflect.DeepEqual(b.Policy(), published) {
				t.Errorf("a refused write moved the epoch: %+v, was %+v", b.Policy(), published)
			}
			if b.ProbeDSDPending() {
				t.Error("a refused write made a probe due")
			}
			lines := log.writes()
			if len(lines) != 1 || lines[0].attrs["outcome"] != string(tc.want) {
				t.Errorf("the write's lines %+v, want one with outcome %s", lines, tc.want)
			}
		})
	}
}

// TestTwoWritesOfOneRevisionLandOnce (binding note 2): writes made together
// from one read of the choice's revision land one at a time on the state the
// last left, so exactly one lands and every other is stale, carrying the
// epoch the landed one published, whose revision a write-back takes.
func TestTwoWritesOfOneRevisionLandOnce(t *testing.T) {
	b, store, _ := choiceBackend(t, dsdstate.Defaults())
	req := []playback.ChoiceRequest{choiceOf(t, b, dsdstate.ModeConvert, 0), choiceOf(t, b, dsdstate.ModeOff, 0)}
	const n = 8
	results := make([]playback.ChoiceResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = b.ChooseDSD(bg, req[i%2])
		}()
	}
	close(start)
	wg.Wait()
	written := 0
	for i, res := range results {
		switch res.Outcome {
		case playback.ChoiceWritten:
			written++
		case playback.ChoiceStale:
			if got := revisionOf(t, res.Policy); got != 1 {
				t.Errorf("write %d was stale carrying revision %d, want 1, the landed write's", i, got)
			}
		default:
			t.Errorf("write %d: %s", i, res.Outcome)
		}
	}
	if written != 1 {
		t.Fatalf("%d writes of revision 0 landed, want exactly one", written)
	}
	if store.Writes() != 1 {
		t.Errorf("the state was written %d times, want once", store.Writes())
	}
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, 1)), playback.ChoiceWritten)
}

// TestAWriteBackOfARevisionReadInTheWindowIsAnsweredWithTheDurableOne (binding
// note 2): between the state's write and the publish the published epoch
// trails the state; a write made from a revision read in that window waits
// for the write in flight, and answers stale with the revision it made
// durable, which a write-back then takes; a write-back straight after a read
// of the published epoch lands.
func TestAWriteBackOfARevisionReadInTheWindowIsAnsweredWithTheDurableOne(t *testing.T) {
	inWindow, release, queued := make(chan struct{}), make(chan struct{}), make(chan struct{}, 4)
	var once sync.Once
	b, store, _ := choiceBackend(t, dsdstate.Defaults())
	seams := &playback.Seams{
		ChoiceWritten: func(error) {
			once.Do(func() {
				close(inWindow)
				<-release
			})
		},
		ChoiceQueued: func() { queued <- struct{}{} },
	}
	playback.SetSeamsForTest(b, seams)
	first := make(chan playback.ChoiceResult, 1)
	go func() { first <- b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 0)) }()
	select {
	case <-inWindow:
	case res := <-first:
		t.Fatalf("the write ended (%s) without coming to the window between its state's write and its publish", res.Outcome)
	case <-time.After(watchdog):
		t.Fatal("the write never came to the window between its state's write and its publish")
	}
	read := b.Policy()
	if got := revisionOf(t, read); got != 0 {
		t.Fatalf("the epoch published in the window says revision %d, want 0: it trails the state", got)
	}
	if mode, _, _ := store.State().ChoiceFor(playbacktest.DACKey()); mode != dsdstate.ModeConvert {
		t.Fatalf("the state in the window holds %s, want convert, written", mode)
	}
	second := make(chan playback.ChoiceResult, 1)
	go func() { second <- b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, revisionOf(t, read))) }()
	select {
	case <-queued:
	case <-time.After(watchdog):
		t.Fatal("a write made in the window did not wait for the write in flight")
	}
	close(release)
	wantChoice(t, <-first, playback.ChoiceWritten)
	res := <-second
	wantChoice(t, res, playback.ChoiceStale)
	if got := revisionOf(t, res.Policy); got != 1 {
		t.Fatalf("the stale answer carries revision %d, want 1, the durable one", got)
	}
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, revisionOf(t, res.Policy))), playback.ChoiceWritten)
	// A write-back straight after a read of the epoch.
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeAuto, revisionOf(t, b.Policy()))), playback.ChoiceWritten)
}

// TestAStaleAnswerNeverCarriesARevisionTheStateHasMovedPast (binding note 2):
// where another writer (the migration's part for a DAC) moved the choice's
// revision and its refresh has not published it yet, a stale answer reads the
// policy again first, so the revision it carries is the state's.
func TestAStaleAnswerNeverCarriesARevisionTheStateHasMovedPast(t *testing.T) {
	b, store, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative))
	st := store.State()
	st.DACs[0].Choice.Revision, st.DACs[0].Choice.Mode = 2, dsdstate.ModeConvert
	store.Set(st)
	res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, 1))
	wantChoice(t, res, playback.ChoiceStale)
	if got := revisionOf(t, res.Policy); got != 2 {
		t.Fatalf("the stale answer carries revision %d, want 2, the state's", got)
	}
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, 2)), playback.ChoiceWritten)
}

// TestARevisionMovedBeforeTheStoresWriteIsStale (binding note 2): the
// compare-and-set reads the state inside the store's one write, so a write
// another writer makes between a choice write's reading and its own (the
// migration's part for a DAC, which writes a choice) makes it stale, and is
// never written over: one that moves the revision alone, what plays as it
// was, which only the compare-and-set inside the write can see, and one that
// changes what plays too, which sends the write round again first.
func TestARevisionMovedBeforeTheStoresWriteIsStale(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
	}{
		{"the revision alone", dsdstate.ModeNative},
		{"what plays too", dsdstate.ModeConvert},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, store, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative))
			var once sync.Once
			playback.SetSeamsForTest(b, &playback.Seams{ChoicePending: func() {
				once.Do(func() {
					if err := store.Update(func(st *dsdstate.State) error {
						st.DACs[0].Choice.Revision, st.DACs[0].Choice.Mode, st.DACs[0].Choice.SetBy = 2, tc.mode, dsdstate.ByMigration
						return nil
					}); err != nil {
						t.Error(err)
					}
				})
			}})
			res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, 1))
			wantChoice(t, res, playback.ChoiceStale)
			if c := store.State().DACs[0].Choice; c.Mode != tc.mode || c.Revision != 2 || c.SetBy != dsdstate.ByMigration {
				t.Errorf("the state holds %+v, want the other writer's %s at revision 2", c, tc.mode)
			}
			if got := revisionOf(t, res.Policy); got != 2 {
				t.Errorf("the stale answer carries revision %d, want 2", got)
			}
		})
	}
}

// TestARejectionAtTheCurrentRevisionNeedsTheAcknowledgement (binding note 2):
// a state that holds a listener's rejection of native at its current revision
// needs the acknowledgement to choose native again, answers the first-use
// word without it, and clears the rejection with it.
func TestARejectionAtTheCurrentRevisionNeedsTheAcknowledgement(t *testing.T) {
	st := playbacktest.ChoiceState(dsdstate.ModeNative)
	st.DACs[0].Listener = []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, Answer: dsdstate.AnswerNoise, On: "2026-11-06"}}
	b, store, _ := choiceBackend(t, st)
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1)), playback.ChoiceFirstUseUnacknowledged)
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1, acknowledged)), playback.ChoiceWritten)
	if d, _ := store.State().DAC(playbacktest.DACKey()); len(d.Listener) != 0 {
		t.Errorf("the rejection is still held: %+v", d.Listener)
	}
}

// TestTheRevisionOfAStateThatDidNotRead (binding notes 2 and 13): a state that
// did not read publishes revision 0, its read side holding no choice; where a
// fresh state replaced the file, a write-back of 0 lands and clears the
// refusal; where the file is refused in memory, every write answers
// state_unreadable and writes nothing.
func TestTheRevisionOfAStateThatDidNotRead(t *testing.T) {
	t.Run("replaced", func(t *testing.T) {
		b, store, _ := choiceBackend(t, dsdstate.Refusing(playbacktest.T0))
		if got := revisionOf(t, b.Policy()); got != 0 {
			t.Fatalf("a replaced state publishes revision %d, want 0", got)
		}
		res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 0))
		wantChoice(t, res, playback.ChoiceWritten)
		if store.State().Unreadable() {
			t.Error("the write left the state refusing")
		}
		if got := res.Policy.Converted; len(got) != 3 {
			t.Errorf("the answer converts %+v, want the three rates", got)
		}
	})
	t.Run("refused in memory", func(t *testing.T) {
		b, store, _ := choiceBackend(t, dsdstate.Refusing(playbacktest.T0))
		store.Fail(fmt.Errorf("%w: a named pipe is there", dsdstate.ErrNotWritable))
		for range 2 {
			wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 0)), playback.ChoiceStateUnreadable)
		}
		if store.Writes() != 0 {
			t.Errorf("a state refused in memory was written %d times", store.Writes())
		}
	})
}

// TestAnEvidenceWriteRacingAChoiceMovesNoRevision (binding note 11): passive
// evidence never moves the choice's revision, so a write whose base was read
// before an evidence write that lands while it is made lands too, the evidence
// kept; and a change that moves what the write would publish between its
// reading and its write sends it round once more, on the state as it is.
func TestAnEvidenceWriteRacingAChoiceMovesNoRevision(t *testing.T) {
	t.Run("streaming observed", func(t *testing.T) {
		b, store, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		var once sync.Once
		playback.SetSeamsForTest(b, &playback.Seams{ChoicePending: func() {
			once.Do(func() {
				if err := store.Update(func(st *dsdstate.State) error {
					st.DACs[0].Evidence = append(st.DACs[0].Evidence, dsdstate.Evidence{Transport: dsdstate.TransportNative, Format: "DSD_U32_BE",
						BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, StreamingObservedAt: playbacktest.T0})
					return nil
				}); err != nil {
					t.Error(err)
				}
			})
		}})
		wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1, func(r *playback.ChoiceRequest) { r.Choice.MaxBitRate = playbacktest.DSD128 })), playback.ChoiceWritten)
		if playbacktest.Evidence(store.State(), dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64) == nil {
			t.Error("the evidence written beside the choice is gone")
		}
		if c := store.State().DACs[0].Choice; c.Revision != 2 || c.MaxBitRate != playbacktest.DSD128 {
			t.Errorf("the choice %+v, want the cap at revision 2", c)
		}
	})
	t.Run("a change that moves the set", func(t *testing.T) {
		b, store, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeAuto), func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) })
		var rounds atomic.Int32
		playback.SetSeamsForTest(b, &playback.Seams{ChoicePending: func() {
			if rounds.Add(1) != 1 {
				return
			}
			if err := store.Update(func(st *dsdstate.State) error {
				st.DACs[0].Listener = []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, Answer: dsdstate.AnswerNoise}}
				return nil
			}); err != nil {
				t.Error(err)
			}
		}})
		wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1, acknowledged)), playback.ChoiceWritten)
		if got := rounds.Load(); got != 2 {
			t.Errorf("the write went round %d times, want twice: once more for the answer that moved the set", got)
		}
		if d, _ := store.State().DAC(playbacktest.DACKey()); len(d.Listener) != 0 {
			t.Errorf("native chosen with its acknowledgement left the rejection: %+v", d.Listener)
		}
	})
}

// landedError is a write whose rename landed and whose barrier failed.
var landedError = fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded)

// TestALandedWriteIsUncertainAndPublishesWhatBothStatesPlay (binding notes 3 and
// 5): a write that landed and is not known to be durable is persist_uncertain:
// the store holds the state as it was, the file the write's, so until the
// next start the backend plays a rate only where both play it alike,
// refusing the rest as dsd_choice_uncertain; every later write is fenced and
// persist_uncertain too, changing nothing.
func TestALandedWriteIsUncertainAndPublishesWhatBothStatesPlay(t *testing.T) {
	b, store, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) })
	before := b.Policy()
	store.Land(landedError)
	res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1, func(r *playback.ChoiceRequest) { r.Choice.MaxBitRate = playbacktest.DSD128 }))
	wantChoice(t, res, playback.ChoicePersistUncertain)
	if landed, ok := store.Landed(); !ok || landed.DACs[0].Choice.MaxBitRate != playbacktest.DSD128 {
		t.Fatalf("the file holds %+v, want the capped choice", landed)
	}
	check := func(p playback.PlayerPolicy) {
		t.Helper()
		if rates, _ := ratesOf(p); !slices.Equal(rates, []int{playbacktest.DSD64, playbacktest.DSD128}) {
			t.Errorf("published tuples at %v, want DSD64 and DSD128, which both states play natively", rates)
		}
		if p.DSD == nil || !slices.Contains(p.DSD.Refused, playback.DSDRefusal{BitRate: playbacktest.DSD256, Reason: string(policy.ReasonChoiceUncertain)}) {
			t.Errorf("the refusals %+v, want DSD256 refused as dsd_choice_uncertain", p.DSD)
		}
	}
	check(b.Policy())
	if b.Policy().Generation != before.Generation+1 {
		t.Errorf("generation %d after %d: the intersection plays less, a new set", b.Policy().Generation, before.Generation)
	}
	b.RefreshDSD(true)
	check(b.Policy())
	published := b.Policy()
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, 1)), playback.ChoicePersistUncertain)
	if !reflect.DeepEqual(b.Policy(), published) {
		t.Errorf("a fenced write moved the epoch: %+v", b.Policy())
	}
}

// TestALandedWriteOfTheRealStoreFencesWhatFollowsAndIsReadAtTheNextStart
// (binding note 5): a sync failure once the rename landed, on the player's
// own store: the write is persist_uncertain, another write and an evidence
// write are fenced, and a reopen of the state reads what landed.
func TestALandedWriteOfTheRealStoreFencesWhatFollowsAndIsReadAtTheNextStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the store's writes are refused where no directory can be synced, Windows, where the player does not run")
	}
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	dir := t.TempDir()
	path := filepath.Join(dir, dsdstate.FileName)
	var failed atomic.Bool
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(d string) error {
		if _, err := os.Stat(path); err == nil && failed.CompareAndSwap(false, true) {
			return errors.New("the directory's sync failed")
		}
		return nil
	}))
	store, err := dsdstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := &choiceLog{}
	b := policyBackend(t, playbacktest.NewBox(5), store, func(p *playback.DSDPolicy) { p.Log = log })
	b.RefreshDSD(true)
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 0)), playback.ChoicePersistUncertain)
	if !failed.Load() {
		t.Fatal("no directory sync failed: the seam did not reach the write")
	}
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, 0)), playback.ChoicePersistUncertain)
	if err := store.Update(func(st *dsdstate.State) error {
		st.DefaultMode = dsdstate.ModeOff
		return nil
	}); !errors.Is(err, dsdstate.ErrLandedNotHeld) {
		t.Errorf("an evidence write after the landed one: %v, want the fence", err)
	}
	again, err := dsdstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode, _, own := again.State().ChoiceFor(playbacktest.DACKey()); !own || mode != dsdstate.ModeConvert {
		t.Errorf("the next start reads %s (own %v), want convert, what landed", mode, own)
	}
}

// TestAChoiceWriteOnWindowsIsRefusedAndWritesNothing (binding note 10): where
// no directory can be synced (Windows) the store refuses every write and
// writes nothing, so a choice is persist_failed and leaves no file; where one
// can, it lands.
func TestAChoiceWriteOnWindowsIsRefusedAndWritesNothing(t *testing.T) {
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	dir := t.TempDir()
	store, err := dsdstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := policyBackend(t, playbacktest.NewBox(5), store)
	b.RefreshDSD(true)
	res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 0))
	_, statErr := os.Stat(filepath.Join(dir, dsdstate.FileName))
	if runtime.GOOS == "windows" {
		if res.Outcome != playback.ChoicePersistFailed || !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("on Windows: %s, the file %v; want persist_failed and no file", res.Outcome, statErr)
		}
		return
	}
	if res.Outcome != playback.ChoiceWritten || statErr != nil {
		t.Fatalf("%s, the file %v; want written", res.Outcome, statErr)
	}
}

// acceptedNative is a report of the probe's that accepted DSD_U32_BE at the
// three rates' frame rates, with the kernel's PAUSE offered.
func acceptedNative(ep alsapcm.Endpoint) alsapcm.ProbeReport {
	rep := alsapcm.ProbeReport{Endpoint: ep, Opened: true}
	for _, rate := range []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256} {
		rep.Results = append(rep.Results, alsapcm.ProbeResult{Candidate: alsapcm.ProbeCandidate{Format: format.DSDU32BE, RateHz: rate / 32}, Answer: alsapcm.ProbeAccepted, Msbits: 32, Pause: true})
	}
	return rep
}

// TestAWrittenChoiceOwesAProbeAndKeepsTheProbesAnswers (binding note 5; the
// design's section 5): a write makes a probe due for the supervisor's next
// ask and keeps the probe's answers, which the descriptors and the kernel
// give and no choice changes, so a write that leaves what plays as it was
// moves no tuple's pause; the answer claims no driver acceptance the probe
// did not give, and the probe's own, when it comes, is published beside the
// set as it was.
func TestAWrittenChoiceOwesAProbeAndKeepsTheProbesAnswers(t *testing.T) {
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	pol := playbacktest.Policy(5, playbacktest.NewBox(5), store)
	unit := alsapcm.ProbeUnit{Model: playbacktest.DACKey().Canonical(), Firmware: playbacktest.DACBCD, Kernel: playbacktest.Kernel, Enumeration: "test/2"}
	pol.Probes.Record(pol.Probes.Begin(unit), acceptedNative(alsapcm.Endpoint{Card: 5}))
	var asked atomic.Int32
	b, err := playback.New(playback.Config{Resolver: noContent{}, Locator: cardFive{}, DSD: pol,
		Probe: func(ep alsapcm.Endpoint, _ alsapcm.ProbeConfig) (alsapcm.ProbeReport, error) {
			asked.Add(1)
			return acceptedNative(ep), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	before := b.Policy()
	for _, tu := range before.Tuples {
		if tu.Pause != policy.PauseKernel {
			t.Fatalf("the probe's answers give %+v, want the kernel's pause", before.Tuples)
		}
	}
	if b.ProbeDSDPending() {
		t.Fatal("a probe is owed before any write")
	}
	res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1))
	wantChoice(t, res, playback.ChoiceWritten)
	if res.Policy.Generation != before.Generation || !reflect.DeepEqual(res.Policy.Tuples, before.Tuples) {
		t.Errorf("a write of the same choice moved the set: generation %d after %d, tuples %+v", res.Policy.Generation, before.Generation, res.Policy.Tuples)
	}
	if !b.ProbeDSDPending() {
		t.Fatal("a written choice owes no probe")
	}
	// The probe the write owes, asked where the device slot is free.
	if pending := b.ProbeDSD(bg, playback.ProbeConditions{Available: true, Fresh: true}); pending || asked.Load() != 1 {
		t.Fatalf("the probe: still pending %v, asked %d times, want run once", pending, asked.Load())
	}
	waitState(t, store, "the probe's driver_accepted recorded", func(st dsdstate.State) bool {
		e := playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64)
		return e != nil && !e.DriverAcceptedAt.IsZero()
	})
	b.RefreshDSD(true)
	after := b.Policy()
	if after.Generation != before.Generation || !reflect.DeepEqual(after.Tuples, before.Tuples) {
		t.Errorf("the probe's answers moved the set: generation %d after %d", after.Generation, before.Generation)
	}
	if got := after.DSD.Evidence[0]; got != (playback.DSDEvidence{Rung: string(policy.RungDriverAccepted), On: "2026-11-06"}) {
		t.Errorf("DSD64's evidence once the probe answered: %+v, want driver_accepted, dated", got)
	}
	if b.ProbeDSDPending() {
		t.Error("the probe ran and is still owed")
	}
	t.Run("never a driver acceptance the probe did not give", func(t *testing.T) {
		b, _, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1))
		wantChoice(t, res, playback.ChoiceWritten)
		for _, e := range res.Policy.DSD.Evidence {
			if e.Rung == string(policy.RungDriverAccepted) {
				t.Errorf("the answer claims %+v with no probe answered and no open made", res.Policy.DSD.Evidence)
			}
		}
	})
}

// TestTheWritesLineNamesTheKeyAndTheModesAlone (binding note 12): a write
// logs one line, the DAC's key as the wire names it, the mode it leaves and
// the one it asks, and its outcome; no serial, port or product string.
func TestTheWritesLineNamesTheKeyAndTheModesAlone(t *testing.T) {
	b, _, log := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative))
	key := keyOf(t, b)
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 1)), playback.ChoiceWritten)
	lines := log.writes()
	if len(lines) != 1 {
		t.Fatalf("the write logged %+v, want one line", lines)
	}
	a := lines[0].attrs
	if a["dac"] != key || a["from"] != dsdstate.ModeNative || a["to"] != dsdstate.ModeConvert || a["outcome"] != string(playback.ChoiceWritten) {
		t.Errorf("the write's line %+v, want dac %s, from native, to convert, written", a, key)
	}
	all := log.everything()
	for _, secret := range []string{"TESTDAC-0001", "usb2-1", playbacktest.DACProductName} {
		if strings.Contains(all, secret) {
			t.Errorf("the log names %q:\n%s", secret, all)
		}
	}
}

// watchdog bounds a wait for something the test is sure comes.
const watchdog = 30 * time.Second

// bg is the tests' context.
var bg = context.Background()

// waitState waits until store's state satisfies cond, at most the watchdog.
func waitState(t *testing.T, store *playbacktest.MemStore, what string, cond func(dsdstate.State) bool) {
	t.Helper()
	deadline := time.After(watchdog)
	for {
		ch := store.Changed()
		if cond(store.State()) {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("waiting for %s: the state is %+v", what, store.State())
		}
	}
}
