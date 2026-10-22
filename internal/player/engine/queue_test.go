package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestInsertAfterZeroIsTheHead pins an insert after 0 at the head and one
// after an ID right after it.
func TestInsertAfterZeroIsTheHead(t *testing.T) {
	h := startHarness(t)
	a := h.insert("a")[0]
	b, err := h.eng.InsertAfter(bg, 0, ref("b"), nil)
	h.must("insert at the head", err)
	c, err := h.eng.InsertAfter(bg, b, ref("c"), nil)
	h.must("insert after b", err)
	if got, want := h.snap().IDs(), []uint32{b, c, a}; !slices.Equal(got, want) {
		t.Fatalf("IDs %v, want %v", got, want)
	}
}

// TestInsertAfterAnUnknownIDIsRefused pins the typed refusal, with the
// queue and its token as they were.
func TestInsertAfterAnUnknownIDIsRefused(t *testing.T) {
	h := startHarness(t)
	h.insert("a", "b")
	token, before := h.snap().IDArray()
	_, err := h.eng.InsertAfter(bg, 77, ref("x"), nil)
	h.refused("insert after an unknown ID", err, ReasonIDNotFound)
	if tok, after := h.snap().IDArray(); tok != token || !slices.Equal(after, before) {
		t.Fatalf("after the refusal: token %d IDs %v, want %d %v", tok, after, token, before)
	}
}

// TestTheQueueIsFullByCountAndByMetadata pins MaxEntries, the total
// metadata bound and one entry's: each refused, typed, with nothing
// changed.
func TestTheQueueIsFullByCountAndByMetadata(t *testing.T) {
	h := startHarness(t)
	last := uint32(0)
	for i := 0; i < MaxEntries; i++ {
		id, err := h.eng.InsertAfter(bg, last, ref("t"), nil)
		h.must("insert", err)
		last = id
	}
	token := h.snap().Token()
	_, err := h.eng.InsertAfter(bg, last, ref("t"), nil)
	h.refused("an insert past MaxEntries", err, ReasonFull)
	if s := h.snap(); s.Len() != MaxEntries || s.Token() != token {
		t.Fatalf("after the refusal: %d entries, token %d; want %d, %d", s.Len(), s.Token(), MaxEntries, token)
	}

	h.must("delete all", h.eng.DeleteAll(bg))
	big := bytes.Repeat([]byte{'m'}, MaxMetadataBytes)
	last = 0
	for i := 0; i < MaxTotalMetadataBytes/MaxMetadataBytes; i++ {
		id, err := h.eng.InsertAfter(bg, last, ref("t"), big)
		h.must("insert", err)
		last = id
	}
	_, err = h.eng.InsertAfter(bg, last, ref("t"), []byte{1})
	h.refused("an insert past MaxTotalMetadataBytes", err, ReasonFull)
	if _, err := h.eng.InsertAfter(bg, last, ref("t"), nil); err != nil {
		t.Fatalf("an insert with no metadata at the metadata bound: %v", err)
	}
	_, err = h.eng.InsertAfter(bg, 0, ref("t"), make([]byte, MaxMetadataBytes+1))
	h.refused("one entry's metadata past MaxMetadataBytes", err, ReasonMetadataTooLarge)
}

// TestTheFirstInsertIntoAnEmptyQueueSelectsIt pins the selection the first
// insert makes, and only into an empty queue.
func TestTheFirstInsertIntoAnEmptyQueueSelectsIt(t *testing.T) {
	h := startHarness(t)
	if sel := h.snap().Selected(); sel != 0 {
		t.Fatalf("an empty queue selects %d, want 0", sel)
	}
	ids := h.insert("a", "b")
	if sel := h.snap().Selected(); sel != ids[0] {
		t.Fatalf("selected %d, want the first inserted, %d", sel, ids[0])
	}
	h.must("delete all", h.eng.DeleteAll(bg))
	x, err := h.eng.InsertAfter(bg, 0, ref("x"), nil)
	h.must("insert", err)
	if _, err := h.eng.InsertAfter(bg, 0, ref("y"), nil); err != nil {
		t.Fatal(err)
	}
	if sel := h.snap().Selected(); sel != x {
		t.Fatalf("selected %d, want %d, the first inserted into the empty queue", sel, x)
	}
}

// TestDuplicateReferencesAreDistinctEntries pins one reference inserted
// three times as three entries with three IDs.
func TestDuplicateReferencesAreDistinctEntries(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "a", "a")
	if ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Fatalf("IDs %v are not distinct", ids)
	}
	es, err := h.eng.ReadList(ids)
	h.must("read list", err)
	if len(es) != 3 {
		t.Fatalf("read %d entries, want 3", len(es))
	}
	for _, e := range es {
		if e.Ref.String() != "a" {
			t.Fatalf("entry %d's reference is %q", e.ID, e.Ref)
		}
	}
}

// TestIDsAreNeverZeroAndNeverReused pins IDs across deletes and restarts.
func TestIDsAreNeverZeroAndNeverReused(t *testing.T) {
	h := startHarness(t)
	seen := map[uint32]bool{}
	for run := 0; run < 3; run++ {
		for i := 0; i < 20; i++ {
			id := h.insert("t")[0]
			if id == 0 || seen[id] {
				t.Fatalf("run %d: ID %d handed out again (or 0)", run, id)
			}
			seen[id] = true
			if i%2 == 0 {
				h.must("delete", h.eng.Delete(bg, id))
			}
		}
		h.restart()
	}
}

// TestIDsAndTokensSurviveRepeatedCrashes pins a crash with unflushed
// changes, four times over: no ID is handed out twice, and every token of a
// run is above every token an earlier run published, so none a controller
// saw before a crash can name another array after it.
func TestIDsAndTokensSurviveRepeatedCrashes(t *testing.T) {
	h := startHarness(t)
	seen := map[uint32]bool{}
	var highest uint32
	for crash := 0; crash < 4; crash++ {
		first := h.snap().Token()
		if first <= highest {
			t.Fatalf("crash %d: the run starts at token %d, not above %d", crash, first, highest)
		}
		for i := 0; i < 10; i++ {
			id := h.insert("t")[0]
			if id == 0 || seen[id] {
				t.Fatalf("crash %d: ID %d handed out again (or 0)", crash, id)
			}
			seen[id] = true
			if i%3 == 0 {
				h.must("delete", h.eng.Delete(bg, id))
			}
			highest = max(highest, h.snap().Token())
		}
		h.crash()
	}
}

// TestTheReservationIsDurableBeforeAnIDIsHandedOut pins the order: every ID
// and token a command hands out is inside a reservation already written,
// across more than one block.
func TestTheReservationIsDurableBeforeAnIDIsHandedOut(t *testing.T) {
	h := startHarness(t)
	for i := 0; i < idBlock+50; i++ {
		id := h.insert("t")[0]
		ids, tokens := h.files.durable()
		if id > ids {
			t.Fatalf("ID %d handed out with the reservation at %d", id, ids)
		}
		if tok := h.snap().Token(); tok > tokens {
			t.Fatalf("token %d published with the reservation at %d", tok, tokens)
		}
		h.must("delete", h.eng.Delete(bg, id))
		if _, tokens := h.files.durable(); h.snap().Token() > tokens {
			t.Fatalf("token %d published with the reservation at %d", h.snap().Token(), tokens)
		}
	}
}

// TestACommandWaitsForAReservationThatHasNotLanded pins the other half: when
// a block runs out before the next one's write has landed, the command that
// needs it waits for the write, and takes nothing past what is durable.
// Whether a command has waited is read without sleeping: the owner notes a
// command it applied in the step that applied it, and admits a second
// command only once that step is over.
func TestACommandWaitsForAReservationThatHasNotLanded(t *testing.T) {
	h := newHarness(t)
	admitted := make(chan op, 8)
	h.tweak = func(c *Config) {
		c.seams.admitted = func(cmd *command) { admitted <- cmd.op }
	}
	h.boot()
	ids, tokens := h.files.durable()
	g := newGate()
	h.files.mu.Lock()
	h.files.holdReserve = g
	h.files.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-g:
		default:
			g.open()
		}
	})
	admission := func(what string) {
		t.Helper()
		select {
		case <-admitted:
		case <-time.After(*watchdog):
			t.Fatalf("waited %v for %s to be admitted", *watchdog, what)
		}
	}
	applied := func(since int, op string) bool {
		h.appliedMu.Lock()
		defer h.appliedMu.Unlock()
		return slices.Contains(h.appliedOps[since:], op)
	}
	var last uint32
	for n := 0; ; n++ {
		if n > 2*idBlock {
			t.Fatal("no block ran out")
		}
		var got uint32
		edit, run := "insert", func() error {
			id, err := h.eng.InsertAfter(bg, 0, ref("t"), nil)
			got = id
			return err
		}
		if h.snap().Len() == MaxEntries-1 {
			edit, run = "delete all", func() error { return h.eng.DeleteAll(bg) }
		}
		mark := h.appliedMark()
		done := async(run)
		admission("the edit")
		h.outGen++
		probe := async(func() error { return h.eng.ReportOutput(bg, OutputReport{Generation: h.outGen, Available: true}) })
		admission("the probe")
		if applied(mark, edit) {
			h.must("edit", h.result("the edit", done))
			last = max(last, got)
			if last > ids || h.snap().Token() > tokens {
				t.Fatalf("an edit took ID %d and token %d with %d and %d durable", last, h.snap().Token(), ids, tokens)
			}
			h.must("probe", h.result("probe", probe))
			continue
		}
		if n < 100 {
			t.Fatalf("edit %d waited with most of the block left", n)
		}
		g.open()
		h.must("the edit that waited", h.result("the edit", done))
		h.must("probe", h.result("probe", probe))
		nowIDs, nowTokens := h.files.durable()
		if got > nowIDs || h.snap().Token() > nowTokens || (nowIDs == ids && nowTokens == tokens) {
			t.Fatalf("the edit that waited took ID %d and token %d with %d and %d durable", got, h.snap().Token(), nowIDs, nowTokens)
		}
		return
	}
}

// TestACancelledInsertKeepsNoneOfTheCallersSlices pins the copy: an insert
// takes its metadata and its conditions as copies before the owner can see
// them, so a caller whose context ended while its insert was parked, and
// who reuses its buffers then, changes nothing the owner later reads. The
// second write is made while the owner runs the insert: under -race, a race
// if the owner read the caller's slice.
func TestACancelledInsertKeepsNoneOfTheCallersSlices(t *testing.T) {
	for _, tc := range []struct {
		name  string
		meta  bool
		conds bool
	}{
		{"metadata", true, false},
		{"conditions", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			h.insert("a")
			g, calls := h.hangReservations()

			meta := []byte("the caller's metadata")
			original := slices.Clone(meta)
			var conds []Cond
			if tc.conds {
				conds = []Cond{IfGeneration(h.snap().Generation())}
			}
			ctx, cancel := context.WithCancel(bg)
			mark := h.appliedMark()
			done := async(func() error {
				_, err := h.eng.InsertAfter(ctx, 0, ref("x"), meta, conds...)
				return err
			})
			h.files.waitFor(t, "the reservation write the insert waits for", func() bool { return h.files.reserveCalls > calls })
			cancel()
			h.refused("the insert whose caller left", h.result("insert", done), ReasonCancelled)
			reuse := func(b byte) {
				if tc.meta {
					for i := range meta {
						meta[i] = b
					}
				}
				if tc.conds {
					conds[0] = IfGeneration(Generation{})
				}
			}
			reuse('X')
			g.open()
			reuse('Y')
			h.waitApplied("insert", mark)
			sn := h.snap()
			if sn.Len() != 2 {
				t.Fatalf("the parked insert did not run at the generation it was sent with: %v", sn.IDs())
			}
			if e, _ := sn.Read(sn.IDs()[0]); !bytes.Equal(e.Meta, original) {
				t.Fatalf("the entry holds %q, the caller's reused buffer, not %q", e.Meta, original)
			}
		})
	}
}

// TestIDExhaustionIsRefused pins the top of uint32: the last IDs are handed
// out, then every insert is refused, typed, with one log line.
func TestIDExhaustionIsRefused(t *testing.T) {
	h := newHarness(t)
	writeReserveFile(t, h, math.MaxUint32-idBlock-2, 1000)
	h.boot()
	a := h.insert("a")[0]
	b := h.insert("b")[0]
	if a != math.MaxUint32-1 || b != math.MaxUint32 {
		t.Fatalf("the last IDs are %d and %d, want %d and %d", a, b, uint32(math.MaxUint32-1), uint32(math.MaxUint32))
	}
	for i := 0; i < 2; i++ {
		_, err := h.eng.InsertAfter(bg, b, ref("c"), nil)
		h.refused("an insert past the last ID", err, ReasonIDsExhausted)
	}
	if n := h.logs.count("IDs exhausted"); n != 1 {
		t.Fatalf("%d log lines say the IDs are exhausted, want 1: %v", n, h.logs.all())
	}
	if ed := h.snap().Edits(); ed.Refused != ReasonIDsExhausted {
		t.Fatalf("Edits %+v, want refused %s", ed, ReasonIDsExhausted)
	}
	if got := h.snap().IDs(); !slices.Equal(got, []uint32{a, b}) {
		t.Fatalf("IDs %v after the refusals", got)
	}
}

// TestTokensAdvanceOnEveryChangeAndAtEveryStart pins the token: one step
// for every change of membership or order, none for anything else, and past
// the last one's whole reservation at a start.
func TestTokensAdvanceOnEveryChangeAndAtEveryStart(t *testing.T) {
	h := startHarness(t)
	tok := h.snap().Token()
	step := func(what string, f func(), advance bool) {
		t.Helper()
		f()
		got := h.snap().Token()
		switch {
		case advance && got != tok+1:
			t.Fatalf("%s: token %d, want %d", what, got, tok+1)
		case !advance && got != tok:
			t.Fatalf("%s: token %d, want it unchanged at %d", what, got, tok)
		}
		tok = got
	}
	var ids []uint32
	step("an insert", func() { ids = h.insert("a") }, true)
	step("another insert", func() { ids = append(ids, h.insert("b")...) }, true)
	step("a delete", func() { h.must("delete", h.eng.Delete(bg, ids[0])) }, true)
	step("repeat on", func() { h.must("repeat", h.eng.SetRepeat(bg, true)) }, false)
	step("a refused insert", func() { _, _ = h.eng.InsertAfter(bg, 99, ref("x"), nil) }, false)
	step("delete all", func() { h.must("delete all", h.eng.DeleteAll(bg)) }, true)
	step("delete all of nothing", func() { h.must("delete all", h.eng.DeleteAll(bg)) }, false)
	_, tokens := h.files.durable()
	h.restart()
	if got := h.snap().Token(); got <= tokens {
		t.Fatalf("after a restart the token is %d, not past the last run's reservation %d", got, tokens)
	}
}

// TestReadListAnswersInTheOrderAskedAndIsBounded pins ReadList.
func TestReadListAnswersInTheOrderAskedAndIsBounded(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b", "c")
	es, err := h.eng.ReadList([]uint32{ids[2], 424242, ids[0]})
	h.must("read list", err)
	if got, want := entriesOf(es), []uint32{ids[2], ids[0]}; !slices.Equal(got, want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	if string(es[0].Meta) != "meta:c" || es[1].Ref.String() != "a" {
		t.Fatalf("entries %+v", es)
	}
	if _, err := h.eng.ReadList(make([]uint32, MaxReadList)); err != nil {
		t.Fatalf("a ReadList of MaxReadList IDs: %v", err)
	}
	_, err = h.eng.ReadList(make([]uint32, MaxReadList+1))
	h.refused("a ReadList past MaxReadList", err, ReasonTooMany)
}

// TestPlayIndexLooksUpTheIndexAsItPlays pins PlayIndex and its typed
// refusal.
func TestPlayIndexLooksUpTheIndexAsItPlays(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b", "c")
	h.available()
	h.must("play index 1", h.eng.PlayIndex(bg, 1))
	if s := h.be.nth(1); s.first.Entry != ids[1] {
		t.Fatalf("PlayIndex(1) started %d, want %d", s.first.Entry, ids[1])
	}
	for _, i := range []int{3, -1} {
		h.refused("PlayIndex out of the queue", h.eng.PlayIndex(bg, i), ReasonIndexNotFound)
	}
	if id, ok := h.snap().IDAt(2); !ok || id != ids[2] {
		t.Fatalf("IDAt(2) %d %v", id, ok)
	}
	if i, ok := h.snap().IndexOf(ids[1]); !ok || i != 1 {
		t.Fatalf("IndexOf %d %v", i, ok)
	}
}

// TestReferencesAreValidated pins NewReference's rule and the insert's.
func TestReferencesAreValidated(t *testing.T) {
	for _, bad := range []string{"", strings.Repeat("r", MaxReferenceBytes+1), "a\x01", "tab\t", "caf\xc3\xa9", "del\x7f"} {
		if _, err := NewReference(bad); ReasonOf(err) != ReasonBadReference {
			t.Errorf("NewReference(%q): %v, want %s", bad, err, ReasonBadReference)
		}
	}
	for _, good := range []string{strings.Repeat("r", MaxReferenceBytes), "/dlna/file/0123abcd", "with space", "~!\"\\"} {
		if _, err := NewReference(good); err != nil {
			t.Errorf("NewReference(%q): %v", good, err)
		}
	}
	h := startHarness(t)
	_, err := h.eng.InsertAfter(bg, 0, Reference{}, nil)
	h.refused("an insert of the zero Reference", err, ReasonBadReference)
	if h.snap().Len() != 0 {
		t.Fatal("the refused insert changed the queue")
	}
}

// TestShuffleIsNotInThisRelease pins shuffle's refusal and that both modes
// persist.
func TestShuffleIsNotInThisRelease(t *testing.T) {
	h := startHarness(t)
	h.refused("shuffle on", h.eng.SetShuffle(bg, true), ReasonNotInRelease)
	h.must("shuffle off", h.eng.SetShuffle(bg, false))
	h.must("repeat on", h.eng.SetRepeat(bg, true))
	h.restart()
	if s := h.snap(); !s.Repeat() || s.Shuffle() {
		t.Fatalf("after a restart repeat %v shuffle %v, want true false", s.Repeat(), s.Shuffle())
	}
}

// writeReserveFile writes a reservation file of the format the engine
// reads, before the engine is built.
func writeReserveFile(t *testing.T, h *harness, ids, tokens uint32) {
	t.Helper()
	data, err := json.Marshal(reserveRecord{Version: 1, IDs: ids, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.reservePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
