package config

import (
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// The device.binding kinds: what a player's binding names.
const (
	// BindingSerial names one unit: its vendor and product ids and its
	// serial. Two units that match it are ambiguous, never the first.
	BindingSerial = "serial"
	// BindingUniqueModel names a model that has one unit on the box: two
	// units of it are ambiguous, never the first.
	BindingUniqueModel = "uniqueModel"
	// BindingPort names a port, not a unit: whatever sound card sits on that
	// interface of that port.
	BindingPort = "port"
)

// DefaultPauseReleaseSeconds is how long a paused player holds the DAC
// before it releases it, when output.player.pauseReleaseSeconds is not set
// (B470 §3).
const DefaultPauseReleaseSeconds = 30

// The player block's bounds.
const (
	maxPauseReleaseSeconds = 3600
	maxRoomRunes           = 64
	maxPlayerEndpoint      = 255
)

// playerOffMessage is the warning Validate logs when output.player asks for
// a player this config cannot have, with the reason as an attribute.
const playerOffMessage = "output.player: the player is off"

// PlayerConfig is output.player: the bridge as a player of its own indexed
// files through one DAC it opens exclusively (B470). It is off by default,
// it runs on Linux only and never on a public bridge, and one configured
// player is all there is. There is no resampling, forced-rate, software
// mixer, normalization or DSD-to-PCM setting, and there never will be: the
// player sends the DAC the file's samples or refuses the file.
//
// A block that does not read, or reads and is refused (Problem), turns the
// player off with a logged reason and stops nothing else: MPD, the library
// and the server load as they would without it. A block that does not read
// is written back by Save as the file held it.
//
// Its serial is a usbid.Serial, which prints and encodes as a redaction
// everywhere but the config file.
type PlayerConfig struct {
	// Enabled asks for the player. PlayerEnabled says whether this config
	// can have it.
	Enabled bool `yaml:"enabled,omitempty"`
	// Room is the room a controller shows the player in: UTF-8, no
	// control characters, at most 64 characters.
	Room string `yaml:"room,omitempty"`
	// PauseReleaseSeconds is how long a pause holds the DAC before
	// releasing it: 1 to 3600, nil for DefaultPauseReleaseSeconds.
	PauseReleaseSeconds *int `yaml:"pauseReleaseSeconds,omitempty"`
	// Device is the binding: which DAC the player opens.
	Device PlayerDevice `yaml:"device,omitempty"`
	// DSDMode is auto, native, dop or off, and this release takes off
	// alone (an empty value is off): a DSD track is refused, never
	// converted.
	DSDMode string `yaml:"dsdMode,omitempty"`
	// AllowDoP may enable DoP once PR 2d qualifies it; this release takes
	// false alone (nil is false).
	AllowDoP *bool `yaml:"allowDoP,omitempty"`
	// Mixer is the hardware volume control, and this release takes type
	// none alone: the output is fixed.
	Mixer PlayerMixer `yaml:"mixer,omitempty"`

	// written is the block as the file holds it, kept only when it did not
	// read (unread says why): Save writes it back as it was, so an
	// operator's text is never lost to a block this release cannot read.
	// It is never changed after the decode, so a Clone shares it.
	written *yaml.Node
	// unread is why the block did not read, naming the field and its line
	// and never quoting a value.
	unread string
}

// PlayerDevice is output.player.device: the binding that says which DAC the
// player opens. Each binding carries only its own fields: a serial binding
// its vendor and product ids and serial, a uniqueModel binding its ids, a
// port binding its port, and each its PCM device and subdevice. A card
// number, an ALSA id or name and a USB bus or device number are never part
// of it: the kernel renumbers them.
type PlayerDevice struct {
	// Transport is usb, the one this release takes ("" is usb).
	Transport string `yaml:"transport,omitempty"`
	// Binding is serial, uniqueModel or port; "" is no binding.
	Binding string `yaml:"binding,omitempty"`
	// VendorID and ProductID are the model's USB ids, four hexadecimal
	// digits, alone or after 0x, in either case (usbid.ParseHex16); the
	// decode keeps them in the canonical form, four lowercase digits.
	VendorID  string `yaml:"vendorId,omitempty"`
	ProductID string `yaml:"productId,omitempty"`
	// Serial is the unit's serial, for a serial binding: password-class,
	// written to the config file and printed nowhere.
	Serial usbid.Serial `yaml:"serial,omitempty"`
	// Port is the anchored port path, for a port binding (usbid.Port).
	Port string `yaml:"port,omitempty"`
	// PCMDevice and Subdevice are the playback endpoint on the card: the
	// D of pcmC<card>D<D>p and its substream, 0 to 255.
	PCMDevice int `yaml:"pcmDevice,omitempty"`
	Subdevice int `yaml:"subdevice,omitempty"`
}

// PlayerMixer is output.player.mixer, an object: {type: none, control:
// null} until hardware volume (PR 8a). A control other than null does not
// read in this release.
type PlayerMixer struct {
	// Type is none or hardware, and this release takes none alone ("" is
	// none).
	Type string `yaml:"type,omitempty"`
}

// Configured reports whether the device block names a binding.
func (d PlayerDevice) Configured() bool { return d.Binding != "" }

// empty reports whether the device block names nothing at all.
func (d PlayerDevice) empty() bool {
	return d.Transport == "" && d.Binding == "" && d.VendorID == "" && d.ProductID == "" && !d.Serial.Present() &&
		d.Port == "" && d.PCMDevice == 0 && d.Subdevice == 0
}

// IDs returns the binding's vendor and product ids, refusing ids that are
// missing or not hexadecimal.
func (d PlayerDevice) IDs() (vendor, product uint16, err error) {
	if vendor, err = usbid.ParseHex16(d.VendorID); err != nil {
		return 0, 0, fmt.Errorf("output.player.device.vendorId: %w", err)
	}
	if product, err = usbid.ParseHex16(d.ProductID); err != nil {
		return 0, 0, fmt.Errorf("output.player.device.productId: %w", err)
	}
	return vendor, product, nil
}

// PauseRelease is how long a pause holds the DAC: pauseReleaseSeconds, or
// DefaultPauseReleaseSeconds when it is not set.
func (p PlayerConfig) PauseRelease() time.Duration {
	if p.PauseReleaseSeconds == nil {
		return DefaultPauseReleaseSeconds * time.Second
	}
	return time.Duration(*p.PauseReleaseSeconds) * time.Second
}

// EffectiveDSDMode is dsdMode, off when it is not set. Under off the engine
// (PR 4b) refuses a DSD track: there is no DSD-to-PCM setting.
func (p PlayerConfig) EffectiveDSDMode() string {
	if p.DSDMode == "" {
		return "off"
	}
	return p.DSDMode
}

// AllowsDoP reports allowDoP, false when it is not set.
func (p PlayerConfig) AllowsDoP() bool { return p.AllowDoP != nil && *p.AllowDoP }

// EffectiveMixer is mixer.type, none when it is not set.
func (p PlayerConfig) EffectiveMixer() string {
	if p.Mixer.Type == "" {
		return "none"
	}
	return p.Mixer.Type
}

// IsZero reports whether the block was not written at all, which is what
// yaml.v3's omitempty asks.
func (p PlayerConfig) IsZero() bool {
	return p.written == nil && !p.Enabled && p.Room == "" && p.PauseReleaseSeconds == nil && p.Device.empty() &&
		p.DSDMode == "" && p.AllowDoP == nil && p.Mixer == PlayerMixer{}
}

// clone is a copy that shares no pointer with p but the never-changed
// written node.
func (p PlayerConfig) clone() PlayerConfig {
	out := p
	if p.PauseReleaseSeconds != nil {
		v := *p.PauseReleaseSeconds
		out.PauseReleaseSeconds = &v
	}
	if p.AllowDoP != nil {
		v := *p.AllowDoP
		out.AllowDoP = &v
	}
	return out
}

// MarshalYAML writes the block: as the file held it when it did not read,
// and otherwise the fields it sets, the serial whole (usbid.Serial's
// MarshalYAML), since the config file is where the serial is kept.
func (p PlayerConfig) MarshalYAML() (any, error) {
	if p.written != nil {
		return p.written, nil
	}
	type plain PlayerConfig
	return plain(p), nil
}

// UnmarshalYAML reads the block, and never fails the load: a block that does
// not read is kept as written, with the reason (Problem), and the player is
// off. yaml.v3 decodes each value, so a bool or an integer reads as it does
// everywhere else in the config, and the reason is this package's own,
// naming the field and its line and quoting no value: a serial in the wrong
// place must not reach a log.
func (p *PlayerConfig) UnmarshalYAML(n *yaml.Node) error {
	var r playerReader
	got := r.player(n)
	if r.problem != "" {
		*p = PlayerConfig{written: n, unread: r.problem}
		return nil
	}
	*p = got
	return nil
}

// Unread is why the block did not read at all, or nil when it read. Such a
// block is kept as the file holds it, so a writer that sets a field on it
// would see Save write the old text back: bridge output dac bind refuses
// it instead.
func (p PlayerConfig) Unread() error {
	if p.unread == "" {
		return nil
	}
	return errors.New(p.unread)
}

// Problem says why the block cannot run as written, naming the field: the
// first refusal, or nil. Every value outside what this release takes is
// refused, and a value the design names for a later PR is said to be not in
// this release. It reads the block and nothing else (no hardware, no sysfs,
// no interface, no file); whether this host can run a player at all is
// PlayerEnabled's. A refusal never quotes the serial.
func (p PlayerConfig) Problem() error {
	if p.unread != "" {
		return errors.New(p.unread)
	}
	const at = "output.player."
	if n := p.PauseReleaseSeconds; n != nil && (*n < 1 || *n > maxPauseReleaseSeconds) {
		return fmt.Errorf(at+"pauseReleaseSeconds: must be from 1 to %d (an hour), and %d is not", maxPauseReleaseSeconds, *n)
	}
	switch {
	case !utf8.ValidString(p.Room):
		return errors.New(at + "room: must be UTF-8")
	case strings.ContainsFunc(p.Room, unicode.IsControl):
		return errors.New(at + "room: must not hold a control character")
	case utf8.RuneCountInString(p.Room) > maxRoomRunes:
		return fmt.Errorf(at+"room: must be at most %d characters", maxRoomRunes)
	}
	switch p.DSDMode {
	case "", "off":
	case "auto", "native", "dop":
		return fmt.Errorf(at+"dsdMode: %s is not in this release: only off is, and a DSD track is refused (native DSD and DoP are later work)", token(p.DSDMode))
	default:
		return fmt.Errorf(at+"dsdMode: %s is not a DSD mode (auto, native, dop or off), and this release takes off alone: the player never converts DSD to PCM", token(p.DSDMode))
	}
	if p.AllowsDoP() {
		return errors.New(at + "allowDoP: true is not in this release: only false is (DoP is later work)")
	}
	switch p.Mixer.Type {
	case "", "none":
	case "hardware":
		return errors.New(at + `mixer.type: "hardware" is not in this release: only none is, and the output is fixed (hardware volume is later work)`)
	default:
		return fmt.Errorf(at+"mixer.type: %s is not a mixer type (none or hardware), and this release takes none alone: there is no software volume", token(p.Mixer.Type))
	}
	return p.Device.Problem()
}

// Problem is PlayerConfig.Problem's part for the device block: why the
// binding cannot be resolved as written, or nil. A block that names
// nothing is no binding (Configured is false), not a refusal.
func (d PlayerDevice) Problem() error {
	const at = "output.player.device."
	if d.empty() {
		return nil
	}
	if d.Transport != "" && d.Transport != "usb" {
		return fmt.Errorf(at+"transport: %s is not in this release: only usb is", token(d.Transport))
	}
	needIDs := func(kind string) error {
		for _, f := range []struct{ name, value string }{{"vendorId", d.VendorID}, {"productId", d.ProductID}} {
			if f.value == "" {
				return fmt.Errorf(at+"%s: a %s binding needs the model's %s (bridge output dac bind reads it from the unit)", f.name, kind, f.name)
			}
			if _, err := usbid.ParseHex16(f.value); err != nil {
				return fmt.Errorf(at+"%s: %w", f.name, err)
			}
		}
		return nil
	}
	switch d.Binding {
	case "":
		return errors.New(at + "binding: must be serial, uniqueModel or port when the device block names anything")
	case BindingSerial:
		if d.Port != "" {
			return errors.New(at + "port: a serial binding names a unit, not a port: remove port, or bind by port")
		}
		if err := needIDs(BindingSerial); err != nil {
			return err
		}
		if !d.Serial.Present() {
			return errors.New(at + "serial: a serial binding needs the unit's serial (bridge output dac bind reads it from the unit and never prints it)")
		}
	case BindingUniqueModel:
		if d.Serial.Present() {
			return errors.New(at + "serial: a uniqueModel binding names a model, not a unit: remove serial, or bind by serial")
		}
		if d.Port != "" {
			return errors.New(at + "port: a uniqueModel binding names a model, not a port: remove port, or bind by port")
		}
		if err := needIDs(BindingUniqueModel); err != nil {
			return err
		}
	case BindingPort:
		if d.Serial.Present() {
			return errors.New(at + "serial: a port binding names a port, not a unit: remove serial, or bind by serial")
		}
		if d.VendorID != "" || d.ProductID != "" {
			return errors.New(at + "vendorId: a port binding names a port, not a model: remove vendorId and productId, or bind by uniqueModel")
		}
		if d.Port == "" {
			return errors.New(at + "port: a port binding needs the port it names (bridge output dac bind writes it)")
		}
		if _, err := usbid.ParsePort(d.Port); err != nil {
			return fmt.Errorf(at+"port: %w", err)
		}
	default:
		return fmt.Errorf(at+"binding: %s is not serial, uniqueModel or port", token(d.Binding))
	}
	for _, f := range []struct {
		name string
		v    int
	}{{"pcmDevice", d.PCMDevice}, {"subdevice", d.Subdevice}} {
		if f.v < 0 || f.v > maxPlayerEndpoint {
			return fmt.Errorf(at+"%s: must be from 0 to %d, and %d is not", f.name, maxPlayerEndpoint, f.v)
		}
	}
	return nil
}

// PlayerEnabled is the player predicate, beside OutputConfig.Mode, which
// stays MPD's alone: whether output.player runs on this config on a goos
// host. It answers false and a nil reason for a player that is simply off
// (enabled is false), and false with the reason when the block asks for
// one this config cannot have: the block does not read or is refused
// (Problem), the host is not Linux, or the bridge is public. A block that
// does not read is refused whatever its enabled said, since that was not
// read either. The reason names the field and never quotes a serial.
//
// It reads nothing but the config. A LAN interface that is not up yet is
// the player's runtime state (lan_unavailable, internal/player/lan), never
// this refusal: on a Raspberry Pi the interface and DHCP often come up
// after the service starts.
func (c *Config) PlayerEnabled(goos string) (bool, error) {
	p := c.Output.Player
	if p.unread != "" {
		return false, errors.New(p.unread)
	}
	if !p.Enabled {
		return false, nil
	}
	if err := p.Problem(); err != nil {
		return false, err
	}
	if goos != "linux" {
		return false, errors.New("output.player.enabled: the player is Linux only, and this is " + goos)
	}
	if c.IsPublic() {
		return false, errors.New("output.player.enabled: a public bridge has no LAN to play for, and refuses every active output operation: the player stays off")
	}
	return true, nil
}

// playerPlatform is the host Validate judges output.player for:
// runtime.GOOS, or the config's playerGOOS, which a test sets.
func (c *Config) playerPlatform() string {
	if c.playerGOOS != "" {
		return c.playerGOOS
	}
	return runtime.GOOS
}

// tokenShape is a value short and plain enough to quote in a refusal; any
// other value is described, not quoted, since a serial pasted into the
// wrong field must not reach a log.
var tokenShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,23}$`)

// token quotes v when it is tokenShape, and describes it otherwise.
func token(v string) string {
	if tokenShape.MatchString(v) {
		return strconv.Quote(v)
	}
	return "that value"
}

// playerSection is one mapping the player block's reader walks: what its
// keys are settings of, for the unknown-key refusal, and the keys it
// refuses by name with why.
type playerSection struct {
	path, noun string
	never      map[string]string
}

// neverSettings are the keys every part of the block refuses by name, with
// why: what an operator who comes from MPD's or a DAC's settings looks for.
// Matched without regard to case.
var neverSettings = keysWithReason("the player never resamples, converts, normalizes or mixes in software: every sample "+
	"reaches the DAC as the file holds it",
	"resample", "resampler", "resampling", "samplerate", "rate", "forcerate", "upsample", "upsampling", "softwarevolume",
	"softvolume", "softvol", "softwaremixer", "volume", "normalize", "normalization", "volumenormalization", "replaygain",
	"dither", "crossfade", "dsdtopcm", "dsd2pcm")

// neverDeviceSettings are neverSettings and the device keys refused by
// name: what names a card today and another one after a replug.
var neverDeviceSettings = mergeReasons(neverSettings, keysWithReason("a card number, an ALSA id or name, a node path and a "+
	"USB bus or device number are not an identity, since the kernel renumbers them: bind by serial, uniqueModel or port "+
	"(bridge output dac bind)",
	"card", "cardindex", "index", "alsaid", "id", "name", "hw", "devnum", "busnum", "bus", "devpath", "path", "node"))

// keysWithReason maps each key to why.
func keysWithReason(why string, keys ...string) map[string]string {
	m := make(map[string]string, len(keys))
	for _, k := range keys {
		m[k] = why
	}
	return m
}

// mergeReasons is a and b in one map.
func mergeReasons(a, b map[string]string) map[string]string {
	m := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = v
	}
	return m
}

// playerReader reads the player block, keeping the first reason it does
// not read.
type playerReader struct{ problem string }

// fail records why the block does not read, at n's line, unless a reason
// is recorded already.
func (r *playerReader) fail(n *yaml.Node, path, format string, args ...any) {
	if r.problem != "" {
		return
	}
	r.problem = fmt.Sprintf("%s (line %d): ", path, n.Line) + fmt.Sprintf(format, args...)
}

// maxAliasHops bounds a chain of YAML aliases the reader follows.
const maxAliasHops = 16

// deref follows n's aliases to the node they name.
func deref(n *yaml.Node) *yaml.Node {
	for i := 0; i < maxAliasHops && n.Kind == yaml.AliasNode && n.Alias != nil; i++ {
		n = n.Alias
	}
	return n
}

// isNull reports whether n is a YAML null: nothing written.
func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" }

// fields walks the mapping n, handing each key and value to set, and
// refuses what is not a mapping, a key written twice, a merge key, a key
// that is not a plain word, a key the player refuses by name (neverKeys),
// and a key set does not take. A null is a mapping with nothing in it.
func (r *playerReader) fields(n *yaml.Node, s playerSection, set func(key string, v *yaml.Node) bool) {
	n = deref(n)
	if isNull(n) {
		return
	}
	if n.Kind != yaml.MappingNode {
		r.fail(n, s.path, "is a mapping of %s settings, and this is not one", s.noun)
		return
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := deref(n.Content[i]), n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			r.fail(k, s.path, "a key is a word, and this one is not")
			return
		}
		key := k.Value
		if key == "<<" {
			r.fail(k, s.path, "a YAML merge key (<<) is not read here: write the settings out")
			return
		}
		at := s.path + "." + key
		if !tokenShape.MatchString(key) {
			at = s.path
		}
		if seen[key] {
			r.fail(k, at, "set twice")
			return
		}
		seen[key] = true
		if why, ok := s.never[strings.ToLower(key)]; ok {
			r.fail(k, at, "%s", why)
			return
		}
		if !set(key, v) {
			r.fail(k, s.path, "%s is not a %s setting", token(key), s.noun)
			return
		}
		if r.problem != "" {
			return
		}
	}
}

// boolean reads a YAML bool into into; a null leaves it false.
func (r *playerReader) boolean(n *yaml.Node, path string, into *bool) {
	n = deref(n)
	if n.Kind != yaml.ScalarNode || n.Decode(into) != nil {
		r.fail(n, path, "must be true or false")
	}
}

// optionalBool reads a YAML bool, nil for a null.
func (r *playerReader) optionalBool(n *yaml.Node, path string) *bool {
	if isNull(deref(n)) {
		return nil
	}
	var b bool
	r.boolean(n, path, &b)
	return &b
}

// whole reads a YAML integer into into; a null leaves it 0.
func (r *playerReader) whole(n *yaml.Node, path string, into *int) {
	n = deref(n)
	if n.Kind != yaml.ScalarNode || n.Decode(into) != nil {
		r.fail(n, path, "must be a whole number")
	}
}

// optionalWhole reads a YAML integer, nil for a null.
func (r *playerReader) optionalWhole(n *yaml.Node, path string) *int {
	if isNull(deref(n)) {
		return nil
	}
	var v int
	r.whole(n, path, &v)
	return &v
}

// text reads a YAML scalar's text into into; a null leaves it "".
func (r *playerReader) text(n *yaml.Node, path string, into *string) {
	n = deref(n)
	if n.Kind != yaml.ScalarNode || n.Decode(into) != nil {
		r.fail(n, path, "must be a string")
	}
}

// hex reads a vendor or product id, kept canonical when it is one
// (usbid.Hex16) and as written otherwise, for Problem to refuse.
func (r *playerReader) hex(n *yaml.Node, path string, into *string) {
	r.text(n, path, into)
	if v, err := usbid.ParseHex16(*into); err == nil {
		*into = usbid.Hex16(v)
	}
}

// player reads the block itself.
func (r *playerReader) player(n *yaml.Node) PlayerConfig {
	var p PlayerConfig
	const at = "output.player"
	r.fields(n, playerSection{at, "player", neverSettings}, func(key string, v *yaml.Node) bool {
		switch key {
		case "enabled":
			r.boolean(v, at+".enabled", &p.Enabled)
		case "room":
			r.text(v, at+".room", &p.Room)
		case "pauseReleaseSeconds":
			p.PauseReleaseSeconds = r.optionalWhole(v, at+".pauseReleaseSeconds")
		case "device":
			p.Device = r.device(v)
		case "dsdMode":
			r.text(v, at+".dsdMode", &p.DSDMode)
		case "allowDoP":
			p.AllowDoP = r.optionalBool(v, at+".allowDoP")
		case "mixer":
			p.Mixer = r.mixer(v)
		default:
			return false
		}
		return true
	})
	return p
}

// device reads output.player.device.
func (r *playerReader) device(n *yaml.Node) PlayerDevice {
	var d PlayerDevice
	const at = "output.player.device"
	r.fields(n, playerSection{at, "device", neverDeviceSettings}, func(key string, v *yaml.Node) bool {
		switch key {
		case "transport":
			r.text(v, at+".transport", &d.Transport)
		case "binding":
			r.text(v, at+".binding", &d.Binding)
		case "vendorId":
			r.hex(v, at+".vendorId", &d.VendorID)
		case "productId":
			r.hex(v, at+".productId", &d.ProductID)
		case "serial":
			if vv := deref(v); vv.Kind != yaml.ScalarNode || vv.Decode(&d.Serial) != nil {
				r.fail(vv, at+".serial", "must be a string")
			}
		case "port":
			r.text(v, at+".port", &d.Port)
		case "pcmDevice":
			r.whole(v, at+".pcmDevice", &d.PCMDevice)
		case "subdevice":
			r.whole(v, at+".subdevice", &d.Subdevice)
		default:
			return false
		}
		return true
	})
	return d
}

// mixer reads output.player.mixer, an object. A word in its place ("mixer:
// none") is refused with what to write instead, and a control other than
// null does not read in this release.
func (r *playerReader) mixer(n *yaml.Node) PlayerMixer {
	var m PlayerMixer
	const at = "output.player.mixer"
	if nn := deref(n); nn.Kind == yaml.ScalarNode && !isNull(nn) {
		r.fail(nn, at, "mixer is an object, not a word: write mixer: {type: none}")
		return m
	}
	r.fields(n, playerSection{at, "mixer", neverSettings}, func(key string, v *yaml.Node) bool {
		switch key {
		case "type":
			r.text(v, at+".type", &m.Type)
		case "control":
			if vv := deref(v); !isNull(vv) {
				r.fail(vv, at+".control", "a mixer control is not in this release (hardware volume is later work): leave it null")
			}
		default:
			return false
		}
		return true
	})
	return m
}
