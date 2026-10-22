package alsa

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxPCMHops is how many slave.pcm follows a name may take. One more
// leaves the device unresolved.
const maxPCMHops = 8

// PCMConfig is the ALSA configuration a resolution reads, loaded once: the
// PCM definitions of etc/alsa/conf.d/*.conf and etc/asound.conf under a
// config root. Resolving from it reads nothing, so a caller that bounds the
// load bounds every resolution made from it.
type PCMConfig struct {
	defs map[string]pcmDef
	// ok is false when a file that is present could not be used whole: no
	// definition is then used.
	ok bool
	// unread is ok false because the read stopped at a file it could not
	// read (Unread).
	unread bool
}

// LoadPCMConfig reads the ALSA configuration under root once.
func LoadPCMConfig(root string) PCMConfig {
	defs, ok, unread := loadPCMDefs(root)
	return PCMConfig{defs: defs, ok: ok, unread: unread}
}

// Unread reports whether the read of the configuration stopped at a file
// that is there and could not be read (an open or read error, what opened
// as no regular file, a file past the read's bounds, a conf.d it could not
// list), rather than at a file
// it read that holds what the parser does not model. Either way no
// definition is used; but what an unread file says is not known, and it
// can read the next time, while a file the parser does not model resolves
// nothing the same way every time. The output reading takes an unread
// configuration for one it did not have.
func (c PCMConfig) Unread() bool { return c.unread }

// ResolveOutputDevice is the package's ResolveOutputDevice from c.
func (c PCMConfig) ResolveOutputDevice(device string, cards []Card) (cardIndex, pcmDevice int, ok bool) {
	if idx, pcm, found := ResolveALSADevice(device, cards); found {
		return idx, pcm, true
	}
	device = strings.TrimSpace(device)
	if device == "" || strings.Contains(device, ":") || !c.ok {
		return 0, 0, false
	}
	return walkPCM(c, device, cards, map[string]bool{}, 0)
}

// ResolveOutputChain is the package's ResolveOutputChain from c: a device
// string's own stages, or the stages a PCM name's definitions take to a
// card. It is the chain as the bridge follows it, information beside the
// output reading's verdict, never a proof of what alsa-lib loaded.
func (c PCMConfig) ResolveOutputChain(device string, cards []Card) (Chain, bool) {
	if idx, pcm, found := ResolveALSADevice(device, cards); found {
		stages, known := deviceStringStages(device)
		if !known {
			return Chain{}, false
		}
		return Chain{Card: idx, Device: pcm, Stages: stages}, true
	}
	device = strings.TrimSpace(device)
	if device == "" || strings.Contains(device, ":") || !c.ok {
		return Chain{}, false
	}
	chain, ok, known := walkChain(c, device, cards, map[string]bool{}, 0)
	if !ok || !known {
		return Chain{}, false
	}
	return chain, true
}

// maxAsoundFiles and maxAsoundBytes bound the configuration read for
// one resolution. One file is also bounded by maxFileBytes.
const (
	maxAsoundFiles = 64
	maxAsoundBytes = 4 << 20
)

// ConfigRoot is the directory that holds etc/asound.conf for a proc
// root. A root whose last element is "proc" uses its parent, so a
// captured tree and /proc both find the asound files beside that tree.
// Anything else reads the host root.
func ConfigRoot(procRoot string) string {
	if procRoot == "" {
		return string(filepath.Separator)
	}
	clean := filepath.Clean(procRoot)
	if filepath.Base(clean) != "proc" {
		return string(filepath.Separator)
	}
	parent := filepath.Dir(clean)
	if parent == clean {
		return string(filepath.Separator)
	}
	return parent
}

// ResolveOutputDevice maps an MPD alsa device onto a card that is in
// cards. A hw or plughw string (and plug and front) names the card
// directly. Any other name is a PCM: it is looked up in etc/alsa/conf.d
// and then etc/asound.conf, and slave.pcm is followed through type empty
// and type plug only, until a type hw. A slave that contains ":" is a
// device string, never a PCM of that name. A name defined more than once
// is unresolved. Inside that block, @args, @func, a $ variable and a
// nested block this parser does not model make the name opaque and leave
// the rest of the file usable. A ctl or defaults stanza is skipped. One
// file that cannot be read, or a top-level @hooks, @args, @func or
// include, makes the whole configuration unresolved. A dmix, a softvol,
// any other plugin, a cycle, or more than maxPCMHops follows leaves the
// device unresolved.
func ResolveOutputDevice(root, device string, cards []Card) (cardIndex, pcmDevice int, ok bool) {
	if idx, pcm, found := ResolveALSADevice(device, cards); found {
		return idx, pcm, true
	}
	device = strings.TrimSpace(device)
	if device == "" || strings.Contains(device, ":") {
		return 0, 0, false
	}
	return LoadPCMConfig(root).ResolveOutputDevice(device, cards)
}

// Chain is how an MPD alsa device reaches a card: the plugin type of each
// stage from the device string to the hw stage that names the card, in
// order. hw:N is ["hw"], plughw:N is ["plug", "hw"], and Volumio's
// "volumio" PCM is ["empty", "plug", "hw"]. A chain resolves through
// empty, plug and hw only, so a dmix, softvol, rate or route stage never
// appears in one: such a device has no Chain.
type Chain struct {
	Card   int
	Device int
	Stages []string
}

// ResolveOutputChain is ResolveOutputDevice with the stages it followed.
// It resolves only where ResolveOutputDevice does, to the same card and
// PCM device, and not for a front or plug device string, at the start or
// as a slave: what those name is defined in alsa-lib's own configuration
// files (the card's front PCM, plug's slave argument), which this does not
// read, so their stages are not known.
func ResolveOutputChain(root, device string, cards []Card) (Chain, bool) {
	return LoadPCMConfig(root).ResolveOutputChain(device, cards)
}

// deviceStringStages is the stages a device string names directly: hw is
// the card itself and plughw a plug over it. front and plug are resolved
// to a card by ResolveALSADevice, and their stages are not known here.
func deviceStringStages(device string) ([]string, bool) {
	plugin, _, _ := strings.Cut(strings.TrimSpace(device), ":")
	switch plugin {
	case "hw":
		return []string{"hw"}, true
	case "plughw":
		return []string{"plug", "hw"}, true
	}
	return nil, false
}

type pcmDef struct {
	typ         string
	slave       string
	card        string
	cardIndex   int
	cardIsIndex bool
	device      int
	hasDevice   bool
	opaque      bool
	// typeUnsure is a definition whose type is not one value: a second
	// type in its block, or its name defined more than once.
	typeUnsure bool
	// routes is a block with a transfer table or a route policy (ttable,
	// route_policy), which a plug runs through the route plugin inside it.
	// Such a field makes the definition opaque too: its chain is not known.
	routes bool
}

// sampleStageTypes are the plugin types whose stage changes the samples it
// passes on, whatever the format it hands its slave: software volume, a
// rate converter, a mixer of several streams into one format and rate of
// its own, a channel router, and a LADSPA filter.
var sampleStageTypes = []string{"softvol", "rate", "dmix", "route", "ladspa"}

// SampleStageTypes is every plugin type SampleStage can name.
func SampleStageTypes() []string { return append([]string(nil), sampleStageTypes...) }

// SampleStage is the plugin type of the first stage that changes the
// samples (SampleStageTypes) the walk from device meets, through empty and
// plug stages, as the configuration read defines them; "" when it meets
// none before it reaches a card or stops. A plug with a transfer table or
// a route policy is the route plugin inside it, a route stage, though it
// has no chain. alsa-lib's own dmix is a dmix stage: a dmix: device string,
// which names its plugin without the files, and the name dmix where the
// files define none. The walk does not go through such a stage, so what
// lies past it is not read, and any other device string names none. It is
// what the files say, not what alsa-lib loaded for the process that opened
// the card: information, not proof.
func (c PCMConfig) SampleStage(device string) string {
	device = strings.TrimSpace(device)
	switch {
	case device == "":
		return ""
	case strings.Contains(device, ":"):
		return deviceStringStage(device)
	case !c.ok:
		return ""
	}
	return sampleStage(c, device, map[string]bool{}, 0)
}

func sampleStage(c PCMConfig, name string, seen map[string]bool, hops int) string {
	if strings.Contains(name, ":") {
		return deviceStringStage(name)
	}
	if hops > maxPCMHops || seen[name] {
		return ""
	}
	def, found := c.defs[name]
	switch {
	case !found && name == "dmix":
		return "dmix"
	case !found || def.typeUnsure:
		return ""
	case slices.Contains(sampleStageTypes, def.typ):
		return def.typ
	case def.typ == "plug" && def.routes:
		// The route plugin inside the plug can swap, mix or attenuate
		// channels at the same format, rate and channels.
		return "route"
	case def.opaque || (def.typ != "empty" && def.typ != "plug") || def.slave == "":
		return ""
	}
	seen[name] = true
	return sampleStage(c, def.slave, seen, hops+1)
}

// deviceStringStage is the sample stage a device string names by its
// plugin: alsa-lib's dmix (dmix:CARD=…,DEV=…), which mixes every stream
// into a format and rate of its own; none for any other.
func deviceStringStage(device string) string {
	if plugin, _, _ := strings.Cut(device, ":"); plugin == "dmix" {
		return "dmix"
	}
	return ""
}

func walkPCM(c PCMConfig, name string, cards []Card, seen map[string]bool, hops int) (int, int, bool) {
	chain, ok, _ := walkChain(c, name, cards, seen, hops)
	return chain.Card, chain.Device, ok
}

// walkChain follows name to a card. ok is whether it reached one, and
// known whether every stage on the way is one whose type it knows: a slave
// that is a front or plug device string reaches a card without that.
func walkChain(c PCMConfig, name string, cards []Card, seen map[string]bool, hops int) (chain Chain, ok, known bool) {
	if strings.Contains(name, ":") {
		idx, pcm, found := ResolveALSADevice(name, cards)
		if !found {
			return Chain{}, false, false
		}
		stages, known := deviceStringStages(name)
		return Chain{Card: idx, Device: pcm, Stages: stages}, true, known
	}
	if hops > maxPCMHops || seen[name] {
		return Chain{}, false, false
	}
	def, found := c.defs[name]
	if !found || def.opaque {
		return Chain{}, false, false
	}
	seen[name] = true
	switch def.typ {
	case "hw":
		pcm := 0
		if def.hasDevice {
			pcm = def.device
		}
		if def.cardIsIndex {
			if !cardIndexExists(cards, def.cardIndex) {
				return Chain{}, false, false
			}
			return Chain{Card: def.cardIndex, Device: pcm, Stages: []string{"hw"}}, true, true
		}
		if def.card == "" {
			return Chain{}, false, false
		}
		idx, unique := uniqueCardID(cards, def.card)
		if !unique {
			return Chain{}, false, false
		}
		return Chain{Card: idx, Device: pcm, Stages: []string{"hw"}}, true, true
	case "empty", "plug":
		if def.slave == "" {
			return Chain{}, false, false
		}
		inner, ok, known := walkChain(c, def.slave, cards, seen, hops+1)
		if !ok {
			return Chain{}, false, false
		}
		inner.Stages = append([]string{def.typ}, inner.Stages...)
		return inner, true, known
	default:
		return Chain{}, false, false
	}
}

// loadPCMDefs reads conf.d and then asound.conf. The second result, ok, is
// false when any file that is present cannot be read, or parses as
// something this reader does not fully model, and the third, unread, is
// true when the read stopped at the first of those; a conf.d that is there
// and cannot be listed is a file that cannot be read. A missing file, or a
// missing conf.d, is neither: the other files still count. A false ok
// means no definition may be used.
func loadPCMDefs(root string) (map[string]pcmDef, bool, bool) {
	defs := map[string]pcmDef{}
	var files int
	var total int
	bad, unread := false, false
	add := func(path string) {
		if bad {
			return
		}
		b, err := readCapped(path, maxFileBytes)
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			bad, unread = true, true
			return
		}
		if files >= maxAsoundFiles || total+len(b) > maxAsoundBytes {
			bad, unread = true, true
			return
		}
		files++
		total += len(b)
		parsed, ok := parsePCMFile(string(b))
		if !ok {
			bad = true
			return
		}
		for name, def := range parsed {
			if prev, exists := defs[name]; exists {
				prev.opaque, prev.typeUnsure = true, true
				defs[name] = prev
				continue
			}
			defs[name] = def
		}
	}
	// conf.d is listed here, not by filepath.Glob, which takes a directory
	// it cannot stat, open or list for one with nothing in it: a conf.d
	// that is there and cannot be listed is a configuration not read.
	confd := filepath.Join(root, "etc", "alsa", "conf.d")
	switch entries, err := os.ReadDir(confd); {
	case err == nil:
		// ReadDir sorts by name.
		for _, e := range entries {
			if ok, _ := filepath.Match("*.conf", e.Name()); ok {
				add(filepath.Join(confd, e.Name()))
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		bad, unread = true, true
	}
	add(filepath.Join(root, "etc", "asound.conf"))
	if bad {
		return nil, false, unread
	}
	return defs, true, false
}

const (
	tokEOF = iota
	tokIdent
	tokString
	tokNumber
	tokDot
	tokBang
	tokLBrace
	tokRBrace
	tokSemi
	tokComma
	tokVar
	tokLBrack
	tokRBrack
	tokColon
)

type pcmTok struct {
	kind int
	val  string
}

type pcmParser struct {
	tok []pcmTok
	i   int
}

func parsePCMFile(src string) (map[string]pcmDef, bool) {
	toks, ok := lexPCM(src)
	if !ok {
		return nil, false
	}
	p := pcmParser{tok: toks}
	defs := map[string]pcmDef{}
	for {
		p.skipSep()
		if p.peek().kind == tokEOF {
			return defs, true
		}
		if atDirective(p.peek()) {
			return nil, false
		}
		if p.peek().kind == tokIdent && p.peek().val == "pcm" {
			if !p.isPCM() {
				return nil, false
			}
			name, def, ok := p.parsePCMDef()
			if !ok {
				return nil, false
			}
			if prev, exists := defs[name]; exists {
				prev.opaque, prev.typeUnsure = true, true
				defs[name] = prev
				continue
			}
			defs[name] = def
			continue
		}
		if !p.skipStanza() {
			return nil, false
		}
	}
}

func (p *pcmParser) isPCM() bool {
	return p.peek().kind == tokIdent && p.peek().val == "pcm" && p.i+1 < len(p.tok) && p.tok[p.i+1].kind == tokDot
}

func (p *pcmParser) parsePCMDef() (string, pcmDef, bool) {
	p.next()
	if p.next().kind != tokDot {
		return "", pcmDef{}, false
	}
	if p.peek().kind == tokBang {
		p.next()
	}
	name := p.next()
	if name.kind != tokIdent && name.kind != tokString {
		return "", pcmDef{}, false
	}
	def, ok := p.parsePCMBlock()
	if !ok {
		return "", pcmDef{}, false
	}
	return name.val, def, true
}

func (p *pcmParser) skipSep() {
	for p.peek().kind == tokSemi || p.peek().kind == tokComma {
		p.next()
	}
}

func atDirective(tok pcmTok) bool {
	return tok.kind == tokIdent && strings.HasPrefix(tok.val, "@")
}

func (p *pcmParser) skipStanza() bool {
	name := p.next()
	if name.kind != tokIdent || atDirective(name) {
		return false
	}
	for p.peek().kind == tokDot {
		p.next()
		if p.peek().kind == tokBang {
			p.next()
		}
		n := p.next()
		if atDirective(n) || (n.kind != tokIdent && n.kind != tokString && n.kind != tokNumber) {
			return false
		}
	}
	if p.peek().kind == tokLBrace {
		return p.skipBlock()
	}
	_, ok := p.parseValue()
	return ok
}

func (p *pcmParser) skipBlock() bool {
	if p.next().kind != tokLBrace {
		return false
	}
	depth := 1
	for depth > 0 {
		switch p.next().kind {
		case tokEOF:
			return false
		case tokLBrace:
			depth++
		case tokRBrace:
			depth--
		}
	}
	return true
}

func (p *pcmParser) skipBrackets() bool {
	if p.next().kind != tokLBrack {
		return false
	}
	depth := 1
	brace := 0
	for depth > 0 || brace > 0 {
		switch p.next().kind {
		case tokEOF:
			return false
		case tokLBrack:
			depth++
		case tokRBrack:
			if depth == 0 {
				return false
			}
			depth--
		case tokLBrace:
			brace++
		case tokRBrace:
			if brace == 0 {
				return false
			}
			brace--
		}
	}
	return true
}

func (p *pcmParser) peek() pcmTok {
	if p.i >= len(p.tok) {
		return pcmTok{kind: tokEOF}
	}
	return p.tok[p.i]
}

func (p *pcmParser) next() pcmTok {
	t := p.peek()
	if p.i < len(p.tok) {
		p.i++
	}
	return t
}

func (p *pcmParser) parsePCMBlock() (pcmDef, bool) {
	if p.next().kind != tokLBrace {
		return pcmDef{}, false
	}
	var def pcmDef
	var sawType, sawCard, sawDevice bool
	for {
		p.skipSep()
		switch p.peek().kind {
		case tokRBrace:
			p.next()
			if def.typ != "hw" && def.typ != "empty" && def.typ != "plug" {
				def.opaque = true
			}
			return def, true
		case tokEOF:
			return pcmDef{}, false
		default:
			if !p.parsePCMField(&def, &sawType, &sawCard, &sawDevice) {
				return pcmDef{}, false
			}
		}
	}
}

// parsePCMField reads one field of a pcm block. A field this parser does
// not model makes the definition opaque and is still consumed, so the
// block's closing brace stays the end of this PCM and the rest of the
// file remains usable.
func (p *pcmParser) parsePCMField(def *pcmDef, sawType, sawCard, sawDevice *bool) bool {
	key, plain, ok := p.readFieldKey()
	if !ok {
		return false
	}
	if key == "ttable" || strings.HasPrefix(key, "ttable.") || key == "route_policy" {
		def.routes = true
	}
	if p.peek().kind == tokLBrace {
		if plain && key == "slave" {
			inner, ok := p.parseSlaveBlock()
			if !ok {
				return false
			}
			if def.slave != "" || inner.unknown || inner.pcm == "" {
				def.opaque = true
			} else {
				def.slave = inner.pcm
			}
			return true
		}
		def.opaque = true
		return p.skipBlock()
	}
	if p.peek().kind == tokLBrack {
		def.opaque = true
		return p.skipBrackets()
	}
	val, plainVal, ok := p.readFieldValue()
	if !ok {
		return false
	}
	if !plain || !plainVal {
		def.opaque = true
		return true
	}
	switch key {
	case "type":
		if *sawType {
			def.opaque, def.typeUnsure = true, true
		}
		*sawType = true
		def.typ = val
	case "slave.pcm":
		if def.slave != "" {
			def.opaque = true
		} else {
			def.slave = val
		}
	case "card":
		if *sawCard {
			def.opaque = true
		}
		*sawCard = true
		if n, err := strconv.Atoi(val); err == nil && cardNumber(val) {
			def.cardIsIndex = true
			def.cardIndex = n
			def.card = ""
		} else {
			def.cardIsIndex = false
			def.card = val
		}
	case "device":
		if *sawDevice {
			def.opaque = true
		}
		*sawDevice = true
		n, err := strconv.Atoi(val)
		if err != nil || !cardNumber(val) {
			def.opaque = true
		} else {
			def.hasDevice = true
			def.device = n
		}
	default:
		def.opaque = true
	}
	return true
}

func (p *pcmParser) readFieldKey() (string, bool, bool) {
	tok := p.next()
	plain := true
	var b strings.Builder
	switch tok.kind {
	case tokIdent:
		if atDirective(tok) {
			plain = false
		}
		b.WriteString(tok.val)
	case tokVar, tokNumber:
		plain = false
		b.WriteString(tok.val)
	default:
		return "", false, false
	}
	for p.peek().kind == tokDot || p.peek().kind == tokColon {
		sep := p.next()
		if sep.kind == tokColon {
			plain = false
		}
		n := p.next()
		if n.kind != tokIdent && n.kind != tokNumber && n.kind != tokVar && n.kind != tokString {
			return "", false, false
		}
		if n.kind != tokIdent || atDirective(n) {
			plain = false
		}
		if sep.kind == tokDot {
			b.WriteByte('.')
		} else {
			b.WriteByte(':')
		}
		b.WriteString(n.val)
	}
	return b.String(), plain, true
}

func (p *pcmParser) readFieldValue() (string, bool, bool) {
	tok := p.next()
	plain := true
	var b strings.Builder
	switch tok.kind {
	case tokString, tokIdent, tokNumber:
		b.WriteString(tok.val)
	case tokVar:
		plain = false
		b.WriteString(tok.val)
	default:
		return "", false, false
	}
	for p.peek().kind == tokColon {
		p.next()
		n := p.next()
		if n.kind != tokIdent && n.kind != tokNumber && n.kind != tokString && n.kind != tokVar {
			return "", false, false
		}
		plain = false
		b.WriteByte(':')
		b.WriteString(n.val)
	}
	return b.String(), plain, true
}

func cardNumber(val string) bool {
	if val == "" {
		return false
	}
	for _, r := range val {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type slaveBlock struct {
	pcm     string
	unknown bool
}

func (p *pcmParser) parseSlaveBlock() (slaveBlock, bool) {
	if p.next().kind != tokLBrace {
		return slaveBlock{}, false
	}
	var b slaveBlock
	for {
		p.skipSep()
		if p.peek().kind == tokRBrace {
			p.next()
			return b, true
		}
		if p.peek().kind == tokEOF {
			return slaveBlock{}, false
		}
		key, plain, ok := p.readFieldKey()
		if !ok {
			return slaveBlock{}, false
		}
		if p.peek().kind == tokLBrace {
			b.unknown = true
			if !p.skipBlock() {
				return slaveBlock{}, false
			}
			continue
		}
		if p.peek().kind == tokLBrack {
			b.unknown = true
			if !p.skipBrackets() {
				return slaveBlock{}, false
			}
			continue
		}
		val, plainVal, ok := p.readFieldValue()
		if !ok {
			return slaveBlock{}, false
		}
		if plain && plainVal && key == "pcm" && b.pcm == "" {
			b.pcm = val
		} else {
			b.unknown = true
		}
	}
}

func (p *pcmParser) parseValue() (string, bool) {
	t := p.next()
	if t.kind == tokString || t.kind == tokIdent || t.kind == tokNumber {
		return t.val, true
	}
	return "", false
}

func lexPCM(src string) ([]pcmTok, bool) {
	var out []pcmTok
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRuneInString(src[i:])
		if r == utf8.RuneError && size == 1 {
			return nil, false
		}
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		if r == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		switch r {
		case '{':
			out = append(out, pcmTok{kind: tokLBrace})
			i += size
		case '}':
			out = append(out, pcmTok{kind: tokRBrace})
			i += size
		case '.':
			out = append(out, pcmTok{kind: tokDot})
			i += size
		case '!':
			out = append(out, pcmTok{kind: tokBang})
			i += size
		case ';':
			out = append(out, pcmTok{kind: tokSemi})
			i += size
		case ',':
			out = append(out, pcmTok{kind: tokComma})
			i += size
		case '[':
			out = append(out, pcmTok{kind: tokLBrack})
			i += size
		case ']':
			out = append(out, pcmTok{kind: tokRBrack})
			i += size
		case ':':
			out = append(out, pcmTok{kind: tokColon})
			i += size
		case '$':
			j := i + size
			if j >= len(src) {
				return nil, false
			}
			r2, n2 := utf8.DecodeRuneInString(src[j:])
			if r2 == '@' || !(r2 == '_' || unicode.IsLetter(r2)) {
				return nil, false
			}
			j += n2
			for j < len(src) {
				c, n := utf8.DecodeRuneInString(src[j:])
				if !isPCMIdentCont(c) {
					break
				}
				j += n
			}
			out = append(out, pcmTok{kind: tokVar, val: src[i:j]})
			i = j
		case '"':
			s, n, ok := lexPCMString(src[i:])
			if !ok {
				return nil, false
			}
			out = append(out, pcmTok{kind: tokString, val: s})
			i += n
		default:
			if isPCMIdentStart(r) {
				j := i + size
				for j < len(src) {
					c, n := utf8.DecodeRuneInString(src[j:])
					if !isPCMIdentCont(c) {
						break
					}
					j += n
				}
				out = append(out, pcmTok{kind: tokIdent, val: src[i:j]})
				i = j
				continue
			}
			if r >= '0' && r <= '9' {
				j := i + size
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
				out = append(out, pcmTok{kind: tokNumber, val: src[i:j]})
				i = j
				continue
			}
			return nil, false
		}
	}
	return out, true
}

func isPCMIdentStart(r rune) bool {
	return r == '_' || r == '@' || unicode.IsLetter(r)
}

func isPCMIdentCont(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || (r >= '0' && r <= '9') || r == '-'
}

func lexPCMString(src string) (string, int, bool) {
	if src == "" || src[0] != '"' {
		return "", 0, false
	}
	var b strings.Builder
	for i := 1; i < len(src); i++ {
		switch src[i] {
		case '"':
			return b.String(), i + 1, true
		case '\\':
			if i+1 >= len(src) {
				return "", 0, false
			}
			i++
			switch src[i] {
			case '"', '\\', '\'':
				b.WriteByte(src[i])
			default:
				return "", 0, false
			}
		case '\n', '\r':
			return "", 0, false
		default:
			b.WriteByte(src[i])
		}
	}
	return "", 0, false
}
