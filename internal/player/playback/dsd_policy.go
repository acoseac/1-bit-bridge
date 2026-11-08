package playback

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// The player's DSD in production (B728 B7, the cutover; the design's
// sections 3.1, 3.5 and 6.2): the effective policy (internal/player/policy)
// over the player's DSD state, the profile database and what the box
// establishes of the bound DAC now decides each DSD rate, and no approval
// record is read. One set, published under a generation, is what selection
// admits by and what GET /v1/outputs publishes; a change of the set is
// reconciled per occurrence (policy.Reconcile); what playback observes is
// recorded in the state. The approval's admission (Authorization) is the
// harness's alone.

// dsdTick is how often a session that holds a DSD occurrence reads the
// policy again, the gain check among it (the design's section 3.4: the 100
// ms monitor).
const dsdTick = 100 * time.Millisecond

// dsdIdle is how often the supervisor's refresh (RefreshDSD) reads the
// policy again when nothing says an input changed: a control, which no
// event announces, moves the DoP gain check's answer, and the published set
// should not lag one by more than this.
const dsdIdle = time.Second

// streamingObserved is the content an occurrence of a native or DoP tuple
// plays without a fault before the tuple's streaming_observed is recorded
// (the design's section 1.1).
const streamingObserved = 5 * time.Second

// DSDPolicy configures production's DSD admission (Config.DSD): the
// player's effective DSD policy, and where playback records what it observes
// of a DAC.
type DSDPolicy struct {
	// Store is the player's DSD state, its one writer: the policy reads its
	// state, and playback writes into it what it observed (a driver's
	// acceptance, streaming observed, first use observed). Nil is a state
	// that did not open, which refuses DSD at every rate.
	Store DSDStore
	// Profiles is the DAC profile database; nil knows no DAC.
	Profiles *dacprofile.Database
	// Kernel is the running kernel's release (uname -r), under which a
	// driver acceptance counts; "" records none.
	Kernel string
	// Bound is the DAC the binding resolves to now, from a reading of the box
	// that is fresh (the hotplug watcher's latest snapshot of the boot
	// binding, not stale, available): false while there is none, which is no
	// ground for a set.
	Bound func() (BoundDAC, bool)
	// Box reads what the policy takes of the bound card, bounded.
	Box DSDBox
	// Probes keeps the silent probe's answers; nil keeps none, and probes
	// nothing.
	Probes *alsapcm.ProbeCache
	// Now stamps what playback records; nil is time.Now.
	Now func() time.Time
	// Tick is what a session holding a DSD occurrence reads the policy again
	// on: a channel and the function that stops it; nil ticks every 100 ms.
	Tick func() (<-chan time.Time, func())
	// Log is where what the policy does is said; nil is the backend's.
	Log interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}
}

// BoundDAC is the DAC the binding resolves to now (DSDPolicy.Bound).
type BoundDAC struct {
	// Card is its card, and Device and Subdevice the bound PCM endpoint.
	Card, Device, Subdevice int
	// Identity is the unit as the reading resolved it: the policy reads the
	// card only while it is that unit at that enumeration (binding.SameUnit
	// and its DevNum), so a card renumbered since is never read for it.
	Identity alsa.USBIdentity
	// Enumeration tells this enumeration of the unit from every other, for
	// the probe's answers (alsapcm.ProbeUnit.Enumeration); in memory alone.
	Enumeration string
}

// DSDStore is the player's DSD state as the policy reads it and playback
// writes it (dsdstate.Store).
type DSDStore interface {
	State() dsdstate.State
	Update(func(*dsdstate.State) error) error
}

// DSDBox reads what the effective policy takes of a card (the design's
// section 3.1): the unit's model, the PCM device's playback altsets, and for
// the DoP gain check the kernel's table of the unit's controls and the card's
// controls. Each read is bounded and confined to its tree.
type DSDBox interface {
	UnitModel(card int) (alsa.UnitModel, error)
	// CardUnit is the card's unit as sysfs reads it now, false when the card
	// is not a readable USB unit.
	CardUnit(card int) (alsa.USBIdentity, bool)
	PlaybackAltsets(card, device int) ([]alsa.Altset, error)
	USBMixer(card int) ([]byte, error)
	// ReadControls is the card's control snapshot, read with alsactl's
	// AllowUnreadable.
	ReadControls(card int) (alsactl.Snapshot, error)
}

// HostBox is DSDBox over the box's trees.
type HostBox struct {
	Sysfs    alsa.SysfsReader
	Proc     alsa.Reader
	Controls alsactl.Reader
}

func (h HostBox) UnitModel(card int) (alsa.UnitModel, error) { return h.Sysfs.UnitModel(card) }

func (h HostBox) CardUnit(card int) (alsa.USBIdentity, bool) {
	id, ok := h.Sysfs.Card(card)
	if !ok || id.Kind != alsa.IdentityUSB || id.USB == nil {
		return alsa.USBIdentity{}, false
	}
	return *id.USB, true
}

func (h HostBox) PlaybackAltsets(card, device int) ([]alsa.Altset, error) {
	return h.Proc.PlaybackAltsets(card, device)
}

func (h HostBox) USBMixer(card int) ([]byte, error) { return h.Proc.USBMixer(card) }

func (h HostBox) ReadControls(card int) (alsactl.Snapshot, error) {
	r := h.Controls
	r.AllowUnreadable = true
	return r.Read(card)
}

// dsdSet is one effective set as the backend publishes and admits by:
// immutable once stored. computed is false while no set could be computed,
// and why says why; every rate is then refused (ReasonDACUnread). Its read
// side is published beside it (dsdPublished), never in it.
type dsdSet struct {
	gen      uint64
	set      policy.Set
	computed bool
	why      string
	// unit is where the set was computed: the bound card and endpoint, the
	// unit's enumeration, and its model's text for publication.
	card, device, subdevice int
	enumeration             string
	model                   string
	// profile is the profile that matched the unit, nil for an unknown DAC:
	// the gain check at an open reads its exemptions.
	profile *dacprofile.Profile
}

// dsdPublished is what GET /v1/outputs reads of the player's DSD (B728 B8):
// the set published and its read side, swapped as one pair, so a reader never
// sees one set's tuples beside a view made for another. The set's pointer
// moves only with what plays (dsdSet.same), since every occurrence admitted
// under it is compared with the set published by that pointer (the gapless
// join, a converted join window's successor), and only a reconciliation
// repoints one; the view moves with every refresh that says another thing of
// the DAC, the pair then holding the set as it was (round 2: a view-only
// change stored a new set and stranded each occurrence admitted under the
// old one).
type dsdPublished struct {
	set  *dsdSet
	view DSDView
}

// rateWords are the rates' names in a log line.
var rateWords = map[int]string{dsdstate.DSD64: "DSD64", dsdstate.DSD128: "DSD128", dsdstate.DSD256: "DSD256"}

// planOutcome is what a rate's plan plays, for a log line: native, dop,
// converted (to PCM by the player, B728 B11) or the word of its refusal.
func planOutcome(p policy.Plan) string {
	switch p.Outcome {
	case policy.OutcomeNative, policy.OutcomeDoP, policy.OutcomeConverted:
		return string(p.Outcome)
	}
	return string(p.Reason)
}

// plays reports whether a plan plays its rate: native, DoP or converted.
func plays(p policy.Plan) bool {
	return p.Outcome == policy.OutcomeNative || p.Outcome == policy.OutcomeDoP || p.Outcome == policy.OutcomeConverted
}

// tuples are the set's native and DoP plans, by rate: what it publishes and
// what selection admits.
func (d *dsdSet) tuples() []engine.NativePlan {
	if !d.computed {
		return nil
	}
	var out []engine.NativePlan
	for _, r := range d.set.All() {
		if r.Plan.Outcome == policy.OutcomeNative || r.Plan.Outcome == policy.OutcomeDoP {
			out = append(out, enginePlan(r.Plan))
		}
	}
	return out
}

// converted are the set's rates converted to PCM (B728 B11), each with its
// plan and packing: what codecs and OpenHome's ProtocolInfo read beside the
// tuples, and what GET /v1/outputs publishes as dsdPlayback.converted (B8;
// the design's section 7.1).
func (d *dsdSet) converted() []ConvertedRate {
	if !d.computed {
		return nil
	}
	var out []ConvertedRate
	for _, r := range d.set.All() {
		if r.Plan.Outcome == policy.OutcomeConverted {
			out = append(out, ConvertedRate{BitRate: r.Plan.BitRate, Plan: convPlanOf(r.Plan.Target), Packing: r.Plan.Target.Packing})
		}
	}
	return out
}

// reason is the set's aggregate refusal as GET /v1/outputs publishes it
// (B7 within today's wire shape): "" while any rate plays, a converted one
// included since the player converts (B728 B11), and otherwise the word of
// the lowest rate's refusal.
func (d *dsdSet) reason() string {
	if !d.computed {
		return string(ReasonDACUnread)
	}
	all := d.set.All()
	for _, r := range all {
		if plays(r.Plan) {
			return ""
		}
	}
	if len(all) == 0 {
		return string(ReasonDACUnread)
	}
	return planOutcome(all[0].Plan)
}

// bcd is the firmware the set was computed for, "" for none.
func (d *dsdSet) bcd() string {
	if !d.computed {
		return ""
	}
	return d.set.BCDDevice
}

// same reports whether e publishes and admits what d does: both computed or
// neither, with the same reason; the same unit, endpoint and identity; and at
// every rate the same plan (policy.Plan.Same) or the same refusal. The read
// side is no part of it: a change of evidence, first use, the choice's
// revision or a warning alone moves neither the set nor its generation, and
// refreshDSD publishes it beside the set as it was (B728 B8).
func (d *dsdSet) same(e *dsdSet) bool {
	if d.computed != e.computed {
		return false
	}
	if !d.computed {
		return d.why == e.why
	}
	if d.card != e.card || d.device != e.device || d.subdevice != e.subdevice || d.model != e.model || !d.set.Identity().Same(e.set.Identity()) {
		return false
	}
	a, b := d.set.All(), e.set.All()
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		p, q := a[i].Plan, b[i].Plan
		if p.Outcome != q.Outcome || p.BitRate != q.BitRate || p.Reason != q.Reason {
			return false
		}
		if (p.Outcome == policy.OutcomeNative || p.Outcome == policy.OutcomeDoP || p.Outcome == policy.OutcomeConverted) && !p.Same(q) {
			return false
		}
	}
	return true
}

// policyID names a plan for the engine and the wire, an opaque word: its
// transport, format, rate and timing, so two occurrences share a sink only
// under one plan.
func policyID(p policy.Plan) string {
	return fmt.Sprintf("%s/%s/%d/%s/%d/%d", p.Outcome, p.Tuple.Format, p.BitRate, p.Tuple.Pause, p.Tuple.Tail.Milliseconds(), p.Tuple.Prefill.Milliseconds())
}

// enginePlan is a native or DoP plan as the engine and the sink take it:
// DoP's tuple names its transport, a native one names none; no edge, since a
// transition needs no approval in production.
func enginePlan(p policy.Plan) engine.NativePlan {
	transport := ""
	if p.Outcome == policy.OutcomeDoP {
		transport = TransportDoP
	}
	return engine.NativePlan{Tuple: engine.NativeTuple{Transport: transport, Format: string(p.Tuple.Format), BitRate: p.BitRate},
		PolicyID: policyID(p), Pause: p.Tuple.Pause, Tail: p.Tuple.Tail, Prefill: p.Tuple.Prefill}
}

// selectFor is d's plan for DSD content of f: the native or DoP plan at its
// rate; a converted rate's plan (B728 B11), with no native plan, which the
// open converts to the target's PCM; or the rate's refusal by the word the
// set names. A set that could not be computed refuses every rate
// (ReasonDACUnread).
func (d *dsdSet) selectFor(f format.NativeDSD) (engine.NativePlan, policy.Plan, error) {
	if !nativeLogicalRate(f.BitRateHz) {
		return engine.NativePlan{}, policy.Plan{}, &engine.Unplayable{Reason: ReasonRate, Detail: "DSD64, DSD128 and DSD256 only: no native, DoP or converted transport here carries this DSD rate"}
	}
	if f.Channels != 2 {
		return engine.NativePlan{}, policy.Plan{}, &engine.Unplayable{Reason: engine.ReasonRefusedContent, Detail: "the player plays stereo DSD alone"}
	}
	if !d.computed {
		return engine.NativePlan{}, policy.Plan{}, &engine.Unplayable{Reason: ReasonDACUnread, Detail: "no effective DSD set: " + d.why}
	}
	p := d.set.At(f.BitRateHz).Plan
	switch p.Outcome {
	case policy.OutcomeNative, policy.OutcomeDoP:
		return enginePlan(p), p, nil
	case policy.OutcomeConverted:
		return engine.NativePlan{}, p, nil
	}
	return engine.NativePlan{}, p, &engine.Unplayable{Reason: p.Reason, Detail: p.Detail}
}

// dsdLog is where the policy says what it does.
func (b *Backend) dsdLog() interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
} {
	if p := b.cfg.DSD; p != nil && p.Log != nil {
		return p.Log
	}
	return b.log
}

// dsdNow is the policy's clock.
func (b *Backend) dsdNow() time.Time {
	if p := b.cfg.DSD; p != nil && p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// dsdState is the player's DSD state as the policy reads it: a state that
// did not open refuses every rate (dsdstate.Refusing).
func (b *Backend) dsdState() dsdstate.State {
	if p := b.cfg.DSD; p != nil && p.Store != nil {
		return p.Store.State()
	}
	return dsdstate.Refusing(b.dsdNow())
}

// noSet is a set that could not be computed, for why, and its read side
// from the state st (noView).
func noSet(why string, st dsdstate.State) (*dsdSet, DSDView) { return &dsdSet{why: why}, noView(st) }

// canonicalBCD is a firmware as the state keys it, four lowercase
// hexadecimal digits, or "" for one that does not read.
func canonicalBCD(s string) string {
	if v, err := usbid.ParseHex16(s); err == nil {
		return usbid.Hex16(v)
	}
	return ""
}

// unitText is a USB string of the unit u as the set publishes it
// (PROTOCOL.md's dsdPlayback.model): every occurrence of u's own port path, in
// each of its spellings (PortForms), and of its serial replaced with
// [redacted], as the dac element's labels have them taken out. The unit's
// own, from the reading, whatever the binding names: a binding by model or by
// port names neither, and a product string may carry the serial (B7 round 2)
// or the port alone (B8 round 2).
func unitText(s string, u alsa.USBIdentity) string {
	for _, p := range PortForms(u.Port) {
		s = strings.ReplaceAll(s, p, "[redacted]")
	}
	return u.Serial.Redact(s)
}

// PortForms are the spellings of the port path p that a USB string or a
// label may hold, the longest first: p as given, which sysfs gives with the
// unit's configuration and interface after the port (usb2-1:1.0), its
// canonical form, and the port alone, as a port binding names it
// (usbid.Port.Place); a path the port grammar does not read, p alone, and
// none for "". B728 B8 round 2: a product string holding the port alone kept
// it in the model, and the dacKey was a hash of it.
func PortForms(p string) []string {
	if p == "" {
		return nil
	}
	forms := []string{p}
	if port, err := usbid.ParsePort(p); err == nil {
		for _, f := range []string{port.String(), port.Place().String()} {
			if f != "" && !slices.Contains(forms, f) {
				forms = append(forms, f)
			}
		}
	}
	slices.SortStableFunc(forms, func(a, b string) int { return len(b) - len(a) })
	return forms
}

// readMixer is what the DoP gain check reads of card, whose unit m is: its
// descriptors and configuration, the kernel's table of its controls, and the
// card's controls, each marked read only when it was.
func readMixer(box DSDBox, card int, m alsa.UnitModel) policy.Mixer {
	mx := policy.Mixer{Descriptors: m.Descriptors, Config: m.Config}
	if snap, err := box.ReadControls(card); err == nil {
		mx.Controls, mx.ControlsRead = snap, true
	}
	if b, err := box.USBMixer(card); err == nil {
		if units, err := policy.ParseUSBMixer(b); err == nil {
			mx.Units, mx.UnitsRead = units, true
		}
	}
	return mx
}

// probeUnit is what the probe's answers for key's model at bd hold for.
func (b *Backend) probeUnit(key dacprofile.Key, bcd string, bd BoundDAC) alsapcm.ProbeUnit {
	return alsapcm.ProbeUnit{Model: key.Canonical(), Firmware: canonicalBCD(bcd), Kernel: b.cfg.DSD.Kernel, Enumeration: bd.Enumeration}
}

// readDSD computes the effective set from what it reads now (the design's
// section 3.1), and its read side: the state, once, which the set and its
// read side are both made from, then the box (readBox), the set held to what
// a landed choice's state plays alike while there is one (setOver). What does
// not read leaves no set, and DSD refused (ReasonDACUnread), its read side the
// state's warnings of the bridge's all the same (noView).
func (b *Backend) readDSD() (*dsdSet, DSDView) {
	st := b.dsdState()
	r, why := b.readBox()
	if r == nil {
		return noSet(why, st)
	}
	return b.setOver(r, st)
}

// dsdReading is what one reading of the box establishes of the bound DAC
// (readBox): the effective policy's inputs but the state, and what a set
// made from them is computed for. A refresh makes its set from it and the
// state; a choice write makes, from one reading, the set the state gives and
// the one the write would publish (B728 B9).
type dsdReading struct {
	m    alsa.UnitModel
	key  dacprofile.Key
	in   policy.Inputs
	tmpl dsdSet
}

// readBox reads the bound DAC from a fresh reading of the box: its unit's
// model read from sysfs while it is still the unit that reading resolved,
// its PCM device's playback altsets, the DoP gain check's mixer, the probe's
// answers, the profiles and the kernel; then the unit read again, so a card
// renumbered while its files were read is never taken. It answers why not
// where nothing could be read.
func (b *Backend) readBox() (*dsdReading, string) {
	p := b.cfg.DSD
	if p.Bound == nil || p.Box == nil {
		return nil, "the player has no reading of the box"
	}
	bd, ok := p.Bound()
	if !ok {
		return nil, "the box's reading of the bound DAC is stale, or the binding names no unit now"
	}
	same := func(u alsa.USBIdentity) bool {
		return binding.SameUnit(u, bd.Identity) && u.DevNum > 0 && u.DevNum == bd.Identity.DevNum
	}
	m, err := p.Box.UnitModel(bd.Card)
	if err != nil {
		return nil, "the bound DAC's identity did not read: " + err.Error()
	}
	if !same(m.USB) {
		return nil, "the bound card is no longer the unit the box's reading resolved"
	}
	key, err := dsdstate.KeyOf(m.USB, m.Product)
	if err != nil {
		return nil, "the bound DAC's model does not key: " + err.Error()
	}
	alts, err := p.Box.PlaybackAltsets(bd.Card, bd.Device)
	if err != nil {
		return nil, "the bound PCM device's stream did not read: " + err.Error()
	}
	mixer := readMixer(p.Box, bd.Card, m)
	if u, ok := p.Box.CardUnit(bd.Card); !ok || !same(u) {
		return nil, "the bound card changed while it was read"
	}
	unit := dacprofile.Unit{Key: key, BCDDevice: m.BCDDevice}
	var answers []alsapcm.ProbeResult
	if p.Probes != nil {
		pu := b.probeUnit(key, m.BCDDevice, bd)
		for _, c := range alsapcm.ProbeCandidates() {
			answers = append(answers, p.Probes.Answer(pu, c))
		}
	}
	r := &dsdReading{m: m, key: key, in: policy.Inputs{Unit: unit, Profiles: p.Profiles, Kernel: p.Kernel, Altsets: alts, Probe: answers, Mixer: mixer},
		tmpl: dsdSet{computed: true, card: bd.Card, device: bd.Device, subdevice: bd.Subdevice, enumeration: bd.Enumeration,
			model: unitText(strings.TrimSpace(strings.TrimSpace(m.Manufacturer)+" "+strings.TrimSpace(m.Product)), m.USB)}}
	if p.Profiles != nil {
		if prof, ok := p.Profiles.Match(unit); ok {
			r.tmpl.profile = &prof
		}
	}
	return r, ""
}

// inputs are the effective policy's inputs of r over st.
func (r *dsdReading) inputs(st dsdstate.State) policy.Inputs { return r.in.WithState(st) }

// setFor is the set r makes with st, and its read side.
func (r *dsdReading) setFor(st dsdstate.State) (*dsdSet, DSDView) {
	d := r.tmpl
	d.set = policy.Effective(r.inputs(st))
	return &d, viewOf(&d, st, r.m.USB)
}

// setOver is the set the backend publishes from r over st, and its read side:
// r's set, held, while a choice's write that landed is not known to be durable
// (landedChoice), to what the state it left in the file plays alike
// (policy.Intersect), so what plays until the next start plays under either
// state that start may read (B728 B9).
func (b *Backend) setOver(r *dsdReading, st dsdstate.State) (*dsdSet, DSDView) {
	d, v := r.setFor(st)
	if l := b.landedChoice.Load(); l != nil {
		ld, _ := r.setFor(*l)
		d.set = policy.Intersect(d.set, ld.set)
		v = viewOf(d, st, r.m.USB)
	}
	return d, v
}

// refreshDSD computes the effective set from fresh inputs (readDSD) and,
// when it publishes or admits anything another way than the one published
// (dsdSet.same), publishes it at the next generation, in one swap that
// selection sees and then one that publication sees, the pair with its read
// side (dsdPublished), then reconciles every session's DSD occurrences
// against it (policy.Reconcile). One that plays and admits what the published
// one does is not stored: the set published stays, pointer and generation,
// and what the reading says of the DAC (its read side: first use observed, an
// evidence step, the choice's revision, a warning), when it says another
// thing, is published beside it (B728 B8). The epoch a client's grant keys on
// is what plays, the design's section 7.1 putting the tuples and the
// converted rates under it, and the set an occurrence was admitted under is
// still the one published, so nothing is reconciled (round 2: a new set
// stored for a view-only change, which no reconciliation followed, left every
// occurrence admitted under the old one outside the set published, and the
// producer waited at the gapless join for good). Refreshes run one at a time,
// so what they publish is in the order their readings were taken; it answers
// the set published once it is done. It reads no file on the audio path: a
// session's admission, its activation and its monitor ask it, and the
// supervisor's RefreshDSD, on a goroutine of the backend's
// (RequestRefreshDSD, B762).
func (b *Backend) refreshDSD() *dsdSet {
	b.dsdMu.Lock()
	defer b.dsdMu.Unlock()
	b.dsdRead = b.dsdNow()
	next, view := b.readDSD()
	cur := b.dsd.Load()
	if cur.same(next) {
		if pub := b.dsdPub.Load(); !reflect.DeepEqual(pub.view, view) {
			b.dsdPub.Store(&dsdPublished{set: cur, view: view})
		}
		return cur
	}
	next.gen = cur.gen + 1
	b.dsd.Store(next)
	b.dsdPub.Store(&dsdPublished{set: next, view: view})
	b.logDSDSet(next)
	b.published()
	b.reconcileDSD(next)
	return next
}

// RefreshDSD is the supervisor's refresh (the design's section 3.5): it
// reads the policy again, and publishes and reconciles a change as every
// refresh does, when force says an input changed (a snapshot of the
// watcher's, a write of the state, a probe's answers), and otherwise once
// dsdIdle has passed since the last reading, so a control's change, which no
// event announces, reaches the published set. A backend with no policy
// configured does nothing. It runs on the caller's goroutine, and waits for
// the refreshes' lock: the supervisor asks it through RequestRefreshDSD.
func (b *Backend) RefreshDSD(force bool) {
	if b.cfg.DSD == nil {
		return
	}
	if !force {
		b.dsdMu.Lock()
		due := b.dsdNow().Sub(b.dsdRead) >= dsdIdle
		b.dsdMu.Unlock()
		if !due {
			return
		}
	}
	b.refreshDSD()
}

// logDSDSet says what the set published plays at each rate, one Info line a
// generation: native, dop, converted, or the word of the rate's refusal,
// which GET /v1/outputs publishes as dsdPlayback.refused (B728 B8).
func (b *Backend) logDSDSet(d *dsdSet) {
	attrs := []any{"generation", d.gen}
	if !d.computed {
		b.dsdLog().Info("player: the effective DSD set is none: DSD is refused at every rate", append(attrs, "reason", string(ReasonDACUnread), "why", d.why)...)
		return
	}
	for _, r := range d.set.All() {
		attrs = append(attrs, rateWords[r.Plan.BitRate], planOutcome(r.Plan))
	}
	if d.set.Known {
		attrs = append(attrs, "profile", d.set.Profile)
	}
	attrs = append(attrs, "mode", d.set.Mode)
	b.dsdLog().Info("player: the effective DSD set changed", attrs...)
}

// admitDSD is a DSD occurrence's admission under the effective policy: the
// set published now, read afresh first for a session's first occurrence (the
// design's section 3.5: every session reads it) and whenever the set was
// read longer ago than a session's monitor reads it (dsdStale), its plan at
// the content's rate or its refusal. The occurrence keeps the set and the
// plan it was admitted under, which the check after open, its activation and
// reconciliation compare with. A converted rate answers no native plan: the
// occurrence converts (occ.converts, B728 B11). A refusal by the set's word,
// at a rate and in channels the policy decides, keeps the set it was made
// under (refusedUnder), which a choice's write or a later set can lift
// (settleOpen, B728 B9 round 2).
func (b *Backend) admitDSD(o *occ, f format.NativeDSD) (engine.NativePlan, error) {
	cur := b.dsd.Load()
	if o.leading || b.dsdStale() {
		cur = b.refreshDSD()
	}
	plan, pp, err := cur.selectFor(f)
	if err != nil {
		if nativeLogicalRate(f.BitRateHz) && f.Channels == 2 {
			o.refusedUnder = cur
		}
		return engine.NativePlan{}, err
	}
	o.admitted, o.policyPlan = cur, pp
	return plan, nil
}

// dsdStale reports whether the set published was read longer ago than a
// session's monitor reads it (dsdTick), or never: a session that played PCM
// alone until now has no monitor, so its first DSD occurrence reads the
// policy afresh rather than take a set the supervisor's last refresh left.
func (b *Backend) dsdStale() bool {
	b.dsdMu.Lock()
	defer b.dsdMu.Unlock()
	return b.dsdRead.IsZero() || b.dsdNow().Sub(b.dsdRead) >= dsdTick
}

// readmitDSDLocked admits o again under the set published now, when it was
// admitted under an older one and its open has not finished: a successor's
// (openSuccessor, before its Prepared is reported, so nothing the engine
// holds changes) and a session's first occurrence's (start, before anything
// is located or opened; round 2). No reconciliation reaches an occurrence
// still opening, so the step that ends its open admits it again, under s.mu,
// and closes its done in that step: a refresh published before it is the one
// it is admitted under, and one published after it judges it. Its open read
// the first DSD chunk and kept its source, whatever it was admitted as, so
// it is admitted again as native, DoP or converted alike: a converted one's
// converter is made after this, in the same step (makeConverterLocked), for
// the plan it is admitted under now (B728 B11). s.mu is held.
func (s *session) readmitDSDLocked(o *occ) {
	cur := s.b.dsd.Load()
	if o.admitted == nil || o.admitted == cur || o.refused != nil {
		return
	}
	plan, pp, err := cur.selectFor(o.nativeFormat)
	if err != nil {
		var u *engine.Unplayable
		if errors.As(err, &u) {
			o.refused = u
		} else {
			o.refused = &engine.Unplayable{Reason: ReasonDSD, Detail: err.Error()}
		}
		return
	}
	o.admitted, o.policyPlan = cur, pp
	if pp.Outcome == policy.OutcomeConverted {
		o.plan = nil
		return
	}
	p := plan
	o.plan = &p
	o.transport = nativeTransport(p)
	if o.transport == (alsapcm.Transport{}) {
		o.refused = &engine.Unplayable{Reason: ReasonTransport, Detail: "the plan names no transport the sink opens"}
	}
}

// startDSDMonitor starts, once, the session's monitor of the effective
// policy, the 100 ms tick of the design's sections 3.4 and 3.5, once it holds
// a DSD occurrence: each tick reads the policy again (refreshDSD), the DoP
// gain check among it, which publishes and reconciles a change. It runs
// until the session holds nothing, a retiring tail included, never on the
// audio path, and a session that plays PCM alone has none.
func (s *session) startDSDMonitor() {
	s.mu.Lock()
	if s.dsdMonitor || s.b.cfg.DSD == nil {
		s.mu.Unlock()
		return
	}
	s.dsdMonitor = true
	s.mu.Unlock()
	tick := s.b.cfg.DSD.Tick
	if tick == nil {
		tick = func() (<-chan time.Time, func()) {
			t := time.NewTicker(dsdTick)
			return t.C, t.Stop
		}
	}
	ticks, stop := tick()
	s.b.spawn(func() {
		defer stop()
		defer func() {
			if r := recover(); r != nil {
				s.b.logPanic("the DSD policy's monitor", r)
			}
		}()
		for {
			select {
			case <-s.down:
				return
			case <-ticks:
				s.b.refreshDSD()
			}
		}
	})
}

// reconcileDSD judges every session's DSD occurrences against next.
func (b *Backend) reconcileDSD(next *dsdSet) {
	b.mu.Lock()
	ss := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		ss = append(ss, s)
	}
	b.mu.Unlock()
	for _, s := range ss {
		s.reconcileDSD(next)
	}
}

// occurrenceFactsLocked is what policy.Reconcile reads of o, each apart (the
// design's section 3.5): its plan; its own frames handed to its sink,
// written to the kernel and played, counted from its first frame there; its
// being heard; its output retiring (a released session's tail playing out);
// and the identity its sink last opened. s.mu is held.
func (s *session) occurrenceFactsLocked(o *occ) policy.Occurrence {
	f := policy.Occurrence{Plan: o.policyPlan, Audible: o.crossed}
	if out := o.out; out != nil {
		f.Submitted = o.written
		pos := out.sink.Position()
		f.Written = min(max(pos.Written-o.start, 0), o.written)
		f.Played = min(max(pos.Played-o.start, 0), o.written)
		f.Retiring = s.releasing && !out.closed
		f.Opened = out.openedIdentity()
	}
	return f
}

// dsdJudgement is what reconciliation does with one of a session's DSD
// occurrences against a set (judgeDSDLocked): nothing (not judged); keep it,
// counting under the set from now on; hold it back from the sink to hand it
// off again (stale); revoke the session's DSD, for why; and whether that
// interrupts what plays, which a choice's write asks first (writeInterrupts).
type dsdJudgement struct {
	keep, stale bool
	revoke      string
	interrupts  bool
}

// judgeDSDLocked is the one classification of o against next that both
// reconciliation (reconcileDSD) and a choice's write (writeInterrupts) act by:
// policy.Reconcile on what o is now (occurrenceFactsLocked), and the
// backend's rest. A re-plan is a revocation for a session's first occurrence
// and for an occurrence of a session being released, and where the engine
// hands nothing off again (replans false) it is held back and the session's
// DSD revoked. An occurrence still opening, dropped, PCM, admitted under next
// already, or played out (its sink drained or closed, or its every frame
// played with its end in its sink) is not judged. A re-plan interrupts where
// policy.Reconcile says so, frames of it in a sink, and also for the
// occurrence the producer writes, one whose first bits are in its
// predecessor's join window, and one whose own sink an open under way will
// adopt (claimed, B728 B9 round 2), whose frames may reach the sink before a
// write publishes. s.mu is held.
func (s *session) judgeDSDLocked(o *occ, next *dsdSet, replans bool) dsdJudgement {
	if !o.opened() || o.admitted == nil || o.dropped || o.admitted == next {
		return dsdJudgement{}
	}
	if out := o.out; out != nil && (out.drained || out.closed || o.eof && out.sink.Position().Played >= o.start+o.written) {
		return dsdJudgement{}
	}
	v := policy.Reconcile(next.set, s.occurrenceFactsLocked(o))
	switch v.Action {
	case policy.Keep:
		return dsdJudgement{keep: true}
	case policy.Replan:
		switch {
		case o.leading || s.releasing:
			return dsdJudgement{revoke: fmt.Sprintf("occurrence %d's DSD plan left the effective set before any of it played", o.id), interrupts: true}
		case !replans:
			return dsdJudgement{stale: true, revoke: "an occurrence's DSD plan left the effective set, and the engine hands nothing off again", interrupts: true}
		}
		return dsdJudgement{stale: true, interrupts: v.Interrupts || s.writing == o || o.inWindowLocked() || o.claimed}
	}
	return dsdJudgement{revoke: fmt.Sprintf("occurrence %d's DSD plan left the effective set, or its DAC changed, while it played", o.id), interrupts: true}
}

// reconcileDSD judges each of s's DSD occurrences against next
// (judgeDSDLocked, over policy.Reconcile): one next still plays on its
// identity is kept and counts under next from now on; one to re-plan is held
// back from the sink (stale), the sink of its own an activation is under way
// for revoked as a drop revokes it (B728 B9 round 4), and the engine is asked
// to hand it off again (ReplanEvents), the path an edit after hand-off takes,
// so it is admitted under next as any hand-off is, a session's first
// occurrence and one a session being released holds excepted, which are
// revoked; one to revoke
// stops the session's DSD at once (Revoke). A revocation of an occurrence
// whose own sink's activation is under way (claimed, its output adopted: a
// successor's in the producer's transition, a session's first occurrence's in
// its Start) records its fault on the session in this step, before anything is
// marked stale, for the reason Revoke is given, and its native sink is fenced
// as a re-plan's is (B728 B9 round 6): the activation then answers errReplanned
// or the sink's own fence, and a take-back of the occurrence, or its sink's
// close, can no longer come before the fault, which Revoke, finding the
// session's fault there, does not record again. A converted occurrence's PCM
// sink is not fenced, and needs none (round 7): it admits its node at its open,
// before its output is adopted, and at a resume's reopen, never in an
// activation, writes nothing of its own, and is written by the producer alone.
// Revoked once its activation completed, the occurrence claimed with its
// output adopted, its fault is recorded here and the producer's next step is
// its stop on that fault, which keeps its bytes off the node before Revoke
// drops the sink (round 8); revoked at its open's admission, it has no output
// of its own, so no fault is recorded, its adoption closes the sink unwritten
// and Revoke drops the occurrence. An occurrence whose sink has
// played everything it was given (drained) or is closed sounds no more, and
// nothing of it can reach the DAC again, so no change of the set can make it
// unsafe: it is not judged, which would revoke the session's DSD and with it
// a successor the set still plays in a sink of its own (a DoP occurrence
// played out, the native one after it filling its prefill; round 2). Nor is
// one whose every frame the sink has played, its end in it, which a sink
// shared with what follows it (a converted occurrence's, PCM after it on the
// same transport, B728 B11) does not drain: judged, it would stop that PCM. A
// converted occurrence is judged as native and DoP are, its plan its target
// (policy.Plan.Same), its frames PCM frames. A change of the set never asks a
// user's consent here: the safety changes (the gain leaving unity, the
// identity changing, the DAC leaving, the state no longer reading) act at
// once; a choice's write (B9) asks first, by the same classification.
func (s *session) reconcileDSD(next *dsdSet) {
	var revoke []string
	var replan []engine.OccurrenceID
	s.mu.Lock()
	re, replans := s.ev.(engine.ReplanEvents)
	js := make([]dsdJudgement, len(s.chain))
	noReplan := ""
	for i, o := range s.chain {
		j := s.judgeDSDLocked(o, next, replans)
		js[i] = j
		switch {
		case j.revoke == "":
		case j.stale:
			noReplan = j.revoke
		default:
			revoke = append(revoke, j.revoke)
		}
	}
	if noReplan != "" {
		revoke = append(revoke, noReplan)
	}
	// What an activation under way does is decided before anything is marked
	// stale (B728 B9 round 6). An occurrence whose own sink's activation is
	// under way (claimed, the output adopted) and whose DSD is revoked records
	// the revocation's fault first, the one Revoke is given (revoke[0]), unless
	// the session has a fault already: the activation's check finds it stale and
	// answers errReplanned, or its sink's own fence, and its take-back closes
	// the sink, either of which could come before Revoke, which records its
	// fault only while a sink is open. A session being released records none:
	// its release ends it.
	activating := func(o *occ, j dsdJudgement) bool {
		return (j.stale || j.revoke != "") && !s.releasing && o.claimed && o.out != nil
	}
	for i, o := range s.chain {
		if j := js[i]; j.revoke != "" && activating(o, j) && s.fault == nil {
			s.fault = &engine.Fault{Reason: "native_revoked", Detail: revoke[0]}
		}
	}
	for i, o := range s.chain {
		j := js[i]
		if j.keep {
			o.admitted = next
			continue
		}
		if j.stale {
			o.stale = true
		}
		if activating(o, j) && o.out.native != nil {
			// Its own sink's activation is under way (B728 B9 round 4): the sink
			// is revoked, as a drop revokes it (dropLocked), so the activation
			// answers the sink's own fence, at its admission or in its prefill,
			// and the sink is taken back with none of the occurrence written; a
			// re-plan's replacement opens one of its own. A revocation's fault is
			// already the session's (round 6): left to Revoke, the sink wrote its
			// prefill at the rate next takes away until Revoke stopped it (round 5).
			o.out.native.Revoke()
			o.out.drained = true
		}
		if j.stale && j.revoke == "" {
			replan = append(replan, o.id)
		}
	}
	s.mu.Unlock()
	for _, id := range replan {
		re.Replan(s.id, id, "its DSD plan left the effective set")
	}
	if len(revoke) > 0 {
		s.b.dsdLog().Warn("player: the effective DSD set no longer plays what this session holds: its DSD stops", "session", uint64(s.id), "generation", next.gen, "why", revoke[0])
		s.b.Revoke(engine.RevokeInput{Session: s.id, Reason: revoke[0]})
	}
	wake(s.wakeP)
	wake(s.wakeR)
}

// errReplanned is an activation finding its occurrence being handed off
// again (checkDSDActivation) or dropped (activateOutput's fence, B728 B9
// round 3): its sink is taken back unwritten (unadopt), never a fault, as one
// whose sink answers its own fence for such a drop is (transition, round 4).
var errReplanned = errors.New("player backend: the occurrence is being handed off again")

// checkDSDActivation is a native, DoP or converted occurrence's check as its
// sink is about to write to the DAC (the design's section 3.5: every
// activation reads the policy): the policy read afresh, which reconciles a
// change, then the occurrence, its set and its plan read here alone, judged
// against the set published now, under the session's lock,
// by reconciliation's own predicate (policy.Reconcile): under that set, or
// kept by it, when it counts under it from now on, as reconciliation would
// have it. A refresh beside the activation can publish a set between the read
// and the check and reconcile the occurrence under it, or not yet, so the set
// the read returned is no test: compared with it, a refresh that kept the plan
// refused the track, and one that took the plan away let the activation write
// to the DAC (round 4). A refresh that publishes after the check judges the
// occurrence as reconciliation judges any. Never a fallback: an occurrence
// the set published now does not keep is refused, never played another way.
// A choice's write pending that plays its rate otherwise is waited for
// first, no lock held (awaitWrite, B728 B9 round 2, point 1), so the check
// judges it under what the write left published; a successor being handed
// off again by then (the write landed, or a refresh changed the set), or
// dropped already for its replacement (a cut of a successor with no frame
// written drops it at once), is errReplanned, which its sink's adoption takes
// back, never written to.
func (s *session) checkDSDActivation(ctx context.Context, o *occ) error {
	s.awaitWrite(ctx, o)
	s.b.refreshDSD()
	s.b.activating()
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.b.dsd.Load()
	switch {
	case (o.stale || o.dropped) && !o.leading:
		return errReplanned
	case o.stale || o.dropped || o.admitted == nil:
	case o.admitted == cur:
		return nil
	case policy.Reconcile(cur.set, s.occurrenceFactsLocked(o)).Action == policy.Keep:
		o.admitted = cur
		return nil
	}
	return errors.New("the effective DSD set no longer plays this occurrence's plan")
}

// checkDSDNode is the check after open of a DSD occurrence's node (the
// design's sections 3.4 and 3.5), at its open and at every reopen after a
// pause's release, admitted is the set the sink's DSD was admitted under:
// the node's unit is the DAC that set was computed for, by its model's key
// and its firmware (policy.Identity), never by its port; and for DoP the
// gain check, read afresh at the node's card, passes for the plan's rate. A
// PCM sink a converted occurrence plays in (B728 B11) has no native plan and
// no gain check: its PCM changes no DoP word. It records the identity the
// sink opened.
func (out *output) checkDSDNode(n alsapcm.NodeInfo, admitted *dsdSet) error {
	p := out.s.b.cfg.DSD
	if p == nil || p.Box == nil {
		return errors.New("player backend: the effective DSD policy has no reading of the box")
	}
	m, err := p.Box.UnitModel(n.Card)
	if err != nil {
		return fmt.Errorf("player backend: the opened node's unit did not read: %w", err)
	}
	key, err := dsdstate.KeyOf(m.USB, m.Product)
	if err != nil {
		return fmt.Errorf("player backend: the opened node's unit does not key: %w", err)
	}
	id := policy.Identity{Key: key, BCDDevice: m.BCDDevice}
	if !id.Same(admitted.set.Identity()) {
		return &alsapcm.Error{Reason: alsapcm.ReasonWrongDevice, Op: "open", Endpoint: alsapcm.Endpoint{Card: n.Card, Device: n.Device},
			Detail: "the node opened is not the DAC the effective DSD set was computed for (its model or its firmware differs)"}
	}
	if out.plan != nil && out.plan.Tuple.Transport == TransportDoP {
		alts, err := p.Box.PlaybackAltsets(n.Card, n.Device)
		if err != nil {
			return fmt.Errorf("player backend: the opened node's stream did not read for the DoP gain check: %w", err)
		}
		g := policy.CheckGain(readMixer(p.Box, n.Card, m), policy.DoPAltsets(alts, out.plan.Tuple.BitRate), admitted.profile, m.BCDDevice)
		if !g.Unity {
			return fmt.Errorf("player backend: DoP refused at the open, %s: %s", g.Reason, g.Detail)
		}
	}
	out.setOpenedIdentity(id)
	return nil
}

// evidenceOf is the state's entry of d for what was observed of transport in
// f at bitRate under bcd, made when there is none.
func evidenceOf(d *dsdstate.DAC, transport, f string, bitRate int, bcd string) *dsdstate.Evidence {
	for i, e := range d.Evidence {
		if e.Transport == transport && e.Format == f && e.BitRate == bitRate && e.BCDDevice == bcd {
			return &d.Evidence[i]
		}
	}
	d.Evidence = append(d.Evidence, dsdstate.Evidence{Transport: transport, Format: f, BitRate: bitRate, BCDDevice: bcd})
	return &d.Evidence[len(d.Evidence)-1]
}

// dacOf is the state's entry of key's model, made when there is none.
func dacOf(st *dsdstate.State, key dacprofile.Key) *dsdstate.DAC {
	for i := range st.DACs {
		if st.DACs[i].Key.Equal(key) {
			return &st.DACs[i]
		}
	}
	st.DACs = append(st.DACs, dsdstate.DAC{Key: key})
	return &st.DACs[len(st.DACs)-1]
}

// tupleTransport is a plan's transport as the state names it.
func tupleTransport(p policy.Plan) string {
	if p.Outcome == policy.OutcomeDoP {
		return dsdstate.TransportDoP
	}
	return dsdstate.TransportNative
}

// recordDSD writes into the player's DSD state what playback observed of
// key's model, on a goroutine of the backend's own, never the audio path's,
// counted among its writes (DSDWrites):
// mutate changes the DAC's entry and reports whether it changed anything,
// and an unchanged state writes nothing. A write the store refuses changes
// nothing and is said, the landed write's fence once (dsdstate's
// ErrLandedNotHeld: the file holds a state the store does not, so no write
// is made from it).
func (b *Backend) recordDSD(what string, key dacprofile.Key, mutate func(d *dsdstate.DAC, now time.Time) bool) {
	p := b.cfg.DSD
	if p == nil || p.Store == nil {
		return
	}
	b.spawnWrite(what, func() {
		defer func() {
			if r := recover(); r != nil {
				b.logPanic("a write of the player's DSD state", r)
			}
		}()
		now := b.dsdNow()
		err := p.Store.Update(func(st *dsdstate.State) error {
			if !mutate(dacOf(st, key), now) {
				return errUnchanged
			}
			return nil
		})
		switch {
		case err == nil, errors.Is(err, errUnchanged):
		case errors.Is(err, dsdstate.ErrLandedNotHeld):
			b.dsdFenceOnce.Do(func() {
				b.dsdLog().Warn("player: what playback observes of a DAC is not written to the player's DSD state in this run: an earlier write reached its file and is not known to be durable, which the store does not hold", "what", what)
			})
		default:
			b.dsdLog().Warn("player: what playback observed of a DAC was not written to the player's DSD state", "what", what, "err", err)
		}
	})
}

// errUnchanged is a record that changes nothing: no write.
var errUnchanged = errors.New("unchanged")

// noteDriverAccepted records a real open's driver_accepted (the design's
// section 1.1): the sink's HW_PARAMS committed exactly the format, rate and
// channels plan pp asked under set, as alsapcm's open requires, so the
// kernel's driver took the tuple on this box, under the running kernel; for
// DoP, its carrier alone. Nothing is recorded with no kernel release or no
// firmware, which no rung counts. The caller reads set and pp under the
// session's lock.
func (b *Backend) noteDriverAccepted(set *dsdSet, pp policy.Plan) {
	if set == nil || !set.computed || b.cfg.DSD.Kernel == "" || canonicalBCD(set.set.BCDDevice) == "" {
		return
	}
	kernel, bcd := b.cfg.DSD.Kernel, canonicalBCD(set.set.BCDDevice)
	b.recordDSD("driver_accepted", set.set.Key, func(d *dsdstate.DAC, now time.Time) bool {
		e := evidenceOf(d, tupleTransport(pp), string(pp.Tuple.Format), pp.BitRate, bcd)
		if !e.DriverAcceptedAt.IsZero() && e.Kernel == kernel {
			return false
		}
		e.DriverAcceptedAt, e.Kernel = now.UTC().Truncate(time.Second), kernel
		return true
	})
}

// noteStreamingObserved records the streaming_observed of plan pp under set,
// and its transport's first use observed (the design's sections 1.1 and
// 6.2): an occurrence of the tuple played streamingObserved of content, the
// kernel establishing it, with no fault. Each is set once. The caller reads
// set and pp under the session's lock.
func (b *Backend) noteStreamingObserved(set *dsdSet, pp policy.Plan) {
	if set == nil || !set.computed || canonicalBCD(set.set.BCDDevice) == "" {
		return
	}
	bcd := canonicalBCD(set.set.BCDDevice)
	transport := tupleTransport(pp)
	b.recordDSD("streaming_observed", set.set.Key, func(d *dsdstate.DAC, now time.Time) bool {
		changed := false
		at := now.UTC().Truncate(time.Second)
		if e := evidenceOf(d, transport, string(pp.Tuple.Format), pp.BitRate, bcd); e.StreamingObservedAt.IsZero() {
			e.StreamingObservedAt, changed = at, true
		}
		for i := range d.FirstUse {
			f := &d.FirstUse[i]
			if f.Transport == transport && f.BCDDevice == bcd && f.Covers(pp.BitRate) && f.ObservedAt.IsZero() {
				f.ObservedAt, changed = at, true
			}
		}
		return changed
	})
}

// observedFrames is how many of a plan's frames, logical bits a channel,
// make streamingObserved.
func observedFrames(p *engine.NativePlan) int64 {
	return int64(p.Tuple.BitRate) * int64(streamingObserved/time.Second)
}

// probeSlotSkips are the probe's refusals by the device slot (Probe's
// reserveProbe): a probe due then stays due, and the supervisor asks it again
// once the slot is free (ProbeDSD).
var probeSlotSkips = []ProbeSkip{ProbeSessionHolds, ProbeSessionPaused, ProbeReleasePending, ProbeReleaseUnconfirmed, ProbeRunning}

// dsdProbe is the probe's bookkeeping: the unit last probed; a probe asked
// for anew (forced: a migration's part, ForgetDSDProbe, or a choice written,
// oweProbe) and the count of those asks (asked), which a run's end checks
// before it clears what is owed (settled); the generation, which the
// migration's part alone moves, in the step that drops the answers, and a
// probe's answers are checked against (publishProbe); a probe owed that only
// the supervisor's next ask makes (ProbeDSDPending); and the goroutine its
// asks run on (RequestProbeDSD, B762).
type dsdProbe struct {
	mu      sync.Mutex
	probed  alsapcm.ProbeUnit
	done    bool
	forced  bool
	asked   uint64
	gen     uint64
	pending bool
	run     coalesced
}

// settled says a run of the probe owes nothing, unless a probe was asked
// anew since it began (asked moved: a migration's part, or a choice written
// for the DAC), which is still owed.
func (ps *dsdProbe) settled(asked uint64) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.asked == asked {
		ps.pending = false
	}
}

// owe says a probe is owed.
func (ps *dsdProbe) owe() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.pending = true
}

// ProbeDSD runs the silent probe of the bound DAC when one is due (the
// design's section 5), on the caller's goroutine, and answers whether one is
// still owed (ProbeDSDPending): at the player's start once the output is
// available, when the unit's model, firmware, kernel or enumeration changes
// (a replug, a reset, a firmware update), after a migration's part for it
// (ForgetDSDProbe) and after a choice was written for it (oweProbe), never on
// a timer. A probe the device slot refuses, a session holding the device
// (playing, opening it, paused, releasing), a Start waiting for it, or a
// release unconfirmed, stays due, opens nothing and waits for nothing: it
// answers owed, and the supervisor asks again at its next tick, so the probe
// runs once the slot is free, B4's preconditions unchanged. One that ran, or
// met a busy node (MPD's), or found no unit, is done for that unit. Its
// answers are kept (alsapcm.ProbeCache), each accepted native format and DoP
// carrier recorded as the tuple's driver_accepted under the kernel, and the
// policy read again, all under the unit it captured before the probe, which
// the node it opens must be (probedUnit): another unit there is asked
// nothing. They are taken only while still current (publishProbe, B762); a
// probe whose answers are dropped keeps nothing of them and is owed again.
// cond is the watcher's view of the bound DAC. The supervisor asks it
// through RequestProbeDSD.
func (b *Backend) ProbeDSD(ctx context.Context, cond ProbeConditions) (pending bool) {
	pending, refresh := b.probeDSD(ctx, cond)
	if refresh {
		b.refreshDSD()
	}
	return pending
}

// probeDSD is one run of the probe (ProbeDSD) but for the policy's reading
// that its answers call for, which it reports (refresh) for its caller to
// make: on its own goroutine, ProbeDSD; on the probe's, RequestProbeDSD, which
// asks it of the refresh's goroutine (RequestRefreshDSD).
func (b *Backend) probeDSD(ctx context.Context, cond ProbeConditions) (pending, refresh bool) {
	p := b.cfg.DSD
	if p == nil || p.Probes == nil || p.Bound == nil || p.Box == nil {
		return false, false
	}
	ps := &b.probeState
	// What its answers are checked against before they change anything
	// (publishProbe), read before anything else: the probe's generation,
	// which a migration's part for the DAC moves, and the Starts that took the
	// device slot; and the asks for a probe anew so far, which a choice
	// written moves too, so a run that ends owing nothing clears no ask made
	// after it began (settled).
	ps.mu.Lock()
	gen, asked := ps.gen, ps.asked
	ps.mu.Unlock()
	starts := b.starts.Load()
	settled := func() (bool, bool) {
		ps.settled(asked)
		return false, false
	}
	bd, ok := p.Bound()
	if !ok || !cond.Available || !cond.Fresh {
		return settled()
	}
	m, err := p.Box.UnitModel(bd.Card)
	// A device number that did not read (0) tells no enumeration from another,
	// so its unit is not probed: its answers would be kept under an
	// enumeration nothing can tell from the next (round 3).
	if err != nil || !binding.SameUnit(m.USB, bd.Identity) || m.USB.DevNum <= 0 || m.USB.DevNum != bd.Identity.DevNum {
		return settled()
	}
	key, err := dsdstate.KeyOf(m.USB, m.Product)
	if err != nil {
		return settled()
	}
	unit := b.probeUnit(key, m.BCDDevice, bd)
	ps.mu.Lock()
	due := !ps.done || ps.probed != unit || ps.forced
	ps.mu.Unlock()
	if !due {
		return settled()
	}
	if ctx.Err() != nil {
		// Its supervisor has gone: the DAC is not asked.
		return false, false
	}
	epoch := p.Probes.Begin(unit)
	rep, err := b.Probe(ctx, ProbeRequest{Conditions: cond, Candidates: alsapcm.ProbeCandidates(), Check: probedUnit(p.Box, m, key)})
	var skipped *ProbeSkipped
	if errors.As(err, &skipped) && slices.Contains(probeSlotSkips, skipped.Reason) {
		ps.owe()
		return true, false
	}
	b.probed()
	if ctx.Err() != nil {
		return false, false
	}
	if why := b.publishProbe(ctx, gen, starts, bd, unit, epoch, rep); why != "" {
		b.dsdLog().Info("player: the silent probe's answers are dropped, and a probe is owed", "why", why)
		return true, false
	}
	// Kept means decided before any Start counted (publishProbe's step). What
	// the probe accepted is written into the player's DSD state after that
	// decision and outside every lock, through spawnWrite, which takes gmu: so
	// the write may run beside a Start that took the slot after the decision,
	// a write of the state and never a call to the device, recording answers
	// the probe got while it alone held the device.
	if rep.Opened {
		b.noteProbed(key, m.BCDDevice, rep)
	}
	b.dsdLog().Info("player: the silent probe asked the bound DAC", "opened", rep.Opened, "err", errorText(err))
	return false, true
}

// publishProbe takes a probe's answers as current, in one step under the
// probe's lock, which ForgetDSDProbe and oweProbe take too: its report kept
// in the probe's cache, its unit done, a probe asked anew satisfied, a choice
// written while it ran among them (oweProbe), whose answers it is. It answers
// "" once it has taken them, and otherwise why they are dropped, nothing of
// them kept and a probe owed: its context ended (its supervisor has gone); a
// migration's part for the DAC was made since it began (gen, ForgetDSDProbe),
// whose probe is owed; or, decided in one step under the backend's lock (takeProbe),
// a Start took the device slot since (starts), which a session holds or held,
// the binding no longer resolves to the unit it probed (bd: a replug, a
// reset, the DAC gone, a reading gone stale), or the cache refused the
// report, its model's epoch moved. Nothing reads the box here: the binding's
// resolution is the watcher's latest, in memory (DSDPolicy.Bound), read before
// the backend's lock: in serve it reads the hotplug feed's latest snapshot
// under the feed's own lock, which is held for in-memory work alone and never
// for I/O, but which is another package's, so it stays out of that step.
//
// The locks, in the one order they are taken in: the probe's (ps.mu), then
// the backend's (b.mu), then the probe cache's own. Nothing takes ps.mu, a
// session's lock or gmu under b.mu, and nothing takes b.mu under the cache's
// lock (the cache calls nothing back); nothing under any of them blocks: they
// compare what is in memory, and Record writes the cache's map.
func (b *Backend) publishProbe(ctx context.Context, gen, starts uint64, bd BoundDAC, unit alsapcm.ProbeUnit, epoch alsapcm.ProbeEpoch, rep alsapcm.ProbeReport) string {
	p := b.cfg.DSD
	ps := &b.probeState
	ps.mu.Lock()
	defer ps.mu.Unlock()
	why := ""
	now, ok := p.Bound()
	switch {
	case ctx.Err() != nil:
		why = "its supervisor has gone"
	case ps.gen != gen:
		why = "a migration's part for the DAC was made while it ran"
	default:
		why = b.takeProbe(starts, bd, now, ok, epoch, rep)
	}
	if why != "" {
		ps.pending = true
		return why
	}
	ps.probed, ps.done, ps.forced, ps.pending = unit, true, false, false
	return ""
}

// takeProbe is publishProbe's decision on the Starts, the binding and the
// cache, in one step under the backend's lock, the lock hold counts a Start
// under in the step that takes the device slot (b.holder, b.starts) and
// reserveProbe reads the slot under (B762 round 3). A Start that has taken
// the slot by then voids the answers, the cache recording nothing; one that
// takes it after finds them recorded, and opens the DAC after the decision.
// Round 2 compared the Starts apart from the Record, so a Start could take
// the slot between the two and open the DAC beside answers kept. The device
// slot itself stays the probe's alone until its node is closed (endProbe):
// this step holds the backend's lock, never the slot. ProbeDeciding runs just
// before the step and ProbeDecided just after it, the backend's lock let go.
func (b *Backend) takeProbe(starts uint64, bd, now BoundDAC, bound bool, epoch alsapcm.ProbeEpoch, rep alsapcm.ProbeReport) string {
	b.probeDeciding()
	defer b.probeDecided()
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.starts.Load() != starts:
		return "a session took the device while it ran"
	case !bound || now.Card != bd.Card || now.Enumeration != bd.Enumeration || !binding.SameUnit(now.Identity, bd.Identity) || now.Identity.DevNum != bd.Identity.DevNum:
		return "the binding no longer resolves to the unit it probed"
	case rep.Opened && !b.cfg.DSD.Probes.Record(epoch, rep):
		return "its model's answers moved on while it ran"
	}
	return ""
}

// probedUnit is the probe's check of the node it opened (ProbeRequest.Check):
// the node's card is the unit the probe was begun for, as m read it with key,
// its model's: the same unit at the same place and enumeration
// (binding.SameUnit and its DevNum, read: 0 is a device number that did not
// read, which tells no enumeration from another, so two of them are no
// enumeration in common; round 3), of the same model and firmware. Its
// answers are recorded under that unit alone (alsapcm.ProbeUnit, and the
// model's driver_accepted), so a unit swapped into the bound place between the
// capture and the open, which the binding's own check passes, is asked nothing
// (round 2).
func probedUnit(box DSDBox, m alsa.UnitModel, key dacprofile.Key) func(alsapcm.NodeInfo) error {
	return func(n alsapcm.NodeInfo) error {
		now, err := box.UnitModel(n.Card)
		if err != nil {
			return fmt.Errorf("player backend: the probed node's unit did not read: %w", err)
		}
		k, err := dsdstate.KeyOf(now.USB, now.Product)
		if err != nil || !k.Equal(key) || !binding.SameUnit(now.USB, m.USB) || now.USB.DevNum <= 0 || now.USB.DevNum != m.USB.DevNum || canonicalBCD(now.BCDDevice) != canonicalBCD(m.BCDDevice) {
			return &alsapcm.Error{Reason: alsapcm.ReasonWrongDevice, Op: "probe", Endpoint: alsapcm.Endpoint{Card: n.Card, Device: n.Device},
				Detail: "the node opened is not the unit the probe was begun for (its place, enumeration, model or firmware differs)"}
		}
		return nil
	}
}

// noteProbed records what a probe accepted as driver_accepted (the design's
// section 5): each native format at its rate, and each DoP carrier whose
// refined significant bits are 24 to 32 (alsapcm.ProbeResult.DoPCarrier), as
// its DoP tuple's. Nothing with no kernel release or firmware.
func (b *Backend) noteProbed(key dacprofile.Key, bcd string, rep alsapcm.ProbeReport) {
	kernel, fw := b.cfg.DSD.Kernel, canonicalBCD(bcd)
	if kernel == "" || fw == "" {
		return
	}
	type tuple struct {
		transport, format string
		bitRate           int
	}
	var accepted []tuple
	for _, r := range rep.Results {
		if r.Answer != alsapcm.ProbeAccepted {
			continue
		}
		if w := format.DSDWordBytes(r.Candidate.Format); w > 0 {
			accepted = append(accepted, tuple{dsdstate.TransportNative, string(r.Candidate.Format), r.Candidate.RateHz * 8 * w})
		} else if r.Candidate.Format == format.S32LE && r.DoPCarrier() && nativeLogicalRate(r.Candidate.RateHz*16) {
			accepted = append(accepted, tuple{dsdstate.TransportDoP, string(format.S32LE), r.Candidate.RateHz * 16})
		}
	}
	if len(accepted) == 0 {
		return
	}
	b.recordDSD("driver_accepted (probe)", key, func(d *dsdstate.DAC, now time.Time) bool {
		changed := false
		for _, a := range accepted {
			if !nativeLogicalRate(a.bitRate) {
				continue
			}
			e := evidenceOf(d, a.transport, a.format, a.bitRate, fw)
			if e.DriverAcceptedAt.IsZero() || e.Kernel != kernel {
				e.DriverAcceptedAt, e.Kernel, changed = now.UTC().Truncate(time.Second), kernel, true
			}
		}
		return changed
	})
}

// ForgetDSDProbe drops the probe's answers for key's model and makes a probe
// due: the migration's part for a DAC (the design's section 5). The probe is
// owed (ProbeDSDPending), which the supervisor asks at its next tick, since
// no snapshot comes of an unchanged DAC, and asked anew (asked), so a run
// begun before it leaves it owed; and the probe's generation moves, in the
// step the answers are dropped, so a probe under way keeps nothing of what it
// asked before the part was made (publishProbe). A choice written for the DAC
// (B9) makes a probe due and owed too, and drops nothing (oweProbe).
func (b *Backend) ForgetDSDProbe(key dacprofile.Key) {
	ps := &b.probeState
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p := b.cfg.DSD; p != nil && p.Probes != nil {
		p.Probes.Forget(key.Canonical())
	}
	ps.gen++
	ps.asked++
	ps.forced, ps.pending = true, true
}
