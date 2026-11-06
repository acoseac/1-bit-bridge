package policy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// Selection names the streaming interface a stream plays through: the
// unit's active configuration (its bConfigurationValue) and the interface
// and alternate setting the kernel's stream file names for an altset
// ("Interface 1", "Altset 1").
type Selection struct {
	Config, Interface, Altset int
}

// UnitKind is a USB Audio Class unit's kind.
type UnitKind string

const (
	UnitFeature       UnitKind = "feature"
	UnitSelector      UnitKind = "selector"
	UnitMixer         UnitKind = "mixer"
	UnitProcessing    UnitKind = "processing"
	UnitExtension     UnitKind = "extension"
	UnitEffect        UnitKind = "effect"
	UnitRateConverter UnitKind = "sample rate converter"
)

// Unit is one unit on a playback path.
type Unit struct {
	ID   int
	Kind UnitKind
	// Gains are a feature unit's gain controls, by the class's control
	// selector, each where its descriptor declares it.
	Gains []GainControl
}

// GainControl is a gain control a feature unit declares: its selector
// (volume 2, bass 3, mid 4, treble 5, graphic equalizer 6, and in audio
// class 2 input gain 11 and input gain pad 12), on the master channel, and
// on any of the logical channels.
type GainControl struct {
	Selector         int
	Master, Channels bool
}

// controls is how many controls the USB audio driver makes of u's gain
// controls: one for each on the master channel, and one for each on the
// logical channels together.
func (u Unit) controls() int {
	n := 0
	for _, g := range u.Gains {
		if g.Master {
			n++
		}
		if g.Channels {
			n++
		}
	}
	return n
}

// Path is a playback streaming interface's path through its audio function
// (the design's section 3.4), as the unit's descriptors give it.
type Path struct {
	// Protocol is the audio class version, 1 or 2.
	Protocol int
	// Terminal is the input terminal the streaming interface links to.
	Terminal int
	// Outputs are the output terminals Terminal reaches, in ID order: every
	// one but a USB streaming terminal, which returns samples to the host.
	Outputs []int
	// Units are the units between Terminal and one of Outputs, in ID order.
	Units []Unit
}

// The USB descriptor types, interface classes and the audio class's
// descriptor subtypes the parse reads (USB 2.0 §9.4; USB Audio 1.0 and 2.0,
// their appendices A).
const (
	descDevice       = 0x01
	descConfig       = 0x02
	descInterface    = 0x04
	descEndpoint     = 0x05
	descAssociation  = 0x0b
	descCSInterface  = 0x24
	classAudio       = 0x01
	subclassControl  = 0x01
	subclassStream   = 0x02
	protocolUAC1     = 0x00
	protocolUAC2     = 0x20
	csHeader         = 0x01
	csInputTerminal  = 0x02
	csOutputTerminal = 0x03
	csMixer          = 0x04
	csSelector       = 0x05
	csFeature        = 0x06
	asGeneral        = 0x01
	terminalStream   = 0x0101
	// The subtypes whose meaning differs between the two classes.
	uac1Processing   = 0x07
	uac1Extension    = 0x08
	uac2Effect       = 0x07
	uac2Processing   = 0x08
	uac2Extension    = 0x09
	uac2ClockSource  = 0x0a
	uac2ClockSel     = 0x0b
	uac2ClockMult    = 0x0c
	uac2RateConvertr = 0x0d
)

// The gain controls' selectors, and the feature unit controls each class
// declares.
var (
	uac1Gains = []int{2, 3, 4, 5, 6}
	uac2Gains = []int{2, 3, 4, 5, 6, 11, 12}
)

// errTopology prefixes every refusal of the parse: a topology that does not
// resolve, which the gain check refuses rather than takes for clean.
var errTopology = errors.New("the descriptors do not resolve")

func unresolved(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errTopology, fmt.Sprintf(format, a...))
}

// ParsePlayback is the playback path of the streaming interface sel names,
// from the unit's USB descriptors as sysfs gives them, read strictly: the
// configuration sel names, held once; the streaming interface's alternate
// setting, held once, with one class-specific general descriptor and an
// isochronous endpoint toward the device; the audio control interface whose
// collection holds it (an audio class 1 header's interface list, an audio
// class 2 interface association), held once, of the same class version
// (UAC1 or UAC2; UAC3 is refused). The streaming interface's terminal link
// must name an input terminal; every output terminal but a USB streaming
// one, which only returns samples to the host, is followed upstream through
// its units' sources, and every unit on the way from that input terminal is
// on the path. A source that names no entity, or a clock entity, an output
// terminal named as a source, a cycle, an entity of a subtype the class
// does not define on the way, a descriptor cut short, and an input terminal
// that reaches no output terminal are refused: what the parse cannot
// follow it never takes for clean. Capture paths (an input terminal of
// another kind toward a USB streaming output) are not the playback path.
func ParsePlayback(desc []byte, sel Selection) (Path, error) {
	cfg, err := configuration(desc, sel.Config)
	if err != nil {
		return Path{}, err
	}
	alts, iads, err := interfaces(cfg)
	if err != nil {
		return Path{}, err
	}
	as, err := streaming(alts, sel)
	if err != nil {
		return Path{}, err
	}
	ac, err := control(alts, iads, as)
	if err != nil {
		return Path{}, err
	}
	protocol := 1
	if as.protocol == protocolUAC2 {
		protocol = 2
	}
	link, err := terminalLink(as, protocol)
	if err != nil {
		return Path{}, err
	}
	ents, err := entities(ac, protocol)
	if err != nil {
		return Path{}, err
	}
	return follow(ents, link, protocol)
}

// alt is one alternate setting of an interface in the configuration: its
// numbers, class, and the descriptors that follow it.
type alt struct {
	number, setting, class, subclass, protocol int
	cs, endpoints                              [][]byte
}

// association is an interface association descriptor.
type association struct {
	first, count, class int
}

// configuration is the descriptors of the configuration whose value is
// value, after its configuration descriptor. The stream is the device
// descriptor, then each configuration's descriptors, as long as its
// wTotalLength says.
func configuration(desc []byte, value int) ([]byte, error) {
	if len(desc) < 18 || desc[0] != 18 || desc[1] != descDevice {
		return nil, unresolved("they do not begin with a device descriptor")
	}
	var found []byte
	for off := 18; off < len(desc); {
		rest := desc[off:]
		if len(rest) < 9 || rest[0] < 9 || rest[1] != descConfig {
			return nil, unresolved("a configuration descriptor is cut short or missing")
		}
		total := int(binary.LittleEndian.Uint16(rest[2:]))
		if total < int(rest[0]) || total > len(rest) {
			return nil, unresolved("a configuration's total length runs past its descriptors")
		}
		if int(rest[5]) == value {
			if found != nil {
				return nil, unresolved("two configurations hold the value %d", value)
			}
			found = rest[rest[0]:total]
		}
		off += total
	}
	if found == nil {
		return nil, unresolved("no configuration holds the value %d", value)
	}
	return found, nil
}

// interfaces are the configuration's interfaces' alternate settings, each
// with its class-specific and endpoint descriptors, and its interface
// associations.
func interfaces(cfg []byte) ([]alt, []association, error) {
	var alts []alt
	var iads []association
	for b := cfg; len(b) > 0; {
		if len(b) < 2 || b[0] < 2 || int(b[0]) > len(b) {
			return nil, nil, unresolved("a descriptor's length runs past its configuration")
		}
		d := b[:b[0]]
		b = b[len(d):]
		switch d[1] {
		case descAssociation:
			if len(d) < 8 {
				return nil, nil, unresolved("an interface association is cut short")
			}
			iads = append(iads, association{first: int(d[2]), count: int(d[3]), class: int(d[4])})
		case descInterface:
			if len(d) < 9 {
				return nil, nil, unresolved("an interface descriptor is cut short")
			}
			alts = append(alts, alt{number: int(d[2]), setting: int(d[3]), class: int(d[5]), subclass: int(d[6]), protocol: int(d[7])})
		case descCSInterface, descEndpoint:
			if len(alts) == 0 {
				return nil, nil, unresolved("a descriptor precedes every interface")
			}
			a := &alts[len(alts)-1]
			if d[1] == descCSInterface {
				if len(d) < 3 {
					return nil, nil, unresolved("a class-specific descriptor is cut short")
				}
				a.cs = append(a.cs, d)
			} else {
				if len(d) < 7 {
					return nil, nil, unresolved("an endpoint descriptor is cut short")
				}
				a.endpoints = append(a.endpoints, d)
			}
		}
	}
	return alts, iads, nil
}

// streaming is the audio streaming interface's alternate setting sel names,
// held once, of an audio class the parse reads, with an isochronous
// endpoint toward the device.
func streaming(alts []alt, sel Selection) (alt, error) {
	var found []alt
	for _, a := range alts {
		if a.class == classAudio && a.subclass == subclassStream && a.number == sel.Interface && a.setting == sel.Altset {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		return alt{}, unresolved("%d audio streaming interfaces are interface %d, altset %d", len(found), sel.Interface, sel.Altset)
	}
	as := found[0]
	if as.protocol != protocolUAC1 && as.protocol != protocolUAC2 {
		return alt{}, unresolved("interface %d is of an audio class the check does not read (protocol %#x)", sel.Interface, as.protocol)
	}
	if !slices.ContainsFunc(as.endpoints, func(e []byte) bool { return e[2]&0x80 == 0 && e[3]&0x03 == 0x01 }) {
		return alt{}, unresolved("interface %d, altset %d has no isochronous endpoint toward the device: it is no playback interface", sel.Interface, sel.Altset)
	}
	return as, nil
}

// control is the audio control interface whose collection holds as: an
// audio class 1 header that lists its number, or an audio class 2 interface
// association that holds both; one, of as's class.
func control(alts []alt, iads []association, as alt) (alt, error) {
	var found []alt
	for _, a := range alts {
		if a.class != classAudio || a.subclass != subclassControl || a.setting != 0 {
			continue
		}
		holds := false
		if as.protocol == protocolUAC1 {
			for _, d := range a.cs {
				if d[2] != csHeader {
					continue
				}
				if len(d) < 8 || len(d) < 8+int(d[7]) {
					return alt{}, unresolved("audio control interface %d's header is cut short", a.number)
				}
				holds = holds || slices.Contains(d[8:8+int(d[7])], byte(as.number))
			}
		} else {
			holds = slices.ContainsFunc(iads, func(i association) bool {
				in := func(n int) bool { return i.first <= n && n < i.first+i.count }
				return i.class == classAudio && in(a.number) && in(as.number)
			})
		}
		if holds {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		return alt{}, unresolved("%d audio control interfaces hold streaming interface %d", len(found), as.number)
	}
	if found[0].protocol != as.protocol {
		return alt{}, unresolved("audio control interface %d and streaming interface %d are of different audio classes", found[0].number, as.number)
	}
	return found[0], nil
}

// terminalLink is the terminal as's general descriptor links it to.
func terminalLink(as alt, protocol int) (int, error) {
	var general [][]byte
	for _, d := range as.cs {
		if d[2] == asGeneral {
			general = append(general, d)
		}
	}
	if len(general) != 1 {
		return 0, unresolved("streaming interface %d, altset %d has %d general descriptors", as.number, as.setting, len(general))
	}
	need := 7
	if protocol == 2 {
		need = 16
	}
	if len(general[0]) < need {
		return 0, unresolved("streaming interface %d's general descriptor is cut short", as.number)
	}
	return int(general[0][3]), nil
}

// entity is an audio control interface's terminal, unit or clock.
type entity struct {
	id      int
	subtype byte
	// kind is a unit's kind, "" for a terminal, a clock and a subtype the
	// class does not define.
	kind     UnitKind
	terminal int
	known    bool
	clock    bool
	sources  []int
	desc     []byte
}

// entities are ac's terminals, units and clocks by ID, each ID held once.
func entities(ac alt, protocol int) (map[int]entity, error) {
	ents := map[int]entity{}
	for _, d := range ac.cs {
		if d[2] == csHeader {
			continue
		}
		if len(d) < 4 {
			return nil, unresolved("an entity of audio control interface %d is cut short", ac.number)
		}
		e, err := entityOf(d, protocol)
		if err != nil {
			return nil, err
		}
		if e.id == 0 {
			return nil, unresolved("an entity has ID 0, which the class reserves")
		}
		if _, dup := ents[e.id]; dup {
			return nil, unresolved("two entities have ID %d", e.id)
		}
		ents[e.id] = e
	}
	return ents, nil
}

// entityOf reads one entity: its ID, its kind and its sources, with the
// lengths its fields need.
func entityOf(d []byte, protocol int) (entity, error) {
	e := entity{id: int(d[3]), subtype: d[2], desc: d, known: true}
	short := func() (entity, error) {
		return entity{}, unresolved("entity %d (subtype %#x) is cut short", e.id, e.subtype)
	}
	pins := func(at int) error {
		if len(d) <= at {
			return errors.New("short")
		}
		p := int(d[at])
		if len(d) < at+1+p {
			return errors.New("short")
		}
		for _, s := range d[at+1 : at+1+p] {
			e.sources = append(e.sources, int(s))
		}
		return nil
	}
	switch {
	case e.subtype == csInputTerminal:
		if protocol == 1 && len(d) < 12 || protocol == 2 && len(d) < 17 {
			return short()
		}
		e.terminal = int(binary.LittleEndian.Uint16(d[4:]))
	case e.subtype == csOutputTerminal:
		if protocol == 1 && len(d) < 9 || protocol == 2 && len(d) < 12 {
			return short()
		}
		e.terminal = int(binary.LittleEndian.Uint16(d[4:]))
		e.sources = []int{int(d[7])}
	case e.subtype == csMixer:
		e.kind = UnitMixer
		if pins(4) != nil {
			return short()
		}
	case e.subtype == csSelector:
		e.kind = UnitSelector
		if pins(4) != nil {
			return short()
		}
	case e.subtype == csFeature:
		e.kind = UnitFeature
		if len(d) < 6 {
			return short()
		}
		e.sources = []int{int(d[4])}
	case protocol == 1 && e.subtype == uac1Processing, protocol == 2 && e.subtype == uac2Processing:
		e.kind = UnitProcessing
		if pins(6) != nil {
			return short()
		}
	case protocol == 1 && e.subtype == uac1Extension, protocol == 2 && e.subtype == uac2Extension:
		e.kind = UnitExtension
		if pins(6) != nil {
			return short()
		}
	case protocol == 2 && e.subtype == uac2Effect:
		e.kind = UnitEffect
		if len(d) < 7 {
			return short()
		}
		e.sources = []int{int(d[6])}
	case protocol == 2 && e.subtype == uac2RateConvertr:
		e.kind = UnitRateConverter
		if len(d) < 8 {
			return short()
		}
		e.sources = []int{int(d[4])}
	case protocol == 2 && (e.subtype == uac2ClockSource || e.subtype == uac2ClockSel || e.subtype == uac2ClockMult):
		e.clock = true
	default:
		e.known = false
	}
	return e, nil
}

// follow is the path from input terminal link to every output terminal but
// a USB streaming one: each such terminal is followed upstream, and a unit
// that reaches link is on the path.
func follow(ents map[int]entity, link, protocol int) (Path, error) {
	it, ok := ents[link]
	if !ok || it.subtype != csInputTerminal {
		return Path{}, unresolved("the streaming interface links to entity %d, which is no input terminal", link)
	}
	const (
		unvisited = iota
		visiting
		visited
	)
	state := map[int]int{}
	reaches := map[int]bool{}
	var visit func(id int) (bool, error)
	visit = func(id int) (bool, error) {
		e, ok := ents[id]
		switch {
		case !ok:
			return false, unresolved("a source names entity %d, which the interface does not hold", id)
		case e.clock:
			return false, unresolved("a source names clock entity %d", id)
		case !e.known:
			return false, unresolved("entity %d is of a subtype (%#x) the class does not define, so its sources cannot be followed", id, e.subtype)
		case e.subtype == csOutputTerminal:
			return false, unresolved("a source names output terminal %d", id)
		case e.subtype == csInputTerminal:
			return id == link, nil
		}
		switch state[id] {
		case visiting:
			return false, unresolved("the units form a cycle through unit %d", id)
		case visited:
			return reaches[id], nil
		}
		state[id] = visiting
		r := false
		for _, s := range e.sources {
			ok, err := visit(s)
			if err != nil {
				return false, err
			}
			r = r || ok
		}
		state[id], reaches[id] = visited, r
		return r, nil
	}
	ids := make([]int, 0, len(ents))
	for id := range ents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	p := Path{Protocol: protocol, Terminal: link}
	for _, id := range ids {
		e := ents[id]
		if e.subtype != csOutputTerminal || e.terminal == terminalStream {
			continue
		}
		ok, err := visit(e.sources[0])
		if err != nil {
			return Path{}, err
		}
		if ok {
			p.Outputs = append(p.Outputs, id)
		}
	}
	if len(p.Outputs) == 0 {
		return Path{}, unresolved("input terminal %d reaches no output terminal toward the device's outputs", link)
	}
	for _, id := range ids {
		e := ents[id]
		if e.kind == "" || state[id] != visited || !reaches[id] {
			continue
		}
		u := Unit{ID: id, Kind: e.kind}
		if e.kind == UnitFeature {
			gains, err := featureGains(e.desc, protocol)
			if err != nil {
				return Path{}, err
			}
			u.Gains = gains
		}
		p.Units = append(p.Units, u)
	}
	return p, nil
}

// featureGains are the gain controls a feature unit's descriptor declares:
// in audio class 1, bControlSize bytes a channel (the master first), one bit
// a control; in audio class 2, four bytes a channel, two bits a control,
// either set declaring it. The bytes after the last channel's are iFeature.
func featureGains(d []byte, protocol int) ([]GainControl, error) {
	id := int(d[3])
	var body []byte
	size := 4
	selectors := uac2Gains
	if protocol == 1 {
		if len(d) < 7 || d[5] == 0 {
			return nil, unresolved("feature unit %d's control size is cut short or zero", id)
		}
		size, body, selectors = int(d[5]), d[6:len(d)-1], uac1Gains
	} else {
		if len(d) < 10 {
			return nil, unresolved("feature unit %d is cut short", id)
		}
		body = d[5 : len(d)-1]
	}
	if len(body) < size || len(body)%size != 0 {
		return nil, unresolved("feature unit %d's controls do not fill whole channels", id)
	}
	declares := func(ch, selector int) bool {
		var m uint64
		for i, b := range body[ch*size : (ch+1)*size] {
			if i < 8 {
				m |= uint64(b) << (8 * i)
			}
		}
		if protocol == 1 {
			return m&(1<<(selector-1)) != 0
		}
		return (m>>(2*(selector-1)))&3 != 0
	}
	var gains []GainControl
	for _, s := range selectors {
		g := GainControl{Selector: s, Master: declares(0, s)}
		for ch := 1; ch < len(body)/size; ch++ {
			g.Channels = g.Channels || declares(ch, s)
		}
		if g.Master || g.Channels {
			gains = append(gains, g)
		}
	}
	return gains, nil
}
