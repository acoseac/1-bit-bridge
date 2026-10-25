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
	ctx, cancel := context.WithTimeout(ctx, b.dacLimit)
	defer cancel()
	b.dacMu.Lock()
	flight := b.dacFlight
	if flight == nil {
		flight = &playerDACFlight{done: make(chan struct{})}
		b.dacFlight = flight
		// The shared read has its own lifetime: one caller leaving must not
		// cancel the evidence another caller is still waiting for.
		readCtx, readCancel := context.WithTimeout(context.WithoutCancel(ctx), b.dacLimit)
		go b.runDACRead(readCtx, readCancel, flight)
	}
	b.dacMu.Unlock()
	select {
	case <-flight.done:
		if ctx.Err() == nil {
			return flight.dac
		}
	case <-ctx.Done():
	}
	return nil
}

// playerDACFlight publishes an immutable result when done closes.
// The view keeps it only until the reader returns, including after a timeout.
type playerDACFlight struct {
	done chan struct{}
	dac  *api.OutputDAC
}

// runDACRead releases the slot when the reader returns or panics, even after
// its waiters have timed out.
func (b *bridgeOutput) runDACRead(ctx context.Context, cancel context.CancelFunc, flight *playerDACFlight) {
	var dac *api.OutputDAC
	defer cancel()
	defer func() {
		if recover() != nil || ctx.Err() != nil {
			// A reader panic is no DAC; never print an identity or path.
			dac = nil
		}
		b.dacMu.Lock()
		flight.dac = dac
		b.dacFlight = nil
		close(flight.done)
		b.dacMu.Unlock()
	}()
	dac = b.dacRead(ctx)
}

// readPlayerDAC uses the binding reader's very /proc reading for the chosen
// card's capabilities. Only the observed vendor:product and redacted card labels
// cross the seam; USBIdentity and the longname (which includes the USB port)
// never do. A port binding intentionally follows whatever model is there.
func readPlayerDAC(ctx context.Context, trees outputTrees, device config.PlayerDevice) *api.OutputDAC {
	return readPlayerDACObserved(ctx, trees.observer(), device)
}

// readPlayerDACObserved projects one observation, with injectable tree readers.
func readPlayerDACObserved(ctx context.Context, observer binding.Observer, device config.PlayerDevice) *api.OutputDAC {
	// Bracket proc with identities. Vendor:product alone cannot detect two
	// identical units exchanging card numbers while proc is being read.
	before := observer.Sysfs.ReadContext(ctx)
	inv, proc, err := observer.ObserveWithProc(ctx)
	if err != nil || !before.Present || !before.CardsComplete || before.Partial || inv.Partial || len(proc.Problems) != 0 {
		return nil
	}
	res, settled := binding.Settled(device, inv)
	if !settled || res.State != binding.StateAvailable {
		return nil
	}
	stable := false
	for _, id := range before.Cards {
		if id.Card == res.Card && !id.Vanished && id.Kind == alsa.IdentityUSB && id.USB != nil {
			stable = binding.SameUnit(*id.USB, res.Identity) && id.USB.DevNum > 0 && id.USB.DevNum == res.Identity.DevNum
			break
		}
	}
	if !stable {
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
		prefix := fmt.Sprintf("card%d/stream%d:", found.Index, d.Device)
		for _, problem := range found.Problems {
			if strings.HasPrefix(problem, prefix) {
				return nil
			}
		}
		caps := alsa.CapabilitiesFromAltsets(d.Stream.Playback.Altsets)
		return &api.OutputDAC{Card: found.Index, ID: playerDACText(found.ID, res.Identity), Name: playerDACText(found.Name, res.Identity), USBID: res.Identity.ID.String(),
			Capabilities: api.OutputCapabilities{MaxPCMRate: caps.MaxPCMRate, PCMBitDepths: append([]int{}, caps.PCMBitDepths...),
				NativeDSD: caps.NativeDSD, NativeDSDRates: append([]int{}, caps.NativeDSDRates...), DSDFormats: append([]string{}, caps.DSDFormats...)}}
	}
	return nil
}

// playerDACText removes known identity substrings from the bridge's public
// labels. MPD's projection is separate and unchanged.
func playerDACText(text string, identity alsa.USBIdentity) string {
	if identity.Port != "" {
		text = strings.ReplaceAll(text, identity.Port, "[redacted]")
	}
	return identity.Serial.Redact(text)
}
