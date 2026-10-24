package main

import (
	"context"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// hostLAN lists the host's interfaces where nothing names another box: what
// serve's OpenHome surface serves (B470 PR 5a2a), through lan.Look. This
// package's TestMain points it at a box with no interface before any test
// runs and never again, so no serve test binds the host's LAN addresses; a
// test of the surface hands serve a box of its own (playerOpenHome).
var hostLAN lan.Lister = lan.HostInterfaces

// openhomeWiring is a boot test's hold on serve's OpenHome surface; the zero
// value is production's: the host's interfaces through hostLAN, the
// platform's listener (openhome.Listen) and openhome.DefaultLimits.
type openhomeWiring struct {
	// Addresses and Listen are the surface's box: a test's lantest.Box.
	Addresses func() (lan.Inventory, error)
	Listen    openhome.ListenFunc
	Limits    openhome.Limits
	Seams     *openhome.Seams
	// Started is told the surface once the player starts it.
	Started func(*openhome.Surface)
}

// startOpenHome starts the player's OpenHome surface, which the plan asks
// for (output.player.openhome) and the player runs: id is the player's
// identity, its UDN; the room and the port are the boot snapshot's; its
// read actions answer from the engine the player runs (B470 PR 5a3), and
// from no engine once that engine's Run has ended. It listens on each LAN
// address and registers bound endpoints with serve's shared SSDP
// subsystem. A surface that cannot start fails alone: the player runs
// without it, and says so; an address that cannot be bound fails that
// address alone, said by the surface.
func (p *bridgePlayer) startOpenHome(id string, w servePlayerWiring) {
	player := w.Cfg.Output.Player
	o := w.OpenHome
	addresses := o.Addresses
	if addresses == nil {
		addresses = func() (lan.Inventory, error) { return lan.Look(hostLAN) }
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return
	}
	var eng openhome.Engine
	if p.eng != nil {
		eng = p.eng
	}
	s, err := openhome.Start(openhome.Config{
		SSDP:      w.SSDP,
		Device:    openhome.Device{UUID: id, Room: player.Room, ModelNumber: version.ServerVersion},
		Engine:    eng,
		Port:      player.EffectiveOpenHomePort(),
		Addresses: addresses,
		Listen:    o.Listen,
		Limits:    o.Limits,
		Log:       playerLog,
		Seams:     o.Seams,
	})
	if err != nil {
		p.mu.Unlock()
		playerLog.Warn("player: the OpenHome surface did not start; the player runs without it", "err", err)
		return
	}
	p.surface = s
	p.mu.Unlock()
	playerLog.Info("player: the OpenHome surface is on; discovery follows its bound LAN addresses", "port", player.EffectiveOpenHomePort())
	if o.Started != nil {
		o.Started(s)
	}
}

// stopOpenHome stops the surface, first in the player's shutdown: every
// request after it is refused, those under way get the surface's grace,
// and what is still open is closed, so no request reaches the engine as it
// stops (openhome.Surface.Stop, bounded by its limits).
func (p *bridgePlayer) stopOpenHome() {
	p.mu.Lock()
	s, ctx := p.surface, p.discoveryStop
	p.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if s != nil {
		s.StopContext(ctx)
	}
}

// refuseOpenHome freezes a surface already running and any start still in
// progress, before another SSDP owner or the shared subsystem is retired.
func (p *bridgePlayer) refuseOpenHome(ctx context.Context) {
	p.mu.Lock()
	p.closing = true
	if p.discoveryStop == nil {
		p.discoveryStop = ctx
	} else if deadline, ok := ctx.Deadline(); ok {
		if old, had := p.discoveryStop.Deadline(); !had || deadline.Before(old) {
			p.discoveryStop = ctx
		}
	}
	s := p.surface
	p.mu.Unlock()
	if s != nil {
		s.Refuse(ctx)
	}
}

// withdrawOpenHome joins discovery alone; HTTP and the engine have their own bounds.
func (p *bridgePlayer) withdrawOpenHome(ctx context.Context) {
	p.refuseOpenHome(ctx)
	p.mu.Lock()
	s := p.surface
	p.mu.Unlock()
	if s != nil {
		s.Withdraw(ctx)
	}
}
