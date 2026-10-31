package config

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// BindingPort names a port, not a unit: whatever unit is in that port,
	// and the sound card it carries (usbid.Port).
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
	maxPort                = 65535
)

// playerOffMessage is the warning Validate logs when output.player asks for
// a player this config cannot have, with the reason as an attribute.
const playerOffMessage = "output.player: the player is off"

// playerOffLogged is the reason Validate last saw the player off for, ""
// for none (a player that runs, or one that is simply off), so that it
// writes playerOffMessage once each time the reason changes (logPlayerOff).
// It is the process's: Validate runs on every Load and every settings save,
// each time on a config of its own (a clone), and a reason that has not
// changed is the line the operator has already read.
var playerOffLogged struct {
	mu     sync.Mutex
	reason string
}

// logPlayerOff writes playerOffMessage with why as its reason, unless why is
// the reason last seen, and records why as that reason. A nil why logs
// nothing and records that there is none, so a reason that went away and
// comes back is written again.
func logPlayerOff(why error) {
	reason := ""
	if why != nil {
		reason = why.Error()
	}
	playerOffLogged.mu.Lock()
	defer playerOffLogged.mu.Unlock()
	if reason == playerOffLogged.reason {
		return
	}
	playerOffLogged.reason = reason
	if reason != "" {
		validateLogger.Warn(playerOffMessage, "reason", reason)
	}
}

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
// is written back by Save as the file held it, an alias to an anchor
// outside the block written as a copy of what it names (keepable).
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
	// DSDMode is auto, native, dop or off (default). Every mode but off plays
	// DSD only under a local owner-approved policy: native its native ones,
	// dop its DoP ones, auto native first and DoP only at a rate the record
	// holds no native policy for (B470 2d.2). DSD is never converted.
	DSDMode string `yaml:"dsdMode,omitempty"`
	// DSDQualification names an owner-approved record within the bridge state directory.
	DSDQualification string `yaml:"dsdQualification,omitempty"`
	// AllowDoP lets dop and auto play an approved DoP policy. Nil is false:
	// the block still loads, and DoP-only DSD is refused as dop_not_allowed
	// while PCM plays.
	AllowDoP *bool `yaml:"allowDoP,omitempty"`
	// Mixer is the hardware volume control, and this release takes type
	// none alone: the output is fixed.
	Mixer PlayerMixer `yaml:"mixer,omitempty"`
	// OpenHome turns on the player's OpenHome surface on the LAN (B470 PR
	// 5a2a): a listener of its own on each of the box's LAN addresses, the
	// device and service descriptions, SSDP discovery as an OpenHome Source
	// and the actions a controller casts with. Off by default, and it stays
	// off by default until the owner decides it (openhome.md): 1-bit 2.1 and
	// earlier never probe a host that advertises an OpenHome Source for MPD.
	// The console's Player page sets it (B612). Serve reads it from the boot
	// snapshot, so a change applies at the next start, and nothing of the
	// surface runs while the player does not.
	OpenHome bool `yaml:"openhome,omitempty"`
	// OpenHomePort is the surface's TCP port on each LAN address: nil for
	// DefaultOpenHomePort, 0 for a port the kernel picks for each address,
	// which every URL the surface hands out then carries. With the surface
	// on, a port another listener of serve binds turns the player off
	// (PlayerEnabled).
	OpenHomePort *int `yaml:"openhomePort,omitempty"`

	// written is the block as the file holds it (keepable), kept only when
	// it did not read (unread says why): Save writes it back as it was, so
	// an operator's text is never lost to a block this release cannot
	// read. It is never changed after the decode, so a Clone shares it.
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
	// Port is the anchored port, for a port binding: the controller and the
	// port chain, and a card's configuration and interface after them only
	// as a preference among the cards of a unit that carries more than one
	// (usbid.Port).
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

// DefaultOpenHomePort is the OpenHome surface's port when
// output.player.openhomePort is not set: the next after the API (7788), the
// console (7789), the DLNA listener (7790) and the loopback track route
// (7791).
const DefaultOpenHomePort = 7792

// EffectiveOpenHomePort is openhomePort, or DefaultOpenHomePort when it is
// not set.
func (p PlayerConfig) EffectiveOpenHomePort() int {
	if p.OpenHomePort == nil {
		return DefaultOpenHomePort
	}
	return *p.OpenHomePort
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
		p.DSDMode == "" && p.DSDQualification == "" && p.AllowDoP == nil && p.Mixer == PlayerMixer{} && !p.OpenHome && p.OpenHomePort == nil
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
	if p.OpenHomePort != nil {
		v := *p.OpenHomePort
		out.OpenHomePort = &v
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
// not read is kept as written (keepable), with the reason (Problem), and the
// player is off. yaml.v3 decodes each value, so a bool or an integer reads
// as it does everywhere else in the config, and the reason is this
// package's own, naming the field and its line and quoting no value: a
// serial in the wrong place must not reach a log.
func (p *PlayerConfig) UnmarshalYAML(n *yaml.Node) error {
	var r playerReader
	got := r.player(n)
	if r.problem == "" {
		*p = got
		return nil
	}
	if kept, ok := keepable(n); ok {
		*p = PlayerConfig{written: kept, unread: r.problem}
	} else {
		*p = PlayerConfig{unread: r.problem + notKept}
	}
	return nil
}

// The bounds on what the aliases of a block kept as written may copy
// (keepable): an alias bomb, or an alias that names the block from inside
// it, passes them.
const (
	maxKeptCopies = 1024
	maxKeptDepth  = 32
)

// notKept is what the reason of a block that cannot be kept ends with.
const notKept = "; it also holds YAML aliases a save could not write back (an alias to the block itself, or more than 1024 nodes copied), " +
	"so it is not kept, and a save drops it"

// keepable is the block n as a save writes it back: as the file held it,
// except that an alias to a node outside the block is a copy of that node.
// The rest of the file is written from the config's fields, which keep no
// anchor, so such an alias would name an anchor the saved file no longer
// has, and the next load would fail. An alias inside the block to an anchor
// inside it stays an alias, written with its anchor. A copy has no anchor
// and no comment but the alias's own, and an alias inside a copy is copied
// in its turn. ok is false when the copies pass maxKeptCopies nodes or
// maxKeptDepth aliases deep. The parsed nodes are left as they are.
func keepable(n *yaml.Node) (kept *yaml.Node, ok bool) {
	inside := map[*yaml.Node]bool{}
	var mark func(*yaml.Node)
	mark = func(m *yaml.Node) {
		if !inside[m] {
			inside[m] = true
			for _, c := range m.Content {
				mark(c)
			}
		}
	}
	mark(n)
	copies := 0
	var walk func(m *yaml.Node, depth int, copying bool) (*yaml.Node, bool)
	walk = func(m *yaml.Node, depth int, copying bool) (*yaml.Node, bool) {
		if m.Kind == yaml.AliasNode {
			if !copying && inside[m.Alias] {
				c := *m
				return &c, true
			}
			if m.Alias == nil || depth >= maxKeptDepth {
				return nil, false
			}
			c, ok := walk(m.Alias, depth+1, true)
			if ok {
				c.HeadComment, c.LineComment, c.FootComment = m.HeadComment, m.LineComment, m.FootComment
			}
			return c, ok
		}
		c := *m
		if copying {
			if copies++; copies > maxKeptCopies {
				return nil, false
			}
			c.Anchor, c.HeadComment, c.LineComment, c.FootComment = "", "", "", ""
		}
		c.Content = nil
		for _, child := range m.Content {
			cc, ok := walk(child, depth, copying)
			if !ok {
				return nil, false
			}
			c.Content = append(c.Content, cc)
		}
		return &c, true
	}
	return walk(n, 0, false)
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
// PlayerEnabled's.
//
// A refusal quotes a value only when it is one of the words its field
// knows, and says "that value" of anything else, a number included: a
// serial pasted into the wrong field, letters or digits, must not reach the
// log line Validate writes when the reason changes.
func (p PlayerConfig) Problem() error {
	if p.unread != "" {
		return errors.New(p.unread)
	}
	const at = "output.player."
	if n := p.PauseReleaseSeconds; n != nil && (*n < 1 || *n > maxPauseReleaseSeconds) {
		return fmt.Errorf(at+"pauseReleaseSeconds: must be from 1 to %d (an hour)", maxPauseReleaseSeconds)
	}
	if n := p.OpenHomePort; n != nil && (*n < 0 || *n > maxPort) {
		return fmt.Errorf(at+"openhomePort: must be a port, 0 to %d (0 is a port the kernel picks for each address)", maxPort)
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
	case "", "off", "auto", "native", "dop":
	default:
		return errors.New(at + "dsdMode: that value is not a DSD mode (auto, native, dop or off); the player never converts DSD to PCM")
	}

	switch p.Mixer.Type {
	case "", "none":
	case "hardware":
		return errors.New(at + `mixer.type: "hardware" is not in this release: only none is, and the output is fixed (hardware volume is later work)`)
	default:
		return errors.New(at + "mixer.type: that value is not a mixer type (none or hardware), and this release takes none alone: there is no software volume")
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
		return errors.New(at + "transport: that value is not in this release: only usb is")
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
		return errors.New(at + "binding: that value is not serial, uniqueModel or port")
	}
	for _, f := range []struct {
		name string
		v    int
	}{{"pcmDevice", d.PCMDevice}, {"subdevice", d.Subdevice}} {
		if f.v < 0 || f.v > maxPlayerEndpoint {
			return fmt.Errorf(at+"%s: must be from 0 to %d", f.name, maxPlayerEndpoint)
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
	if p.OpenHome {
		if err := c.OpenHomePortClash(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// OpenHomePortClash refuses an OpenHome port another listener of serve
// binds: the API's (listenAddress), the console's (adminAddress), the DLNA
// listener's (dlna.listenAddress, its default included, whether or not
// DLNA is on, since it can be turned on later) and the loopback track
// route's (output.loopbackPort, which serve binds once output.mpd resolves
// on, at any time). An empty address is its default, as applyDefaults
// fills it. Port 0, a port the kernel picks, clashes with nothing. The
// refusal names the other setting and never the port: a value a setting
// holds is not printed (config-lifecycle.md).
//
// It reads the port and the addresses alone, whatever output.player.enabled
// and .openhome say. PlayerEnabled asks it only of a player that is on with
// its surface on, and turns the player off over a clash; the console's
// OpenHome switch (B612) asks it of the config it would save before it
// turns the opt-in on, the player on or off, and refuses the save over a
// clash, so the player turned on later finds none.
func (c *Config) OpenHomePortClash() error {
	port := c.Output.Player.EffectiveOpenHomePort()
	if port == 0 {
		return nil
	}
	listen, admin := c.ListenAddress, c.AdminAddress
	if listen == "" {
		listen = DefaultListenAddress
	}
	if admin == "" {
		admin = DefaultAdminAddress
	}
	clash := func(setting string) error {
		return errors.New("output.player.openhomePort: bridge serve binds that port already, for " + setting +
			": give the OpenHome surface a port of its own")
	}
	for _, other := range []struct{ setting, addr string }{
		{"listenAddress", listen},
		{"adminAddress", admin},
		{"dlna.listenAddress", c.DLNA.EffectiveDLNAListenAddress()},
	} {
		if p, ok := namedPort(other.addr); ok && p == port {
			return clash(other.setting)
		}
	}
	if c.Output.EffectiveLoopbackPort() == port {
		return clash("output.loopbackPort")
	}
	return nil
}

// namedPort is the port a host:port address names, false for none: one
// that does not parse (Validate refuses it on its own), port 0, or one
// outside the TCP range.
func namedPort(addr string) (int, bool) {
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(ps)
	if err != nil || p <= 0 || p > maxPort {
		return 0, false
	}
	return p, true
}

// playerPlatform is the host Validate judges output.player for:
// runtime.GOOS, or the config's playerGOOS, which a test sets.
func (c *Config) playerPlatform() string {
	if c.playerGOOS != "" {
		return c.playerGOOS
	}
	return runtime.GOOS
}

// playerSection is one mapping the player block's reader walks: what its
// keys are settings of, the keys it takes, and the keys it refuses by name
// with why. A refusal names a key only when it is one of those: any other
// is "that key", since a serial written as a key must not reach a log.
type playerSection struct {
	path, noun string
	// keys are the settings the section takes, in the order the refusal
	// of any other key lists them.
	keys  []string
	never map[string]string
}

// The keys each section takes.
var (
	playerKeys = []string{"enabled", "room", "pauseReleaseSeconds", "device", "dsdMode", "dsdQualification", "allowDoP", "mixer", "openhome", "openhomePort"}
	deviceKeys = []string{"transport", "binding", "vendorId", "productId", "serial", "port", "pcmDevice", "subdevice"}
	mixerKeys  = []string{"type", "control"}
)

// orList is keys joined as a person lists them: "a, b or c".
func orList(keys []string) string {
	if len(keys) < 2 {
		return strings.Join(keys, "")
	}
	return strings.Join(keys[:len(keys)-1], ", ") + " or " + keys[len(keys)-1]
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
// refuses what is not a mapping, a merge key, a key that is not a plain
// word, a key the player refuses by name (never), a key the section does
// not take (keys, or one set refuses), and a key written twice. A null is a
// mapping with nothing in it. A key is named in a refusal only once it is
// one of the section's or one refused by name.
func (r *playerReader) fields(n *yaml.Node, s playerSection, set func(key string, v *yaml.Node) bool) {
	n = deref(n)
	if isNull(n) {
		return
	}
	if n.Kind != yaml.MappingNode {
		r.fail(n, s.path, "is a mapping of %s settings, and this is not one", s.noun)
		return
	}
	unknown := func(k *yaml.Node) {
		r.fail(k, s.path, "that key is not a %s setting (%s)", s.noun, orList(s.keys))
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
		if why, ok := s.never[strings.ToLower(key)]; ok {
			r.fail(k, s.path+"."+key, "%s", why)
			return
		}
		if !slices.Contains(s.keys, key) {
			unknown(k)
			return
		}
		if seen[key] {
			r.fail(k, s.path+"."+key, "set twice")
			return
		}
		seen[key] = true
		if !set(key, v) {
			unknown(k)
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

// whole reads a YAML whole number into into; a null leaves it 0. A value
// yaml.v3 would change on the way in is refused as a word is, a fraction
// among them: it decodes a float into an int by dropping what follows the
// point, so openhomePort: 7792.9 read as 7792 (exactInteger, backlog B528).
// An otherwise accepted leading-zero spelling is refused too (B533).
func (r *playerReader) whole(n *yaml.Node, path string, into *int) {
	n = deref(n)
	if n.Kind != yaml.ScalarNode || !exactInteger(n, intType) || n.Decode(into) != nil {
		r.fail(n, path, "must be a whole number")
		return
	}
	if leadingZero(n.Value) {
		r.fail(n, path, "must be written without a leading zero, %s", leadingZeroRemedy)
	}
}

// intType is the type of the block's whole numbers.
var intType = reflect.TypeFor[int]()

// optionalWhole reads a whole number as whole does, nil for a null.
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
	r.fields(n, playerSection{path: at, noun: "player", keys: playerKeys, never: neverSettings}, func(key string, v *yaml.Node) bool {
		switch key {
		case "enabled":
			r.boolean(v, at+".enabled", &p.Enabled)
		case "room":
			r.text(v, at+".room", &p.Room)
		case "pauseReleaseSeconds":
			p.PauseReleaseSeconds = r.optionalWhole(v, at+".pauseReleaseSeconds")
		case "device":
			p.Device = r.device(v)
		case "dsdQualification":
			r.text(v, at+".dsdQualification", &p.DSDQualification)
		case "dsdMode":
			r.text(v, at+".dsdMode", &p.DSDMode)
		case "allowDoP":
			p.AllowDoP = r.optionalBool(v, at+".allowDoP")
		case "mixer":
			p.Mixer = r.mixer(v)
		case "openhome":
			r.boolean(v, at+".openhome", &p.OpenHome)
		case "openhomePort":
			p.OpenHomePort = r.optionalWhole(v, at+".openhomePort")
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
	r.fields(n, playerSection{path: at, noun: "device", keys: deviceKeys, never: neverDeviceSettings}, func(key string, v *yaml.Node) bool {
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
	r.fields(n, playerSection{path: at, noun: "mixer", keys: mixerKeys, never: neverSettings}, func(key string, v *yaml.Node) bool {
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
