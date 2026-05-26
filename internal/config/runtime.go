package config

import "sync/atomic"

// RuntimeConfig is the process-wide holder for the live config
// snapshot. Readers call Load() per-request; writers clone, mutate,
// validate, save, run hooks, and atomically swap with Store().
type RuntimeConfig struct {
	ptr atomic.Pointer[Config]
}

// NewRuntimeConfig initialises a RuntimeConfig from cfg.
func NewRuntimeConfig(cfg *Config) *RuntimeConfig {
	rc := &RuntimeConfig{}
	rc.Store(cfg)
	return rc
}

// Load returns the current immutable snapshot.
func (r *RuntimeConfig) Load() *Config {
	if r == nil {
		return nil
	}
	return r.ptr.Load()
}

// Store atomically swaps the current snapshot. Nil is ignored.
func (r *RuntimeConfig) Store(cfg *Config) {
	if r == nil || cfg == nil {
		return
	}
	r.ptr.Store(cfg)
}

// Clone returns a deep clone of the current snapshot.
func (r *RuntimeConfig) Clone() *Config {
	return Clone(r.Load())
}

// Clone returns a deep clone of cfg. Nil input returns nil.
//
// IMPORTANT: When adding pointer or slice fields to Config, update
// this function to deep-copy them. TestConfigCloneIsDeep verifies
// coverage via the reflection-based assertNoSharedPointers walker
// over a fillNonZero-populated fixture.
func Clone(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	out := *cfg
	if cfg.LibraryRoots != nil {
		out.LibraryRoots = append([]string(nil), cfg.LibraryRoots...)
	}
	if cfg.CustomEndpoints != nil {
		out.CustomEndpoints = append([]string(nil), cfg.CustomEndpoints...)
	}
	if cfg.Backup.IntervalHours != nil {
		v := *cfg.Backup.IntervalHours
		out.Backup.IntervalHours = &v
	}
	if cfg.Limits.Manifest.RequestsPerMinute != nil {
		v := *cfg.Limits.Manifest.RequestsPerMinute
		out.Limits.Manifest.RequestsPerMinute = &v
	}
	if cfg.Limits.Manifest.Burst != nil {
		v := *cfg.Limits.Manifest.Burst
		out.Limits.Manifest.Burst = &v
	}
	if cfg.Integrity.VariantSweepIntervalSec != nil {
		v := *cfg.Integrity.VariantSweepIntervalSec
		out.Integrity.VariantSweepIntervalSec = &v
	}
	if cfg.Integrity.OrphanSidecarSweepIntervalSec != nil {
		v := *cfg.Integrity.OrphanSidecarSweepIntervalSec
		out.Integrity.OrphanSidecarSweepIntervalSec = &v
	}
	if cfg.Upscale.OptimizeEnabled != nil {
		v := *cfg.Upscale.OptimizeEnabled
		out.Upscale.OptimizeEnabled = &v
	}
	if cfg.MDNS.Enabled != nil {
		v := *cfg.MDNS.Enabled
		out.MDNS.Enabled = &v
	}
	if cfg.DLNA.TelemetryEnabled != nil {
		v := *cfg.DLNA.TelemetryEnabled
		out.DLNA.TelemetryEnabled = &v
	}
	return &out
}
