package hotplug

import "github.com/acoseac/1-bit-bridge/internal/config"

// Settings is what the watcher reads of the live config: whether the
// player runs, why not when the config asks for one it cannot have, and
// the binding. The watcher reads it on every reading and at every change
// of the config, never once at start, so a binding changed while serve
// runs is what the next reading resolves.
type Settings struct {
	// Enabled is config.PlayerEnabled: the player runs on this host.
	Enabled bool
	// Off is PlayerEnabled's reason when the block asks for a player this
	// config cannot have, nil when the player is simply off.
	Off error
	// Device is output.player.device.
	Device config.PlayerDevice
}

// SettingsFrom reads the player's settings from cfg as a goos host runs
// them. A nil cfg is a player that is off.
func SettingsFrom(cfg *config.Config, goos string) Settings {
	if cfg == nil {
		return Settings{}
	}
	on, why := cfg.PlayerEnabled(goos)
	return Settings{Enabled: on, Off: why, Device: cfg.Output.Player.Device}
}

// offReason is the disabled snapshot's reason, as binding.Status words it.
func (s Settings) offReason() string {
	if s.Off != nil {
		return s.Off.Error()
	}
	return "the player is off (output.player.enabled is not true)"
}
