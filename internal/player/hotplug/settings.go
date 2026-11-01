package hotplug

import "github.com/acoseac/1-bit-bridge/internal/config"

// Settings is what the watcher reads of the live config: whether the
// player runs, why not when the config asks for one it cannot have, and
// the binding. The watcher reads it on every reading and at every change
// of the config, never once at start, so a binding changed while serve
// runs is what the next reading resolves.
type Settings struct {
	// Enabled is config.PlayerEnabledAsBooted: the player runs on this
	// host, the OpenHome port clash judged on serve's boot snapshot.
	Enabled bool
	// Off is its reason when the block asks for a player this config
	// cannot have, nil when the player is simply off.
	Off error
	// Device is output.player.device.
	Device config.PlayerDevice
}

// SettingsFrom reads the player's settings from the live config live as a
// goos host runs them, for a serve whose boot snapshot is boot: the
// OpenHome port clash is judged on boot, whose listeners serve bound and
// whose opt-in its surface started from, and the rest on live
// (config.PlayerEnabledAsBooted). A nil live is a player that is off; a nil
// boot judges the clash on live.
func SettingsFrom(live, boot *config.Config, goos string) Settings {
	if live == nil {
		return Settings{}
	}
	on, why := live.PlayerEnabledAsBooted(goos, boot)
	return Settings{Enabled: on, Off: why, Device: live.Output.Player.Device}
}

// offReason is the disabled snapshot's reason, as binding.Status words it.
func (s Settings) offReason() string {
	if s.Off != nil {
		return s.Off.Error()
	}
	return "the player is off (output.player.enabled is not true)"
}
