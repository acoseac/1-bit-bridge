package alsapcm

import (
	"fmt"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// brooklyn is a unit the cache tests file answers under.
var brooklyn = ProbeUnit{Model: "dacKey/1\n262a\n9023\n=1\n=Brooklyn DAC+", Firmware: "0102", Kernel: "6.12.47+rpt-rpi-v8", Enumeration: "feed 1, generation 4"}

var (
	nativeDSD64  = ProbeCandidate{format.DSDU32BE, 88200}
	nativeDSD256 = ProbeCandidate{format.DSDU32BE, 352800}
	carrierDSD64 = ProbeCandidate{format.S32LE, 176400}
	target441    = ProbeCandidate{format.S16LE, 44100}
)

// answered is a report holding one accepted, one refused and one not probed
// answer.
func answered() ProbeReport {
	return ProbeReport{Endpoint: testEndpoint, Opened: true, Results: []ProbeResult{
		{Candidate: nativeDSD64, Answer: ProbeAccepted, Msbits: 32, Pause: true},
		{Candidate: nativeDSD256, Answer: ProbeRefused},
		{Candidate: carrierDSD64, Answer: ProbeAccepted, Msbits: 24, Pause: true},
		{Candidate: target441, Answer: ProbeNotProbed},
	}}
}

// TestTheProbeCacheKeepsAnAnswerForExactlyItsUnit pins the cache's key (B728
// B4): an answer is kept under the DAC model, its firmware, the kernel and
// the unit's enumeration it was given for, and the candidate, with its
// significant bits and its pause flag, and a lookup under any other model,
// firmware (a bcdDevice change), kernel (a new kernel is probed again) or
// enumeration (a replug, a reset) finds none: each of the four lapses it
// (the design's sections 1.2 and 5). A candidate never answered is not
// probed, and a later probe of the same unit replaces an answer.
func TestTheProbeCacheKeepsAnAnswerForExactlyItsUnit(t *testing.T) {
	c := NewProbeCache()
	c.Record(brooklyn, answered())
	for _, want := range []ProbeResult{
		{Candidate: nativeDSD64, Answer: ProbeAccepted, Msbits: 32, Pause: true},
		{Candidate: nativeDSD256, Answer: ProbeRefused},
		{Candidate: carrierDSD64, Answer: ProbeAccepted, Msbits: 24, Pause: true},
		{Candidate: target441, Answer: ProbeNotProbed},
		{Candidate: ProbeCandidate{format.S243LE, 88200}, Answer: ProbeNotProbed},
	} {
		if got := c.Answer(brooklyn, want.Candidate); got != want {
			t.Errorf("%s: %+v; want %+v", want.Candidate, got, want)
		}
	}
	for name, u := range map[string]ProbeUnit{
		"another model":       {Model: "dacKey/1\n262a\n9023\n=1\n=Brooklyn DAC+ II", Firmware: brooklyn.Firmware, Kernel: brooklyn.Kernel, Enumeration: brooklyn.Enumeration},
		"another firmware":    {Model: brooklyn.Model, Firmware: "0103", Kernel: brooklyn.Kernel, Enumeration: brooklyn.Enumeration},
		"another kernel":      {Model: brooklyn.Model, Firmware: brooklyn.Firmware, Kernel: "6.12.62+rpt-rpi-v8", Enumeration: brooklyn.Enumeration},
		"another enumeration": {Model: brooklyn.Model, Firmware: brooklyn.Firmware, Kernel: brooklyn.Kernel, Enumeration: "feed 1, generation 9"},
	} {
		for _, cand := range []ProbeCandidate{nativeDSD64, nativeDSD256, carrierDSD64} {
			if got := c.Answer(u, cand); got != (ProbeResult{Candidate: cand, Answer: ProbeNotProbed}) {
				t.Errorf("%s, %s: %+v; want not probed", name, cand, got)
			}
		}
	}
	c.Record(brooklyn, ProbeReport{Results: []ProbeResult{{Candidate: nativeDSD64, Answer: ProbeRefused}}})
	if got := c.Answer(brooklyn, nativeDSD64); got != (ProbeResult{Candidate: nativeDSD64, Answer: ProbeRefused}) {
		t.Errorf("after a later probe refused it: %+v", got)
	}
	if got := c.Answer(brooklyn, carrierDSD64); got.Answer != ProbeAccepted || got.Msbits != 24 {
		t.Errorf("a later probe that did not ask the carrier changed its answer: %+v", got)
	}
}

// TestAProbeOfTheModelAnewLapsesWhatWasKeptBefore pins the lapse itself: once
// the model is probed under another firmware, kernel or enumeration (a busy
// probe that answered nothing included), what was kept under the old one is
// dropped, so a lookup under the old unit finds nothing either; another
// model's answers stay.
func TestAProbeOfTheModelAnewLapsesWhatWasKeptBefore(t *testing.T) {
	other := ProbeUnit{Model: "dacKey/1\n2972\n0047\n-\n=GO bar", Firmware: "0100", Kernel: brooklyn.Kernel, Enumeration: "feed 1, generation 2"}
	for name, next := range map[string]ProbeUnit{
		"another firmware":    {Model: brooklyn.Model, Firmware: "0103", Kernel: brooklyn.Kernel, Enumeration: "feed 1, generation 5"},
		"another kernel":      {Model: brooklyn.Model, Firmware: brooklyn.Firmware, Kernel: "6.12.62+rpt-rpi-v8", Enumeration: brooklyn.Enumeration},
		"another enumeration": {Model: brooklyn.Model, Firmware: brooklyn.Firmware, Kernel: brooklyn.Kernel, Enumeration: "feed 1, generation 9"},
	} {
		t.Run(name, func(t *testing.T) {
			c := NewProbeCache()
			c.Record(brooklyn, answered())
			c.Record(other, answered())
			busy := ProbeReport{Results: []ProbeResult{{Candidate: nativeDSD64, Answer: ProbeNotProbed}}}
			c.Record(next, busy)
			for _, cand := range []ProbeCandidate{nativeDSD64, nativeDSD256, carrierDSD64} {
				if got := c.Answer(brooklyn, cand); got.Answer != ProbeNotProbed {
					t.Errorf("the old unit's %s outlived the model's probe under %s: %+v", cand, name, got)
				}
				if got := c.Answer(next, cand); got.Answer != ProbeNotProbed {
					t.Errorf("the new unit's %s: %+v; want not probed", cand, got)
				}
			}
			if got := c.Answer(other, nativeDSD64); got.Answer != ProbeAccepted {
				t.Errorf("another model's answer: %+v; want it kept", got)
			}
		})
	}
}

// TestAChoiceWriteForgetsTheModelsAnswers pins Forget, what a choice written
// for a DAC calls (the design's section 7.2): every answer kept for that
// model, under any unit, goes, and another model's stay.
func TestAChoiceWriteForgetsTheModelsAnswers(t *testing.T) {
	other := ProbeUnit{Model: "dacKey/1\n2972\n0047\n-\n=GO bar", Firmware: "0100", Kernel: brooklyn.Kernel, Enumeration: "feed 1, generation 2"}
	c := NewProbeCache()
	c.Record(brooklyn, answered())
	c.Record(other, answered())
	c.Forget(brooklyn.Model)
	for _, cand := range []ProbeCandidate{nativeDSD64, nativeDSD256, carrierDSD64} {
		if got := c.Answer(brooklyn, cand); got.Answer != ProbeNotProbed {
			t.Errorf("%s outlived the choice written for its model: %+v", cand, got)
		}
	}
	if got := c.Answer(other, nativeDSD256); got.Answer != ProbeRefused {
		t.Errorf("another model's answer: %+v; want it kept", got)
	}
}

// TestABusyProbeIsNeverCachedAsARefusal pins EBUSY as not probed, never a
// refusal (B728 B4): a busy probe's report, every candidate not probed, keeps
// nothing for a unit nothing was kept for, and changes nothing kept for the
// same unit before.
func TestABusyProbeIsNeverCachedAsARefusal(t *testing.T) {
	k := dsdDAC(t)
	k.openErr = errnoEBUSY
	cands := []ProbeCandidate{nativeDSD64, nativeDSD256, carrierDSD64}
	rep, err := probe(k, testEndpoint, ProbeConfig{Candidates: cands})
	wantReason(t, err, ReasonBusy)
	c := NewProbeCache()
	c.Record(brooklyn, rep)
	for _, cand := range cands {
		if got := c.Answer(brooklyn, cand); got.Answer != ProbeNotProbed {
			t.Errorf("a busy probe kept %s as %+v; want not probed", cand, got)
		}
	}
	c.Record(brooklyn, answered())
	c.Record(brooklyn, rep)
	if got := c.Answer(brooklyn, nativeDSD256); got.Answer != ProbeRefused {
		t.Errorf("a busy probe changed a refusal kept before: %+v", got)
	}
	if got := c.Answer(brooklyn, nativeDSD64); got.Answer != ProbeAccepted {
		t.Errorf("a busy probe changed an acceptance kept before: %+v", got)
	}
}

// TestTheProbeCacheIsSafeForConcurrentUse records, looks up and forgets
// from several goroutines at once, for the race detector.
func TestTheProbeCacheIsSafeForConcurrentUse(t *testing.T) {
	c := NewProbeCache()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := brooklyn
			u.Enumeration = fmt.Sprint("generation ", i%3)
			for range 100 {
				c.Record(u, answered())
				_ = c.Answer(u, nativeDSD64)
				if i == 0 {
					c.Forget(u.Model)
				}
			}
		}()
	}
	wg.Wait()
}
