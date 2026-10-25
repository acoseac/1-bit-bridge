package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
)

// dac projects the boot binding only while the player and the watcher's
// latest resolution are available. The snapshot has no stream altsets, so
// take one bounded reading of both trees, resolving the boot binding again:
// neither the old index nor ALSA id identifies a unit after a replug.
func (b *bridgeOutput) dac(ctx context.Context, state string) *api.OutputDAC {
	if state == bridgeStateUnavailable || b.p.feed == nil || b.dacRead == nil || ctx.Err() != nil {
		return nil
	}
	snap, ok := b.p.feed.Latest()
	if !ok || !snap.Valid() || !hotplug.SameBinding(snap.Binding(), b.p.device) {
		return nil
	}
	if _, stale := snap.Stale(); stale || snap.Resolution().State != binding.StateAvailable {
		return nil
	}
	if !b.dacBusy.CompareAndSwap(false, true) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, b.dacLimit)
	defer cancel()
	done := make(chan *api.OutputDAC, 1)
	go func() {
		var dac *api.OutputDAC
		defer func() {
			if recover() != nil {
				// A reader panic is no DAC; never print an identity or path.
				dac = nil
			}
			b.dacBusy.Store(false)
			done <- dac
		}()
		dac = b.dacRead(ctx)
	}()
	select {
	case dac := <-done:
		if ctx.Err() == nil {
			return dac
		}
	case <-ctx.Done():
	}
	return nil
}

// readPlayerDAC uses the binding reader's very /proc reading for the chosen
// card's capabilities. Only the observed vendor:product and short card name
// cross the seam; USBIdentity and the longname (which includes the USB port)
// never do. A port binding intentionally follows whatever model is there.
func readPlayerDAC(ctx context.Context, trees outputTrees, device config.PlayerDevice) *api.OutputDAC {
	inv, proc, err := trees.observer().ObserveWithProc(ctx)
	if err != nil || inv.Partial || len(proc.Problems) != 0 {
		return nil
	}
	res, settled := binding.Settled(device, inv)
	if !settled || res.State != binding.StateAvailable {
		return nil
	}
	var found *alsa.CardReading
	for i := range proc.Cards {
		if proc.Cards[i].Index == res.Card {
			if found != nil {
				return nil
			}
			found = &proc.Cards[i]
		}
	}
	if found == nil || found.Partial {
		return nil
	}
	for _, d := range found.Devices {
		if d.Device != res.Endpoint.Device || d.Stream == nil || d.Stream.Playback == nil || len(d.Stream.Playback.Altsets) == 0 {
			continue
		}
		// A parsed prefix of a failed or truncated stream is not capabilities.
		for _, problem := range found.Problems {
			if strings.HasPrefix(problem, fmt.Sprintf("card%d/stream%d:", found.Index, d.Device)) {
				return nil
			}
		}
		caps := alsa.CapabilitiesFromAltsets(d.Stream.Playback.Altsets)
		return &api.OutputDAC{Card: found.Index, ID: found.ID, Name: found.Name, USBID: res.Identity.ID.String(),
			Capabilities: api.OutputCapabilities{MaxPCMRate: caps.MaxPCMRate, PCMBitDepths: caps.PCMBitDepths,
				NativeDSD: caps.NativeDSD, NativeDSDRates: caps.NativeDSDRates, DSDFormats: caps.DSDFormats}}
	}
	return nil
}
