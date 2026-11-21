package config

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
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
	// file is the config file's bytes as this holder last recorded them:
	// read for the live snapshot (RecordFile), found by a turn whose bytes
	// differ, whatever its save then does (compareTurn, backlog B535), or
	// written by a save (Update); fileKnown whether it has any; target is
	// that file, as a turn resolves its name (the target of a link at its
	// last element), and unresolved that serve's boot read could resolve the
	// name at none of its reads, so that there is no file the holder may save
	// into (target is never "" once fileKnown, unless unresolved). All under
	// mu.
	file       []byte
	fileKnown  bool
	target     string
	unresolved bool
	// outside is the yaml path of each setting another writer changed in the
	// file since the holder's snapshot was read, sorted (backlog B535): every
	// save writes these as the file holds them, and refuses a change of one
	// of its own. The turn that finds one records it, with the bytes it found
	// it in (file), whatever its save then does, and nothing but a restart,
	// a new holder, forgets one. Under mu.
	outside []string
	// unsaid are the settings of outside recorded at a turn that found a file
	// the next start would not load (leftErr), which no line said then: the
	// turn that next finds the file loading says each once, and empties it
	// (backlog B974). Under mu.
	unsaid []string
	// outsideEnv is the same record for the settings a BRIDGE_* variable set
	// at the start (Config.envSet), sorted: B761's rule writes them
	// (filedEnvValues, filedSwitches), so they serve the conflict check alone,
	// never fileOutside, the deployment refusal or the log line saying a
	// change takes effect at the next start, which the variable makes false
	// (B535's round 2). Under mu.
	outsideEnv []string
	// leftErr is why the file another writer left, the bytes file holds once
	// a turn has found that writer's change, would not load as the next start
	// would load it, with this process's environment (loadsAtNextStart), nil
	// where it would: every save is then refused (OutsideLeftUnloadable),
	// since no save made that file. leftClashes is the setting of each
	// listener whose port that file's OpenHome surface sits on
	// (openHomeClashes), which a save may keep and may not add to. Both are
	// taken once, at the turn that finds the bytes, and again from the merged
	// file a save writes; never per save (B535's rounds 2 and 3). Under mu.
	leftErr     error
	leftClashes []string
	// changedMu guards changed, the channel the next Store closes
	// (Changed), nil until someone asks for it.
	changedMu sync.Mutex
	changed   chan struct{}
}

// ErrChangedOutside is what a *ChangedOutsideError matches.
var ErrChangedOutside = errors.New("config: the config file changed outside this holder")

// OutsideKind is what made the live holder refuse a save of the config file
// once another writer had changed it, or its name had moved
// (ChangedOutsideError.Kind). A change another writer made to a setting the
// save does not change refuses nothing on its own (backlog B535): the save
// writes it as the file holds it.
type OutsideKind int

// The refusals of a *ChangedOutsideError.
const (
	// OutsideInTurn: another writer changed the file while the save had its
	// turn (a writer that takes no turns: an edit, a bridge older than the
	// lock), as the save's comparison before its rename or a check found.
	// A save made again merges it.
	OutsideInTurn OutsideKind = iota
	// OutsideRemoved: the file the holder read is gone.
	OutsideRemoved
	// OutsideRepointed: the config's name opens another file than the one
	// the holder read (a link repointed), at the turn or within it.
	OutsideRepointed
	// OutsideUndecodable: the file another writer left does not decode as
	// Load decodes it.
	OutsideUndecodable
	// OutsideDeployment: another writer changed a deployment setting, which
	// decides what the console may change: every save is refused until a
	// restart.
	OutsideDeployment
	// OutsideConflict: the save changes a setting another writer changed,
	// from a value it never read.
	OutsideConflict
	// OutsideUnloadable: the file the save would write, the settings
	// another writer changed beside the save's, would not load as it is at
	// the next start, where the file that writer left would: the save's own
	// change makes it so (a new OpenHome port clash among them).
	OutsideUnloadable
	// OutsideLeftUnloadable: the file another writer left would not load as
	// it is at the next start, whatever the save changes: the file is to be
	// corrected, since a restart would fail on it (B535's round 2).
	OutsideLeftUnloadable
)

// ChangedOutsideError is the live holder's refusal of a save over a config
// file another writer changed since the holder last recorded its bytes, of one
// gone, or of one whose name now opens another file than the one the holder
// read, a link repointed; Kind says which. It names the file, the settings it
// is about by their yaml paths, and nothing the file holds.
type ChangedOutsideError struct {
	// Path is the config file.
	Path string
	// Kind is what refused the save.
	Kind OutsideKind
	// Settings are the yaml paths of the settings it is about, sorted: the
	// settings another writer changed that the save changes too (a
	// conflict), the deployment settings another writer changed, and every
	// setting another writer changed beside a file the save would leave that
	// the next start would refuse.
	Settings []string
	// Err is why the next start would refuse the file the save would write
	// (OutsideUnloadable) or the one another writer left
	// (OutsideLeftUnloadable), as Load says it, which names no value.
	Err error
}

func (e *ChangedOutsideError) Error() string {
	settings, them := strings.Join(e.Settings, ", "), "it"
	if len(e.Settings) > 1 {
		them = "them"
	}
	const restart = "restart the bridge to load the file, then make the change again"
	switch e.Kind {
	case OutsideRemoved:
		return fmt.Sprintf("%s was removed since this bridge read it, so this change was not saved: put the file back, "+
			"then %s", e.Path, restart)
	case OutsideRepointed:
		return fmt.Sprintf("%s now opens another file than the one this bridge read (a link at it repointed), so this "+
			"change was not saved: %s", e.Path, restart)
	case OutsideUndecodable:
		return fmt.Sprintf("%s does not read as a bridge config as another writer left it, so this change was not saved: "+
			"correct the file, then %s", e.Path, restart)
	case OutsideDeployment:
		return fmt.Sprintf("another writer changed the deployment settings in %s (%s), which decide what this console may "+
			"change, so this change was not saved, and the file keeps what that writer left: %s", e.Path, settings, restart)
	case OutsideConflict:
		return fmt.Sprintf("another writer changed %s in %s since this bridge read it, and this change changes %s too, so "+
			"it was not saved, and the file keeps what that writer left: %s", settings, e.Path, them, restart)
	case OutsideUnloadable:
		return fmt.Sprintf("this change, beside the settings another writer changed in %s (%s), would leave a file this "+
			"bridge would not load at its next start (%v), so it was not saved, and the file keeps what that writer left: %s",
			e.Path, settings, e.Err, restart)
	case OutsideLeftUnloadable:
		return fmt.Sprintf("%s, as another writer left it, would not load at this bridge's next start (%v), so this change "+
			"was not saved: correct the file", e.Path, e.Err)
	}
	return fmt.Sprintf("%s was changed by another writer while this save had its turn at it (an edit, or a bridge "+
		"older than the config's lock), so this change was not saved, and the file keeps what that writer left: make the "+
		"change again, or restart the bridge to load the file first", e.Path)
}

// Is makes a *ChangedOutsideError match ErrChangedOutside.
func (e *ChangedOutsideError) Is(target error) bool { return target == ErrChangedOutside }

// ErrBootUnresolved is what a *BootUnresolvedError matches.
var ErrBootUnresolved = errors.New("config: serve's boot read could not resolve the config file's name")

// BootUnresolvedError is the holder's refusal of a save once serve's boot
// read could resolve the config's name at none of its reads (LoadFileAt, a
// BootFile with no file): the holder cannot say which file its snapshot was
// read from, so it saves into none, and goes on refusing once the name
// resolves, since the file it resolves to then is not one it read. It names
// the config file and nothing it holds.
type BootUnresolvedError struct {
	// Path is the config file.
	Path string
}

func (e *BootUnresolvedError) Error() string {
	return fmt.Sprintf("this bridge could not resolve the file %s opens when it started, so it cannot say which file "+
		"its settings were read from, and this change was not saved: restart the bridge once that name resolves, "+
		"then make the change again", e.Path)
}

// Is makes a *BootUnresolvedError match ErrBootUnresolved.
func (e *BootUnresolvedError) Is(target error) bool { return target == ErrBootUnresolved }

// RecordFile tells the holder raw, the bytes the config file held when the
// live snapshot was read from it, and file, the file its name opened for that
// read, resolved before and after it (LoadFileAt): its first Update then
// saves only into that file, and only while it still holds them. file is
// taken as it is, never resolved again here: a link at the name repointed
// between the read and this call (a dotfiles restow, a home-manager switch
// during serve's boot) would otherwise have the holder record a file it never
// read. A name that opens another file than file at a save, the link
// repointed since, is refused by Update. A boot read that resolved the name
// at none of its reads is no file, never one named "": the holder then
// refuses every Update (*BootUnresolvedError), until a restart reads the
// file again. A holder told nothing saves its first Update over whatever the
// file holds, and records from then on.
func (r *RuntimeConfig) RecordFile(file BootFile, raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.file, r.fileKnown = append([]byte(nil), raw...), true
	r.target, r.unresolved = file.target, !file.resolved
	// The snapshot was read from raw: what was recorded against other bytes
	// is a record of another read (backlog B535).
	r.outside, r.unsaid, r.outsideEnv = nil, nil, nil
	r.leftErr, r.leftClashes = nil, nil
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
// read before the turn, so it is changed and saved only into the file this
// holder read (RecordFile: a name whose link was repointed since resolves to
// another file), and over a file another writer changed since (a library add,
// init, a restore or an edit beside the running bridge) it merges (backlog
// B535, compareTurn): the turn's bytes and the ones this holder last recorded
// (read at the boot or at a turn that found them, or written) are read as Load
// reads a file for its settings, every setting the two set differently is
// recorded, at the turn that finds it and until a restart, and every save
// writes each recorded setting as the turn's file holds it, beside its own
// change, but one a BRIDGE_* variable set at the start, which B761's rule
// writes, while the snapshot it publishes is its own: nothing of the other
// writer's goes live, and the bridge loads it when it next starts. Nothing is
// saved or stored, and a *ChangedOutsideError says why, for a file gone, a
// name the turn finds repointed, a file that does not decode, a recorded
// deployment setting, a file that writer left that the next start would
// refuse (loadsAtNextStart: to be corrected, a restart failing on it), a
// change of a recorded setting, a variable's included (the save made it from
// a value it never read), and a file the save would write that the next start
// would refuse where the one that writer left would not, an OpenHome port
// clash the save adds among them: the file keeps that writer's change, and
// nothing reloads it now. The save compares once more before each attempt at
// its rename (Save's own comparison), so a writer that takes no turns landing in
// the turn is refused too (a *ChangedOutsideError, OutsideInTurn), and the
// next save merges what it left. A name repointed while the save waited for
// the lock, at fn's check or while the save staged is a *ChangedOutsideError
// too, and nothing is tried again on the file the name opens now. A lock
// another writer holds past LockWait is a *BusyError. A holder whose boot read
// resolved the config's name at none of its reads (RecordFile of no file)
// cannot say which file its snapshot came from, and refuses every save with a
// *BootUnresolvedError before it takes a turn, until a restart reads the file
// again; a CLI writer's own turn, which resolves the name itself, is not the
// holder's and is not refused. Each refusal of the file comes before fn runs,
// so a caller that changes something besides the config in fn (the console's
// root changes change the library index there) changes nothing when the save
// is refused before it, and such a caller sets every setting its change makes
// on the clone first and asks the turn's check just before that change
// (UpdateChecked), which refuses a change of a recorded setting and a file the
// next start would refuse too; only a writer that takes no turns landing
// during that change itself, or the save's staging, is refused after it has
// run.
//
// When fn returns an error the clone is discarded: nothing is saved
// or stored, and fn's error is returned verbatim. A Save failure
// likewise leaves both the live snapshot and the on-disk file
// untouched. fn runs under the write lock and in the file's turn, so it
// must not call Update re-entrantly (the mutex is not re-entrant), must
// not retain the clone after returning, and holds every other writer of
// the file off while it runs.
func (r *RuntimeConfig) Update(path string, fn func(*Config) error) error {
	return r.UpdateChecked(path, func(next *Config, _ func() error) error { return fn(next) })
}

// UpdateChecked is Update, handing fn as well the turn's check, asked again
// when called (check): the file read again and its name resolved again
// (FileLock.Check), then, while settings another writer changed are recorded,
// a change next has made so far of one of them, and the file the save would
// write as the next start loads it (refuseMerge). fn calls it just before work
// of its own that a refusal of the save after it would leave done, every
// setting its change makes set on next first, as the console's root changes
// set the root list before their library index change, so that a file a
// writer that takes no turns changed since the turn read it, a change of a
// setting another writer changed, and a merged file the next start would
// refuse each refuse the change before that work. A check that refuses is a
// *ChangedOutsideError, which fn returns, and Update returns as it is. The
// change and the merged file are checked again once fn returns, before the
// save, for every writer, whether or not it asked check. No other console
// writer does work outside the clone in fn (the settings PATCH, the variants
// folder, the UPnP servers and the Player page change the clone alone, the
// Player page reading the box and the player): a writer that does takes the
// root changes' rule, its settings set on next and check asked before that
// work.
func (r *RuntimeConfig) UpdateChecked(path string, fn func(next *Config, check func() error) error) error {
	if r == nil {
		return errors.New("config: Update on nil RuntimeConfig")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Load() == nil {
		return errors.New("config: Update with no config loaded")
	}
	if r.unresolved {
		return &BootUnresolvedError{Path: path}
	}
	l, err := LockFile(path)
	if err != nil {
		return changedOutside(path, err)
	}
	defer l.Unlock()
	found, ok := l.Found()
	before := r.Load()
	// The turn's bytes compared with the ones this holder last read or
	// wrote: the settings another writer changed since are recorded, and a
	// file the save cannot merge into is refused before fn runs.
	if err := r.compareTurn(path, l.target, found, ok, before); err != nil {
		return err
	}
	next := Clone(before)
	check := func() error {
		if err := changedOutside(path, l.Check()); err != nil {
			return err
		}
		_, err := r.refuseMerge(path, before, next, found)
		return err
	}
	if err := fn(next, check); err != nil {
		return err
	}
	clashes, err := r.refuseMerge(path, before, next, found)
	if err != nil {
		return err
	}
	// The snapshot, with the three opt-outs, every other leaf a variable set
	// and every setting another writer changed as the file is to hold them;
	// the live values are what is published.
	saved := *next
	r.fileSaved(&saved, before, found)
	written, err := saved.save(l, nil)
	if err != nil {
		return changedOutside(path, err)
	}
	r.file, r.fileKnown, r.target = written, true, l.target
	// The file as this save left it, the one the next turn compares with: it
	// loads (a merged save's was loaded just now, with this process's
	// environment as the file another writer left is, and refused
	// otherwise), and its clashes are that load's, so the cache holds the
	// merged file's load; none where nothing was merged, which no save
	// reads before a turn that finds a change takes both again (B535's
	// round 3).
	r.leftErr, r.leftClashes = nil, clashes
	r.Store(next)
	return nil
}

// filedSwitches sets, in saved, a copy of an Update's next, the three
// opt-outs (switchEnvPaths: enrich.enabled, enrich.deezerEnabled,
// update.checkEnabled) as the file is to hold them, where next holds the live
// values, BRIDGE_ENRICH_ENABLED's, BRIDGE_ENRICH_DEEZER_ENABLED's or
// BRIDGE_UPDATE_CHECK_ENABLED's where set (backlog B617; the update check's
// since B761). A switch the update left as it was in before, the snapshot it
// cloned, is written as the file holds it (file, the bytes the turn read), so
// a save of any other setting writes no variable's value into the file, and
// a start without the variable finds what the operator chose; a switch the
// update changed is written as the change, off as false and on as no key.
// The snapshot published is next, the live values. Every other leaf a
// variable set is filedEnvValues' (B761), and every setting another writer
// changed fileOutside's (B535). Only these three fields of saved are set here.
func filedSwitches(saved, before *Config, file []byte) {
	var own struct {
		Enrich struct {
			Enabled       *bool `yaml:"enabled"`
			DeezerEnabled *bool `yaml:"deezerEnabled"`
		} `yaml:"enrich"`
		Update struct {
			CheckEnabled *bool `yaml:"checkEnabled"`
		} `yaml:"update"`
	}
	if err := yaml.Unmarshal(file, &own); err != nil {
		// The bytes loaded once (the holder's own read or write, or a file
		// it was told nothing of); one that does not decode keeps what the
		// update holds.
		return
	}
	saved.Enrich.Enabled = filedSwitch(saved.Enrich.EffectiveEnabled(), before.Enrich.EffectiveEnabled(), own.Enrich.Enabled)
	saved.Enrich.DeezerEnabled = filedSwitch(saved.Enrich.EffectiveDeezerEnabled(), before.Enrich.EffectiveDeezerEnabled(), own.Enrich.DeezerEnabled)
	saved.Update.CheckEnabled = filedSwitch(saved.Update.EffectiveCheckEnabled(), before.Update.EffectiveCheckEnabled(), own.Update.CheckEnabled)
}

// filedSwitch is one opt-out as an Update writes it: the file's own value
// where the update left the live one as it was, else the change, off as
// false and on as no key.
func filedSwitch(now, was bool, filed *bool) *bool {
	if now == was {
		return filed
	}
	if now {
		return nil
	}
	off := false
	return &off
}

// filedEnvValues sets, in saved, a copy of an Update's next, each leaf a
// BRIDGE_* variable set at the load the holder's snapshot came from
// (before's envSet), the three opt-outs aside (filedSwitches writes them), as
// the file is to hold it (backlog B761): where the update left the leaf at
// its live value, the variable's, compared by value (sameValue: an empty list
// is the same as none, which Clone makes of it), the file's own value, read
// from file, the bytes the turn found, by Load's own decode (DecodeFile), its
// yaml path recorded in saved.filed, so that the save writes the leaf as no
// key where that value is its zero value, whatever the field's tag (marshal):
// Load decodes the file into a zero config, so the zero value is what a file
// that writes no value for the leaf gives, and one that writes its zero
// value, which a start reads alike. Where the file does not decode, the live
// value is kept, as it is. So a save of any other setting writes no
// variable's value into the file, and no key the file did not write, and a
// start without the variable finds what the file said. A leaf the update
// changed is written as the change: saved, published, and set by the variable
// again at the next start. The snapshot published is next, the live values.
// Only those leaves of saved are set here, each by assignment to its own
// field, never through a pointer next holds.
func filedEnvValues(saved, before *Config, file []byte) {
	if len(before.envSet) == 0 {
		return
	}
	var own Config
	if err := DecodeFile(file, &own); err != nil && !errors.Is(err, io.EOF) {
		// The bytes loaded once (the holder's own read or write); a file the
		// holder was told nothing of that does not decode keeps what the
		// update holds.
		return
	}
	bindings := make(map[string]envBinding)
	for _, b := range envBindings() {
		bindings[b.YAMLPath] = b
	}
	for _, path := range before.envSet {
		b, ok := bindings[path]
		if !ok || switchEnvPaths[path] {
			continue
		}
		now := envLeaf(saved, b)
		if !sameValue(now, envLeaf(before, b)) {
			continue
		}
		now.Set(envLeaf(&own, b))
		saved.filed = append(saved.filed, path)
	}
}

// sameValue reports whether a and b, two values of one setting (a leaf
// envBindings names, or one settingsChanged compares), are one setting: a list
// by its entries, so that an empty list is the same as none (a BRIDGE_* list
// of separators alone loads as an empty list, Clone makes none of it, and Load
// reads `[]` as it reads no key), an entry the same way, a struct whose fields
// are all exported (an upstream server's entry) field by field, and any other
// value as reflect.DeepEqual compares it, a pointer by what it points to.
func sameValue(a, b reflect.Value) bool {
	switch a.Kind() {
	case reflect.Slice:
		if a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !sameValue(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Struct:
		if allExported(a.Type()) {
			for i := range a.NumField() {
				if !sameValue(a.Field(i), b.Field(i)) {
					return false
				}
			}
			return true
		}
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

// allExported reports whether every field of t, a struct type, is exported.
func allExported(t reflect.Type) bool {
	for i := range t.NumField() {
		if !t.Field(i).IsExported() {
			return false
		}
	}
	return true
}

// changedOutside is err, a turn's answer to the holder's save of the config
// file at path, as the holder says it: a file another writer changed in the
// turn (ErrChangedInTurn) and a name that opens another file than the turn
// locked (ErrNameMoved), whether found when the turn is taken, at a check or
// at the save's rename, are each a *ChangedOutsideError (OutsideInTurn,
// OutsideRepointed); any other error is returned as it is.
func changedOutside(path string, err error) error {
	switch {
	case errors.Is(err, ErrNameMoved):
		return &ChangedOutsideError{Path: path, Kind: OutsideRepointed}
	case errors.Is(err, ErrChangedInTurn):
		return &ChangedOutsideError{Path: path, Kind: OutsideInTurn}
	}
	return err
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
	if cfg.Enrich.Enabled != nil {
		v := *cfg.Enrich.Enabled
		out.Enrich.Enabled = &v
	}
	if cfg.Enrich.DeezerEnabled != nil {
		v := *cfg.Enrich.DeezerEnabled
		out.Enrich.DeezerEnabled = &v
	}
	if cfg.Update.CheckEnabled != nil {
		v := *cfg.Update.CheckEnabled
		out.Update.CheckEnabled = &v
	}
	if cfg.Output.LoopbackPort != nil {
		v := *cfg.Output.LoopbackPort
		out.Output.LoopbackPort = &v
	}
	out.Output.Player = cfg.Output.Player.clone()
	if cfg.envSet != nil {
		out.envSet = append([]string(nil), cfg.envSet...)
	}
	out.filed = nil
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
