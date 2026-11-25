package alsapcm_test

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// A generic board's native check (B728 B12 PR 3, round 3), through the
// engine's Run, the backend and the real native sink over the fake kernel:
// on a DAC no row knows whose USB IDs are a generic board's, an explicit
// native refuses each rate native_check_required until a listener's clean
// answer there, and the refusal carries the plan that answer gives
// (policy.Plan.Candidate). The check of native at the rate plays that plan,
// so the answer can be earned, and nothing else; no session plays it. Each
// listener's answer here is simulated: written to the state through its
// store, as B12 PR 4's route will write it.

// onBoard puts the test DAC's IDs on the DAC list's shared reference IDs and
// names no row: an unknown DAC whose native offer is the board's.
func onBoard(p *playback.DSDPolicy) {
	p.Profiles = &dacprofile.Database{SchemaVersion: dacprofile.SchemaVersion,
		SharedReferenceIDs: []dacprofile.SharedID{{VendorID: "1234", ProductID: "5678", Why: "the tests' board"}}}
}

// heard writes a listener's latest answer for native at rate under the test
// DAC's firmware into the state through its store: a simulated answer.
func (r *checkRig) heard(rate int, answer string) {
	r.t.Helper()
	if err := r.store.Update(func(st *dsdstate.State) error {
		d := st.Entry(playbacktest.DACKey(), playbacktest.DACStamp())
		d.Listener = append(d.Listener, dsdstate.Answer{Transport: dsdstate.TransportNative, BitRate: rate, BCDDevice: playbacktest.DACBCD, Answer: answer, On: "2026-11-23"})
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

// sessionRefused plays a track of DSD at rate in a session of its own and
// fails the test unless the engine skips it with word, no sink opened for it,
// then waits for the backend to let go of its record.
func (r *checkRig) sessionRefused(ref string, rate int, word engine.Reason) {
	r.t.Helper()
	opens := len(r.openFormats())
	r.files.add(ref, dsd(rate, 0.25))
	id := r.insert(ref)[0]
	ctx, cancel := within()
	defer cancel()
	_ = r.eng.PlayID(ctx, id)
	s := r.waitSnap("the session's DSD skipped, or a sink opened for it", func(s *engine.Snapshot) bool {
		_, skipped := s.Skipped(id)
		return skipped && s.State() == engine.Stopped || len(r.openFormats()) > opens
	})
	if sk, ok := s.Skipped(id); !ok || sk.Reason != word {
		r.t.Errorf("a session's DSD at %d: skipped %+v (%v), the engine %s; want it skipped %s", rate, sk, ok, s.State(), word)
	}
	if n := len(r.openFormats()); n != opens {
		r.t.Errorf("a session's DSD at %d opened %d sinks; want it refused before any", rate, n-opens)
	}
	waitOn(r.t, "the backend to let go of the refused session's record", r.rec.ch, func() bool { return r.be.Stats().Sessions == 0 })
}

// nativeOpens are the native and DoP opens asked, in order.
func (d *dsdRig) nativeOpens() []alsapcm.DSDConfig {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]alsapcm.DSDConfig(nil), d.opens...)
}

// TestAGenericBoardsNativeCheckLetsItsAnswerBeEarned: on a generic board's
// IDs under an explicit native, a session's DSD64 is refused
// native_check_required, before the check and after it. The check of native
// at DSD64 is admitted by the refusal's candidate and plays to complete on
// the identity it was admitted under, its driver_accepted recorded; with a
// listener's clean answer at DSD64 (simulated) a session then plays DSD64
// natively, its sink opened as the check's was, by the same plan: kind,
// endpoint, format, rate, pause, tail and prefill, and the same format and
// frame rate asked of the kernel.
func TestAGenericBoardsNativeCheckLetsItsAnswerBeEarned(t *testing.T) {
	r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), onBoard)
	r.sessionRefused("before", playbacktest.DSD64, policy.ReasonNativeCheckRequired)
	c := r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
	r.k.Free()
	st := r.endedAs(playback.CheckComplete, "", "the candidate's check")
	r.k.Hold()
	if st.ID != c.st.ID || !st.Identity.Same(checkIdentity()) || st.Opened == nil || !st.Opened.Same(checkIdentity()) {
		t.Errorf("the check's record once complete: %+v; want the identity it was admitted under opened", st)
	}
	if e := playbacktest.Evidence(r.store.State(), dsdstate.TransportNative, string(format.DSDU32BE), playbacktest.DSD64); e == nil || e.DriverAcceptedAt.IsZero() {
		t.Errorf("the check's evidence: %+v; want its driver_accepted", e)
	}
	r.sessionRefused("after the check", playbacktest.DSD64, policy.ReasonNativeCheckRequired)

	r.heard(playbacktest.DSD64, dsdstate.AnswerClean)
	r.be.RefreshDSD(true)
	r.files.add("heard", dsd(playbacktest.DSD64, 0.25))
	id := r.insert("heard")[0]
	// No row sets the plan's prefill, so it is the default 400 ms, which the
	// Start's answer waits for: the DAC plays freely.
	r.k.Free()
	ctx, cancel := within()
	defer cancel()
	if err := r.eng.PlayID(ctx, id); err != nil {
		t.Fatalf("a session's DSD64 once heard clean: %v", err)
	}
	s := r.waitEnded()
	if _, skipped := s.Skipped(id); skipped || len(s.Faults()) != 0 || s.LastAttempt().Failed {
		t.Errorf("the heard session: skipped %v, faults %+v, attempt %+v; want it played", skipped, s.Faults(), s.LastAttempt())
	}
	opens, asked := r.nativeOpens(), r.k.View().Asked
	if len(opens) != 2 || len(asked) < 2 {
		t.Fatalf("%d native opens and %d asked of the kernel; want the check's and the session's", len(opens), len(asked))
	}
	check, session := opens[0], opens[1]
	if check.Kind != session.Kind || check.Endpoint != session.Endpoint || check.Format != session.Format || check.BitRate != session.BitRate ||
		check.Pause != session.Pause || check.Tail != session.Tail || check.Prefill != session.Prefill || check.Format != format.DSDU32BE {
		t.Errorf("the check opened %+v and the session %+v; want one plan, DSD_U32_BE at DSD64", check, session)
	}
	want := alsapcm.Ask{Format: format.DSDU32BE, RateHz: playbacktest.DSD64 / 32}
	if a, b := asked[len(asked)-2], asked[len(asked)-1]; a != want || b != want {
		t.Errorf("the kernel was asked %+v by the check and %+v by the session; want both %+v", a, b, want)
	}
}

// TestAGenericBoardsCheckTakesItsCandidateAlone: the candidate admits the
// check of native at its rate and nothing else. On a generic board's IDs,
// each refused by the set's word for it, the record and the opens as they
// were: DoP asked where native waits for a listener's answer
// (native_check_required, the rate's own word: the candidate is native's);
// native above the cap (native_check_required, whose refusal carries no
// candidate, the cap asked after the board); native a listener rejected
// (listener_rejected); native at a rate the DAC does not offer
// (native_not_offered); auto, which converts; and a token another reading
// made (dac_changed).
func TestAGenericBoardsCheckTakesItsCandidateAlone(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	capped := playbacktest.ChoiceState(dsdstate.ModeNative)
	capped.DACs[0].Choice.MaxBitRate = playbacktest.DSD64
	rejected := playbacktest.ChoiceState(dsdstate.ModeNative)
	rejected.DACs[0].Listener = []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, Answer: dsdstate.AnswerNoise, On: "2026-11-23"}}
	noDSD256 := func(p *playback.DSDPolicy) {
		onBoard(p)
		a := playbacktest.NativeAltset()
		a.Rates = []int{88200, 176400}
		p.Box.(*playbacktest.Box).SetAltsets(card, 0, playbacktest.S32Altset(), playbacktest.S24Altset(), a)
	}
	for _, c := range []struct {
		name      string
		st        dsdstate.State
		edit      func(*playback.DSDPolicy)
		transport string
		rate      int
		token     playback.DACToken
		want      string
	}{
		{name: "DoP where native waits for a listener's answer", st: native, edit: onBoard, transport: dsdstate.TransportDoP, rate: playbacktest.DSD64,
			want: string(policy.ReasonNativeCheckRequired)},
		{name: "native above the cap", st: capped, edit: onBoard, transport: dsdstate.TransportNative, rate: playbacktest.DSD128,
			want: string(policy.ReasonNativeCheckRequired)},
		{name: "native a listener rejected", st: rejected, edit: onBoard, transport: dsdstate.TransportNative, rate: playbacktest.DSD64,
			want: string(policy.ReasonListenerRejected)},
		{name: "native the DAC does not offer at the rate", st: native, edit: noDSD256, transport: dsdstate.TransportNative, rate: playbacktest.DSD256,
			want: string(policy.ReasonNativeNotOffered)},
		{name: "auto", st: playbacktest.ChoiceState(dsdstate.ModeAuto), edit: onBoard, transport: dsdstate.TransportNative, rate: playbacktest.DSD64,
			want: playback.CheckConverted},
		{name: "a token another reading made", st: native, edit: onBoard, transport: dsdstate.TransportNative, rate: playbacktest.DSD64,
			token: "t1-00000000000000000000000000000000", want: playback.CheckDACChanged},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCheckRig(t, c.st, c.edit)
			req, stop := r.request(c.transport, c.rate)
			if c.token != "" {
				req.DACToken = c.token
			}
			r.refused(req, stop, c.want, c.name)
			if _, ok := r.be.CheckStatus(); ok {
				t.Error("a refused check made a record")
			}
		})
	}
}

// TestACandidatesCheckIsJudgedByItsCandidate: a check the candidate admitted
// is judged by the candidate published at its rate, on the identity it was
// admitted under and opened, as policy.Reconcile judges a session's
// occurrence by the set's plan. A set that leaves its candidate as it was
// keeps it playing to complete: another rate's candidate gone (the cap moved
// to DSD128), and, at the check's own monitor's tick after its open recorded
// its driver_accepted, a reading that changes evidence alone and publishes no
// new set, a session's DSD at the rate refused native_check_required after
// it. Its candidate gone (the cap moved below its rate, the refusal's word
// the same) or changed (the kernel's pause found once its driver_accepted
// was recorded, or DSD_U32_BE refused where the DAC offers DSD_U32_LE) ends
// it failed, policy_changed; and so does another maker's unit offered the
// same candidate, before anything opens.
func TestACandidatesCheckIsJudgedByItsCandidate(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	capAt := func(max int) dsdstate.State {
		st := playbacktest.ChoiceState(dsdstate.ModeNative)
		st.DACs[0].Choice.MaxBitRate = max
		return st
	}
	probed := func(r *checkRig, res alsapcm.ProbeResult) {
		u := alsapcm.ProbeUnit{Model: playbacktest.DACKey().Canonical(), Stamp: string(playbacktest.DACStamp()), Firmware: playbacktest.DACBCD, Kernel: playbacktest.Kernel, Enumeration: "test/2"}
		if !r.pol.Probes.Record(r.pol.Probes.Begin(u), alsapcm.ProbeReport{Opened: true, Results: []alsapcm.ProbeResult{res}}) {
			r.t.Fatal("the probe's answer was not kept")
		}
	}
	u32be64 := alsapcm.ProbeCandidate{Format: format.DSDU32BE, RateHz: playbacktest.DSD64 / 32}
	accepted := func(r *checkRig) {
		r.waitState("the check's driver_accepted", func(st dsdstate.State) bool {
			e := playbacktest.Evidence(st, dsdstate.TransportNative, string(format.DSDU32BE), playbacktest.DSD64)
			return e != nil && !e.DriverAcceptedAt.IsZero()
		})
	}
	// refreshed refreshes the policy and fails the test unless it published
	// a new set.
	refreshed := func(r *checkRig, what string) {
		r.t.Helper()
		g := r.be.Policy().Generation
		r.be.RefreshDSD(true)
		if now := r.be.Policy().Generation; now == g {
			r.t.Fatalf("%s left the set at generation %d: the case needs a new set", what, g)
		}
	}
	t.Run("another rate's candidate gone", func(t *testing.T) {
		r := newCheckRig(t, native, onBoard)
		r.playingCheck(dsdstate.TransportNative)
		r.store.Set(capAt(playbacktest.DSD128))
		refreshed(r, "the cap moved to DSD128")
		if p := r.phase(); p != playback.CheckPlaying {
			t.Fatalf("the check is %s once DSD256's candidate went; want it playing", p)
		}
		r.k.Free()
		r.endedAs(playback.CheckComplete, "", "a check whose candidate the new set keeps")
	})
	t.Run("its driver_accepted recorded", func(t *testing.T) {
		r := newCheckRig(t, native, onBoard)
		r.playingCheck(dsdstate.TransportNative)
		accepted(r)
		g := r.be.Policy().Generation
		r.monitorTick()
		if now := r.be.Policy().Generation; now != g {
			t.Errorf("a reading that changed evidence alone published generation %d after %d", now, g)
		}
		if p := r.phase(); p != playback.CheckPlaying {
			t.Fatalf("the check is %s after its monitor's tick; want it playing", p)
		}
		r.k.Free()
		r.endedAs(playback.CheckComplete, "", "a check whose candidate its driver_accepted leaves as it was")
		r.k.Hold()
		r.sessionRefused("after", playbacktest.DSD64, policy.ReasonNativeCheckRequired)
	})
	t.Run("its candidate gone", func(t *testing.T) {
		r := newCheckRig(t, native, onBoard)
		r.admitted(dsdstate.TransportNative, playbacktest.DSD128)
		r.k.Await(t, "the check's writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
		r.store.Set(capAt(playbacktest.DSD64))
		refreshed(r, "the cap moved below the check's rate")
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "the cap moved below the check's rate")
	})
	t.Run("its candidate's pause changed once its driver_accepted was recorded", func(t *testing.T) {
		r := newCheckRig(t, native, onBoard)
		r.playingCheck(dsdstate.TransportNative)
		accepted(r)
		probed(r, alsapcm.ProbeResult{Candidate: u32be64, Answer: alsapcm.ProbeAccepted, Msbits: 32, Pause: true})
		refreshed(r, "the kernel's pause found")
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "the kernel's pause found")
	})
	t.Run("its candidate's format changed", func(t *testing.T) {
		both := func(p *playback.DSDPolicy) {
			onBoard(p)
			a := playbacktest.NativeAltset()
			a.Formats = []string{"SPECIAL", "DSD_U32_BE", "DSD_U32_LE"}
			p.Box.(*playbacktest.Box).SetAltsets(card, 0, playbacktest.S32Altset(), playbacktest.S24Altset(), a)
		}
		r := newCheckRig(t, native, both)
		r.playingCheck(dsdstate.TransportNative)
		probed(r, alsapcm.ProbeResult{Candidate: u32be64, Answer: alsapcm.ProbeRefused})
		refreshed(r, "DSD_U32_BE refused")
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "DSD_U32_BE refused")
	})
	t.Run("another maker's unit offered the same candidate", func(t *testing.T) {
		r := newCheckRig(t, identicalElsewhere(), onBoard)
		_, p := r.preparingCheck()
		m := playbacktest.DACUnit()
		m.Manufacturer = "Another Maker"
		r.box.SetUnit(card, m)
		refreshed(r, "another maker's unit")
		p.letGo()
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "another maker's unit")
		if v := r.k.View(); len(v.Opened) != 0 {
			t.Errorf("a check reconciled out asked %d opens", len(v.Opened))
		}
	})
}
