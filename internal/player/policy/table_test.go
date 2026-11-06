package policy

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// designRow is one row of section 3.2's table: its choice, its native and
// DoP cells, and its otherwise cell.
type designRow struct {
	choice, native, dop, otherwise string
}

// designTable is section 3.2's table as the design file holds it, the rows
// under its header.
func designTable(t *testing.T) []designRow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "ops", "plan-b728-dsd-capability-design.md"))
	if err != nil {
		t.Fatal(err)
	}
	const header = "| Choice | Plays native at r when | Plays DoP at r when | Otherwise, at r |"
	var rows []designRow
	in := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == header:
			if in || rows != nil {
				t.Fatal("the design holds section 3.2's table twice")
			}
			in = true
		case in && strings.HasPrefix(line, "|---"):
		case in && strings.HasPrefix(line, "| "):
			cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ")
			if len(cells) != 4 {
				t.Fatalf("a row of section 3.2's table has %d cells: %q", len(cells), line)
			}
			rows = append(rows, designRow{cells[0], cells[1], cells[2], cells[3]})
		case in:
			in = false
		}
	}
	if len(rows) == 0 {
		t.Fatal("the design holds no section 3.2 table under its header")
	}
	return rows
}

// testRate is the rate the table is run at: DSD128, with DSD64 below it for
// a cap and DSD256 above it for a profile's claims.
const testRate = dsd128

// condition is one clause of a cell: how it is made to fail, each way with
// the refusal word an explicit choice gives for it.
type condition struct {
	fail []failure
}

type failure struct {
	how    string
	reason engine.Reason
	do     func(*rig)
}

// conditions are the clauses section 3.2's cells hold, by their text, for
// the transport of the cell they are in. A clause this test does not know is
// a design change it must learn.
func conditions(transport string) map[string]condition {
	notOffered := ReasonNativeNotOffered
	if transport == dsdstate.TransportDoP {
		notOffered = ReasonDoPNotOffered
	}
	return map[string]condition{
		"native offered": {fail: []failure{
			{"no altset lists it", notOffered, func(r *rig) { r.withoutNative() }},
			{"the probe refused it", notOffered, func(r *rig) { r.answer(format.DSDU32BE, testRate/32, alsapcm.ProbeRefused, 0) }},
		}},
		"carrier offered": {fail: []failure{
			{"no altset lists it", notOffered, func(r *rig) { r.withoutCarrier() }},
			{"the probe refused it", notOffered, func(r *rig) { r.answer(format.S32LE, testRate/16, alsapcm.ProbeRefused, 0) }},
			{"its refined bits are 16", notOffered, func(r *rig) { r.answer(format.S32LE, testRate/16, alsapcm.ProbeAccepted, 16) }},
		}},
		"gain at unity": {fail: []failure{
			{"a level control reads off 0 dB", ReasonGainNotUnity, func(r *rig) {
				r.mixer = withVolumeUnit(r.mixer, volume("PCM Playback Volume", 200), switchControl("PCM Playback Switch"))
			}},
			{"a level control cannot be read", ReasonGainUnverifiable, func(r *rig) {
				r.mixer.Controls.Controls = append(r.mixer.Controls.Controls, unreadable("Master Playback Volume", 0))
			}},
		}},
		"acknowledged at r": {fail: []failure{
			{"no acknowledgement", ReasonFirstUsePending, func(r *rig) { r.unacknowledge(transport) }},
			{"a migrated one of DSD64 alone", ReasonFirstUsePending, func(r *rig) { r.acknowledge(transport, dsd64) }},
			{"one under another firmware", ReasonFirstUsePending, func(r *rig) {
				for i := range r.firstUse {
					if r.firstUse[i].Transport == transport {
						r.firstUse[i].BCDDevice = "0200"
					}
				}
			}},
		}},
		"no rejection": {fail: []failure{
			{"the latest answer is noise", ReasonListenerRejected, func(r *rig) { r.hear(transport, testRate, dsdstate.AnswerNoise) }},
			{"the latest answer is nothing", ReasonListenerRejected, func(r *rig) { r.hear(transport, testRate, dsdstate.AnswerNothing) }},
		}},
		"r ≤ `maxBitRate`": {fail: []failure{
			{"the cap is DSD64", ReasonAboveChoice, func(r *rig) { r.max = dsd64 }},
		}},
		"the latest listener answer for native at r is `clean`": {fail: []failure{
			{"no answer", "", func(r *rig) { r.forget(dsdstate.TransportNative, testRate) }},
			{"the latest answer is noise", "", func(r *rig) { r.hear(dsdstate.TransportNative, testRate, dsdstate.AnswerNoise) }},
		}},
		"the latest listener answer for DoP at r is `clean`": {fail: []failure{
			{"no answer", "", func(r *rig) { r.forget(dsdstate.TransportDoP, testRate) }},
			{"the latest answer is nothing", "", func(r *rig) { r.hear(dsdstate.TransportDoP, testRate, dsdstate.AnswerNothing) }},
		}},
		"DoP confirmed": {fail: []failure{
			{"no confirmation", "", func(r *rig) { r.confirmed = nil }},
			{"one under another firmware", "", func(r *rig) { r.confirmed.BCDDevice = "0200" }},
		}},
		heardOrConfirmedClause: {fail: []failure{
			{"neither", "", func(r *rig) { r.profileDoP = &dacprofile.DoP{StatedMaxBitRate: dsd256}; r.confirmed = nil }},
		}},
		"the profile heard or stated DoP at r, or DoP confirmed": {fail: []failure{
			{"none of them", ReasonConfirmationRequired, func(r *rig) { r.profileDoP = nil; r.confirmed = nil }},
			{"a claim only below the rate", ReasonConfirmationRequired, func(r *rig) {
				r.profileDoP = &dacprofile.DoP{HeardMaxBitRate: dsd64, StatedMaxBitRate: dsd64}
				r.confirmed = nil
			}},
		}},
	}
}

// heardOrConfirmedClause is the auto row's claim of DoP on a known DAC, and
// heardOrConfirmed the ways it holds, each tried alone.
const heardOrConfirmedClause = "the profile heard DoP at r, or the user confirmed DoP"

var heardOrConfirmed = []failure{
	{"the profile heard DoP at r alone", "", func(r *rig) { r.confirmed = nil }},
	{"the user confirmed DoP alone", "", func(r *rig) { r.profileDoP = &dacprofile.DoP{StatedMaxBitRate: dsd256} }},
}

// unacknowledge drops transport's acknowledgement.
func (r *rig) unacknowledge(transport string) {
	r.firstUse = slices.DeleteFunc(r.firstUse, func(f dsdstate.FirstUse) bool { return f.Transport == transport })
}

var (
	reWords    = regexp.MustCompile("`([a-z_]+)`")
	reRefused  = regexp.MustCompile("^refused: (`[a-z_]+`(?:, `[a-z_]+`)*)$")
	reConvertN = regexp.MustCompile("^converted \\(refused as `([a-z_]+)` if the DAC takes no target\\)$")
)

// TestTheEffectivePolicyTable runs every row of section 3.2's table, as the
// design file holds it, through Effective: for each cell, every clause in it
// holding plays its transport, and each clause failing in each way the test
// knows does not, but gives the row's otherwise cell, a refusal with the
// word the clause names; a cell of never plays its transport whatever holds;
// and an otherwise cell's refusal words are exactly the words its clauses
// give. A clause, a choice or an otherwise cell the test does not know fails
// it, so the table cannot change under the code unseen, and a row added or
// dropped is a choice missing.
func TestTheEffectivePolicyTable(t *testing.T) {
	rows := designTable(t)
	choices := map[string]struct {
		mode  string
		known bool
	}{
		"`auto`, known DAC":   {dsdstate.ModeAuto, true},
		"`auto`, unknown DAC": {dsdstate.ModeAuto, false},
		"`native`":            {dsdstate.ModeNative, true},
		"`dop`":               {dsdstate.ModeDoP, true},
		"`convert`":           {dsdstate.ModeConvert, true},
		"`off`":               {dsdstate.ModeOff, true},
	}
	seen := map[string]bool{}
	for _, row := range rows {
		c, ok := choices[row.choice]
		if !ok {
			t.Fatalf("section 3.2's table has a choice this test does not know: %q", row.choice)
		}
		if seen[row.choice] {
			t.Fatalf("section 3.2's table holds %q twice", row.choice)
		}
		seen[row.choice] = true
		t.Run(strings.ReplaceAll(row.choice, "`", ""), func(t *testing.T) {
			// base is every positive input there is, under the row's choice,
			// on a known or an unknown DAC.
			base := func() *rig {
				r := newRig(t).everything(c.mode)
				r.max = dsd256
				if !c.known {
					r.known, r.profileNative, r.profileDoP = false, nil, nil
				}
				return r
			}
			otherwise := row.otherwise
			var words []engine.Reason
			wantWords := map[engine.Reason]bool{}
			switch {
			case otherwise == "converted":
			case reRefused.MatchString(otherwise):
				for _, m := range reWords.FindAllStringSubmatch(otherwise, -1) {
					wantWords[engine.Reason(m[1])] = true
				}
			case reConvertN.MatchString(otherwise):
			default:
				t.Fatalf("an otherwise cell this test does not know: %q", otherwise)
			}
			for _, cell := range []struct {
				transport, text string
				outcome         Outcome
			}{{dsdstate.TransportNative, row.native, OutcomeNative}, {dsdstate.TransportDoP, row.dop, OutcomeDoP}} {
				// out takes the other transport out, so the cell's alone decides.
				out := func(r *rig) {
					if cell.transport == dsdstate.TransportNative {
						r.withoutCarrier()
					} else {
						r.withoutNative()
					}
				}
				if cell.text == "never" {
					r := base()
					out(r)
					if p := r.plan(testRate); p.Outcome == cell.outcome {
						t.Errorf("%s is never played under %s, and plays", cell.transport, row.choice)
					}
					continue
				}
				known := conditions(cell.transport)
				clauses := strings.Split(cell.text, "; ")
				for _, clause := range clauses {
					if _, ok := known[clause]; !ok {
						t.Fatalf("a clause of the %s cell this test does not know: %q", cell.transport, clause)
					}
				}
				r := base()
				out(r)
				wantOutcome(t, cell.transport+" with every clause holding", r.plan(testRate), cell.outcome)
				if slices.Contains(clauses, heardOrConfirmedClause) {
					for _, w := range heardOrConfirmed {
						r := base()
						out(r)
						w.do(r)
						wantOutcome(t, cell.transport+" with "+w.how, r.plan(testRate), cell.outcome)
					}
				}
				for _, clause := range clauses {
					for _, f := range known[clause].fail {
						r := base()
						out(r)
						f.do(r)
						p := r.plan(testRate)
						what := cell.transport + ", " + clause + " failing: " + f.how
						switch {
						case p.Outcome == cell.outcome:
							t.Errorf("%s: plays %s", what, p.Outcome)
						case otherwise == "converted", reConvertN.MatchString(otherwise):
							wantOutcome(t, what, p, OutcomeConverted)
						default:
							wantOutcome(t, what, p, OutcomeRefused, f.reason)
							words = append(words, p.Reason)
						}
					}
				}
			}
			switch {
			case reRefused.MatchString(otherwise):
				r := base().withoutNative().withoutCarrier()
				p := r.plan(testRate)
				if p.Outcome != OutcomeRefused || !wantWords[p.Reason] {
					t.Errorf("with neither transport offered: %s %s, want a refusal of %q", p.Outcome, p.Reason, otherwise)
				}
				words = append(words, p.Reason)
				for w := range wantWords {
					if !slices.Contains(words, w) {
						t.Errorf("%q is in the otherwise cell, and no clause failing gives it", w)
					}
				}
				for _, w := range words {
					if !wantWords[w] {
						t.Errorf("a clause failing gives %q, which the otherwise cell does not list", w)
					}
				}
			default:
				r := base().withoutNative().withoutCarrier()
				wantOutcome(t, "with neither transport offered", r.plan(testRate), OutcomeConverted)
				if m := reConvertN.FindStringSubmatch(otherwise); m != nil {
					r := base().withoutNative().withoutCarrier()
					r.altsets = nil
					wantOutcome(t, "with no conversion target", r.plan(testRate), OutcomeRefused, engine.Reason(m[1]))
				}
			}
		})
	}
	for choice := range choices {
		if !seen[choice] {
			t.Errorf("section 3.2's table no longer holds %q", choice)
		}
	}
}
