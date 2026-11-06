package main

import (
	"context"
	"errors"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// playerDSD is serve's player's DSD state (B728 B6; the design's sections 2.5
// and 3.6): the one dsdstate.Store the player's supervisor opens, at the
// player's start, and the migration of the legacy keys (dsdMode, allowDoP,
// dsdQualification) into it. Production still authorizes DSD from the keys,
// through the legacy reader, until the cutover (B7): nothing plays by the
// state yet.
//
// It is the supervisor's alone, and every step it makes runs on the
// supervisor's goroutine: the store's open and the part of the bridge's at
// the start, before the player's identity, its backend and its engine; the
// part of a DAC's at a snapshot of the watcher's that resolves the boot
// binding, before that snapshot's report reaches the engine, so the output
// is never available on a DAC whose part is still to be made for it, and a
// DAC found after the start (the watcher reads the box on its own time) gets
// its part then. So the steps are made one at a time, the start's first,
// while the player holds its share of the data directory's lock, which serve
// took before anything here ran (dataDirHold), and none starts once the
// player's stop has begun: a step under way finishes its write, which nothing
// interrupts, and the share is let go only once the supervisor has returned.
// No step takes bridge.yaml's lock or writes the config: the keys are the boot
// snapshot's.
//
// The migration runs once: whenever the state holds no marker, the keys
// setting any, and again after the state is deleted (dsdstate.Migrate);
// while the marker says its part for a DAC waits (bound_dac, record_model),
// each new generation of the watcher's that resolves the boot binding asks it
// again, with the keys the marker captured at the first start, a later start
// included (dsdstate.RecordToRead), never the ones read now.
type playerDSD struct {
	dataDir string
	legacy  dsdstate.Legacy
	// boot is the binding the player started with: the DAC a part of a DAC's
	// goes to is the one it resolves to.
	boot  config.PlayerDevice
	sysfs alsa.SysfsReader
	seams *servePlayerSeams
	// store is nil when it could not be opened.
	store *dsdstate.Store
}

// legacyDSD is the legacy keys as p holds them, the boot snapshot's: dsdMode
// "" where unset, which EffectiveDSDMode maps to off with an explicit off, a
// difference the migration keeps.
func legacyDSD(p config.PlayerConfig) dsdstate.Legacy {
	l := dsdstate.Legacy{DSDMode: p.DSDMode, DSDQualification: p.DSDQualification}
	if p.AllowDoP != nil {
		v := *p.AllowDoP
		l.AllowDoP = &v
	}
	return l
}

// openPlayerDSD opens the player's DSD state in dataDir, the data directory
// serve holds the lock of, and says what it found there when that is not a
// file that read or no file: a damaged file set aside, or one left unread,
// over which nothing is migrated.
func openPlayerDSD(dataDir string, player config.PlayerConfig, trees outputTrees, seams *servePlayerSeams) *playerDSD {
	d := &playerDSD{dataDir: dataDir, legacy: legacyDSD(player), boot: player.Device, sysfs: trees.observer().Sysfs, seams: seams}
	store, err := dsdstate.Open(dataDir)
	if err != nil {
		playerLog.Warn("player: the player's DSD state is not opened, and nothing is migrated into it", "err", err)
		return d
	}
	d.store = store
	switch rep := store.Report(); rep.Outcome {
	case dsdstate.Damaged:
		if rep.Kept != nil {
			playerLog.Warn("player: the player's DSD state did not read: its bytes are kept aside, the fresh state that took its place in memory could not be written over them, and nothing is migrated",
				"why", rep.Why, "aside", rep.Aside, "kept", rep.Kept)
			break
		}
		playerLog.Warn("player: the player's DSD state did not read: its bytes are kept aside, a fresh state took its place, and nothing is migrated over it",
			"why", rep.Why, "aside", rep.Aside)
	case dsdstate.Unread:
		playerLog.Warn("player: the player's DSD state could not be read and stays where it is: nothing is migrated over it, and no write replaces it until it is moved",
			"why", rep.Why, "kept", rep.Kept)
	}
	return d
}

// migrate makes one step of the migration (dsdstate.Store.Migrate) for
// bound, the DAC the boot binding resolves to now (nil for the start's step,
// the part of the bridge's), unless the player's stop has begun, and says
// what the step did.
func (d *playerDSD) migrate(ctx context.Context, bound *dsdstate.Bound) {
	if d == nil || d.store == nil || !d.steps(bound) {
		return
	}
	if d.seams != nil && d.seams.migrates != nil {
		d.seams.migrates(ctx, bound)
	}
	if ctx.Err() != nil {
		return
	}
	step, err := d.store.Migrate(d.dataDir, d.legacy, bound)
	d.say(step, err, bound)
}

// say logs what a step did: a write refused or failed as one Warn line, which
// claims neither that the state is as it was nor that it is durable, since a
// write that failed may have left its bytes in the file (dsdstate.Store's
// Update), and the next start reads the file as it finds it; otherwise each
// warning the step wrote into the state, a Warn line beside it, and what the
// step migrated.
func (d *playerDSD) say(step dsdstate.Step, err error, bound *dsdstate.Bound) {
	if err != nil {
		what := "player: the migration of the legacy DSD keys was not saved, and the player starts without it: the next start goes on from what the state's file holds then"
		if errors.Is(err, atomicwrite.ErrLanded) {
			what = "player: the migration of the legacy DSD keys reached the state's file and is not known to be durable, and the player starts without it: the next start goes on from what the file holds then"
		}
		playerLog.Warn(what, "err", err)
		return
	}
	for _, w := range step.Warnings {
		attrs := []any{"code", w.Code}
		if w.Reason != "" {
			attrs = append(attrs, "reason", w.Reason)
		}
		if w.DAC != nil {
			attrs = append(attrs, "dac", dacLabel(*w.DAC))
		}
		playerLog.Warn("player: the DSD migration recorded a warning in the player's DSD state", attrs...)
	}
	if !step.Changed {
		return
	}
	st := d.store.State()
	attrs := []any{"defaultMode", st.DefaultMode}
	if bound != nil {
		attrs = append(attrs, "dac", dacLabel(bound.Key))
		if mode, _, own := st.ChoiceFor(bound.Key); own {
			attrs = append(attrs, "choice", mode)
		}
	}
	if m := st.MigratedFrom; m != nil && m.WaitingFor != "" {
		attrs = append(attrs, "waitingFor", m.WaitingFor)
	}
	playerLog.Info("player: the legacy DSD keys were migrated into the player's DSD state", attrs...)
}

// steps reports whether a step for bound would make anything of the state as
// it holds now: at the start (bound nil), a migration never run while the
// keys set any; for a DAC, its part waiting. Nothing else asks the store, so
// a state that did not read, whose marker no migration runs over, and a
// migration done write nothing and say nothing.
func (d *playerDSD) steps(bound *dsdstate.Bound) bool {
	m := d.store.State().MigratedFrom
	if m == nil {
		l := d.legacy
		return l.DSDMode != "" || l.AllowDoP != nil || l.DSDQualification != ""
	}
	return bound != nil && m.From == dsdstate.FromKeys && m.WaitingFor != ""
}

// waits reports whether the migration's part for a DAC waits, as the state
// holds it: a start whose own step was not saved holds no marker, and the
// next start reads what the file holds then.
func (d *playerDSD) waits() bool {
	if d == nil || d.store == nil {
		return false
	}
	m := d.store.State().MigratedFrom
	return m != nil && m.From == dsdstate.FromKeys && m.WaitingFor != ""
}

// resolve asks the migration's part for a DAC of snap, a snapshot of the
// watcher's, while the migration waits for one: when snap is a good reading
// (not stale) of the boot binding and resolves it to its DAC, available. The
// DAC's model and identity are read again from the box (boundOf), and a
// reading that cannot be had, or is of another enumeration than snap's, asks
// nothing: the watcher's next snapshot asks again.
func (d *playerDSD) resolve(ctx context.Context, snap hotplug.Snapshot) {
	if !d.waits() || !snap.Valid() {
		return
	}
	if _, stale := snap.Stale(); stale || !hotplug.SameBinding(snap.Binding(), d.boot) {
		return
	}
	r := snap.Resolution()
	if r.State != binding.StateAvailable {
		return
	}
	bound, err := d.boundOf(r)
	if err != nil {
		playerLog.Info("player: the DSD migration waits for the bound DAC: its model could not be read", "err", err)
		return
	}
	d.migrate(ctx, &bound)
}

// boundOf is the DAC r resolves to, as the migration asks of it
// (dsdstate.BoundOf): its card's identity and its descriptors, read again from
// the box, and only while both are of the unit and enumeration r names.
func (d *playerDSD) boundOf(r binding.Resolution) (dsdstate.Bound, error) {
	card, ok := d.sysfs.Card(r.Card)
	if !ok || card.Kind != alsa.IdentityUSB || card.USB == nil || !binding.SameUnit(*card.USB, r.Identity) || card.USB.DevNum != r.Identity.DevNum {
		return dsdstate.Bound{}, errors.New("the bound card is no longer the unit the watcher resolved")
	}
	q, err := d.sysfs.Qualification(r.Card)
	if err != nil {
		return dsdstate.Bound{}, err
	}
	if q.Port != r.Identity.Port || q.DevNum != r.Identity.DevNum || q.Interface != r.Identity.Interface {
		return dsdstate.Bound{}, errors.New("the bound card's descriptors are of another unit than the watcher resolved")
	}
	return dsdstate.BoundOf(card, q, r.Endpoint.Device, r.Endpoint.Subdevice)
}

// dacLabel names a DAC model for a log line: its USB IDs and its product
// string, which its key holds and nothing else does (no serial, no port).
func dacLabel(k dacprofile.Key) string {
	label := usbid.Hex16(k.VendorID()) + ":" + usbid.Hex16(k.ProductID())
	if p, ok := k.Product(); ok {
		label += " " + p
	}
	return label
}
