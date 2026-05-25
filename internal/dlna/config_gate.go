package dlna

// DeploymentMode tells shouldEnableDLNA whether the bridge is running in
// the loopback/LAN posture (self-signed cert pinned at iOS pair time) or
// the public posture (Let's Encrypt cert, internet-reachable). Mirror of
// what config.go already tracks via cfg.PublicMode.
type DeploymentMode int

const (
	// DeploymentLoopback is the default LAN posture — self-signed cert,
	// mDNS on, admin console on 127.0.0.1, intended for home-network
	// deployments. DLNA may be enabled.
	DeploymentLoopback DeploymentMode = iota

	// DeploymentPublic is the public-internet posture — Let's Encrypt
	// cert, customEndpoints, intended for VPS-hosted bridges accessed
	// over the internet. DLNA is REFUSED in this mode regardless of
	// operator config (see ShouldEnableDLNA).
	DeploymentPublic
)

// DLNAConfig is the subset of bridge.yaml's `dlna:` block consulted by
// ShouldEnableDLNA. Mirror of internal/config.DLNAConfig — kept in this
// package to avoid an import cycle with internal/config.
type DLNAConfig struct {
	// Enabled is the operator opt-in. Default false. Setting this true
	// is NOT sufficient — ShouldEnableDLNA additionally refuses in
	// public deployment mode.
	Enabled bool
}

// ShouldEnableDLNA is the LOAD-BEARING SAFETY GATE that determines
// whether the bridge's DLNA listener may start. It must return (false,
// reason) in EVERY case where DLNA binding would expose the library
// unauthenticated to a non-LAN audience.
//
// The decision is asymmetric on purpose:
//   - Loopback (LAN) posture + cfg.Enabled=true  → (true, "enabled")
//   - Loopback (LAN) posture + cfg.Enabled=false → (false, "operator opt-out")
//   - Public posture, REGARDLESS of cfg.Enabled  → (false, "public deployment mode")
//
// The public-mode refusal is non-overridable on purpose. The bridge has
// no IP allowlist or per-listener auth mechanism today, so a
// publicly-reachable unauthenticated `/dlna/file/{trackID}` endpoint
// would let any internet user enumerate and download the library.
// Operators who genuinely need DLNA over the internet should run a
// loopback bridge on the home LAN and reach it via Tailscale (which
// authenticates the peer before the DLNA path runs).
//
// **DO NOT add a public-mode override flag.** This isn't a stylistic
// preference; it's the architectural decision to keep an unauthenticated
// endpoint off the public internet at all costs. If a future use case
// surfaces requiring public-mode DLNA, the right shape is signed-URL
// per-file authentication + IP allowlist, not flipping this gate.
func ShouldEnableDLNA(cfg DLNAConfig, mode DeploymentMode) (enabled bool, reason string) {
	if mode == DeploymentPublic {
		return false, "public deployment mode"
	}
	if !cfg.Enabled {
		return false, "operator opt-out (cfg.DLNA.Enabled = false)"
	}
	return true, "enabled"
}
