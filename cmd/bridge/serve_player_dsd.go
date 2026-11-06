package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// The player's DSD in serve and bridge output play since B728's cutover
// (B7): the backend admits DSD by the effective DSD policy over the player's
// DSD state (playerDSD's store), the profile database and what the box
// establishes of the bound DAC now (dsdPolicyFor), and reads no approval
// record: dsdQualification is read once, by the migration, and never again.

// kernelReleaseBytes bounds the kernel release read (uname -r).
const kernelReleaseBytes = 256

// kernelRelease is the running kernel's release, read from the proc tree
// the player reads (sys/kernel/osrelease), bounded and opened as a file
// alone; "" when it does not read, under which no driver acceptance is
// recorded or counted.
func kernelRelease(trees outputTrees) string {
	f, _, err := fsutil.OpenAsFile(filepath.Join(trees.proc, "sys", "kernel", "osrelease"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, kernelReleaseBytes+1))
	if err != nil || len(b) > kernelReleaseBytes {
		return ""
	}
	v := strings.TrimSpace(string(b))
	if strings.ContainsFunc(v, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return ""
	}
	return v
}

// boundFromFeed is the DAC the boot binding resolves to now, from the
// watcher's latest snapshot when it is of the boot binding, not stale, and
// available (playback.DSDPolicy.Bound): a stale reading is no ground for a
// set (the design's section 3.1). The enumeration is the feed's instance and
// the unit's port and device number, which a replug changes, kept in memory
// alone.
func boundFromFeed(feed *hotplug.Feed, boot config.PlayerDevice) func() (playback.BoundDAC, bool) {
	return func() (playback.BoundDAC, bool) {
		if feed == nil {
			return playback.BoundDAC{}, false
		}
		snap, ok := feed.Latest()
		if !ok || !snap.Valid() || !hotplug.SameBinding(snap.Binding(), boot) {
			return playback.BoundDAC{}, false
		}
		if _, stale := snap.Stale(); stale {
			return playback.BoundDAC{}, false
		}
		r := snap.Resolution()
		if r.State != binding.StateAvailable {
			return playback.BoundDAC{}, false
		}
		return playback.BoundDAC{Card: r.Card, Device: r.Endpoint.Device, Subdevice: r.Endpoint.Subdevice, Identity: r.Identity,
			Enumeration: fmt.Sprintf("%s/%s/%d", snap.Instance(), r.Identity.Port, r.Identity.DevNum)}, true
	}
}

// probeConditions is the silent probe's conditions as a snapshot of the
// watcher's gives them (playback.ProbeConditions): the boot binding resolved
// to its DAC, available, and the reading fresh.
func probeConditions(snap hotplug.Snapshot, boot config.PlayerDevice) playback.ProbeConditions {
	if !snap.Valid() || !hotplug.SameBinding(snap.Binding(), boot) {
		return playback.ProbeConditions{}
	}
	_, stale := snap.Stale()
	return playback.ProbeConditions{Available: snap.Resolution().State == binding.StateAvailable, Fresh: !stale}
}

// dsdPolicyFor is the effective DSD policy the player's backend admits DSD
// by (B728 B7): d's store, the reviewed profile database embedded in the
// binary, the running kernel's release, the boot binding's DAC as the
// watcher's feed resolves it, and the box read through trees, the card's
// controls through controls. A store that did not open refuses DSD at every
// rate; a profile database that does not load knows no DAC, which the log
// says.
func dsdPolicyFor(d *playerDSD, feed *hotplug.Feed, boot config.PlayerDevice, trees outputTrees, controls alsactl.Reader) *playback.DSDPolicy {
	p := &playback.DSDPolicy{Kernel: kernelRelease(trees), Bound: boundFromFeed(feed, boot), Probes: alsapcm.NewProbeCache(), Log: playerLog,
		Box: playback.HostBox{Sysfs: alsa.SysfsReader{Root: trees.sysfs}, Proc: alsa.Reader{ProcRoot: trees.proc}, Controls: controls}}
	if d != nil && d.store != nil {
		p.Store = d.store
	}
	db, err := dacprofile.Default()
	if err != nil {
		playerLog.Warn("player: the DAC profile database does not load: the player knows no DAC by its profile", "err", err)
	} else {
		p.Profiles = db
	}
	return p
}

// playerDSD is serve's player's DSD state (B728 B6; the design's sections 2.5
// and 3.6): the one dsdstate.Store the player's supervisor opens, at the
// player's start, and the migration of the legacy keys (dsdMode, allowDoP,
// dsdQualification) into it. Since the cutover (B7) the player plays DSD by
// the effective policy over this state (dsdPolicyFor), and the keys and the
// owner's record are read by the migration alone. bridge output play keeps
// one of its own in its own state directory, reading the record named in
// serve's data directory and never serve's state (recordDir).
//
// It is the supervisor's alone, and every step it makes runs on the
// supervisor's goroutine: the store's open and the part of the bridge's at
// the start, before the player's identity, its backend and its engine, and
// neither once the player's stop has begun; the part of a DAC's at a snapshot
// of the watcher's that resolves the boot binding, before that snapshot's
// report reaches the engine, so the output is not available on a DAC whose
// part could be made first, and a DAC found after the start (the watcher
// reads the box on its own time) gets its part then. A part that cannot be
// made then, the DAC's descriptors not read or its write refused before it
// landed, holds the report back no further: the supervisor's policy tick asks
// it again, bounded (ask), since a DAC that stays as it was gives the watcher
// nothing new to publish. So the steps are made one at a time, the start's
// first, while the player holds its share of the data directory's lock, which
// serve took before anything here ran (dataDirHold), and none starts once the
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
	// recordDir is the directory the owner's record dsdQualification names
	// is read from (dsdstate.Store.Migrate's stateDir): serve's data
	// directory, which is dataDir in serve, and bridge output play's reading
	// of it beside a state of its own.
	recordDir string
	legacy    dsdstate.Legacy
	// boot is the binding the player started with: the DAC a part of a DAC's
	// goes to is the one it resolves to.
	boot  config.PlayerDevice
	sysfs alsa.SysfsReader
	seams *servePlayerSeams
	// store is nil when it could not be opened.
	store *dsdstate.Store
	// again is the resolution a part for a DAC is asked again for, on the
	// supervisor's policy tick (retry), nil while none is; tries counts the
	// times it was asked again, and wait the ticks until the next.
	again       *binding.Resolution
	tries, wait int
	// done says this run makes no step more: a write landed and is not known
	// to be durable, which the store does not hold in memory (dsdstate's
	// Store.Update), so a step now would be made from the state as it was
	// before that write, over the file that holds it; the next start goes on
	// from the file. The store fences every writer alike since B7
	// (dsdstate.ErrLandedNotHeld), the migration's and playback's evidence.
	done bool
	// changed, when set, is told of a DAC's part that changed the state, the
	// DAC's key: the player's backend probes the DAC anew and reads its
	// policy again, as after a choice written for it (B728 B7).
	changed func(dacprofile.Key)
}

// The bounds of asking again a part for a DAC that could not be made (ask):
// at the policy tick after the first try (100 ms), then twice as many ticks
// after each, at most dsdRetryTicksMax apart and dsdRetries times in all,
// about 80 s: room for a read or a write that fails for a moment, never a
// loop.
const (
	dsdRetries       = 10
	dsdRetryTicksMax = 300
)

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
	d := &playerDSD{dataDir: dataDir, recordDir: dataDir, legacy: legacyDSD(player), boot: player.Device, sysfs: trees.observer().Sysfs, seams: seams}
	store, err := dsdstate.Open(dataDir)
	if err != nil {
		playerLog.Warn("player: the player's DSD state is not opened, and nothing is migrated into it", "err", err)
		return d
	}
	d.store = store
	switch rep := store.Report(); rep.Outcome {
	case dsdstate.Damaged:
		switch {
		case rep.Kept == nil:
			playerLog.Warn("player: the player's DSD state did not read: its bytes are kept aside, a fresh state took its place, and nothing is migrated over it",
				"why", rep.Why, "aside", rep.Aside)
		case errors.Is(rep.Kept, atomicwrite.ErrLanded):
			// The replacement's rename landed and the barrier after it failed:
			// the file holds the fresh state, which is not known to be durable.
			playerLog.Warn("player: the player's DSD state did not read: its bytes are kept aside, a fresh state took its place in the file and is not known to be durable, and nothing is migrated over it",
				"why", rep.Why, "aside", rep.Aside, "kept", rep.Kept)
		default:
			playerLog.Warn("player: the player's DSD state did not read: its bytes are kept aside, the fresh state that took its place in memory could not be written over them, and nothing is migrated",
				"why", rep.Why, "aside", rep.Aside, "kept", rep.Kept)
		}
	case dsdstate.Unread:
		playerLog.Warn("player: the player's DSD state could not be read and stays where it is: nothing is migrated over it, and no write replaces it until it is moved",
			"why", rep.Why, "kept", rep.Kept)
	}
	return d
}

// migrate makes one step of the migration (dsdstate.Store.Migrate) for
// bound, the DAC the boot binding resolves to now (nil for the start's step,
// the part of the bridge's), unless the player's stop has begun, says what
// the step did, and returns its error: nil when no step was made, or the step
// was saved. A write that landed and is not known to be durable ends the
// run's steps (done).
func (d *playerDSD) migrate(ctx context.Context, bound *dsdstate.Bound) error {
	if d == nil || d.store == nil || !d.steps(bound) {
		return nil
	}
	if d.seams != nil && d.seams.migrates != nil {
		d.seams.migrates(ctx, bound)
	}
	if ctx.Err() != nil {
		return nil
	}
	step, err := d.store.Migrate(d.recordDir, d.legacy, bound)
	if errors.Is(err, atomicwrite.ErrLanded) || errors.Is(err, dsdstate.ErrLandedNotHeld) {
		d.done = true
	}
	d.say(step, err, bound)
	if err == nil && step.Changed && bound != nil && d.changed != nil {
		d.changed(bound.Key)
	}
	return err
}

// say logs what a step did, in words that claim neither the state as it was
// nor a durable write, since a write that failed may have left its bytes in
// the file (dsdstate's Store.Update), and a start reads the file as it finds
// it. A write that landed and is not known to be durable
// (atomicwrite.ErrLanded) is one Warn line, for the start's step or a DAC's
// part, which this run asks no more, and the warnings the step wrote, which
// the file holds, each a Warn line beside it. One refused or failed before it
// landed is one Warn line for the start's step, which the next start makes
// again, and for a DAC's part the lines ask writes as it asks the part again
// in this run. A step saved logs each warning it wrote, a Warn line beside
// it, and what it migrated.
func (d *playerDSD) say(step dsdstate.Step, err error, bound *dsdstate.Bound) {
	landed := errors.Is(err, atomicwrite.ErrLanded)
	switch {
	case errors.Is(err, dsdstate.ErrLandedNotHeld):
		// The store's fence (B7): an earlier write, the migration's or
		// playback's, landed and is not known to be durable, and no write is
		// made from the state the store holds.
		playerLog.Warn("player: the DSD migration makes no step more in this run: an earlier write of the player's DSD state reached its file and is not known to be durable, which the store does not hold; the next start goes on from what the file holds then")
		return
	case err != nil && !landed && bound != nil:
		return
	case err != nil && !landed:
		playerLog.Warn("player: the migration of the legacy DSD keys was not saved, and the player starts without it: this run does not ask it again, and the next start goes on from what the state's file holds then",
			"err", err)
		return
	case landed && bound != nil:
		playerLog.Warn("player: the DSD migration's part for the bound DAC reached the state's file and is not known to be durable: the player plays on, this run asks it no more, and the next start goes on from what the file holds then",
			"dac", dacLabel(bound.Key), "err", err)
	case landed:
		playerLog.Warn("player: the migration of the legacy DSD keys reached the state's file and is not known to be durable, and the player starts without it: this run asks it no more, and the next start goes on from what the file holds then",
			"err", err)
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
	if err != nil || !step.Changed {
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
// keys set any; for a DAC, its part waiting; never once a write landed that
// the store does not hold (done). Nothing else asks the store, so a state
// that did not read, whose marker no migration runs over, and a migration
// done write nothing and say nothing.
func (d *playerDSD) steps(bound *dsdstate.Bound) bool {
	if d.done {
		return false
	}
	m := d.store.State().MigratedFrom
	if m == nil {
		l := d.legacy
		return l.DSDMode != "" || l.AllowDoP != nil || l.DSDQualification != ""
	}
	return bound != nil && m.From == dsdstate.FromKeys && m.WaitingFor != ""
}

// waits reports whether the migration's part for a DAC waits, as the state
// holds it: a start whose own step was not saved holds no marker, and the
// next start reads what the file holds then; and never once a write landed
// that the store does not hold (done), for which the next start does too.
func (d *playerDSD) waits() bool {
	if d == nil || d.store == nil || d.done {
		return false
	}
	m := d.store.State().MigratedFrom
	return m != nil && m.From == dsdstate.FromKeys && m.WaitingFor != ""
}

// resolve asks the migration's part for a DAC of snap, a snapshot of the
// watcher's, while the migration waits for one: when snap is a good reading
// (not stale) of the boot binding and resolves it to its DAC, available. A
// snapshot ends whatever was being asked again, and asks afresh.
func (d *playerDSD) resolve(ctx context.Context, snap hotplug.Snapshot) {
	if d == nil {
		return
	}
	d.again = nil
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
	d.tries = 0
	d.ask(ctx, r)
}

// retry, at the supervisor's policy tick, asks again a part for a DAC that
// could not be made, once its wait has passed (ask), while the migration
// still waits for it.
func (d *playerDSD) retry(ctx context.Context) {
	if d == nil || d.again == nil {
		return
	}
	if !d.waits() {
		d.again = nil
		return
	}
	if d.wait--; d.wait > 0 {
		return
	}
	d.ask(ctx, *d.again)
}

// ask asks the part for the DAC r resolves to, its model and identity read
// again from the box (boundOf). One that cannot be made, the DAC's
// descriptors not read, or of another enumeration than r's, or its write
// refused before it landed, is asked again at the supervisor's policy tick,
// the first time at the next, then twice as many ticks after each try, at
// most dsdRetries times (retry). It says so at the first try that fails, and
// once it asks no more, when the watcher's next reading of the DAC asks it
// again, and so does the next start, where the marker still waits.
func (d *playerDSD) ask(ctx context.Context, r binding.Resolution) {
	d.again = nil
	bound, err := d.boundOf(r)
	read := err == nil
	if read {
		if err = d.migrate(ctx, &bound); err == nil || errors.Is(err, atomicwrite.ErrLanded) || errors.Is(err, dsdstate.ErrLandedNotHeld) || ctx.Err() != nil {
			return
		}
	}
	switch {
	case d.tries == dsdRetries:
		playerLog.Warn("player: the DSD migration's part for the bound DAC was not made, and this run asks it no more until the watcher reads the DAC again: the next start asks it too",
			"err", err)
		return
	case d.tries == 0 && read:
		playerLog.Warn("player: the DSD migration's part for the bound DAC was not saved, and the player plays on without it: it is asked again in this run",
			"dac", dacLabel(bound.Key), "err", err)
	case d.tries == 0:
		playerLog.Info("player: the DSD migration's part for the bound DAC waits: its model could not be read, and it is asked again in this run",
			"err", err)
	}
	d.tries++
	d.wait = min(1<<(d.tries-1), dsdRetryTicksMax)
	d.again = &r
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
