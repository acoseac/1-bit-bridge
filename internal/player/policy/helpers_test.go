package policy

import (
	"encoding/binary"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// t0 is the tests' clock.
var t0 = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

const (
	dsd64  = dsdstate.DSD64
	dsd128 = dsdstate.DSD128
	dsd256 = dsdstate.DSD256
	// bcd is the unit's firmware, kernel the running kernel.
	bcd    = "0100"
	kernel = "6.12.47-v8-16k+"
)

func ptr[T any](v T) *T { return &v }

// dacKey is the test DAC's model.
func dacKey(t *testing.T) dacprofile.Key {
	t.Helper()
	k, err := dacprofile.NewKey(0x1234, 0x5678, ptr(0), ptr("Test DAC"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// otherModel is another DAC model, one the test DAC's port may hold next.
func otherModel(t *testing.T) dacprofile.Key {
	t.Helper()
	k, err := dacprofile.NewKey(0x1234, 0x9999, ptr(0), ptr("Other DAC"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The test DAC's playback altsets, on interface 1: S32_LE with 24
// significant bits (DoP's carriers and a conversion's packing), S24_3LE to
// 176.4 kHz, and native DSD_U32_BE at DSD64, DSD128 and DSD256's frame rates.
func s32Altset() alsa.Altset {
	return alsa.Altset{Interface: ptr(1), Altset: ptr(1), Formats: []string{"S32_LE"}, Channels: ptr(2),
		Rates: []int{44100, 88200, 176400, 352800, 705600}, Bits: ptr(24)}
}

func s24Altset() alsa.Altset {
	return alsa.Altset{Interface: ptr(1), Altset: ptr(2), Formats: []string{"S24_3LE"}, Channels: ptr(2),
		Rates: []int{44100, 88200, 176400}, Bits: ptr(24)}
}

func nativeAltset() alsa.Altset {
	return alsa.Altset{Interface: ptr(1), Altset: ptr(3), Formats: []string{"SPECIAL", "DSD_U32_BE"}, Channels: ptr(2),
		Rates: []int{88200, 176400, 352800}, Bits: ptr(32)}
}

// rig is the test DAC and what the box establishes of it, with the knobs
// the tests turn; inputs builds Effective's inputs from it.
type rig struct {
	t *testing.T
	// known gives the DAC a profile, with these claims.
	known         bool
	profileNative *dacprofile.Native
	profileDoP    *dacprofile.DoP
	timing        *dacprofile.Timing
	// The choice: the DAC's own when mode is set, else defaultMode; by is
	// who set it, the app when it is empty.
	mode        string
	max         int
	by          string
	defaultMode string
	// The live capabilities.
	altsets []alsa.Altset
	probe   map[alsapcm.ProbeCandidate]alsapcm.ProbeResult
	mixer   Mixer
	// The state's entry for the DAC.
	firstUse   []dsdstate.FirstUse
	listener   []dsdstate.Answer
	evidence   []dsdstate.Evidence
	confirmed  *dsdstate.DoPConfirmation
	unreadable bool
	// unitBCD is the unit's firmware now.
	unitBCD string
}

// newRig is a DAC that offers everything at every rate, with no gain stage
// and nothing the state holds for it.
func newRig(t *testing.T) *rig {
	r := &rig{t: t, defaultMode: dsdstate.ModeAuto, unitBCD: bcd,
		altsets: []alsa.Altset{s32Altset(), s24Altset(), nativeAltset()},
		probe:   map[alsapcm.ProbeCandidate]alsapcm.ProbeResult{},
		mixer:   cleanMixer(),
	}
	for _, rate := range dsdRates {
		r.answer(format.DSDU32BE, rate/32, alsapcm.ProbeAccepted, 32)
		r.answer(format.S32LE, rate/16, alsapcm.ProbeAccepted, 24)
	}
	r.answer(format.S243LE, 176400, alsapcm.ProbeAccepted, 24)
	return r
}

// answer sets the probe's answer for f at rateHz, with the refined
// significant bits and PAUSE offered when accepted.
func (r *rig) answer(f format.ALSAFormat, rateHz int, a alsapcm.ProbeAnswer, msbits int) {
	c := alsapcm.ProbeCandidate{Format: f, RateHz: rateHz}
	res := alsapcm.ProbeResult{Candidate: c, Answer: a}
	if a == alsapcm.ProbeAccepted {
		res.Msbits, res.Pause = msbits, true
	}
	r.probe[c] = res
}

// everything gives the DAC a profile that claims native and DoP to DSD256,
// a choice of mode, both transports' first use acknowledged at every rate,
// a clean answer for both at every rate, and DoP confirmed: every positive
// input there is.
func (r *rig) everything(mode string) *rig {
	r.known = true
	r.profileNative = &dacprofile.Native{MaxBitRate: dsd256, Formats: []string{"DSD_U32_BE"}, Source: dacprofile.Source{Kind: dacprofile.SourceCapture, Ref: "owner capture, Linux, 2026-11-04"}}
	r.profileDoP = &dacprofile.DoP{HeardMaxBitRate: dsd256, StatedMaxBitRate: dsd256}
	r.mode = mode
	r.acknowledge(dsdstate.TransportNative)
	r.acknowledge(dsdstate.TransportDoP)
	for _, rate := range dsdRates {
		r.hear(dsdstate.TransportNative, rate, dsdstate.AnswerClean)
		r.hear(dsdstate.TransportDoP, rate, dsdstate.AnswerClean)
	}
	r.confirm()
	return r
}

// acknowledge acknowledges transport's first use at every rate, under the
// unit's firmware, as a write does.
func (r *rig) acknowledge(transport string, rates ...int) {
	r.firstUse = slices.DeleteFunc(r.firstUse, func(f dsdstate.FirstUse) bool { return f.Transport == transport })
	f := dsdstate.FirstUse{Transport: transport, BCDDevice: bcd, AcknowledgedAt: t0}
	if len(rates) > 0 {
		f.BitRates = rates
	}
	r.firstUse = append(r.firstUse, f)
}

// hear makes answer the latest listener answer for transport at rate under
// the unit's firmware, replacing any earlier one, as the state does.
func (r *rig) hear(transport string, rate int, answer string) {
	r.listener = slices.DeleteFunc(r.listener, func(a dsdstate.Answer) bool {
		return a.Transport == transport && a.BitRate == rate && a.BCDDevice == bcd
	})
	r.listener = append(r.listener, dsdstate.Answer{Transport: transport, BitRate: rate, BCDDevice: bcd, Answer: answer, On: "2026-11-05"})
}

// forget drops the listener answer for transport at rate.
func (r *rig) forget(transport string, rate int) {
	r.listener = slices.DeleteFunc(r.listener, func(a dsdstate.Answer) bool { return a.Transport == transport && a.BitRate == rate })
}

func (r *rig) confirm() {
	r.confirmed = &dsdstate.DoPConfirmation{BCDDevice: bcd, ConfirmedAt: t0, SetBy: dsdstate.ByApp}
}

// withoutNative takes native DSD away: no altset lists it.
func (r *rig) withoutNative() *rig {
	r.altsets = slices.DeleteFunc(r.altsets, func(a alsa.Altset) bool { return a.DSDFormat() != "" })
	return r
}

// withoutCarrier takes the DoP carrier away: no altset lists S32_LE.
func (r *rig) withoutCarrier() *rig {
	r.altsets = slices.DeleteFunc(r.altsets, func(a alsa.Altset) bool { return slices.Contains(a.Formats, "S32_LE") })
	return r
}

func (r *rig) inputs() Inputs {
	key := dacKey(r.t)
	in := Inputs{Unit: dacprofile.Unit{Key: key, BCDDevice: r.unitBCD}, Kernel: kernel, Altsets: r.altsets, Mixer: r.mixer}
	for _, res := range r.probe {
		in.Probe = append(in.Probe, res)
	}
	if r.known {
		iface, _ := key.Interface()
		product, _ := key.Product()
		in.Profiles = &dacprofile.Database{SchemaVersion: dacprofile.SchemaVersion, Profiles: []dacprofile.Profile{{
			ID: "test.dac", Name: "Test DAC",
			Match:  []dacprofile.Match{{VendorID: "1234", ProductID: "5678", Interface: &iface, Product: &product}},
			Native: r.profileNative, DoP: r.profileDoP, Timing: r.timing,
		}}}
	}
	in.State = dsdstate.State{DefaultMode: r.defaultMode}
	d := dsdstate.DAC{Key: key, DoPConfirmed: r.confirmed, FirstUse: r.firstUse, Listener: r.listener, Evidence: r.evidence}
	if r.mode != "" {
		by := r.by
		if by == "" {
			by = dsdstate.ByApp
		}
		d.Choice = &dsdstate.Choice{Mode: r.mode, MaxBitRate: r.max, Revision: 1, SetAt: t0, SetBy: by}
	}
	in.State.DACs = []dsdstate.DAC{d}
	if r.unreadable {
		in.State = dsdstate.Refusing(t0)
	}
	return in
}

// plan is the set's plan at rate.
func (r *rig) plan(rate int) Plan { return Effective(r.inputs()).At(rate).Plan }

// wantOutcome fails t unless p is outcome, with reason for a refusal.
func wantOutcome(t *testing.T, what string, p Plan, outcome Outcome, reason ...any) {
	t.Helper()
	if p.Outcome != outcome || len(reason) > 0 && p.Reason != reason[0] {
		t.Errorf("%s: %s %s (%s), want %s %v", what, p.Outcome, p.Reason, p.Detail, outcome, reason)
	}
}

// The tests' USB descriptors, as sysfs gives them: the device descriptor,
// then the configuration's.
func usbDescriptors(config byte, descs ...[]byte) []byte {
	out := []byte{18, 0x01, 0x00, 0x02, 0xef, 0x02, 0x01, 64, 0x34, 0x12, 0x78, 0x56, 0x00, 0x01, 1, 2, 3, 1}
	var body []byte
	for _, d := range descs {
		body = append(body, d...)
	}
	total := 9 + len(body)
	out = append(out, 9, 0x02, byte(total), byte(total>>8), 2, config, 0, 0x80, 50)
	return append(out, body...)
}

func iad(first, count byte) []byte { return []byte{8, 0x0b, first, count, 1, 0, 0x20, 0} }

// ifc is an interface's alternate setting: subclass 1 audio control, 2
// audio streaming; protocol 0 audio class 1, 0x20 audio class 2.
func ifc(number, setting, subclass, protocol, endpoints byte) []byte {
	return []byte{9, 0x04, number, setting, endpoints, 1, subclass, protocol, 0}
}

func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }

// The audio class 2 descriptors.
func uac2Header() []byte { return []byte{9, 0x24, 0x01, 0x00, 0x02, 0x08, 0x40, 0x00, 0x00} }
func uac2Clock(id byte) []byte {
	return []byte{8, 0x24, 0x0a, id, 0x03, 0x07, 0, 0}
}
func uac2IT(id byte, typ uint16) []byte {
	return append(append([]byte{17, 0x24, 0x02, id}, le16(typ)...), 0, 0x29, 2, 3, 0, 0, 0, 0, 0, 0, 0)
}
func uac2OT(id byte, typ uint16, source byte) []byte {
	return append(append([]byte{12, 0x24, 0x03, id}, le16(typ)...), 0, source, 0x29, 0, 0, 0)
}

// uac2FU is an audio class 2 feature unit: its master's controls, then each
// logical channel's, two bits a control (mute 0x3, volume 0xc).
func uac2FU(id, source byte, controls ...uint32) []byte {
	d := []byte{byte(6 + 4*len(controls)), 0x24, 0x06, id, source}
	for _, c := range controls {
		d = binary.LittleEndian.AppendUint32(d, c)
	}
	return append(d, 0)
}
func uac2General(link byte) []byte {
	return []byte{16, 0x24, 0x01, link, 0, 1, 1, 0, 0, 0, 2, 3, 0, 0, 0, 0}
}
func epOut() []byte      { return []byte{7, 0x05, 0x01, 0x05, 0x00, 0x04, 1} }
func epFeedback() []byte { return []byte{7, 0x05, 0x81, 0x11, 0x04, 0x00, 4} }
func epIn() []byte       { return []byte{7, 0x05, 0x82, 0x05, 0x00, 0x04, 1} }

// The audio class 1 descriptors.
func uac1Header(streams ...byte) []byte {
	return append([]byte{byte(8 + len(streams)), 0x24, 0x01, 0x00, 0x01, 0, 0, byte(len(streams))}, streams...)
}
func uac1IT(id byte, typ uint16) []byte {
	return append(append([]byte{12, 0x24, 0x02, id}, le16(typ)...), 0, 2, 3, 0, 0, 0)
}
func uac1OT(id byte, typ uint16, source byte) []byte {
	return append(append([]byte{9, 0x24, 0x03, id}, le16(typ)...), 0, source, 0)
}

// uac1FU is an audio class 1 feature unit with a control size of one byte:
// mute 0x01, volume 0x02, bass 0x04.
func uac1FU(id, source byte, controls ...byte) []byte {
	return append(append([]byte{byte(7 + len(controls)), 0x24, 0x06, id, source, 1}, controls...), 0)
}

// uac1FU2 is an audio class 1 feature unit with a control size of two
// bytes, for the controls past the first eight (bass boost 0x100, loudness
// 0x200).
func uac1FU2(id, source byte, controls ...uint16) []byte {
	d := []byte{byte(7 + 2*len(controls)), 0x24, 0x06, id, source, 2}
	for _, c := range controls {
		d = binary.LittleEndian.AppendUint16(d, c)
	}
	return append(d, 0)
}
func uac1General(link byte) []byte { return []byte{7, 0x24, 0x01, link, 1, 1, 0} }
func uac1EpOut() []byte            { return []byte{9, 0x05, 0x01, 0x09, 0xc8, 0x00, 1, 0, 0} }

// The terminal types.
const (
	usbStreaming = 0x0101
	speaker      = 0x0301
	headphones   = 0x0302
	microphone   = 0x0201
	spdif        = 0x0605
)

// stream2 is an audio class 2 streaming interface's settings: an idle
// setting 0, and settings 1 to 3 linked to link, each streaming toward the
// device.
func stream2(number, link byte) [][]byte {
	out := [][]byte{ifc(number, 0, 2, 0x20, 0)}
	for setting := byte(1); setting <= 3; setting++ {
		out = append(out, ifc(number, setting, 2, 0x20, 2), uac2General(link), epOut(), epFeedback())
	}
	return out
}

// dac2 is the test DAC's descriptors in audio class 2: an interface
// association over interfaces 0 and 1, the control interface 0 holding
// units, and streaming interface 1 linked to input terminal 2.
func dac2(units ...[]byte) []byte {
	descs := [][]byte{iad(0, 2), ifc(0, 0, 1, 0x20, 0), uac2Header(), uac2Clock(0x29)}
	descs = append(descs, units...)
	descs = append(descs, stream2(1, 2)...)
	return usbDescriptors(1, descs...)
}

// cleanMixer is a card with no gain stage: input terminal 2 straight to a
// speaker, and no control.
func cleanMixer() Mixer {
	return Mixer{Descriptors: dac2(uac2IT(2, usbStreaming), uac2OT(20, speaker, 2)), Config: 1, ControlsRead: true, UnitsRead: true}
}

// typeEnumerated is the enumerated control element type (asound.h).
const typeEnumerated = 3

// The TLVs, as alsactl reads them: type, length, payload.
func tlv(kind uint32, words ...int32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, kind)
	b = binary.LittleEndian.AppendUint32(b, uint32(4*len(words)))
	for _, w := range words {
		b = binary.LittleEndian.AppendUint32(b, uint32(w))
	}
	return b
}
func dbMinMax(min, max int32) []byte     { return tlv(tlvDBMinMax, min, max) }
func dbMinMaxMute(min, max int32) []byte { return tlv(tlvDBMinMaxMut, min, max) }
func dbScaleTLV(min int32, step uint16, mute bool) []byte {
	sm := int32(step)
	if mute {
		sm |= 0x10000
	}
	return tlv(tlvDBScale, min, sm)
}
func dbLinear(min, max int32) []byte { return tlv(tlvDBLinear, min, max) }

// dbRange is a DB_RANGE of parts, each its range and its TLV.
func dbRange(parts ...[]byte) []byte {
	var payload []byte
	for _, p := range parts {
		payload = append(payload, p...)
	}
	b := binary.LittleEndian.AppendUint32(nil, tlvDBRange)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(payload)))
	return append(b, payload...)
}
func rangePart(min, max int32, inner []byte) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(min))
	b = binary.LittleEndian.AppendUint32(b, uint32(max))
	return append(b, inner...)
}
func container(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	b := binary.LittleEndian.AppendUint32(nil, tlvContainer)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(payload)))
	return append(b, payload...)
}

// integer is a MIXER integer control as alsactl reads it: one value a
// channel, its range min to max, and its TLV.
func integer(name string, index uint32, min, max int64, tlv []byte, values ...int64) alsactl.Control {
	c := alsactl.Control{Interface: ifaceMixer, Name: name, Index: index, Type: typeInteger, Access: 1, Count: uint32(len(values)), Info: make([]byte, 128), TLV: tlv}
	if tlv != nil {
		c.Access |= 1 << 4
	}
	binary.LittleEndian.PutUint64(c.Info[0:], uint64(min))
	binary.LittleEndian.PutUint64(c.Info[8:], uint64(max))
	binary.LittleEndian.PutUint64(c.Info[16:], 1)
	for _, v := range values {
		c.Values = binary.LittleEndian.AppendUint64(c.Values, uint64(v))
	}
	return c
}

// volume is a USB volume control at value of 0 to 255, -127 dB to 0 dB,
// on two channels.
func volume(name string, value int64) alsactl.Control {
	return integer(name, 0, 0, 255, dbMinMaxMute(-12700, 0), value, value)
}

// unreadable is a MIXER control alsactl could not read: its identity, its
// ReadError, its type unknown (control_linux.go).
func unreadable(name string, index uint32) alsactl.Control {
	return alsactl.Control{Interface: ifaceMixer, Name: name, Index: index, ReadError: "input/output error"}
}

// switchControl is a MIXER boolean control, off.
func switchControl(name string) alsactl.Control {
	return alsactl.Control{Interface: ifaceMixer, Name: name, Type: typeBoolean, Access: 1, Count: 1, Values: make([]byte, 8), Info: make([]byte, 128)}
}

// boolean is a MIXER boolean control with one value a channel, 1 on.
func boolean(name string, values ...int64) alsactl.Control {
	c := alsactl.Control{Interface: ifaceMixer, Name: name, Type: typeBoolean, Access: 1, Count: uint32(len(values)), Info: make([]byte, 128)}
	for _, v := range values {
		c.Values = binary.LittleEndian.AppendUint64(c.Values, uint64(v))
	}
	return c
}

// withVolumeUnit puts feature unit 10, master mute and volume, between
// input terminal 2 and the speaker, the kernel's table naming its controls,
// and the card's controls.
func withVolumeUnit(m Mixer, controls ...alsactl.Control) Mixer {
	m.Descriptors = dac2(uac2IT(2, usbStreaming), uac2FU(10, 2, 0x0f, 0, 0), uac2OT(20, speaker, 10))
	m.Units = []UnitControl{entry(0, 10, selMute, "PCM Playback Switch"), entry(0, 10, selVolume, "PCM Playback Volume")}
	m.Controls = alsactl.Snapshot{Controls: controls}
	return m
}

// selVolume is a feature unit's volume, the selector the tests name most.
const selVolume = 0x02

// entry is the kernel's mixer table's entry for a control the driver made
// of selector of unit, in the block of audio control interface ctrlif, at
// index 0.
func entry(ctrlif, unit, selector int, name string) UnitControl {
	return UnitControl{ControlInterface: ptr(ctrlif), Unit: unit, Selector: selector, Name: name}
}

// gainAt is the gain check of the test DAC's DoP carrier altset on m, for
// profile on the unit's firmware.
func gainAt(m Mixer, profile *dacprofile.Profile) Gain {
	return CheckGain(m, []alsa.Altset{s32Altset()}, profile, bcd)
}
