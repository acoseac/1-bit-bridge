package main

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// TestAnUncertainChoiceGoesOutAsAWordTheProtocolNames (B728 B9 PR 3): a
// choice's write that reaches the state's file and is not known to be kept
// is 500 persist_uncertain, its answer's refused naming dsd_choice_uncertain
// for the rate the two states play apart, as GET does after it and after a
// refresh; every later write answers persist_uncertain, a stale one too; and
// a write of the state other than a choice's (an evidence write) that lands
// fences the next choice the same way.
func TestAnUncertainChoiceGoesOutAsAWordTheProtocolNames(t *testing.T) {
	uncertain := api.OutputDSDRefused{BitRate: playbacktest.DSD256, Reason: string(policy.ReasonChoiceUncertain)}
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	b := readSideBackend(t, playbacktest.NewBox(5), store, withKnownDAC)
	r := newChoiceRig(t, b)
	d := r.dac(t)
	store.Land(fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded))
	body := choiceBody(t, d, dsdstate.ModeNative, fmt.Sprintf(`"maxBitRate":%d`, playbacktest.DSD128))
	obj := wantAnswer(t, "the write that landed", r.put(t, body), http.StatusInternalServerError, "persist_uncertain", 4)
	if p := dsdPlaybackOf(t, obj); !slices.Contains(p.Refused, uncertain) {
		t.Fatalf("the answer refuses %+v; want DSD256 refused as dsd_choice_uncertain", p.Refused)
	}
	for _, when := range []string{"after the write", "after a refresh"} {
		if when == "after a refresh" {
			b.RefreshDSD(true)
		}
		if p := dsdPlaybackOf(t, r.bridge(t)); !slices.Contains(p.Refused, uncertain) {
			t.Errorf("GET %s refuses %+v; want DSD256 refused as dsd_choice_uncertain", when, p.Refused)
		}
	}
	wantAnswer(t, "a later write, its base stale", r.put(t, body), http.StatusInternalServerError, "persist_uncertain", 4)

	t.Run("an evidence write that landed", func(t *testing.T) {
		store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		r := newChoiceRig(t, readSideBackend(t, playbacktest.NewBox(5), store, withKnownDAC))
		d := r.dac(t)
		store.Land(fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded))
		if err := store.Update(func(st *dsdstate.State) error {
			st.DACs[0].Evidence = append(st.DACs[0].Evidence, dsdstate.Evidence{Transport: dsdstate.TransportNative, Format: "DSD_U32_BE",
				BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, DriverAcceptedAt: playbacktest.T0, Kernel: playbacktest.Kernel})
			return nil
		}); err == nil {
			t.Fatal("the evidence write did not land")
		}
		wantAnswer(t, "a choice after it", r.put(t, choiceBody(t, d, dsdstate.ModeOff)), http.StatusInternalServerError, "persist_uncertain", 4)
	})
}
