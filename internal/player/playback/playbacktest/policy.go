package playbacktest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// The effective DSD policy's fixtures (B728 B7): a test DAC the box
// describes through values a test sets (Box), the player's DSD state in
// memory (MemStore), and the policy over both (Policy). Nothing here reads a
// host tree or opens a node.

// The test DAC: its USB IDs, its product and manufacturer strings, its
// firmware, the interface its card is, and the kernel release the tests run
// under.
const (
	DACVendor       = 0x1234
	DACProduct      = 0x5678
	DACProductName  = "Test DAC"
	DACManufacturer = "Test"
	DACBCD          = "0100"
	Kernel          = "6.12.47-v8-16k+"
)

// DACKey is the test DAC's model key, as the state keys it
// (dsdstate.KeyOf): its IDs, interface 0 and its product string.
func DACKey() dacprofile.Key {
	k, err := dsdstate.KeyOf(DACUSB(0), DACProductName)
	if err != nil {
		panic(err)
	}
	return k
}

// DACUSB is the test DAC's unit as sysfs reads it, at enumeration devnum.
func DACUSB(devnum int) alsa.USBIdentity {
	if devnum == 0 {
		devnum = 2
	}
	return alsa.USBIdentity{ID: alsa.USBID{Vendor: DACVendor, Product: DACProduct}, Serial: usbid.New("TESTDAC-0001"), Port: "platform-xhci-hcd.0/usb2-1", Interface: 0, DevNum: devnum}
}

// DACUnit is the test DAC's model, its descriptors those of a card
// with no gain stage (CleanDescriptors).
func DACUnit() alsa.UnitModel {
	return alsa.UnitModel{USB: DACUSB(0), Manufacturer: DACManufacturer, Product: DACProductName, BCDDevice: DACBCD, Descriptors: CleanDescriptors(), Config: 1}
}

func ptr[T any](v T) *T { return &v }

// The test DAC's playback altsets, on interface 1: S32_LE with 24
// significant bits (DoP's carriers and a conversion's packing), S24_3LE to
// 176.4 kHz, and native DSD_U32_BE at DSD64, DSD128 and DSD256's frame rates.
func S32Altset() alsa.Altset {
	return alsa.Altset{Interface: ptr(1), Altset: ptr(1), Formats: []string{"S32_LE"}, Channels: ptr(2),
		Rates: []int{44100, 88200, 176400, 352800, 705600}, Bits: ptr(24)}
}

// S24Altset is S24_3LE to 176.4 kHz.
func S24Altset() alsa.Altset {
	return alsa.Altset{Interface: ptr(1), Altset: ptr(2), Formats: []string{"S24_3LE"}, Channels: ptr(2),
		Rates: []int{44100, 88200, 176400}, Bits: ptr(24)}
}

// NativeAltset is DSD_U32_BE at DSD64, DSD128 and DSD256's frame rates.
func NativeAltset() alsa.Altset {
	return alsa.Altset{Interface: ptr(1), Altset: ptr(3), Formats: []string{"SPECIAL", "DSD_U32_BE"}, Channels: ptr(2),
		Rates: []int{88200, 176400, 352800}, Bits: ptr(32)}
}

// AllAltsets are the three.
func AllAltsets() []alsa.Altset { return []alsa.Altset{S32Altset(), S24Altset(), NativeAltset()} }

// Box is a playback.DSDBox over values a test sets, each read counted.
type Box struct {
	mu       sync.Mutex
	units    map[int]alsa.UnitModel
	alts     map[[2]int][]alsa.Altset
	mixer    map[int][]byte
	controls map[int]alsactl.Snapshot
	reads    int
}

// NewBox is a box with the test DAC at card, its altsets all three, no gain
// stage and no control.
func NewBox(card int) *Box {
	b := &Box{units: map[int]alsa.UnitModel{}, alts: map[[2]int][]alsa.Altset{}, mixer: map[int][]byte{}, controls: map[int]alsactl.Snapshot{}}
	b.SetUnit(card, DACUnit())
	b.SetAltsets(card, 0, AllAltsets()...)
	b.SetMixer(card, []byte("USB Mixer: usb_id=0x12345678, ctrlif=0, ctlerr=0\nCard: Test DAC\n"))
	b.SetControls(card)
	return b
}

// SetUnit puts m at card; a zero UnitModel takes the card away.
func (b *Box) SetUnit(card int, m alsa.UnitModel) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.USB.ID.Vendor == 0 {
		delete(b.units, card)
		return
	}
	b.units[card] = m
}

// SetAltsets sets card's device's playback altsets.
func (b *Box) SetAltsets(card, device int, alts ...alsa.Altset) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.alts[[2]int{card, device}] = slices.Clone(alts)
}

// SetMixer sets card's usbmixer table.
func (b *Box) SetMixer(card int, table []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mixer[card] = slices.Clone(table)
}

// SetControls sets card's controls.
func (b *Box) SetControls(card int, cs ...alsactl.Control) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.controls[card] = alsactl.Snapshot{Card: card, Controls: slices.Clone(cs)}
}

// Reads is how many times the box was read.
func (b *Box) Reads() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

func (b *Box) UnitModel(card int) (alsa.UnitModel, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	m, ok := b.units[card]
	if !ok {
		return alsa.UnitModel{}, fmt.Errorf("card %d: %w", card, fs.ErrNotExist)
	}
	m.Descriptors = slices.Clone(m.Descriptors)
	return m, nil
}

func (b *Box) CardUnit(card int) (alsa.USBIdentity, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.units[card]
	return m.USB, ok
}

func (b *Box) PlaybackAltsets(card, device int) ([]alsa.Altset, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.alts[[2]int{card, device}]
	if !ok {
		return nil, errors.New("no stream file")
	}
	return slices.Clone(a), nil
}

func (b *Box) USBMixer(card int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.mixer[card]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return slices.Clone(t), nil
}

func (b *Box) ReadControls(card int) (alsactl.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.controls[card]
	if !ok {
		return alsactl.Snapshot{}, errors.New("no control node")
	}
	s.Controls = slices.Clone(s.Controls)
	return s, nil
}

// MemStore is the player's DSD state in memory: Update applies its change
// to a copy and keeps it, writing nothing, each write counted.
type MemStore struct {
	mu      sync.Mutex
	state   dsdstate.State
	writes  int
	changed chan struct{}
	// fail, when set, is what every Update answers, changing nothing.
	fail error
	// land, when set, is what the next Update answers once its change is
	// made, as dsdstate.Store answers a write whose rename landed and whose
	// barrier failed: the change is not held (landed keeps it, as the file
	// would), and every later Update is fenced (dsdstate.ErrLandedNotHeld).
	land   error
	landed *dsdstate.State
}

// NewMemStore holds st.
func NewMemStore(st dsdstate.State) *MemStore {
	return &MemStore{state: st.Clone(), changed: make(chan struct{})}
}

func (m *MemStore) State() dsdstate.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Clone()
}

func (m *MemStore) Update(fn func(*dsdstate.State) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	if m.landed != nil {
		return dsdstate.ErrLandedNotHeld
	}
	next := m.state.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	if err := m.land; err != nil {
		m.land, m.landed = nil, &next
		return fmt.Errorf("playbacktest: the state's write failed: %w", err)
	}
	m.state = next
	m.writes++
	m.changedLocked()
	return nil
}

// Land makes the next Update answer err, which wraps atomicwrite.ErrLanded,
// once its change is made, as a write whose rename landed and whose barrier
// failed: the store keeps the state as it was, the change is what the file
// holds (LandedState), and every Update after it is fenced (Landed).
func (m *MemStore) Land(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.land = err
}

// LandedState is the state a landed write left in the file, false while
// none did.
func (m *MemStore) LandedState() (dsdstate.State, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.landed == nil {
		return dsdstate.State{}, false
	}
	return m.landed.Clone(), true
}

// Landed is the fence a landed write left, as dsdstate.Store.Landed says it:
// dsdstate.ErrLandedNotHeld once a write landed, nil while none did.
func (m *MemStore) Landed() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.landed == nil {
		return nil
	}
	return dsdstate.ErrLandedNotHeld
}

// Set replaces the state, as another writer's write does.
func (m *MemStore) Set(st dsdstate.State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = st.Clone()
	m.changedLocked()
}

// Fail makes every later Update answer err, changing nothing; nil ends it.
func (m *MemStore) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

func (m *MemStore) changedLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// Changed is closed at the next write or Set.
func (m *MemStore) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

// Writes is how many writes landed.
func (m *MemStore) Writes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes
}

// Evidence is what the state holds as observed of the test DAC's transport in
// f at bitRate under its firmware, nil when nothing.
func Evidence(st dsdstate.State, transport, f string, bitRate int) *dsdstate.Evidence {
	d, ok := st.DAC(DACKey())
	if !ok {
		return nil
	}
	for i, e := range d.Evidence {
		if e.Transport == transport && e.Format == f && e.BitRate == bitRate && e.BCDDevice == DACBCD {
			return &d.Evidence[i]
		}
	}
	return nil
}

// FirstUseOf is the test DAC's first-use entry for transport, nil when none.
func FirstUseOf(st dsdstate.State, transport string) *dsdstate.FirstUse {
	d, ok := st.DAC(DACKey())
	if !ok {
		return nil
	}
	for i, f := range d.FirstUse {
		if f.Transport == transport {
			return &d.FirstUse[i]
		}
	}
	return nil
}

// MigratedState is the state the migration leaves (dsdstate.Migrate, B6) for
// the legacy keys dsdMode mode and allowDoP and an owner's record of the test
// DAC that approved the native and DoP rates given, on its firmware: what
// serve's player starts with on an owner's box after the upgrade.
func MigratedState(mode string, allowDoP bool, native, dop []int) dsdstate.State {
	allow := allowDoP
	st, _, err := dsdstate.Migrate(dsdstate.Defaults(), dsdstate.Input{
		Legacy: dsdstate.Legacy{DSDMode: mode, AllowDoP: &allow, DSDQualification: "dsd-qualification.json"},
		Bound:  &dsdstate.Bound{Key: DACKey()},
		Record: dsdstate.Record{Outcome: dsdstate.RecordTaken, SHA256: "5eed", BCDDevice: DACBCD, QualifiedOn: "2026-10-01",
			Native: slices.Clone(native), DoP: slices.Clone(dop)},
		Now: T0,
	})
	if err != nil {
		panic(err)
	}
	return st
}

// The DSD rates the tests play.
const (
	DSD64  = dsdstate.DSD64
	DSD128 = dsdstate.DSD128
	DSD256 = dsdstate.DSD256
)

// DSF is a stereo DSF file of DSD at bitRate, seconds long, least
// significant bit first, its channels' bytes fill(channel, index): the shape
// lsb_dsd64_partial_block.dsf has, blocks of 4,096 bytes a channel.
func DSF(bitRate int, seconds float64, fill func(ch, i int) byte) []byte {
	return DSFCut(bitRate, seconds, seconds, fill)
}

// DSFCut is DSF declaring seconds of DSD and holding only the first held
// seconds of it: a file cut short, which a decoder refuses as truncated once
// it reads past what it holds.
func DSFCut(bitRate int, seconds, held float64, fill func(ch, i int) byte) []byte {
	const block = 4096
	samples := uint64(seconds * float64(bitRate))
	perCh := int((samples + 7) / 8)
	blocks := (perCh + block - 1) / block
	keep := int(held * float64(bitRate) / 8)
	data := make([]byte, 0, blocks*block*2)
	for b := range blocks {
		for ch := range 2 {
			for i := range block {
				at := b*block + i
				v := byte(0)
				if at < perCh {
					v = fill(ch, at)
				}
				data = append(data, v)
			}
		}
	}
	declared := len(data)
	if keepBytes := (keep + block - 1) / block * block * 2; keepBytes < len(data) {
		data = data[:keepBytes]
	}
	le64, le32 := binary.LittleEndian.AppendUint64, binary.LittleEndian.AppendUint32
	out := append(make([]byte, 0, 92+len(data)), "DSD "...)
	out = le64(out, 28)
	out = le64(out, uint64(28+52+12+declared))
	out = le64(out, 0)
	out = append(out, "fmt "...)
	out = le64(out, 52)
	out = le32(out, 1)
	out = le32(out, 0)
	out = le32(out, 2)
	out = le32(out, 2)
	out = le32(out, uint32(bitRate))
	out = le32(out, 1)
	out = le64(out, samples)
	out = le32(out, block)
	out = le32(out, 0)
	out = append(out, "data"...)
	out = le64(out, uint64(12+declared))
	return append(out, data...)
}

// T0 is the fixtures' clock.
var T0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

// ChoiceState is a state whose test DAC has chosen mode, both transports'
// first use acknowledged at every rate under its firmware, and DoP confirmed:
// what a user's write (B9) leaves.
func ChoiceState(mode string) dsdstate.State {
	return dsdstate.State{DefaultMode: dsdstate.ModeAuto, DACs: []dsdstate.DAC{{
		Key:          DACKey(),
		Choice:       &dsdstate.Choice{Mode: mode, Revision: 1, SetAt: T0, SetBy: dsdstate.ByApp},
		DoPConfirmed: &dsdstate.DoPConfirmation{BCDDevice: DACBCD, ConfirmedAt: T0, SetBy: dsdstate.ByApp},
		FirstUse: []dsdstate.FirstUse{
			{Transport: dsdstate.TransportNative, BCDDevice: DACBCD, AcknowledgedAt: T0},
			{Transport: dsdstate.TransportDoP, BCDDevice: DACBCD, AcknowledgedAt: T0},
		},
	}}}
}

// KnownProfiles is a profile database that knows the test DAC: native DSD
// in DSD_U32_BE to DSD256 from a capture, DoP heard to DSD128, and timing
// (nil for the defaults).
func KnownProfiles(timing *dacprofile.Timing) *dacprofile.Database {
	iface, product := 0, DACProductName
	return &dacprofile.Database{SchemaVersion: dacprofile.SchemaVersion, Profiles: []dacprofile.Profile{{
		ID: "test.dac", Name: "Test DAC",
		Match: []dacprofile.Match{{VendorID: "1234", ProductID: "5678", Interface: &iface, Product: &product}},
		Native: &dacprofile.Native{MaxBitRate: DSD256, Formats: []string{"DSD_U32_BE"},
			Source: dacprofile.Source{Kind: dacprofile.SourceCapture, Ref: "a test's capture, 2026-11-06"}},
		DoP: &dacprofile.DoP{HeardMaxBitRate: DSD128,
			Source: dacprofile.Source{Kind: dacprofile.SourceCapture, Ref: "a test's listening, 2026-11-06"}},
		Timing: timing,
	}}}
}

// Bound is the test DAC bound at card, device 0.
func Bound(card int) func() (playback.BoundDAC, bool) {
	return func() (playback.BoundDAC, bool) {
		return playback.BoundDAC{Card: card, Identity: DACUSB(0), Enumeration: "test/2"}, true
	}
}

// Policy is the effective DSD policy over box and store, the test DAC bound
// at card, no profile database, the tests' kernel and a probe cache.
func Policy(card int, box *Box, store playback.DSDStore) *playback.DSDPolicy {
	return &playback.DSDPolicy{Store: store, Kernel: Kernel, Bound: Bound(card), Box: box, Probes: alsapcm.NewProbeCache(),
		Now: func() time.Time { return T0 }}
}

// The tests' USB descriptors, as sysfs gives them: the device descriptor,
// then the configuration's (internal/player/policy's tests build the same).
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

func ifc(number, setting, subclass, protocol, endpoints byte) []byte {
	return []byte{9, 0x04, number, setting, endpoints, 1, subclass, protocol, 0}
}

func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }

func uac2Header() []byte       { return []byte{9, 0x24, 0x01, 0x00, 0x02, 0x08, 0x40, 0x00, 0x00} }
func uac2Clock(id byte) []byte { return []byte{8, 0x24, 0x0a, id, 0x03, 0x07, 0, 0} }
func uac2IT(id byte, typ uint16) []byte {
	return append(append([]byte{17, 0x24, 0x02, id}, le16(typ)...), 0, 0x29, 2, 3, 0, 0, 0, 0, 0, 0, 0)
}
func uac2OT(id byte, typ uint16, source byte) []byte {
	return append(append([]byte{12, 0x24, 0x03, id}, le16(typ)...), 0, source, 0x29, 0, 0, 0)
}
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

func dac2(units ...[]byte) []byte {
	descs := [][]byte{iad(0, 2), ifc(0, 0, 1, 0x20, 0), uac2Header(), uac2Clock(0x29)}
	descs = append(descs, units...)
	descs = append(descs, ifc(1, 0, 2, 0x20, 0))
	for setting := byte(1); setting <= 3; setting++ {
		descs = append(descs, ifc(1, setting, 2, 0x20, 2), uac2General(2), epOut(), epFeedback())
	}
	return usbDescriptors(1, descs...)
}

// CleanDescriptors are the test DAC's descriptors with no gain stage: input
// terminal 2 straight to a speaker.
func CleanDescriptors() []byte { return dac2(uac2IT(2, 0x0101), uac2OT(20, 0x0301, 2)) }

// VolumeDescriptors put feature unit 10, a master mute and volume, between
// input terminal 2 and the speaker: a gain stage DoP needs at 0 dB.
func VolumeDescriptors() []byte {
	return dac2(uac2IT(2, 0x0101), uac2FU(10, 2, 0x0f, 0, 0), uac2OT(20, 0x0301, 10))
}

// VolumeMixer is the kernel's table for VolumeDescriptors' unit 10, its
// mute and volume as the driver names them.
func VolumeMixer() []byte {
	return []byte("USB Mixer: usb_id=0x12345678, ctrlif=0, ctlerr=0\nCard: Test DAC\n" +
		"  Unit: 10\n    Control: name=\"PCM Playback Switch\", index=0\n    Info: id=10, control=1, cmask=0x0, channels=1, type=\"INV_BOOLEAN\"\n" +
		"  Unit: 10\n    Control: name=\"PCM Playback Volume\", index=0\n    Info: id=10, control=2, cmask=0x0, channels=1, type=\"S16\"\n")
}

// VolumeControls are the card's controls for VolumeMixer: the mute off and
// the volume at value of 0 to 255, -127 dB to 0 dB, so 255 is 0 dB.
func VolumeControls(value int64) []alsactl.Control {
	// The mixer interface (2), a boolean (1) and an integer (2) element, and
	// a DB_MINMAX_MUTE TLV (5) of -127 dB to 0 dB (asound.h, tlv.h).
	sw := alsactl.Control{Interface: 2, Name: "PCM Playback Switch", Type: 1, Access: 1, Count: 1, Values: make([]byte, 8), Info: make([]byte, 128)}
	minDB, maxDB := int32(-12700), int32(0)
	tlv := binary.LittleEndian.AppendUint32(nil, 5)
	tlv = binary.LittleEndian.AppendUint32(tlv, 8)
	tlv = binary.LittleEndian.AppendUint32(tlv, uint32(minDB))
	tlv = binary.LittleEndian.AppendUint32(tlv, uint32(maxDB))
	vol := alsactl.Control{Interface: 2, Name: "PCM Playback Volume", Type: 2, Access: 1 | 1<<4, Count: 2, Info: make([]byte, 128), TLV: tlv}
	binary.LittleEndian.PutUint64(vol.Info[0:], 0)
	binary.LittleEndian.PutUint64(vol.Info[8:], 255)
	binary.LittleEndian.PutUint64(vol.Info[16:], 1)
	for range 2 {
		vol.Values = binary.LittleEndian.AppendUint64(vol.Values, uint64(value))
	}
	return []alsactl.Control{sw, vol}
}
