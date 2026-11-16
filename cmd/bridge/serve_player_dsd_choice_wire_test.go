package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// TestAnUncertainChoiceGoesOutAsAWordTheProtocolNames (B728 B9 PR 2): a
// choice's write that lands and is not known to be durable holds the set to
// what the two states play alike, the rate they play apart refused in the
// backend as dsd_choice_uncertain (policy.ReasonChoiceUncertain), a word
// PROTOCOL.md's table does not name until B9 PR 3. GET /v1/outputs'
// serialized body sends no such word, dsd_state_unreadable in its place
// (wireDSDReason), after the write and again after the supervisor's refresh,
// which keeps the intersection.
func TestAnUncertainChoiceGoesOutAsAWordTheProtocolNames(t *testing.T) {
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	b := readSideBackend(t, playbacktest.NewBox(5), store, func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) })
	store.Land(fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := b.ChooseDSD(ctx, playback.ChoiceRequest{DACKey: b.Policy().DSD.DAC.Key,
		Choice: policy.Choice{BaseRevision: 1, Mode: dsdstate.ModeNative, MaxBitRate: playbacktest.DSD128, By: dsdstate.ByApp}})
	if res.Outcome != playback.ChoicePersistUncertain {
		t.Fatalf("the write that landed answered %s; want persist_uncertain", res.Outcome)
	}
	uncertain := playback.DSDRefusal{BitRate: playbacktest.DSD256, Reason: string(policy.ReasonChoiceUncertain)}
	check := func(when string) {
		t.Helper()
		if v := b.Policy().DSD; v == nil || !slices.Contains(v.Refused, uncertain) {
			t.Fatalf("%s the backend refuses %+v; the case needs DSD256 refused as dsd_choice_uncertain there", when, v)
		}
		body := outputsBodyOf(t, readSideFacts(t, b, config.PlayerDevice{}))
		if bytes.Contains(body, []byte(policy.ReasonChoiceUncertain)) {
			t.Errorf("%s GET /v1/outputs sends dsd_choice_uncertain, which PROTOCOL.md's table does not name:\n%s", when, body)
		}
		var dsd struct {
			Refused []struct {
				BitRate int    `json:"bitRate"`
				Reason  string `json:"reason"`
			} `json:"refused"`
		}
		if err := json.Unmarshal(bridgeObjectOf(t, body)["dsdPlayback"], &dsd); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range dsd.Refused {
			if r.BitRate == playbacktest.DSD256 {
				found = r.Reason == string(policy.ReasonStateUnreadable)
			}
		}
		if !found {
			t.Errorf("%s DSD256's refusal goes out as %+v; want dsd_state_unreadable", when, dsd.Refused)
		}
	}
	check("after the write,")
	b.RefreshDSD(true)
	check("after the supervisor's refresh,")
}
