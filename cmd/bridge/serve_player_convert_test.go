package main

import (
	"context"
	"runtime"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// TestTheElementListsDSDWhileThePlayerConverts (B728 B11; output.md's codecs
// bullet): the player's element lists dsf and dff while the effective set
// plays a DSD rate, a rate the player converts to PCM included, and not while
// it plays none; a set that converts publishes no tuple and no reason, the
// converted rates the backend's alone until B8 publishes them, and so does the
// observation of a converted occurrence: the element sends none rather than
// call it PCM or native DSD.
func TestTheElementListsDSDWhileThePlayerConverts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the player's identity is written durably, which Windows cannot do, and the player whose element this is runs on Linux alone")
	}
	off := dsdstate.Defaults()
	off.DefaultMode = dsdstate.ModeOff
	for _, tc := range []struct {
		name   string
		st     dsdstate.State
		dsd    bool
		reason string
	}{
		{"an unknown DAC, converted", dsdstate.Defaults(), true, ""},
		{"DSD off", off, false, "dsd_off_by_choice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be, err := playback.New(playback.Config{DSD: playbacktest.Policy(5, playbacktest.NewBox(5), playbacktest.NewMemStore(tc.st)),
				Resolver: dopContents{}, Locator: refusedNativeLocator{new(int)}})
			if err != nil {
				t.Fatal(err)
			}
			be.RefreshDSD(true)
			if got := be.Policy().PlaysDSD(); got != tc.dsd {
				t.Fatalf("the set plays DSD: %v, want %v", got, tc.dsd)
			}
			ready := make(chan struct{})
			close(ready)
			cfg := playerOutputConfig(t.TempDir(), nil)
			seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
			out := newBridgeOutput(servePlayerPlan{lists: true}, &bridgePlayer{be: be, ready: ready}, cfg)
			f := out.Facts(context.Background())
			want := []string{"flac", "wav", "aiff", "alac"}
			if tc.dsd {
				want = append(want, "dsf", "dff")
			}
			if !slices.Equal(f.Codecs, want) {
				t.Errorf("the element's codecs %v, want %v", f.Codecs, want)
			}
			if f.DSDPlayback == nil || len(f.DSDPlayback.Tuples) != 0 || f.DSDPlayback.Reason != tc.reason {
				t.Errorf("the element's dsdPlayback %+v, want no tuple and the reason %q", f.DSDPlayback, tc.reason)
			}
		})
	}
	_, out := bridgeDSDFacts(playback.PlayerPolicy{BootID: "boot", Generation: 3}, &playback.PlaybackFacts{BootID: "boot", PolicyGeneration: 3, Session: 1, Occurrence: 2,
		State: "open", Format: "S24_3LE", RateHz: 176400, Channels: 2, ValidBits: 24, Transport: playback.TransportConverted,
		Converted: &playback.ConvertedPlayback{FromBitRate: playbacktest.DSD64, Filter: "dsdconv-1", PassbandHz: 30000, Dither: "none"}})
	if out != nil {
		t.Errorf("a converted occurrence's observation was published as %+v: it is B8's", *out)
	}
}
