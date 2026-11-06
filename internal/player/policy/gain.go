package policy

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// Mixer is what the DoP gain check reads of the bound card (the design's
// section 3.4), each a value B7's bounded adapters read: B5 reads nothing.
type Mixer struct {
	// Descriptors are the unit's USB descriptors as sysfs gives them (the
	// device's descriptors attribute: its device descriptor, then each
	// configuration's descriptors), nil when they did not read.
	Descriptors []byte
	// Config is the unit's active configuration, its bConfigurationValue,
	// which the card's interface's sysfs name carries
	// (<bus>-<port>:<config>.<interface>).
	Config int
	// Controls is the card's control snapshot, read with alsactl.Reader's
	// AllowUnreadable, so a control that could not be read stays in it with
	// its identity and its ReadError, its type unknown (0). ControlsRead is
	// false when no snapshot was read.
	Controls     alsactl.Snapshot
	ControlsRead bool
	// Units is the kernel's own table of the controls its USB audio driver
	// made of each unit (the card's /proc/asound/card<N>/usbmixer: one
	// block a mixer, "USB Mixer: … ctrlif=<interface> …", and in it a
	// "Unit:" line, its "Control: name=…, index=…" line and its "Info: …
	// control=<selector> …" line for each). UnitsRead is false when it did
	// not read.
	Units     []UnitControl
	UnitsRead bool
}

// UnitControl is one control the USB audio driver made of a unit, as the
// kernel's mixer table names it: the audio control interface whose block
// lists it (an audio function's units are numbered within the function
// alone), the unit's ID, the class's control selector it was made of, and
// the control's name and index on the card's MIXER interface.
type UnitControl struct {
	// ControlInterface is the block's ctrlif, nil where the table does not
	// say: then the control cannot be put in a function, and a stage of its
	// unit's ID is unverifiable.
	ControlInterface *int
	Unit             int
	// Selector is the Info line's control=, 0 where the table does not say:
	// then the control cannot be told from its unit's others, and the stage
	// is unverifiable.
	Selector int
	Name     string
	// Index is the control's index, as the card's controls hold it.
	Index uint32
}

// Gain is the DoP gain check's verdict at a rate.
type Gain struct {
	// Unity reports that the check passed: the playback path has no stage
	// and the card no level control, or every stage is visible, its
	// switches off, and every level control at unity.
	Unity bool
	// Reason is dop_gain_unverifiable or dop_gain_not_unity when the check
	// did not pass, unverifiable outranking not unity, since a stage the
	// check cannot see refuses whatever the levels read; Detail says which
	// unit or control, never a serial or a port.
	Reason engine.Reason
	Detail string
}

// The ALSA control element's interface and types the check reads (asound.h,
// SNDRV_CTL_ELEM_IFACE_* and SNDRV_CTL_ELEM_TYPE_*).
const (
	ifaceMixer    = 2
	typeBoolean   = 1
	typeInteger   = 2
	typeInteger64 = 6
)

// controlInterfaces are the control interfaces by the names a profile's
// control names them by (dacprofile.ControlID, as amixer prints them).
var controlInterfaces = map[string]uint32{"CARD": 0, "HWDEP": 1, "MIXER": 2, "PCM": 3, "RAWMIDI": 4, "TIMER": 5, "SEQUENCER": 6}

// CheckGain is the DoP gain check (the design's section 3.4) for a stream
// through each of sels, the playback altsets that list the DoP carrier at a
// rate: every one of them must pass, since the kernel chooses among them at
// the open. It fails closed:
//   - Descriptors, controls and the kernel's unit table must have read, and
//     the descriptors must give each altset's playback path (ParsePlayback):
//     a topology that does not resolve is refused, never taken for clean.
//   - On the path, a selector unit passes samples as they are, and a feature
//     unit is a stage when it declares any control but mute and audio class
//     2's read-only underflow and overflow (B748's decision): a level
//     (volume, bass, mid, treble, graphic equalizer, delay, input gain, input
//     gain pad) or a switch (automatic gain, bass boost, loudness, the phase
//     inverter), each of which rewrites the DoP markers and the DSD bits; a
//     mute alone makes none, since muted DoP is silence, not altered bits.
//     Every other unit (a mixer, processing, extension or effect unit, a
//     sample rate converter) is unverifiable: no control shows what it does
//     to the samples.
//   - A stage maps to the controls the kernel made of it, by the kernel's
//     table, never by a name: the driver synthesizes control names (from
//     string descriptors, terminal types and quirk maps), so a name alone
//     may not identify a unit. An entry counts only in the block of the
//     path's audio control interface (ctrlif), since entity IDs are unique
//     within an audio function alone, and by the control selector it was
//     made of; an entry for the stage's unit that does not say either is
//     unverifiable. The stage must have a visible MIXER control for each
//     control the driver makes of each declared control, one on the master
//     channel and one on the logical channels together: an integer, or one
//     that could not be read, for a level; a boolean, or one that could not
//     be read, for a switch. A stage with fewer is one the check cannot see,
//     a missing control that never stands for unity.
//   - Each switch the table maps to a stage must read off on every channel;
//     on, or not readable as off, it is dop_gain_unverifiable.
//   - A level control is a MIXER-interface control that is an integer, or
//     that could not be read, so its type is unknown (asked before any type
//     filter), whose name does not mark it a capture control (captureNamed),
//     whatever else its name says; and every control the table maps to a
//     level of a stage on the path, whatever its name. Every level control
//     on the card counts, not only the path's.
//   - Each level control must be at unity: on a known DAC, one of its
//     profile's analogVolumeControls (passed over), or one of its
//     unityAtRawMax controls at exactly its rawMax on every channel, its
//     range's maximum still rawMax, on a firmware the entry lists, each
//     matched by interface, name and index exactly; otherwise readable and
//     reading exactly 0 dB on every channel by its dB TLV (DB_SCALE,
//     DB_MINMAX and its mute variant, DB_LINEAR, DB_RANGE, or one of those
//     in a container). One off 0 dB, or a raw-maximum control below its
//     rawMax, is dop_gain_not_unity; one that cannot be read, has no dB
//     scale, or a raw-maximum control whose range or firmware changed, is
//     dop_gain_unverifiable.
//
// Elsewhere, switches and enumerations are not gains: a mute is silence,
// not corruption, and an enumeration that selects a DSP mode is the profile
// caveats' domain. profile is the matched profile, nil for an unknown DAC;
// bcd the unit's firmware, "" when it did not read.
func CheckGain(m Mixer, sels []alsa.Altset, profile *dacprofile.Profile, bcd string) Gain {
	if v, err := usbid.ParseHex16(bcd); err == nil {
		bcd = usbid.Hex16(v)
	} else {
		bcd = ""
	}
	return newGainCheck(m, profile, bcd).at(sels)
}

// gainCheck is the gain check's reading of one card.
type gainCheck struct {
	m       Mixer
	profile *dacprofile.Profile
	bcd     string
}

func newGainCheck(m Mixer, profile *dacprofile.Profile, bcd string) *gainCheck {
	return &gainCheck{m: m, profile: profile, bcd: bcd}
}

// verdict collects the check's findings: unverifiable outranks not unity,
// and the first of each kind is its detail.
type verdict struct {
	unverifiable, notUnity string
}

func (v *verdict) unknown(format string, a ...any) {
	if v.unverifiable == "" {
		v.unverifiable = fmt.Sprintf(format, a...)
	}
}

func (v *verdict) off(format string, a ...any) {
	if v.notUnity == "" {
		v.notUnity = fmt.Sprintf(format, a...)
	}
}

func (v verdict) gain() Gain {
	switch {
	case v.unverifiable != "":
		return Gain{Reason: ReasonGainUnverifiable, Detail: v.unverifiable}
	case v.notUnity != "":
		return Gain{Reason: ReasonGainNotUnity, Detail: v.notUnity}
	}
	return Gain{Unity: true}
}

// controlKey is a control's identity on the card: its interface, device,
// subdevice, name and index.
type controlKey struct {
	iface, device, subdevice uint32
	name                     string
	index                    uint32
}

func keyOf(c alsactl.Control) controlKey {
	return controlKey{c.Interface, c.Device, c.Subdevice, c.Name, c.Index}
}

func (g *gainCheck) at(sels []alsa.Altset) Gain {
	var v verdict
	if g.m.Descriptors == nil {
		v.unknown("the unit's USB descriptors did not read")
	}
	if !g.m.ControlsRead {
		v.unknown("the card's controls did not read")
	}
	if len(sels) == 0 {
		v.unknown("no playback altset carries the stream")
	}
	if v.unverifiable != "" {
		return v.gain()
	}
	// stages are the feature units the paths make stages of, by the audio
	// control interface of their function and their ID.
	stages := map[[2]int]Unit{}
	for _, a := range sels {
		if a.Interface == nil || a.Altset == nil {
			v.unknown("a playback altset's interface or alternate setting did not read")
			continue
		}
		p, err := ParsePlayback(g.m.Descriptors, Selection{Config: g.m.Config, Interface: *a.Interface, Altset: *a.Altset})
		if err != nil {
			v.unknown("the playback path of interface %d, altset %d does not resolve: %v", *a.Interface, *a.Altset, err)
			continue
		}
		for _, u := range p.Units {
			switch u.Kind {
			case UnitSelector:
			case UnitFeature:
				if len(u.Controls) > 0 {
					stages[[2]int{p.ControlInterface, u.ID}] = u
				}
			default:
				v.unknown("unit %d of audio control interface %d on the playback path is a %s unit, which no control shows the effect of", u.ID, p.ControlInterface, u.Kind)
			}
		}
	}
	keys := make([][2]int, 0, len(stages))
	for k := range stages {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]int) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
	controls := map[controlKey]alsactl.Control{}
	for _, c := range g.m.Controls.Controls {
		controls[keyOf(c)] = c
	}
	// mapped are the controls the kernel's table makes of a level of a stage
	// on the path: level controls whatever their names.
	mapped := map[controlKey]bool{}
	for _, k := range keys {
		g.stage(k[0], stages[k], controls, mapped, &v)
	}
	for _, c := range g.m.Controls.Controls {
		if !level(c, mapped[keyOf(c)]) {
			continue
		}
		g.judge(c, &v)
	}
	return v.gain()
}

// stage maps feature unit u of the audio function whose control interface
// is ctrlif to the controls the kernel's table says the driver made of it
// (CheckGain's doc), and judges what it finds: each switch read off on every
// channel, each level marked a level control whatever its name. Entries of
// mute, underflow and overflow change nothing.
func (g *gainCheck) stage(ctrlif int, u Unit, controls map[controlKey]alsactl.Control, mapped map[controlKey]bool, v *verdict) {
	if !g.m.UnitsRead {
		v.unknown("the kernel's table of the controls it made of each unit did not read, so feature unit %d's controls cannot be found", u.ID)
		return
	}
	// found are the visible controls the table makes of the unit, by
	// selector.
	found := map[int][]alsactl.Control{}
	for _, uc := range g.m.Units {
		if uc.Unit != u.ID {
			continue
		}
		switch {
		case uc.ControlInterface == nil:
			v.unknown("the kernel's table does not say which audio function lists unit %d's control %q", u.ID, uc.Name)
			continue
		case *uc.ControlInterface != ctrlif:
			continue
		case uc.Selector == 0:
			v.unknown("the kernel's table does not say which control of feature unit %d %q is", u.ID, uc.Name)
			continue
		}
		c, ok := controls[controlKey{iface: ifaceMixer, name: uc.Name, index: uc.Index}]
		if ok && !slices.ContainsFunc(found[uc.Selector], func(f alsactl.Control) bool { return keyOf(f) == keyOf(c) }) {
			found[uc.Selector] = append(found[uc.Selector], c)
		}
	}
	for _, fc := range u.Controls {
		have := 0
		for _, c := range found[fc.Selector] {
			if fits(fc.Switch(), c) {
				have++
			}
		}
		if want := fc.controls(); have < want {
			v.unknown("feature unit %d on the playback path declares control %d %d times, and the kernel shows %d visible controls of it", u.ID, fc.Selector, want, have)
		}
	}
	selectors := make([]int, 0, len(found))
	for s := range found {
		selectors = append(selectors, s)
	}
	slices.Sort(selectors)
	for _, s := range selectors {
		if s == selMute || s == selUnderflow || s == selOverflow {
			continue
		}
		for _, c := range found[s] {
			if switchSelectors[s] {
				switchOff(u.ID, c, v)
			} else {
				mapped[keyOf(c)] = true
			}
		}
	}
}

// fits reports whether c can be the control the driver made of a switch (a
// boolean) or of a level (an integer): either, when it could not be read.
func fits(isSwitch bool, c alsactl.Control) bool {
	switch {
	case c.ReadError != "":
		return true
	case isSwitch:
		return c.Type == typeBoolean
	}
	return c.Type == typeInteger || c.Type == typeInteger64
}

// switchOff adds to v what c, a stage's switch, does: on any channel, or not
// readable as a boolean, it is a stage the check cannot see through.
func switchOff(unit int, c alsactl.Control, v *verdict) {
	if c.ReadError != "" || c.Type != typeBoolean || c.Count == 0 || c.Count > 128 || len(c.Values) != int(c.Count)*8 {
		v.unknown("feature unit %d's switch %q, index %d, cannot be read as off", unit, c.Name, c.Index)
		return
	}
	for ch := 0; ch < int(c.Count); ch++ {
		if binary.LittleEndian.Uint64(c.Values[8*ch:]) != 0 {
			v.unknown("feature unit %d's switch %q, index %d, is on on channel %d, and rewrites the samples", unit, c.Name, c.Index, ch+1)
			return
		}
	}
}

// level reports whether c is a level control (CheckGain's doc): a MIXER
// control that is an integer, or whose type is unknown because it could not
// be read, asked before the type is, and whose name does not mark it a
// capture control unless the kernel's table maps it to a gain stage on the
// playback path.
func level(c alsactl.Control, mapped bool) bool {
	if c.Interface != ifaceMixer {
		return false
	}
	if c.ReadError == "" && c.Type != typeInteger && c.Type != typeInteger64 {
		return false
	}
	return mapped || !captureNamed(c.Name)
}

// captureNamed reports whether name marks its control a capture control by
// ALSA's naming (SOURCE DIRECTION FUNCTION, Documentation/sound/designs/
// control-names.rst): its direction, the word before its last, is Capture,
// as in "Mic Capture Volume", "Capture Volume" or "Line Bypass Capture
// Volume". The words are split on single spaces and compared exactly, so a
// name only mentioning a capture elsewhere ("Capture Card Playback Volume")
// counts.
func captureNamed(name string) bool {
	words := strings.Split(name, " ")
	return len(words) >= 2 && words[len(words)-2] == "Capture"
}

// listed reports whether c is the control a profile names by id: on its
// interface, by its name and its index, exactly, at device and subdevice 0,
// where a profile's MIXER control is.
func listed(c alsactl.Control, id dacprofile.ControlID) bool {
	iface, ok := controlInterfaces[id.Interface]
	return ok && id.Index >= 0 && c.Interface == iface && c.Device == 0 && c.Subdevice == 0 && c.Name == id.Name && c.Index == uint32(id.Index)
}

// judge adds what c, a level control, does to the verdict.
func (g *gainCheck) judge(c alsactl.Control, v *verdict) {
	if d := g.profileDoP(); d != nil {
		if slices.ContainsFunc(d.AnalogVolumeControls, func(a dacprofile.AnalogControl) bool { return listed(c, a.ControlID) }) {
			return
		}
		if i := slices.IndexFunc(d.UnityAtRawMax, func(r dacprofile.RawMaxControl) bool { return listed(c, r.ControlID) }); i >= 0 {
			g.judgeRawMax(c, d.UnityAtRawMax[i], v)
			return
		}
	}
	if c.ReadError != "" {
		v.unknown("level control %q, index %d, cannot be read", c.Name, c.Index)
		return
	}
	values, rmin, rmax, err := integerReading(c)
	if err != nil {
		v.unknown("level control %q, index %d: %v", c.Name, c.Index, err)
		return
	}
	scale, err := dbScaleOf(c.TLV)
	if err != nil {
		v.unknown("level control %q, index %d, has no dB scale the check reads: %v", c.Name, c.Index, err)
		return
	}
	for ch, x := range values {
		unity, err := scale.unity(x, rmin, rmax)
		switch {
		case err != nil:
			v.unknown("level control %q, index %d, channel %d: %v", c.Name, c.Index, ch+1, err)
			return
		case !unity:
			v.off("level control %q, index %d, reads off 0 dB on channel %d", c.Name, c.Index, ch+1)
			return
		}
	}
}

// judgeRawMax judges a level control the profile names among its
// unityAtRawMax controls: it passes only on a firmware the entry lists, its
// range's maximum still the entry's rawMax and every channel reading
// exactly that.
func (g *gainCheck) judgeRawMax(c alsactl.Control, e dacprofile.RawMaxControl, v *verdict) {
	listedFirmware := g.bcd != "" && slices.ContainsFunc(e.BCDDevice, func(b string) bool {
		x, err := usbid.ParseHex16(b)
		return err == nil && usbid.Hex16(x) == g.bcd
	})
	if !listedFirmware {
		v.unknown("raw-maximum control %q, index %d, is on a firmware its profile entry does not list", c.Name, c.Index)
		return
	}
	if c.ReadError != "" {
		v.unknown("raw-maximum control %q, index %d, cannot be read", c.Name, c.Index)
		return
	}
	values, _, rmax, err := integerReading(c)
	if err != nil {
		v.unknown("raw-maximum control %q, index %d: %v", c.Name, c.Index, err)
		return
	}
	if rmax != e.RawMax {
		v.unknown("raw-maximum control %q, index %d: its range's maximum is no longer its profile's rawMax", c.Name, c.Index)
		return
	}
	for ch, x := range values {
		if x != e.RawMax {
			v.off("raw-maximum control %q, index %d, reads below its raw maximum on channel %d", c.Name, c.Index, ch+1)
			return
		}
	}
}

func (g *gainCheck) profileDoP() *dacprofile.DoP {
	if g.profile == nil {
		return nil
	}
	return g.profile.DoP
}

// integerReading is an integer control's values, one a channel, and its
// range, as alsactl reads them on the hosts its reader runs on (Linux on
// amd64 and arm64, little-endian): each value eight bytes, and the info's
// minimum and maximum the first sixteen of its value union.
func integerReading(c alsactl.Control) (values []int64, rmin, rmax int64, err error) {
	if c.Type != typeInteger && c.Type != typeInteger64 {
		return nil, 0, 0, errors.New("it is not an integer control")
	}
	if c.Count == 0 || c.Count > 128 || len(c.Values) != int(c.Count)*8 {
		return nil, 0, 0, errors.New("its values did not read whole")
	}
	if len(c.Info) < 16 {
		return nil, 0, 0, errors.New("its range did not read")
	}
	for i := 0; i < int(c.Count); i++ {
		values = append(values, int64(binary.LittleEndian.Uint64(c.Values[8*i:])))
	}
	return values, int64(binary.LittleEndian.Uint64(c.Info[0:])), int64(binary.LittleEndian.Uint64(c.Info[8:])), nil
}

// The TLV types the check reads (include/uapi/sound/tlv.h).
const (
	tlvContainer   = 0
	tlvDBScale     = 1
	tlvDBLinear    = 2
	tlvDBRange     = 3
	tlvDBMinMax    = 4
	tlvDBMinMaxMut = 5
	// tlvDBGainMute is SNDRV_CTL_TLVD_DB_GAIN_MUTE, a dB value that is mute.
	tlvDBGainMute = -9999999
	// tlvDepth bounds a container's and a range's nesting.
	tlvDepth = 4
)

// errNoScale is a TLV that holds no dB scale.
var errNoScale = errors.New("no dB scale")

// dbScale is one dB TLV: its type, its two values (DB_SCALE's minimum and
// step with its mute flag, or the minimum and maximum of the others), and a
// range's parts.
type dbScale struct {
	kind   uint32
	a, b   int64
	mute   bool
	ranges []dbPart
}

// dbPart is one part of a DB_RANGE: the values from min to max, which its
// own scale reads.
type dbPart struct {
	min, max int64
	scale    dbScale
}

// dbScaleOf is the dB scale a control's TLV gives, as alsactl reads it (the
// TLV's type, its length and its payload, host order, little-endian on the
// reader's hosts): a dB TLV, or the one dB TLV a container holds.
func dbScaleOf(tlv []byte) (dbScale, error) {
	if len(tlv) == 0 {
		return dbScale{}, errNoScale
	}
	s, n, err := parseTLV(tlv, 0)
	if err != nil {
		return dbScale{}, err
	}
	if n != len(tlv) {
		return dbScale{}, errors.New("its TLV holds bytes past its length")
	}
	return s, nil
}

// parseTLV reads one TLV from b: its scale, and the bytes it took, its
// payload rounded up to whole words.
func parseTLV(b []byte, depth int) (dbScale, int, error) {
	if depth > tlvDepth {
		return dbScale{}, 0, errors.New("its TLV nests too deep")
	}
	if len(b) < 8 {
		return dbScale{}, 0, errors.New("its TLV is cut short")
	}
	kind := binary.LittleEndian.Uint32(b[0:])
	n := binary.LittleEndian.Uint32(b[4:])
	if uint64(n) > uint64(len(b)-8) {
		return dbScale{}, 0, errors.New("its TLV's length runs past it")
	}
	payload := b[8 : 8+n]
	size := 8 + int(n+3)&^3
	if size > len(b) {
		size = len(b)
	}
	word := func(i int) int64 { return int64(int32(binary.LittleEndian.Uint32(payload[4*i:]))) }
	switch kind {
	case tlvContainer:
		var found []dbScale
		for p := payload; len(p) > 0; {
			s, used, err := parseTLV(p, depth+1)
			switch {
			case err == nil:
				found = append(found, s)
			case !errors.Is(err, errNoScale):
				return dbScale{}, 0, err
			}
			if used <= 0 {
				return dbScale{}, 0, errors.New("its TLV container is malformed")
			}
			p = p[used:]
		}
		switch len(found) {
		case 0:
			return dbScale{}, size, errNoScale
		case 1:
			return found[0], size, nil
		}
		return dbScale{}, 0, errors.New("its TLV container holds more than one dB scale")
	case tlvDBScale, tlvDBLinear, tlvDBMinMax, tlvDBMinMaxMut:
		if n < 8 {
			return dbScale{}, 0, errors.New("its dB TLV is cut short")
		}
		s := dbScale{kind: kind, a: word(0), b: word(1)}
		if kind == tlvDBScale {
			stepMute := binary.LittleEndian.Uint32(payload[4:])
			s.b, s.mute = int64(stepMute&0xffff), stepMute&0x10000 != 0
		}
		return s, size, nil
	case tlvDBRange:
		s := dbScale{kind: kind}
		for p := payload; len(p) > 0; {
			if len(p) < 16 {
				return dbScale{}, 0, errors.New("its dB range is cut short")
			}
			part := dbPart{min: int64(int32(binary.LittleEndian.Uint32(p[0:]))), max: int64(int32(binary.LittleEndian.Uint32(p[4:])))}
			inner, used, err := parseTLV(p[8:], depth+1)
			if errors.Is(err, errNoScale) {
				err = errors.New("a part of its dB range holds no dB scale")
			}
			if err != nil {
				return dbScale{}, 0, err
			}
			part.scale = inner
			s.ranges = append(s.ranges, part)
			p = p[8+used:]
		}
		if len(s.ranges) == 0 {
			return dbScale{}, 0, errors.New("its dB range holds no part")
		}
		return s, size, nil
	}
	return dbScale{}, size, errNoScale
}

// unity reports whether value x of a control whose range is rmin to rmax
// reads exactly 0 dB on s, in exact arithmetic over what alsa-lib's
// snd_tlv_convert_to_dB computes in integers or floating point: a value at
// a mute position is not unity, and a value no part of a range holds is an
// error.
func (s dbScale) unity(x, rmin, rmax int64) (bool, error) {
	switch s.kind {
	case tlvDBScale:
		if s.mute && x <= rmin {
			return false, nil
		}
		// min + (x - rmin) * step
		g := new(big.Int).Sub(big.NewInt(x), big.NewInt(rmin))
		g.Mul(g, big.NewInt(s.b))
		g.Add(g, big.NewInt(s.a))
		return g.Sign() == 0, nil
	case tlvDBMinMax, tlvDBMinMaxMut:
		mindb, maxdb := s.a, s.b
		switch {
		case x <= rmin || rmax <= rmin:
			return s.kind == tlvDBMinMax && mindb == 0, nil
		case x >= rmax:
			return maxdb == 0, nil
		}
		// mindb + (maxdb - mindb) * (x - rmin) / (rmax - rmin) == 0, scaled
		// by (rmax - rmin), which is positive here.
		lhs := new(big.Int).Mul(big.NewInt(maxdb-mindb), new(big.Int).Sub(big.NewInt(x), big.NewInt(rmin)))
		rhs := new(big.Int).Mul(big.NewInt(-mindb), new(big.Int).Sub(big.NewInt(rmax), big.NewInt(rmin)))
		return lhs.Cmp(rhs) == 0, nil
	case tlvDBLinear:
		mindb, maxdb := s.a, s.b
		switch {
		case x <= rmin || rmax <= rmin:
			return mindb == 0, nil
		case x >= rmax:
			return maxdb == 0, nil
		}
		return linearUnity(mindb, maxdb, x-rmin, rmax-rmin), nil
	case tlvDBRange:
		for _, p := range s.ranges {
			if p.min <= x && x <= p.max {
				return p.scale.unity(x, p.min, p.max)
			}
		}
		return false, errors.New("its value lies in no part of its dB range")
	}
	return false, errNoScale
}

// linearUnity reports whether a DB_LINEAR scale from mindb to maxdb (0.01
// dB) reads exactly 0 dB at the fraction num/den of its range, 0 < num <
// den: whether its amplitude, lmin + (lmax - lmin) * num/den with lmin and
// lmax 10^(dB/2000), lmin 0 for a mute minimum, is exactly 1. An amplitude
// 10^(k/2000) is rational only where k is a multiple of 2000, and with an
// irrational one at either end it is never exactly 1 between them.
func linearUnity(mindb, maxdb, num, den int64) bool {
	amp := func(db int64) (*big.Rat, bool) {
		if db <= tlvDBGainMute {
			return new(big.Rat), true
		}
		if db%2000 != 0 {
			return nil, false
		}
		e := db / 2000
		p := new(big.Int).Exp(big.NewInt(10), big.NewInt(max(e, -e)), nil)
		if e < 0 {
			return new(big.Rat).SetFrac(big.NewInt(1), p), true
		}
		return new(big.Rat).SetInt(p), true
	}
	lmin, ok1 := amp(mindb)
	lmax, ok2 := amp(maxdb)
	if !ok1 || !ok2 {
		return false
	}
	a := new(big.Rat).Sub(lmax, lmin)
	a.Mul(a, new(big.Rat).SetFrac(big.NewInt(num), big.NewInt(den)))
	a.Add(a, lmin)
	return a.Cmp(new(big.Rat).SetInt64(1)) == 0
}
