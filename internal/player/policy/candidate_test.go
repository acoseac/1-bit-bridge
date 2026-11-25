package policy

import (
	"fmt"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The candidate a native_check_required refusal carries (B728 B12 PR 3,
// round 3): what an explicit native plays at the rate once a listener's clean
// answer vouches for the DAC there, which the DSD check alone may play, so the
// answer can be earned on a generic board's IDs. Nothing else carries one.

// boardList is a DAC list that names no row and holds the test DAC's IDs as a
// shared reference ID: the test DAC is unknown, on a generic board's IDs.
func boardList() *dacprofile.Database {
	return &dacprofile.Database{SchemaVersion: dacprofile.SchemaVersion,
		SharedReferenceIDs: []dacprofile.SharedID{{VendorID: "1234", ProductID: "5678", Why: "the tests' board"}}}
}

// onBoard is r on a generic board's IDs, its choice an explicit native.
func onBoard(r *rig) *rig {
	r.known, r.catalog, r.mode = false, boardList(), dsdstate.ModeNative
	return r
}

// frameBits is the bits of a channel's frame in each native format the sink
// opens, from the formats' definitions and not from the format package: the
// sink's frame rate is the one-bit rate over it.
var frameBits = map[format.ALSAFormat]int{format.DSDU32BE: 32, format.DSDU32LE: 32, format.DSDU16LE: 16}

// TestTheCandidateIsThePlanACleanAnswerGives: on a generic board's IDs under
// an explicit native, each rate's native_check_required refusal carries as
// its candidate the very plan the rate has once a listener's latest answer
// there is clean: the same by Plan.Same and field by field, transport, format,
// rate, pause, tail and prefill, so the sink the check opens takes the format
// and the frame rate (the one-bit rate over the format's frame) the answer's
// playback opens. Each way the inputs choose the tuple: the kernel's pause
// the probe found, none probed, DSD_U32_LE where the probe refused
// DSD_U32_BE, and DSD_U16_LE.
func TestTheCandidateIsThePlanACleanAnswerGives(t *testing.T) {
	both := nativeAltset()
	both.Formats = []string{"SPECIAL", "DSD_U32_BE", "DSD_U32_LE"}
	u16 := nativeAltset()
	u16.Formats, u16.Rates = []string{"SPECIAL", "DSD_U16_LE"}, []int{176400, 352800, 705600}
	for _, c := range []struct {
		name  string
		edit  func(r *rig)
		f     format.ALSAFormat
		pause string
	}{
		{"DSD_U32_BE with the kernel's pause", nil, format.DSDU32BE, PauseKernel},
		{"DSD_U32_BE not probed", func(r *rig) { r.probe = map[alsapcm.ProbeCandidate]alsapcm.ProbeResult{} }, format.DSDU32BE, PauseSilence},
		{"DSD_U32_LE where the probe refused DSD_U32_BE", func(r *rig) {
			r.altsets = []alsa.Altset{s32Altset(), s24Altset(), both}
			for _, rate := range dsdRates {
				r.answer(format.DSDU32BE, rate/32, alsapcm.ProbeRefused, 0)
			}
		}, format.DSDU32LE, PauseSilence},
		{"DSD_U16_LE", func(r *rig) { r.altsets = []alsa.Altset{s32Altset(), s24Altset(), u16} }, format.DSDU16LE, PauseSilence},
	} {
		for _, rate := range dsdRates {
			t.Run(fmt.Sprintf("%s at %d", c.name, rate), func(t *testing.T) {
				r := onBoard(newRig(t))
				if c.edit != nil {
					c.edit(r)
				}
				refused := r.plan(rate)
				wantOutcome(t, "native on a generic board's IDs", refused, OutcomeRefused, ReasonNativeCheckRequired)
				if refused.Candidate == nil {
					t.Fatal("the refusal carries no candidate")
				}
				cand := *refused.Candidate
				r.hear(dsdstate.TransportNative, rate, dsdstate.AnswerClean)
				heard := r.plan(rate)
				wantOutcome(t, "native heard clean at the rate", heard, OutcomeNative)
				if !cand.Same(heard) {
					t.Errorf("the candidate %+v is not the plan the clean answer gives, %+v", cand, heard)
				}
				ct, ht := cand.Tuple, heard.Tuple
				for _, f := range []struct {
					field     string
					got, want any
				}{
					{"outcome", cand.Outcome, heard.Outcome}, {"rate", cand.BitRate, heard.BitRate},
					{"transport", ct.Transport, ht.Transport}, {"format", ct.Format, ht.Format}, {"tuple's rate", ct.BitRate, ht.BitRate},
					{"pause", ct.Pause, ht.Pause}, {"tail", ct.Tail, ht.Tail}, {"prefill", ct.Prefill, ht.Prefill},
				} {
					if f.got != f.want {
						t.Errorf("the candidate's %s is %v; the clean answer's plan's %v", f.field, f.got, f.want)
					}
				}
				if ct.Transport != dsdstate.TransportNative || ct.Format != c.f || ct.BitRate != rate || ct.Pause != c.pause || ct.Tail != DefaultTail ||
					ct.Prefill != dacprofile.DefaultDSDPrefillMs*time.Millisecond {
					t.Errorf("the candidate's tuple is %+v; want native in %s at %d, pause %s, the default tail and prefill", ct, c.f, rate, c.pause)
				}
				if bits := frameBits[ct.Format]; bits == 0 || ct.BitRate/(8*format.DSDWordBytes(ct.Format)) != rate/bits || ht.BitRate/(8*format.DSDWordBytes(ht.Format)) != rate/bits {
					t.Errorf("the sinks' frame rates: the check's %s at %d and the answer's %s at %d; want both %d frames a second", ct.Format, ct.BitRate, ht.Format, ht.BitRate, rate/max(bits, 1))
				}
				if heard.Candidate != nil {
					t.Errorf("the plan a clean answer gives carries a candidate: %+v", heard.Candidate)
				}
			})
		}
	}
}

// TestOnlyANativeCheckRequiredRefusalCarriesACandidate: a candidate is the
// check's alone, where native waits for a listener's answer and an explicit
// native would play once it is clean: the refusal of an explicit native on a
// generic board's IDs at a rate under its cap. Above the cap the refusal is
// the same word with none (the bridge's blind spot is asked before the user's
// cap), and no other outcome or refusal carries one: native a listener
// rejected or the DAC does not offer, native heard clean, on a DAC on no
// generic board's IDs or known to the list, auto on the board (which
// converts), dop, convert, off, and a state that does not read. A set held
// to what two states play alike (Intersect) keeps a candidate only where the
// landed state's is the same plan, and a session's reconciliation never
// keeps an occurrence by one.
func TestOnlyANativeCheckRequiredRefusalCarriesACandidate(t *testing.T) {
	for _, c := range []struct {
		name    string
		edit    func(r *rig)
		reason  map[int]any // the refusal at each rate, nil where it plays
		carries map[int]bool
	}{
		{"native on the board", nil, refusedAt(ReasonNativeCheckRequired, dsd64, dsd128, dsd256), carriesAt(dsd64, dsd128, dsd256)},
		{"native on the board, capped at DSD64", func(r *rig) { r.max = dsd64 }, refusedAt(ReasonNativeCheckRequired, dsd64, dsd128, dsd256), carriesAt(dsd64)},
		{"native on the board, capped at DSD128", func(r *rig) { r.max = dsd128 }, refusedAt(ReasonNativeCheckRequired, dsd64, dsd128, dsd256), carriesAt(dsd64, dsd128)},
		{"native on the board, rejected at DSD64", func(r *rig) { r.hear(dsdstate.TransportNative, dsd64, dsdstate.AnswerNoise) },
			merge(refusedAt(ReasonListenerRejected, dsd64), refusedAt(ReasonNativeCheckRequired, dsd128, dsd256)), carriesAt(dsd128, dsd256)},
		{"native on the board, offered nowhere", func(r *rig) { r.withoutNative() }, refusedAt(ReasonNativeNotOffered, dsd64, dsd128, dsd256), nil},
		{"native on the board, heard clean at DSD128", func(r *rig) { r.hear(dsdstate.TransportNative, dsd128, dsdstate.AnswerClean) },
			refusedAt(ReasonNativeCheckRequired, dsd64, dsd256), carriesAt(dsd64, dsd256)},
		{"native on no board's IDs", func(r *rig) { r.catalog = &dacprofile.Database{SchemaVersion: dacprofile.SchemaVersion} }, nil, nil},
		{"native on a known DAC", func(r *rig) { r.known = true }, nil, nil},
		{"auto on the board", func(r *rig) { r.mode = dsdstate.ModeAuto }, nil, nil},
		{"dop on the board", func(r *rig) { r.mode = dsdstate.ModeDoP }, refusedAt(ReasonConfirmationRequired, dsd64, dsd128, dsd256), nil},
		{"convert on the board", func(r *rig) { r.mode = dsdstate.ModeConvert }, nil, nil},
		{"off on the board", func(r *rig) { r.mode = dsdstate.ModeOff }, refusedAt(ReasonOffByChoice, dsd64, dsd128, dsd256), nil},
		{"a state that does not read", func(r *rig) { r.unreadable = true }, refusedAt(ReasonStateUnreadable, dsd64, dsd128, dsd256), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := onBoard(newRig(t))
			if c.edit != nil {
				c.edit(r)
			}
			s := Effective(r.inputs())
			for _, rate := range dsdRates {
				p := s.At(rate).Plan
				if want, ok := c.reason[rate]; ok {
					wantOutcome(t, fmt.Sprintf("at %d", rate), p, OutcomeRefused, want)
				} else if p.Outcome == OutcomeRefused {
					t.Errorf("at %d: refused %s; want it played", rate, p.Reason)
				}
				if got := p.Candidate != nil; got != c.carries[rate] {
					t.Errorf("at %d: a candidate %v (%s %s); want %v", rate, got, p.Outcome, p.Reason, c.carries[rate])
				}
				if p.Candidate != nil && (p.Candidate.Outcome != OutcomeNative || p.Candidate.Candidate != nil) {
					t.Errorf("at %d: the candidate %+v is not a native plan of its own", rate, p.Candidate)
				}
			}
		})
	}
	t.Run("a set held to what two states play alike", func(t *testing.T) {
		cur := Effective(onBoard(newRig(t)).inputs())
		for _, c := range []struct {
			name    string
			edit    func(r *rig)
			carries map[int]bool
		}{
			{"the landed state's auto", func(r *rig) { r.mode = dsdstate.ModeAuto }, nil},
			{"the landed state's native capped at DSD64", func(r *rig) { r.max = dsd64 }, carriesAt(dsd64)},
			{"the landed state's native as it was", nil, carriesAt(dsd64, dsd128, dsd256)},
		} {
			l := onBoard(newRig(t))
			if c.edit != nil {
				c.edit(l)
			}
			s := Intersect(cur, Effective(l.inputs()))
			for _, rate := range dsdRates {
				p := s.At(rate).Plan
				wantOutcome(t, fmt.Sprintf("%s at %d", c.name, rate), p, OutcomeRefused, ReasonNativeCheckRequired)
				if got := p.Candidate != nil; got != c.carries[rate] {
					t.Errorf("%s at %d: a candidate %v; want %v", c.name, rate, got, c.carries[rate])
				}
			}
		}
	})
	t.Run("a session's reconciliation", func(t *testing.T) {
		s := Effective(onBoard(newRig(t)).inputs())
		cand := s.At(dsd64).Plan.Candidate
		if cand == nil {
			t.Fatal("the board's native at DSD64 carries no candidate")
		}
		id := s.Identity()
		for _, o := range []Occurrence{{Plan: *cand}, {Plan: *cand, Opened: &id, Submitted: 1, Written: 1, Played: 1, Audible: true}} {
			if v := Reconcile(s, o); v.Action == Keep {
				t.Errorf("an occurrence of the candidate's plan %+v is kept: %+v", o, v)
			}
		}
	})
}

// refusedAt is a refusal with reason at each of rates.
func refusedAt(reason any, rates ...int) map[int]any {
	m := map[int]any{}
	for _, r := range rates {
		m[r] = reason
	}
	return m
}

// carriesAt is a candidate at each of rates.
func carriesAt(rates ...int) map[int]bool {
	m := map[int]bool{}
	for _, r := range rates {
		m[r] = true
	}
	return m
}

func merge(a, b map[int]any) map[int]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}
