package config

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// RuntimeConfig is the process-wide holder for the live config
// snapshot. Readers call Load() per-request; writers go through
// Update, which serialises the clone→mutate→Save→Store cycle behind
// one mutex shared by every config writer in the process, and takes
// the config file's lock (LockFile) shared with every writer outside it.
type RuntimeConfig struct {
	ptr atomic.Pointer[Config]
	// mu serialises Update calls. Two writers that each clone the
	// same base snapshot and then Save would have the last write
	// silently drop the other's fields from both bridge.yaml and the
	// final live snapshot (2026-07-21 review finding M13).
	mu sync.Mutex
	// file is the config file's bytes as this holder last read them for
	// the live snapshot (RecordFile) or wrote them (Update), and fileKnown
	// whether it has either; both under mu.
	file      []byte
	fileKnown bool
	// changedMu guards changed, the channel the next Store closes
	// (Changed), nil until someone asks for it.
	changedMu sync.Mutex
	changed   chan struct{}
}

// ErrChangedOutside is what a *ChangedOutsideError matches.
var ErrChangedOutside = errors.New("config: the config file changed outside this holder")

// ChangedOutsideError is Update's refusal of a save over a config file that
// no longer holds what the holder last read or wrote: another writer saved
// it since. It names the file and nothing it holds.
type ChangedOutsideError struct {
	// Path is the config file.
	Path string
}

func (e *ChangedOutsideError) Error() string {
	return fmt.Sprintf("%s was changed or removed by another writer since this bridge read it "+
		"(a bridge command such as library add or init, or an edit), so this change was not saved, "+
		"and the file keeps that writer's: restart the bridge to load the file, then make the change again", e.Path)
}

// Is makes a *ChangedOutsideError match ErrChangedOutside.
func (e *ChangedOutsideError) Is(target error) bool { return target == ErrChangedOutside }

// RecordFile tells the holder raw, the bytes the config file held when the
// live snapshot was read from it (LoadFile): its first Update then saves
// only over a file that still holds them. A holder told nothing saves its
// first Update over whatever the file holds, and records from then on.
func (r *RuntimeConfig) RecordFile(raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.file, r.fileKnown = append([]byte(nil), raw...), true
}

// Changed returns a channel that the next Store closes, the Store an
// Update makes included: a consumer that follows a setting without polling
// (the player's hotplug watcher follows output.player) takes the channel,
// then Loads, then waits on it, so a Store between its Load and its wait
// still wakes it. An Update that stores nothing closes nothing. A nil
// holder's channel is nil: it never changes.
func (r *RuntimeConfig) Changed() <-chan struct{} {
	if r == nil {
		return nil
	}
	r.changedMu.Lock()
	defer r.changedMu.Unlock()
	if r.changed == nil {
		r.changed = make(chan struct{})
	}
	return r.changed
}

// notifyChanged closes the channel Changed handed out, after a Store.
func (r *RuntimeConfig) notifyChanged() {
	r.changedMu.Lock()
	defer r.changedMu.Unlock()
	if r.changed != nil {
		close(r.changed)
		r.changed = nil
	}
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

// Store atomically swaps the current snapshot, and then closes the
// channel Changed handed out. Nil is ignored.
func (r *RuntimeConfig) Store(cfg *Config) {
	if r == nil || cfg == nil {
		return
	}
	r.ptr.Store(cfg)
	r.notifyChanged()
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
// The save is a turn of the config file's lock (LockFile), shared with
// every writer outside the process (backlog B506), taken before the clone
// and before fn runs, and in it the file is read again: the snapshot was
// read before the turn, so it is changed and saved only over a file that
// still holds the bytes this holder last read (RecordFile) or wrote, and
// the save compares once more just before its rename (Save's own
// comparison). Over a file another writer changed since (a library add,
// init, a restore or an edit beside the running bridge), nothing is saved
// or stored and a *ChangedOutsideError says so: the file keeps that
// writer's change, which the bridge loads when it next starts, and nothing
// reloads it now. A lock another writer holds past LockWait is a
// *BusyError. Either refusal comes before fn runs, so a caller that changes
// something besides the config in fn (the console's root changes change
// the library index there) changes nothing when the save is refused before
// it; only the comparison just before the rename, a writer that takes no
// turns landing in the save's own staging, refuses after fn has run.
//
// When fn returns an error the clone is discarded: nothing is saved
// or stored, and fn's error is returned verbatim. A Save failure
// likewise leaves both the live snapshot and the on-disk file
// untouched. fn runs under the write lock and in the file's turn, so it
// must not call Update re-entrantly (the mutex is not re-entrant), must
// not retain the clone after returning, and holds every other writer of
// the file off while it runs.
func (r *RuntimeConfig) Update(path string, fn func(*Config) error) error {
	if r == nil {
		return errors.New("config: Update on nil RuntimeConfig")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Load() == nil {
		return errors.New("config: Update with no config loaded")
	}
	l, err := LockFile(path)
	if err != nil {
		return err
	}
	defer l.Unlock()
	if r.fileKnown && (l.absent || !bytes.Equal(l.found, r.file)) {
		return &ChangedOutsideError{Path: path}
	}
	next := Clone(r.Load())
	if err := fn(next); err != nil {
		return err
	}
	written, err := next.save(l, nil)
	if errors.Is(err, ErrChangedInTurn) {
		return &ChangedOutsideError{Path: path}
	}
	if err != nil {
		return err
	}
	r.file, r.fileKnown = written, true
	r.Store(next)
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
	if cfg.Deployment.ManagedSettings != nil {
		out.Deployment.ManagedSettings = append([]string(nil), cfg.Deployment.ManagedSettings...)
	}
	if cfg.Deployment.ManagedControls != nil {
		out.Deployment.ManagedControls = append([]string(nil), cfg.Deployment.ManagedControls...)
	}
	if cfg.Metrics.AllowCIDRs != nil {
		out.Metrics.AllowCIDRs = append([]string(nil), cfg.Metrics.AllowCIDRs...)
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
	if cfg.Limits.Write.RequestsPerMinute != nil {
		v := *cfg.Limits.Write.RequestsPerMinute
		out.Limits.Write.RequestsPerMinute = &v
	}
	if cfg.Limits.Write.Burst != nil {
		v := *cfg.Limits.Write.Burst
		out.Limits.Write.Burst = &v
	}
	if cfg.Limits.Search.RequestsPerMinute != nil {
		v := *cfg.Limits.Search.RequestsPerMinute
		out.Limits.Search.RequestsPerMinute = &v
	}
	if cfg.Limits.Search.Burst != nil {
		v := *cfg.Limits.Search.Burst
		out.Limits.Search.Burst = &v
	}
	if cfg.Integrity.VariantSweepIntervalSec != nil {
		v := *cfg.Integrity.VariantSweepIntervalSec
		out.Integrity.VariantSweepIntervalSec = &v
	}
	if cfg.Integrity.OrphanSidecarSweepIntervalSec != nil {
		v := *cfg.Integrity.OrphanSidecarSweepIntervalSec
		out.Integrity.OrphanSidecarSweepIntervalSec = &v
	}
	if cfg.Integrity.VariantSweepMaxDeletePercent != nil {
		v := *cfg.Integrity.VariantSweepMaxDeletePercent
		out.Integrity.VariantSweepMaxDeletePercent = &v
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
	if cfg.SmartPlaylists.Enabled != nil {
		v := *cfg.SmartPlaylists.Enabled
		out.SmartPlaylists.Enabled = &v
	}
	if cfg.Output.LoopbackPort != nil {
		v := *cfg.Output.LoopbackPort
		out.Output.LoopbackPort = &v
	}
	out.Output.Player = cfg.Output.Player.clone()
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
