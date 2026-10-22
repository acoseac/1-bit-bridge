// Package binding resolves the player's DAC binding (output.player.device)
// to the card that is that DAC now, and checks, once the PCM is open and
// before a sample is sent, that the node opened is that card (B470 PR 4a).
// It reads the box's /proc/asound and sysfs through the alsa readers and
// nothing else: it opens no PCM, plays nothing, and latches nothing, so a
// caller resolves again whenever it needs an answer (the hotplug watcher of
// PR 4a2 will).
//
// A card index and an ALSA id are observations, never the binding: a
// serial binding names a unit (its vendor and product ids and its serial),
// a uniqueModel binding the one unit of a model, and a port binding a port
// (usbid.Port). Two matches are ambiguous and never the first, and a unit
// whose identity cannot be read is never ruled out.
package binding

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// State is where the player's binding stands.
type State string

const (
	// StateAvailable is a binding that names one card, which has the
	// bound playback endpoint.
	StateAvailable State = "available"
	// StateNotConfigured is no binding at all.
	StateNotConfigured State = "not_configured"
	// StateDisabled is a player the config does not run (Status).
	StateDisabled State = "disabled"
	// StateDeviceMissing is no card the binding names.
	StateDeviceMissing State = "device_missing"
	// StateDeviceAmbiguous is two or more cards the binding names, or one
	// beside a card whose identity cannot be read: neither is chosen.
	StateDeviceAmbiguous State = "device_ambiguous"
	// StateEndpointMissing is the unit there with no playback stream at the
	// bound PCM device and subdevice.
	StateEndpointMissing State = "endpoint_missing"
	// StateBindingInvalid is a binding that cannot be resolved as it
	// stands: a port binding whose controller is gone or whose port a hub
	// has taken (a topology change: rebind), or one the config refuses.
	// It fails closed, which an empty port (StateDeviceMissing) does not
	// need to.
	StateBindingInvalid State = "binding_invalid"
	// StateIdentityUnsupported is a box whose cards' identities cannot be
	// read at all: no sysfs.
	StateIdentityUnsupported State = "identity_unsupported"
	// StateIdentityTransient is a card the kernel is adding or removing:
	// one tree lists it and the other does not, or the two name different
	// units. Nothing is concluded from it: resolve again.
	StateIdentityTransient State = "identity_transient"
)

// CardState is what the readings say about one card.
type CardState string

const (
	// CardUSB is a USB card whose unit and place sysfs gave.
	CardUSB CardState = "usb"
	// CardUnsupported is a card that is not USB: unselectable.
	CardUnsupported CardState = "identity_unsupported"
	// CardTransient is a card the kernel is adding or removing.
	CardTransient CardState = "transient"
	// CardUnreadable is a card whose identity could not be read.
	CardUnreadable CardState = "unreadable"
)

// Endpoint is a playback stream on a card: the PCM device (the D of
// pcmC<card>D<D>p) and its substream.
type Endpoint struct {
	Device, Subdevice int
}

func (e Endpoint) String() string {
	return "device " + strconv.Itoa(e.Device) + " subdevice " + strconv.Itoa(e.Subdevice)
}

// Card is one card as /proc/asound and sysfs see it together.
type Card struct {
	Index int
	// ID and Name are /proc/asound/cards's, empty for a card only sysfs
	// lists.
	ID, Name string
	State    CardState
	// USB is the unit's identity, for CardUSB.
	USB *alsa.USBIdentity
	// Endpoints are the card's playback substreams in /proc/asound.
	Endpoints []Endpoint
	// Why says what makes the card transient or unreadable, or what it is
	// when unsupported. It quotes no serial.
	Why string
}

// has reports whether the card has the playback endpoint e.
func (c Card) has(e Endpoint) bool {
	for _, x := range c.Endpoints {
		if x == e {
			return true
		}
	}
	return false
}

// Inventory is one observation of the box's cards and USB topology.
type Inventory struct {
	// Sysfs says the sysfs tree could be read at all; SysfsRoot is where.
	Sysfs     bool
	SysfsRoot string
	// Cards are every card either tree lists, by index.
	Cards []Card
	// Controllers and Devices are sysfs's USB controllers and devices,
	// which a port binding is judged against.
	Controllers []string
	Devices     []alsa.USBDevice
	// Problems are what the trees could not say beyond one card.
	Problems []string
}

// Card returns the card with index n.
func (inv Inventory) Card(n int) (Card, bool) {
	for _, c := range inv.Cards {
		if c.Index == n {
			return c, true
		}
	}
	return Card{}, false
}

// Observer reads the box: /proc/asound and sysfs.
type Observer struct {
	Proc  alsa.Reader
	Sysfs alsa.SysfsReader
}

// sndUSBAudio is the driver name snd-usb-audio gives its cards in
// /proc/asound/cards.
const sndUSBAudio = "USB-Audio"

// Observe takes one reading of both trees. The error is /proc/asound's
// cards file's alone, a box with no ALSA: everything else that cannot be
// read is a card's state or a problem.
//
// A USB card's identity is sysfs's. /proc/asound's usbid cross-checks it
// where there is one, and only snd-usb-audio writes one: a USB card another
// driver made (snd-usb-hiface, caiaq, ua101, 6fire, line6) has none, ever,
// and is read from sysfs alone. A USB-Audio card with no usbid is in
// snd-usb-audio's own window, transient: it lists a card in
// /proc/asound/cards before it writes the card's proc files, and its usbid
// reads empty while it disconnects the card.
func (o Observer) Observe() (Inventory, error) {
	snap, err := o.Proc.Read()
	if err != nil {
		return Inventory{}, err
	}
	sys := o.Sysfs.Read()
	inv := Inventory{Sysfs: sys.Present, SysfsRoot: sys.Root, Controllers: sys.Controllers, Devices: sys.Devices,
		Problems: append(append([]string{}, snap.Problems...), sys.Problems...)}
	ids := map[int]alsa.Identity{}
	for _, id := range sys.Cards {
		ids[id.Card] = id
	}
	seen := map[int]bool{}
	for _, cr := range snap.Cards {
		seen[cr.Index] = true
		c := Card{Index: cr.Index, ID: cr.ID, Name: cr.Name}
		for _, d := range cr.Devices {
			for _, s := range d.Substreams {
				c.Endpoints = append(c.Endpoints, Endpoint{Device: d.Device, Subdevice: s.Subdevice})
			}
		}
		id, inSysfs := ids[cr.Index]
		switch {
		case !sys.Present:
			c.State, c.Why = CardUnreadable, "sysfs cannot be read at "+sys.Root
		case !inSysfs:
			c.State, c.Why = CardTransient, "in /proc/asound and not in sysfs: being added or removed"
		case id.Vanished:
			c.State, c.Why = CardTransient, "its sysfs files went while they were read: being added or removed"
		case id.Kind == alsa.IdentityUnsupported:
			c.State, c.Why = CardUnsupported, "a "+orUnknown(id.Bus)+" card, not USB: it has no identity a binding names"
		case id.Kind == alsa.IdentityUnreadable:
			c.State, c.Why = CardUnreadable, strings.Join(id.Problems, "; ")
		case cr.USBID == nil && cr.Driver == sndUSBAudio:
			c.State, c.Why = CardTransient, "a USB-Audio card with no usbid in /proc/asound: snd-usb-audio is adding or disconnecting it"
		case cr.USBID != nil && *cr.USBID != id.USB.ID:
			c.State, c.Why = CardTransient, fmt.Sprintf("/proc/asound says %s and sysfs %s: renumbered between the readings", cr.USBID, id.USB.ID)
		default:
			c.State, c.USB = CardUSB, id.USB
		}
		inv.Cards = append(inv.Cards, c)
	}
	for _, id := range sys.Cards {
		if !seen[id.Card] {
			inv.Cards = append(inv.Cards, Card{Index: id.Card, State: CardTransient, Why: "in sysfs and not in /proc/asound: being added or removed"})
		}
	}
	sort.Slice(inv.Cards, func(i, j int) bool { return inv.Cards[i].Index < inv.Cards[j].Index })
	return inv, nil
}

// orUnknown is s, or "unknown-bus" when it is empty.
func orUnknown(s string) string {
	if s == "" {
		return "unknown-bus"
	}
	return s
}

// Resolution is how a binding resolves on one inventory.
type Resolution struct {
	State State
	// Reason says why, naming cards, models and ports and never a serial.
	Reason string
	// Card is the card the binding names, for StateAvailable and
	// StateEndpointMissing, and -1 otherwise.
	Card int
	// Identity is that card's identity as Resolve read it: what the check
	// after open compares the opened node's card with, as one unit or,
	// under a port binding, as one port.
	Identity alsa.USBIdentity
	// Endpoint is the bound playback endpoint.
	Endpoint Endpoint
	// Matches are the cards an ambiguous binding names.
	Matches []int
	// Binding is the binding's kind (config.BindingSerial,
	// BindingUniqueModel or BindingPort), and Port a port binding's port as
	// it reads (usbid.Port: the place, and the configuration and interface
	// it records, if any): what the check after open judges by.
	Binding string
	Port    usbid.Port
}

// Resolve resolves dev on inv: the card it names now, or the state that
// says why there is none. It decides nothing it cannot read: a transient
// card makes the whole answer identity_transient, and a card whose identity
// cannot be read is never ruled out as a second unit, or as a second card
// of the unit in a bound port.
//
// A port binding names a port and not a unit (usbid.Port): its card is the
// one the unit in that port carries, whatever model it is and whichever
// interface the card sits on. A unit can carry more than one card (a
// composite device): then the configuration and interface the binding
// records choose the card on them, and with none recorded, or none on
// them, the answer is device_ambiguous, never the first.
func Resolve(dev config.PlayerDevice, inv Inventory) Resolution {
	r := Resolution{Card: -1, Endpoint: Endpoint{Device: dev.PCMDevice, Subdevice: dev.Subdevice}, Binding: dev.Binding}
	fail := func(s State, format string, args ...any) Resolution {
		r.State, r.Reason = s, fmt.Sprintf(format, args...)
		return r
	}
	if !dev.Configured() {
		return fail(StateNotConfigured, "no device is bound: bridge output dac bind <card> --binding serial|uniqueModel|port")
	}
	if err := dev.Problem(); err != nil {
		return fail(StateBindingInvalid, "%s", err)
	}
	if !inv.Sysfs {
		return fail(StateIdentityUnsupported, "sysfs cannot be read at %s, so no card's identity can be", inv.SysfsRoot)
	}
	for _, c := range inv.Cards {
		if c.State == CardTransient {
			return fail(StateIdentityTransient, "card %d is %s; resolve again", c.Index, c.Why)
		}
	}
	var match func(u *alsa.USBIdentity) bool
	var what string
	switch dev.Binding {
	case config.BindingSerial, config.BindingUniqueModel:
		vendor, product, _ := dev.IDs()
		id := alsa.USBID{Vendor: vendor, Product: product}
		what = id.String()
		match = func(u *alsa.USBIdentity) bool { return u.ID == id }
		if dev.Binding == config.BindingSerial {
			what += " with the bound serial (" + dev.Serial.Label() + ")"
			match = func(u *alsa.USBIdentity) bool { return u.ID == id && u.Serial.Equal(dev.Serial) }
		}
	case config.BindingPort:
		r.Port, _ = usbid.ParsePort(dev.Port)
		if !contains(inv.Controllers, r.Port.Anchor()) {
			return fail(StateBindingInvalid, "the controller the port binding names, %s, is not on the box: a topology change, so rebind (bridge output dac bind)", r.Port.Anchor())
		}
		place := r.Port.Place().String()
		what = "at " + place
		match = func(u *alsa.USBIdentity) bool { return placeOf(u.Port) == place }
	}
	var unreadable []int
	for _, c := range inv.Cards {
		switch {
		case c.State == CardUnreadable:
			unreadable = append(unreadable, c.Index)
		case c.State == CardUSB && match(c.USB):
			r.Matches = append(r.Matches, c.Index)
		}
	}
	// The cards at a port are one unit's, and the card on the configuration
	// and interface the binding records is the one card there it can name,
	// whatever else the unit carries or an unreadable card may be.
	byInterface := false
	if dev.Binding == config.BindingPort {
		if n, ok := inv.onInterface(r.Matches, r.Port); ok {
			r.Matches, byInterface = []int{n}, true
		}
	}
	switch {
	case len(r.Matches) > 1 && dev.Binding == config.BindingPort:
		why := "the binding records no interface"
		if r.Port.HasInterface() {
			why = fmt.Sprintf("neither is on the interface the binding records (configuration %d, interface %d)", r.Port.Config, r.Port.Interface)
		}
		return fail(StateDeviceAmbiguous, "cards %s are each %s, cards of the one unit in that port, and %s: neither is chosen", joinInts(r.Matches), what, why)
	case len(r.Matches) == 1 && len(unreadable) > 0 && dev.Binding == config.BindingPort && !byInterface:
		return fail(StateDeviceAmbiguous, "card %d is %s, and the identity of card %s could not be read, so it may be another card of the unit in that port: neither is chosen",
			r.Matches[0], what, joinInts(unreadable))
	case len(r.Matches) > 1:
		return fail(StateDeviceAmbiguous, "cards %s are each %s: neither is chosen", joinInts(r.Matches), what)
	case len(r.Matches) == 1 && len(unreadable) > 0 && dev.Binding != config.BindingPort:
		return fail(StateDeviceAmbiguous, "card %d is %s, and the identity of card %s could not be read, so it may be another: neither is chosen",
			r.Matches[0], what, joinInts(unreadable))
	case len(r.Matches) == 0:
		if dev.Binding == config.BindingPort {
			if hub, ok := inv.deviceAt(dev.Port); ok && hub.Hub {
				return fail(StateBindingInvalid, "a hub is in the bound port %s now: a port binding names that port, not what is behind a hub, so rebind (bridge output dac bind)",
					hub.Port())
			}
		}
		more := ""
		if len(unreadable) > 0 {
			more = fmt.Sprintf(" (the identity of card %s could not be read)", joinInts(unreadable))
		}
		return fail(StateDeviceMissing, "no card is %s%s", what, more)
	}
	c, _ := inv.Card(r.Matches[0])
	r.Matches = nil
	return r.found(c, what)
}

// found finishes a resolution on its one card.
func (r Resolution) found(c Card, what string) Resolution {
	r.Card, r.Identity = c.Index, *c.USB
	if !c.has(r.Endpoint) {
		r.State, r.Reason = StateEndpointMissing, fmt.Sprintf("card %d is %s, and has no playback stream at %s", c.Index, what, r.Endpoint)
		return r
	}
	r.State, r.Reason = StateAvailable, fmt.Sprintf("card %d is %s, %s", c.Index, what, r.Endpoint)
	return r
}

// deviceAt is the USB device at a port path's place: its anchor,
// generation and chain.
func (inv Inventory) deviceAt(port string) (alsa.USBDevice, bool) {
	place := placeOf(port)
	if place == "" {
		return alsa.USBDevice{}, false
	}
	for _, d := range inv.Devices {
		if d.Port() == place {
			return d, true
		}
	}
	return alsa.USBDevice{}, false
}

// placeOf is the port of a port path, the place a port binding names
// (usbid.Port.Place), or "" for a path that does not read: the empty path
// of a card whose port this box cannot anchor among them.
func placeOf(port string) string {
	p, err := usbid.ParsePort(port)
	if err != nil {
		return ""
	}
	return p.Place().String()
}

// cardsAt is the USB cards at place, a port binding's port: the cards of
// the unit in it.
func (inv Inventory) cardsAt(place string) []int {
	var at []int
	for _, c := range inv.Cards {
		if c.State == CardUSB && placeOf(c.USB.Port) == place {
			at = append(at, c.Index)
		}
	}
	return at
}

// onInterface is the card among cards that sits on the configuration and
// interface p records, when p records them: the card a port binding's
// preference names on a unit that carries more than one.
func (inv Inventory) onInterface(cards []int, p usbid.Port) (int, bool) {
	if !p.HasInterface() {
		return 0, false
	}
	for _, n := range cards {
		if c, ok := inv.Card(n); ok && c.USB != nil && c.USB.Port == p.String() {
			return n, true
		}
	}
	return 0, false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	if len(s) == 2 {
		return s[0] + " and " + s[1]
	}
	return strings.Join(s, ", ")
}

// Status is the player's DAC state: disabled, with the config's reason,
// while the config does not run the player on goos (config.PlayerEnabled),
// and the binding's resolution otherwise.
func Status(cfg *config.Config, goos string, inv Inventory) Resolution {
	on, why := cfg.PlayerEnabled(goos)
	if !on {
		reason := "the player is off (output.player.enabled is not true)"
		if why != nil {
			reason = why.Error()
		}
		return Resolution{State: StateDisabled, Reason: reason, Card: -1}
	}
	return Resolve(cfg.Output.Player.Device, inv)
}

// ErrWrongDevice is every refusal of the check after open: the node opened
// is not the unit the binding resolved to.
var ErrWrongDevice = errors.New("the opened node is not the bound unit")

// Checker is the check after open: what PR 4b's engine runs between
// opening the PCM and sending samples, at every open (SinkConfig.Admit is
// Check, so a sink's reopen after a release is checked as its open is).
type Checker struct {
	// Sysfs is where the opened card's identity is read again.
	Sysfs alsa.SysfsReader
	// Want is the resolution the open was made from.
	Want Resolution
}

// Check takes what the kernel says the opened node is (alsapcm.Device.Node,
// INFO on the open descriptor), reads that card's identity from sysfs
// again, and requires it to be what the binding resolved to, on the bound
// endpoint, playing: the unit Resolve read under a serial or uniqueModel
// binding, and under a port binding the card the binding chooses at the
// bound port now, whatever unit is in it (checkPort). Anything else wraps
// ErrWrongDevice and says why: a renumber or a swap between the resolve and
// the open, the card gone, or another stream. It never quotes a serial.
func (c Checker) Check(n alsapcm.NodeInfo) error {
	w := c.Want
	if w.State != StateAvailable {
		return wrongDevice("the binding resolved to no card (%s)", w.State)
	}
	if !n.Playback {
		return wrongDevice("the node of card %d device %d is not a playback stream", n.Card, n.Device)
	}
	if got := (Endpoint{Device: n.Device, Subdevice: n.Subdevice}); got != w.Endpoint {
		return wrongDevice("the kernel opened %s of card %d, and the binding names %s", got, n.Card, w.Endpoint)
	}
	id, ok := c.Sysfs.Card(n.Card)
	switch {
	case !ok:
		return wrongDevice("card %d is not in sysfs now: it went away (unplugged or renumbered) between the resolve and the open", n.Card)
	case id.Kind != alsa.IdentityUSB || id.USB == nil:
		return wrongDevice("card %d is not a USB unit whose identity can be read now", n.Card)
	case w.Binding == config.BindingPort:
		return c.checkPort(n, *id.USB)
	case !SameUnit(*id.USB, w.Identity):
		return wrongDevice("card %d is %s, not the bound unit %s: renumbered or swapped between the resolve and the open",
			n.Card, Describe(*id.USB), Describe(w.Identity))
	case n.Card != w.Card:
		return wrongDevice("card %d is the bound unit, and the binding resolved to card %d: resolve again", n.Card, w.Card)
	}
	return nil
}

// checkPort is Check under a port binding, which names a port and not a
// unit: the opened card must be at the bound port, and be the card the
// binding chooses there now, which on a unit swapped into the port between
// the resolve and the open it may be. A card on the configuration and
// interface the binding records is that card whatever else the unit
// carries; any other card is only while it is the one card the unit
// carries, which sysfs is read again for. Like Check under the other
// bindings, it judges by the cards it can read: an unreadable or a
// transient card is Resolve's to fail closed on.
func (c Checker) checkPort(n alsapcm.NodeInfo, u alsa.USBIdentity) error {
	w := c.Want
	place := w.Port.Place().String()
	if !SamePort(u, w.Identity) {
		return wrongDevice("card %d is %s, not at the bound port %s: renumbered or moved between the resolve and the open", n.Card, Describe(u), place)
	}
	if !w.Port.HasInterface() || u.Port != w.Port.String() {
		var at []int
		for _, id := range c.Sysfs.Read().Cards {
			if id.Kind == alsa.IdentityUSB && id.USB != nil && placeOf(id.USB.Port) == place {
				at = append(at, id.Card)
			}
		}
		switch {
		case len(at) > 1 && w.Port.HasInterface():
			return wrongDevice("the unit at the bound port %s carries cards %s, and card %d is not on the interface the binding records (configuration %d, interface %d): resolve again",
				place, joinInts(at), n.Card, w.Port.Config, w.Port.Interface)
		case len(at) > 1:
			return wrongDevice("the unit at the bound port %s carries cards %s, and the binding records no interface to choose one: resolve again", place, joinInts(at))
		case len(at) == 0 || at[0] != n.Card:
			return wrongDevice("card %d is not at the bound port %s now: resolve again", n.Card, place)
		}
	}
	if n.Card != w.Card {
		return wrongDevice("card %d is at the bound port, and the binding resolved to card %d: resolve again", n.Card, w.Card)
	}
	return nil
}

// wrongDevice is a refusal of the check after open, saying why.
func wrongDevice(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrWrongDevice, fmt.Sprintf(format, args...))
}

// SameUnit reports whether a and b are one unit in one place: the same ids,
// the same serial (or none on both), the same port and interface.
func SameUnit(a, b alsa.USBIdentity) bool {
	sameSerial := a.Serial.Equal(b.Serial) || (!a.Serial.Present() && !b.Serial.Present())
	return a.ID == b.ID && sameSerial && a.Port == b.Port && a.Interface == b.Interface
}

// SamePort reports whether a and b are cards at one port: the same
// controller anchor, generation and port chain, whatever units they are and
// whichever interfaces they sit on. It is SameUnit's counterpart under a
// port binding, which names a port and not a unit. A card whose port this
// box's topology cannot anchor is at no port.
func SamePort(a, b alsa.USBIdentity) bool {
	place := placeOf(a.Port)
	return place != "" && place == placeOf(b.Port)
}

// Describe names a unit for a person: its ids, its serial's label and its
// port.
func Describe(u alsa.USBIdentity) string {
	port := u.Port
	if port == "" {
		port = "a port this box's topology cannot anchor"
	}
	return fmt.Sprintf("%s (%s) at %s", u.ID, u.Serial.Label(), port)
}

// For builds the binding of kind for card n's unit, on endpoint ep, as
// bridge output dac bind writes it: each kind with its own fields only, the
// serial straight from sysfs, and a port binding's port alone, or, when the
// unit in that port carries more than one card, the card's port path, whose
// configuration and interface choose it among them. It refuses a card that
// cannot be bound (not there, transient, not USB, unreadable, no such
// endpoint) and a binding that would not resolve to that card now, an
// ambiguous one above all.
func For(inv Inventory, n int, kind string, ep Endpoint) (config.PlayerDevice, error) {
	c, ok := inv.Card(n)
	if !ok {
		return config.PlayerDevice{}, fmt.Errorf("there is no card %d", n)
	}
	switch c.State {
	case CardTransient:
		return config.PlayerDevice{}, fmt.Errorf("card %d is %s: try again", n, c.Why)
	case CardUnsupported:
		return config.PlayerDevice{}, fmt.Errorf("card %d is %s (%s), so it cannot be bound", n, c.Why, StateIdentityUnsupported)
	case CardUnreadable:
		return config.PlayerDevice{}, fmt.Errorf("card %d's identity cannot be read: %s", n, c.Why)
	}
	u := c.USB
	dev := config.PlayerDevice{Transport: "usb", Binding: kind, PCMDevice: ep.Device, Subdevice: ep.Subdevice}
	switch kind {
	case config.BindingSerial:
		if !u.Serial.Present() {
			return config.PlayerDevice{}, fmt.Errorf("card %d's unit (%s) has no serial: bind by uniqueModel or port", n, u.ID)
		}
		dev.VendorID, dev.ProductID, dev.Serial = usbid.Hex16(u.ID.Vendor), usbid.Hex16(u.ID.Product), u.Serial
	case config.BindingUniqueModel:
		dev.VendorID, dev.ProductID = usbid.Hex16(u.ID.Vendor), usbid.Hex16(u.ID.Product)
	case config.BindingPort:
		if u.Port == "" {
			return config.PlayerDevice{}, fmt.Errorf("card %d has no anchored port (%s): bind by serial or uniqueModel", n, u.PortProblem)
		}
		dev.Port = placeOf(u.Port)
		if len(inv.cardsAt(dev.Port)) > 1 {
			dev.Port = u.Port
		}
	default:
		return config.PlayerDevice{}, errors.New("that binding kind is not serial, uniqueModel or port")
	}
	r := Resolve(dev, inv)
	switch {
	case r.State == StateAvailable && r.Card == n:
		return dev, nil
	case r.State == StateDeviceAmbiguous && kind != config.BindingPort:
		return config.PlayerDevice{}, fmt.Errorf("binding card %d by %s would be %s now: %s; bind by port, or by serial where the units have their own", n, kind, r.State, r.Reason)
	default:
		return config.PlayerDevice{}, fmt.Errorf("binding card %d by %s would be %s now: %s", n, kind, r.State, r.Reason)
	}
}
