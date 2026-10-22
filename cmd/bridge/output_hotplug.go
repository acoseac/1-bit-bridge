package main

import (
	"context"
	"runtime"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
)

// playerHotplugWiring is what serve hands the player's hotplug watcher
// (internal/player/hotplug, B470 PR 4a2).
type playerHotplugWiring struct {
	// GOOS stands in for runtime.GOOS; "" is this host.
	GOOS string
	// Holder is the live config: the watcher reads output.player from it
	// on every reading and wakes on its Changed.
	Holder *config.RuntimeConfig
	// OpenSource opens the event source; nil is the kernel's netlink
	// socket.
	OpenSource func(context.Context) hotplug.Source
	// Boot, when set, is told the decision: the service, the holder, and a
	// channel closed once the service's Run has returned; a nil service
	// where the watcher does not run.
	Boot func(*hotplug.Service, *config.RuntimeConfig, <-chan struct{})
}

// startPlayerHotplug starts the player's hotplug watcher on Linux, where
// alone the player runs: a service that runs a watch of the box while the
// live config enables the player (config.PlayerEnabled: enabled, its block
// valid, not public) and nothing while it does not, so turning the player
// on or off while serve runs starts or stops the watch. It reads the box
// through hostTrees, as every reader here does, and opens nothing under
// /dev/snd. Off Linux nothing starts.
//
// stop is serve's teardown: it cancels the service and waits for its Run,
// no longer than shutdownGrace. Run itself returns within the watcher's
// stop deadline (hotplug.DefaultStopWait), abandoning a read stuck in the
// kernel, so the wait only ends early on a defect, which it reports.
func startPlayerHotplug(ctx context.Context, w playerHotplugWiring) (stop func()) {
	goos := w.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "linux" {
		if w.Boot != nil {
			w.Boot(nil, w.Holder, nil)
		}
		return func() {}
	}
	svc := hotplug.NewService(hotplug.Config{
		Settings: func() hotplug.Settings { return hotplug.SettingsFrom(w.Holder.Load(), goos) },
		Changed:  w.Holder.Changed,
		Observe: binding.Observer{
			Proc:  alsa.Reader{ProcRoot: hostTrees.proc},
			Sysfs: alsa.SysfsReader{Root: hostTrees.sysfs},
		}.ObserveContext,
		OpenSource: w.OpenSource,
	})
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Run(sctx)
	}()
	if w.Boot != nil {
		w.Boot(svc, w.Holder, done)
	}
	return func() {
		cancel()
		grace := time.NewTimer(shutdownGrace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
			logger.Warn("player hotplug: the watcher's service did not return within the shutdown grace", "grace", shutdownGrace)
		}
	}
}
