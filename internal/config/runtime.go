package config

import (
	"errors"
	"sync"
	"sync/atomic"
)

// RuntimeConfig is the process-wide holder for the live config
// snapshot. Readers call Load() per-request; writers go through
// Update, which serialises the clone→mutate→Save→Store cycle behind
// one mutex shared by every config writer in the process.
type RuntimeConfig struct {
	ptr atomic.Pointer[Config]
	// mu serialises Update calls. Two writers that each clone the
	// same base snapshot and then Save would have the last write
	// silently drop the other's fields from both bridge.yaml and the
	// final live snapshot (2026-07-21 review finding M13).
	mu sync.Mutex
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

// Update atomically applies a read-modify-write cycle to the live
// config: it clones the current snapshot, hands the clone to fn to
// mutate (and validate), persists the result to path via Save, and
// publishes it with Store. The whole clone→Save→Store sequence runs
// under the holder's write lock — shared by every config writer in
// the process — so concurrent mutators (admin settings PATCH, roots
// add/remove, UPnP server CRUD, …) can't clone the same base and
// have the last Save silently drop the loser's field changes.
//
// When fn returns an error the clone is discarded: nothing is saved
// or stored, and fn's error is returned verbatim. A Save failure
// likewise leaves both the live snapshot and the on-disk file
// untouched. fn runs under the write lock, so it must not call
// Update re-entrantly (the mutex is not re-entrant) and must not
// retain the clone after returning.
func (r *RuntimeConfig) Update(path string, fn func(*Config) error) error {
	if r == nil {
		return errors.New("config: Update on nil RuntimeConfig")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := Clone(r.Load())
	if next == nil {
		return errors.New("config: Update with no config loaded")
	}
	if err := fn(next); err != nil {
		return err
	}
	if err := next.Save(path); err != nil {
		return err
	}
	r.ptr.Store(next)
	return nil
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
	// UPnPUpstream: the Servers slice + its per-element SkipTopLevelContainers
	// slice both need deep copies so mutations on the clone don't leak back.
	if cfg.UPnPUpstream.Servers != nil {
		out.UPnPUpstream.Servers = make([]UPnPUpstreamServerConfig, len(cfg.UPnPUpstream.Servers))
		for i, s := range cfg.UPnPUpstream.Servers {
			cp := s
			if s.SkipTopLevelContainers != nil {
				cp.SkipTopLevelContainers = append([]string(nil), s.SkipTopLevelContainers...)
			}
			out.UPnPUpstream.Servers[i] = cp
		}
	}
	return &out
}
