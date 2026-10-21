package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// tableState brings an engine with the queue a, b, c to one state with b
// selected. hold, when set, is the gate a held release waits on: the test
// opens it once the command has run.
type tableState struct {
	name  string
	setup func(h *harness, ids []uint32) (hold gate)
}

// tableStates are the columns of the package documentation's table.
var tableStates = []tableState{
	{"stopped", func(h *harness, ids []uint32) gate {
		h.must("play b", h.eng.PlayID(bg, ids[1]))
		h.must("stop", h.eng.Stop(bg))
		return nil
	}},
	{"playing", func(h *harness, ids []uint32) gate {
		h.must("play b", h.eng.PlayID(bg, ids[1]))
		s := h.be.nth(h.be.starts())
		s.audible(s.first.ID)
		h.waitSnap("b audible", func(sn *Snapshot) bool { _, ok := sn.Audible(); return ok })
		return nil
	}},
	{"starting", func(h *harness, ids []uint32) gate {
		h.must("play b", h.eng.PlayID(bg, ids[1]))
		if !h.snap().Starting() {
			h.t.Fatal("a session nothing of which is audible is not starting")
		}
		return nil
	}},
	{"paused", func(h *harness, ids []uint32) gate {
		h.must("play b", h.eng.PlayID(bg, ids[1]))
		s := h.be.nth(h.be.starts())
		s.audible(s.first.ID)
		h.waitSnap("b audible", func(sn *Snapshot) bool { _, ok := sn.Audible(); return ok })
		h.must("pause", h.eng.Pause(bg))
		return nil
	}},
	{"paused with no session", func(h *harness, ids []uint32) gate {
		h.must("play a", h.eng.PlayID(bg, ids[0]))
		h.must("pause", h.eng.Pause(bg))
		h.must("next", h.eng.Next(bg))
		if s := h.snap(); s.State() != Paused || s.Selected() != ids[1] {
			h.t.Fatalf("setup: %s at %d", s.State(), s.Selected())
		}
		return nil
	}},
	{"releasing", func(h *harness, ids []uint32) gate {
		h.must("play b", h.eng.PlayID(bg, ids[1]))
		g := holdReleases(h)
		stop := async(func() error { return h.eng.Stop(bg) })
		h.waitSnap("releasing", func(sn *Snapshot) bool { return sn.State() == Releasing })
		h.be.waitFor("the release", func() bool {
			for _, c := range h.be.calls {
				if c.kind == "release" {
					return true
				}
			}
			return false
		})
		h.t.Cleanup(func() { _ = h.result("the setup's stop", stop) })
		return g
	}},
	{"maintenance", func(h *harness, ids []uint32) gate {
		h.must("play b", h.eng.PlayID(bg, ids[1]))
		h.must("stop", h.eng.Stop(bg))
		if _, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the table"); err != nil {
			h.t.Fatal(err)
		}
		return nil
	}},
}

// tableCell is one command in one state: what it answers, the state and
// selection it leaves once the transport settles (after a held release is
// let go), and the transport calls it makes, prepares aside.
type tableCell struct {
	err   Reason
	state State
	sel   string
	calls []string
}

// tableCommands are the rows.
var tableCommands = []struct {
	name string
	run  func(h *harness, ids []uint32) error
}{
	{"play", func(h *harness, ids []uint32) error { return h.eng.Play(bg) }},
	{"pause", func(h *harness, ids []uint32) error { return h.eng.Pause(bg) }},
	{"stop", func(h *harness, ids []uint32) error { return h.eng.Stop(bg) }},
	{"next", func(h *harness, ids []uint32) error { return h.eng.Next(bg) }},
	{"previous", func(h *harness, ids []uint32) error { return h.eng.Previous(bg) }},
	{"play id c", func(h *harness, ids []uint32) error { return h.eng.PlayID(bg, ids[2]) }},
	{"play index 0", func(h *harness, ids []uint32) error { return h.eng.PlayIndex(bg, 0) }},
}

// tableOps names each row's command as the owner does.
var tableOps = map[string]string{
	"play": "play", "pause": "pause", "stop": "stop", "next": "next", "previous": "previous",
	"play id c": "play id", "play index 0": "play index",
}

// tableCells is the table, by state and then command. Calls name entries
// a, b and c.
var tableCells = map[string]map[string]tableCell{
	"stopped": {
		"play":         {state: Playing, sel: "b", calls: []string{"start b"}},
		"pause":        {state: Stopped, sel: "b"},
		"stop":         {state: Stopped, sel: "b"},
		"next":         {state: Stopped, sel: "c"},
		"previous":     {state: Stopped, sel: "a"},
		"play id c":    {state: Playing, sel: "c", calls: []string{"start c"}},
		"play index 0": {state: Playing, sel: "a", calls: []string{"start a"}},
	},
	"playing": {
		"play":         {state: Playing, sel: "b"},
		"pause":        {state: Paused, sel: "b", calls: []string{"pause"}},
		"stop":         {state: Stopped, sel: "b", calls: []string{"release"}},
		"next":         {state: Playing, sel: "c", calls: []string{"release", "start c"}},
		"previous":     {state: Playing, sel: "a", calls: []string{"release", "start a"}},
		"play id c":    {state: Playing, sel: "c", calls: []string{"release", "start c"}},
		"play index 0": {state: Playing, sel: "a", calls: []string{"release", "start a"}},
	},
	"starting": {
		"play":         {state: Playing, sel: "b"},
		"pause":        {state: Paused, sel: "b", calls: []string{"pause"}},
		"stop":         {state: Stopped, sel: "b", calls: []string{"release"}},
		"next":         {state: Playing, sel: "c", calls: []string{"release", "start c"}},
		"previous":     {state: Playing, sel: "a", calls: []string{"release", "start a"}},
		"play id c":    {state: Playing, sel: "c", calls: []string{"release", "start c"}},
		"play index 0": {state: Playing, sel: "a", calls: []string{"release", "start a"}},
	},
	"paused": {
		"play":         {state: Playing, sel: "b", calls: []string{"resume"}},
		"pause":        {state: Paused, sel: "b"},
		"stop":         {state: Stopped, sel: "b", calls: []string{"release"}},
		"next":         {state: Paused, sel: "c", calls: []string{"release"}},
		"previous":     {state: Paused, sel: "a", calls: []string{"release"}},
		"play id c":    {state: Playing, sel: "c", calls: []string{"release", "start c"}},
		"play index 0": {state: Playing, sel: "a", calls: []string{"release", "start a"}},
	},
	"paused with no session": {
		"play":         {state: Playing, sel: "b", calls: []string{"start b"}},
		"pause":        {state: Paused, sel: "b"},
		"stop":         {state: Stopped, sel: "b"},
		"next":         {state: Paused, sel: "c"},
		"previous":     {state: Paused, sel: "a"},
		"play id c":    {state: Playing, sel: "c", calls: []string{"start c"}},
		"play index 0": {state: Playing, sel: "a", calls: []string{"start a"}},
	},
	"releasing": {
		"play":         {state: Playing, sel: "b", calls: []string{"start b"}},
		"pause":        {state: Stopped, sel: "b"},
		"stop":         {state: Stopped, sel: "b"},
		"next":         {state: Stopped, sel: "c"},
		"previous":     {state: Stopped, sel: "a"},
		"play id c":    {state: Playing, sel: "c", calls: []string{"start c"}},
		"play index 0": {state: Playing, sel: "a", calls: []string{"start a"}},
	},
	"maintenance": {
		"play":         {err: ReasonMaintenance, state: Maintenance, sel: "b"},
		"pause":        {err: ReasonMaintenance, state: Maintenance, sel: "b"},
		"stop":         {state: Maintenance, sel: "b"},
		"next":         {err: ReasonMaintenance, state: Maintenance, sel: "b"},
		"previous":     {err: ReasonMaintenance, state: Maintenance, sel: "b"},
		"play id c":    {err: ReasonMaintenance, state: Maintenance, sel: "b"},
		"play index 0": {err: ReasonMaintenance, state: Maintenance, sel: "b"},
	},
}

// TestTheStateCommandTable pins every cell of the package documentation's
// table: each command in each state, what it answers, the state and
// selection it settles in, and the backend calls it makes.
func TestTheStateCommandTable(t *testing.T) {
	for _, st := range tableStates {
		for _, cmd := range tableCommands {
			t.Run(st.name+"/"+cmd.name, func(t *testing.T) {
				cell, ok := tableCells[st.name][cmd.name]
				if !ok {
					t.Fatalf("the table has no cell for %s in %s", cmd.name, st.name)
				}
				h := startHarness(t)
				ids := h.insert("a", "b", "c")
				name := map[uint32]string{ids[0]: "a", ids[1]: "b", ids[2]: "c"}
				h.available()
				hold := st.setup(h, ids)
				mark, applied := h.be.mark(), h.appliedMark()
				done := async(func() error { return cmd.run(h, ids) })
				if hold != nil {
					h.waitApplied(tableOps[cmd.name], applied)
					hold.open()
				}
				err := h.result(cmd.name, done)
				if got := ReasonOf(err); got != cell.err || (cell.err == "" && err != nil) {
					t.Fatalf("answered %v, want reason %q", err, cell.err)
				}
				s := h.waitSnap(fmt.Sprintf("%s at %s", cell.state, cell.sel), func(sn *Snapshot) bool {
					return sn.State() == cell.state && name[sn.Selected()] == cell.sel
				})
				if s.State() == Playing && !strings.Contains(strings.Join(cell.calls, " "), "start") && st.name == "playing" {
					if _, ok := s.Audible(); !ok {
						t.Fatal("a command that did nothing lost the audible occurrence")
					}
				}
				var calls []string
				for _, c := range h.be.callsSince(mark) {
					if strings.HasPrefix(c, "prepare") || c == "discard" {
						continue
					}
					for id, n := range name {
						c = strings.Replace(c, fmt.Sprintf("start %d", id), "start "+n, 1)
					}
					calls = append(calls, c)
				}
				if !slices.Equal(calls, cell.calls) && !(len(calls) == 0 && len(cell.calls) == 0) {
					t.Fatalf("calls %v, want %v", calls, cell.calls)
				}
			})
		}
	}
}

// TestTheTableEdges pins the cells at the queue's ends and the refusals
// that hold in every state: Next past the end, Previous at the head, Play
// with nothing selected, an empty queue, and the output.
func TestTheTableEdges(t *testing.T) {
	t.Run("next past the end while stopped stays on the last", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "b")
		h.available()
		h.must("play b", h.eng.PlayID(bg, ids[1]))
		h.must("stop", h.eng.Stop(bg))
		h.must("next", h.eng.Next(bg))
		if s := h.snap(); s.State() != Stopped || s.Selected() != ids[1] {
			t.Fatalf("%s at %d", s.State(), s.Selected())
		}
	})
	t.Run("next past the end while playing stops on the last", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(1, "a", "b")
		h.must("next", h.eng.Next(bg))
		s := h.waitSnap("stopped", func(s *Snapshot) bool { return s.State() == Stopped })
		if s.Selected() != ids[1] || s.LastStop().Reason != ReasonEndOfQueue {
			t.Fatalf("selected %d, last stop %+v", s.Selected(), s.LastStop())
		}
	})
	t.Run("next past the end while paused stops on the last", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(1, "a", "b")
		h.must("pause", h.eng.Pause(bg))
		h.must("next", h.eng.Next(bg))
		s := h.waitSnap("stopped", func(s *Snapshot) bool { return s.State() == Stopped })
		if s.Selected() != ids[1] {
			t.Fatalf("selected %d", s.Selected())
		}
	})
	t.Run("next past the end with repeat on wraps", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(1, "a", "b")
		h.must("repeat", h.eng.SetRepeat(bg, true))
		h.must("next", h.eng.Next(bg))
		if s := h.snap(); s.State() != Playing || s.Selected() != ids[0] || h.be.nth(2).first.Entry != ids[0] {
			t.Fatalf("%s at %d", s.State(), s.Selected())
		}
	})
	t.Run("previous at the head restarts the selection", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(0, "a", "b")
		h.must("previous", h.eng.Previous(bg))
		if s := h.be.nth(2); s.first.Entry != ids[0] {
			t.Fatalf("previous at the head started %d, want %d", s.first.Entry, ids[0])
		}
	})
	t.Run("play with nothing selected plays the first", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "b", "c")
		h.available()
		h.must("play c", h.eng.PlayID(bg, ids[2]))
		h.must("stop", h.eng.Stop(bg))
		h.must("delete c", h.eng.Delete(bg, ids[2]))
		if sel := h.snap().Selected(); sel != 0 {
			t.Fatalf("deleting the last selected entry while stopped selects %d, want 0", sel)
		}
		h.must("play", h.eng.Play(bg))
		if s := h.be.nth(2); s.first.Entry != ids[0] {
			t.Fatalf("play with nothing selected started %d, want the first, %d", s.first.Entry, ids[0])
		}
	})
	t.Run("an empty queue refuses play", func(t *testing.T) {
		h := startHarness(t)
		h.available()
		h.refused("play of an empty queue", h.eng.Play(bg), ReasonEmptyQueue)
		h.must("next of an empty queue", h.eng.Next(bg))
		if h.be.starts() != 0 {
			t.Fatal("an empty queue started a session")
		}
	})
	t.Run("an unknown output refuses what would open the device", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "b")
		h.refused("play", h.eng.Play(bg), ReasonOutputUnknown)
		h.refused("play id", h.eng.PlayID(bg, ids[1]), ReasonOutputUnknown)
		h.refused("play index", h.eng.PlayIndex(bg, 1), ReasonOutputUnknown)
		h.must("next only moves the selection", h.eng.Next(bg))
		if s := h.snap(); s.Selected() != ids[1] || h.be.starts() != 0 {
			t.Fatalf("selected %d, %d sessions", s.Selected(), h.be.starts())
		}
		h.report(OutputReport{Available: false, Reason: "device_missing"})
		h.refused("play", h.eng.Play(bg), ReasonOutputUnavailable)
		h.refused("play id of an unknown ID", h.eng.PlayID(bg, 99), ReasonIDNotFound)
	})
}
