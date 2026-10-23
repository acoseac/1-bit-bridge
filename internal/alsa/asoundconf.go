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
// files define none. alsa-lib's plug as a device string (plug:SLAVE) is a
// plug stage over its slave, which is judged as a slave is. The walk does
// not go through a stage that changes samples, so what lies past it is not
// read, and any other device string names none. It is
// what the files say, not what alsa-lib loaded for the process that opened
// the card: information, not proof.
func (c PCMConfig) SampleStage(device string) string {
	device = strings.TrimSpace(device)
	switch {
	case device == "":
		return ""
	case strings.Contains(device, ":"):
		return deviceStringStage(c, device, map[string]bool{}, 0)
	case !c.ok:
		return ""
	}
	return sampleStage(c, device, map[string]bool{}, 0)
}

func sampleStage(c PCMConfig, name string, seen map[string]bool, hops int) string {
	if strings.Contains(name, ":") {
		return deviceStringStage(c, name, seen, hops)
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
// into a format and rate of its own; for alsa-lib's plug (plug:SLAVE), its
// slave, judged as a slave is: a device string here, and a name walked in
// the files, which needs them read whole; none for any other plugin.
func deviceStringStage(c PCMConfig, device string, seen map[string]bool, hops int) string {
	plugin, arg, _ := strings.Cut(device, ":")
	switch plugin {
	case "dmix":
		return "dmix"
	case "plug":
		slave, ok := plugSlave(arg)
		switch {
		case !ok || hops > maxPCMHops:
			return ""
		case strings.Contains(slave, ":"):
			return deviceStringStage(c, slave, seen, hops+1)
		case !c.ok:
			return ""
		}
		return sampleStage(c, slave, seen, hops+1)
	}
	return ""
}

// confBlank is what alsa-lib's configuration code skips between tokens
// (skip_blank and get_nonwhite in conf.c).
const confBlank = " \f\t\n\r"

// plugSlave is the slave a plug: device string names, read as alsa-lib
// reads the arguments of a PCM that takes them (parse_args in conf.c). plug
// takes one, SLAVE (pcm.plug's @args in alsa.conf), given in a list
// (plugList) or a compound (plugCompound). ok is false for what alsa-lib
// would not open as a plug, and for a form this does not model, which
// names nothing rather than a wrong name; so is a NUL, where alsa-lib's
// string would end.
func plugSlave(arg string) (string, bool) {
	arg = strings.TrimLeft(arg, confBlank)
	switch {
	case strings.IndexByte(arg, 0) >= 0:
		return "", false
	case strings.HasPrefix(arg, "{"):
		return plugCompound(arg)
	}
	return plugList(arg)
}

// plugList reads plug's arguments as a list, as parse_args does: arguments
// one after another, a comma between them, each a value by position (plug's
// first position is SLAVE) or SLAVE= and a value (parseArg), the last SLAVE
// standing. Another position or another name is a parameter plug does not
// take, and nothing but a comma or the end may follow a value, a blank
// included.
func plugList(s string) (string, bool) {
	var slave string
	for pos := 0; ; pos++ {
		name, value, rest, ok := parseArg(s)
		if !ok || (name == "" && pos > 0) || (name != "" && name != "SLAVE") {
			return "", false
		}
		slave = value
		switch {
		case rest == "":
			return slave, slave != ""
		case rest[0] != ',':
			return "", false
		}
		s = rest[1:]
	}
}

// parseArg reads one argument as parse_arg does: blanks skipped, then a
// quoted value (confQuoted), or an id that runs to a tab, a newline, a
// carriage return, a form feed, a comma, an = or the end (a space does not
// end it), which names the argument when an = follows it past blanks and is
// its value otherwise. The value after an = is read the same way, past
// blanks. rest is what follows the value.
func parseArg(s string) (name, value, rest string, ok bool) {
	s = strings.TrimLeft(s, confBlank)
	switch {
	case s == "":
		return "", "", "", false
	case s[0] == '"' || s[0] == '\'':
		value, rest, ok = confQuoted(s)
		return "", value, rest, ok
	}
	id, after := argID(s)
	after = strings.TrimLeft(after, confBlank)
	if !strings.HasPrefix(after, "=") {
		return "", id, after, true
	}
	s = strings.TrimLeft(after[1:], confBlank)
	switch {
	case id == "" || s == "":
		// alsa-lib reads an = with no name before it as a value by
		// position, which this does not model; nothing after it is no value.
		return "", "", "", false
	case s[0] == '"' || s[0] == '\'':
		value, rest, ok = confQuoted(s)
		return id, value, rest, ok
	}
	value, rest = argID(s)
	return id, value, rest, true
}

// argID is parse_id's id: s up to a tab, a newline, a carriage return, a
// form feed, a comma or an =, and what follows it.
func argID(s string) (id, rest string) {
	if i := strings.IndexAny(s, "\f\t\n\r,="); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// confQuoted reads a string quoted in ' or ", as alsa-lib's parse_string
// and get_delimstring do for the escapes this models: \", \\ and \' are the
// character escaped. Any other escape (a control character, an octal byte,
// a letter alsa-lib reads as itself) and a string the end cuts short are
// not read. rest is what follows the closing quote.
func confQuoted(s string) (value, rest string, ok bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == s[0]:
			return b.String(), s[i+1:], true
		case c != '\\':
			b.WriteByte(c)
		case i+1 < len(s) && strings.IndexByte(`"\'`, s[i+1]) >= 0:
			i++
			b.WriteByte(s[i])
		default:
			return "", "", false
		}
	}
	return "", "", false
}

// plugCompound reads plug's arguments as a compound, { … }: parse_args
// hands what lies between the braces to alsa-lib's configuration parser in
// override mode, so it is definitions one after another, each SLAVE, then
// an = or none, then a value quoted or bare, then a ; or a , or none, with
// blanks and # comments between; the last SLAVE stands. A definition of
// another name is a parameter plug does not take, which alsa-lib refuses,
// and so is a compound with no value. What this does not model names
// nothing: a mode prefix (+ - ? !) or a dotted name, a compound or array
// value, an include (<…>), an escape confQuoted does not read, and a bare
// value alsa-lib may read as a number.
func plugCompound(s string) (string, bool) {
	s = strings.TrimRight(s, confBlank)
	if len(s) < 2 || s[len(s)-1] != '}' {
		return "", false
	}
	in := confInput{s: s[1 : len(s)-1]}
	var slave string
	for {
		c, ok := in.nonwhite()
		switch {
		case !ok:
			return "", false
		case c == 0:
			return slave, slave != ""
		}
		if name, _, ok := in.token(true); !ok || name != "SLAVE" {
			return "", false
		}
		if c, ok = in.nonwhite(); ok && c == '=' {
			in.pos++
			c, ok = in.nonwhite()
		}
		if !ok || c == '.' {
			return "", false
		}
		value, quoted, ok := in.token(false)
		if !ok || value == "" || (!quoted && strings.IndexByte("-0123456789", value[0]) >= 0) {
			return "", false
		}
		slave = value
		if c, ok = in.nonwhite(); !ok {
			return "", false
		} else if c == ';' || c == ',' {
			in.pos++
		}
	}
}

// confInput reads a string as alsa-lib's configuration parser reads its
// input (get_nonwhite, get_string, get_freestring and get_delimstring in
// conf.c), as far as plugCompound needs.
type confInput struct {
	s   string
	pos int
}

// nonwhite skips blanks and # comments, each to the end of its line, and
// returns the next byte, 0 at the end. ok is false at an include (<…>),
// which this does not read.
func (in *confInput) nonwhite() (byte, bool) {
	for in.pos < len(in.s) {
		switch c := in.s[in.pos]; c {
		case ' ', '\f', '\t', '\n', '\r':
			in.pos++
		case '#':
			if i := strings.IndexByte(in.s[in.pos:], '\n'); i >= 0 {
				in.pos += i + 1
			} else {
				in.pos = len(in.s)
			}
		case '<':
			return 0, false
		default:
			return c, true
		}
	}
	return 0, true
}

// token reads a string at the next byte, quoted (confQuoted) or bare: a
// bare string runs to a blank, = , ; { } [ ] ' " \ or #, and a name to a
// dot too. As in alsa-lib v1.2.14 conf.c get_string, an initial dot is
// refused for both names and values, before get_freestring/parse_value.
// ok is also false at the other non-string starts or an unsupported escape.
func (in *confInput) token(name bool) (s string, quoted, ok bool) {
	rest := in.s[in.pos:]
	if rest == "" || strings.IndexByte("=,;.{}[]\\", rest[0]) >= 0 {
		return "", false, false
	}
	if rest[0] == '"' || rest[0] == '\'' {
		value, after, ok := confQuoted(rest)
		in.pos = len(in.s) - len(after)
		return value, true, ok
	}
	end := " \f\t\n\r=,;{}[]'\"\\#"
	if name {
		end += "."
	}
	i := strings.IndexAny(rest, end)
	if i < 0 {
		i = len(rest)
	}
	in.pos += i
	return rest[:i], false, true
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
	// that is there and cannot be listed is a configuration not read. It
	// is stat'ed first, so that one that is there and is not a directory is
	// that on every platform: Windows's os.ReadDir of a regular file
	// answered an empty listing and no error (the windows-latest run on
	// 1ab90068; os/dir_windows.go ends a listing at a no-more-files or a
	// file-not-found answer without one).
	confd := filepath.Join(root, "etc", "alsa", "conf.d")
	switch fi, err := os.Stat(confd); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil || !fi.IsDir():
		bad, unread = true, true
	default:
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
	tokReal
)

type pcmTok struct {
	kind int
	val  string
	// opaque is a string form the resolver did not model before value
	// lexing was widened. Consuming it must not grant a new chain.
	opaque bool
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
	if plain && key == "type" {
		if *sawType {
			def.opaque, def.typeUnsure = true, true
		}
		*sawType = true
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
	value, plainVal, ok := p.readFieldValue()
	if !ok {
		return false
	}
	val := value.val
	if !plain || !plainVal || ((key == "type" || key == "slave.pcm") && value.kind != tokString) {
		def.opaque = true
		if key == "type" {
			def.typeUnsure = true
		}
		return true
	}
	switch key {
	case "type":
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
		if value.kind == tokNumber && !cardNumber(val) {
			def.opaque = true
			return true
		}
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

func (p *pcmParser) readFieldValue() (pcmTok, bool, bool) {
	t := p.next()
	switch t.kind {
	case tokString, tokNumber:
		return t, !t.opaque, true
	case tokReal, tokVar:
		return t, false, true
	}
	return pcmTok{}, false, false
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
		if plain && plainVal && val.kind == tokString && key == "pcm" && b.pcm == "" {
			b.pcm = val.val
		} else {
			b.unknown = true
		}
	}
}

func (p *pcmParser) parseValue() (string, bool) {
	t := p.next()
	if t.kind == tokString || t.kind == tokNumber || t.kind == tokReal {
		return t.val, true
	}
	return "", false
}

// lexPCM distinguishes keys from values: dots split a key, including
// after blanks/comments; inside a value they are kept, but cannot start it.
// Basis: alsa-lib v1.2.14 src/conf.c get_string, get_freestring, parse_def and
// parse_value (https://github.com/alsa-project/alsa-lib/blob/v1.2.14/src/conf.c).
// parse_def accepts an optional '=' before a scalar, compound or array.
// parse_value tries a digit/'-' word as an integer, then a real; failed
// conversions fall through to STRING, as do other admitted free words.
// Strings and comments are byte-oriented, not restricted to UTF-8. NUL,
// includes, mode syntax and escapes outside our existing subset remain
// refused. Reading a scalar does not evaluate a plugin or extend routing.
func lexPCM(src string) ([]pcmTok, bool) {
	if strings.IndexByte(src, 0) >= 0 {
		return nil, false
	}
	in := confInput{s: src}
	var out []pcmTok
	// A stack avoids recursion on arbitrary nesting. Braces contain
	// definitions; brackets contain values without keys.
	stack := []int{tokLBrace}
	for {
		c, ok := in.nonwhite()
		if !ok {
			return nil, false
		}
		if c == 0 {
			return out, len(stack) == 1
		}
		if c == ';' || c == ',' {
			kind := tokSemi
			if c == ',' {
				kind = tokComma
			}
			out = append(out, pcmTok{kind: kind})
			in.pos++
			continue
		}
		if c == '}' || c == ']' {
			open, close := tokLBrace, tokRBrace
			if c == ']' {
				open, close = tokLBrack, tokRBrack
			}
			if len(stack) == 1 || stack[len(stack)-1] != open {
				return nil, false
			}
			stack = stack[:len(stack)-1]
			out = append(out, pcmTok{kind: close})
			in.pos++
			continue
		}
		if stack[len(stack)-1] == tokLBrace {
			start := in.pos
			for {
				if _, _, ok := in.token(true); !ok {
					return nil, false
				}
				// parse_def tests get_nonwhite's result: blanks and
				// comments before a dot still continue this key.
				c, ok = in.nonwhite()
				if !ok {
					return nil, false
				}
				if c != '.' {
					break
				}
				in.pos++
				if _, ok := in.nonwhite(); !ok {
					return nil, false
				}
			}
			key, ok := lexPCMKey(src[start:in.pos])
			if !ok {
				return nil, false
			}
			out = append(out, key...)
			if c == '=' {
				in.pos++
				c, ok = in.nonwhite()
				if !ok {
					return nil, false
				}
			}
		}
		// get_char_skip_comments expands includes even at a value or
		// array element start. Never consume one as an opaque string.
		if c == '<' {
			return nil, false
		}
		switch c {
		case '{', '[':
			kind := tokLBrace
			if c == '[' {
				kind = tokLBrack
			}
			out = append(out, pcmTok{kind: kind})
			stack = append(stack, kind)
			in.pos++
		default:
			value, ok := lexPCMValue(&in)
			if !ok {
				return nil, false
			}
			out = append(out, value)
		}
	}
}

// lexPCMValue keeps ALSA's scalar kinds separate. Numeric conversions
// must consume the whole word (safe_strtoll/safe_strtod in alsa-lib);
// failure leaves an opaque string, never a refusal of the file.
func lexPCMValue(in *confInput) (pcmTok, bool) {
	if in.pos >= len(in.s) || in.s[in.pos] == '<' {
		return pcmTok{}, false
	}
	if in.s[in.pos] == '"' || in.s[in.pos] == '\'' {
		s, n, ok := lexPCMString(in.s[in.pos:])
		in.pos += n
		return pcmTok{kind: tokString, val: s}, ok
	}
	s, _, ok := in.token(false)
	if !ok || s == "" {
		return pcmTok{}, false
	}
	if strings.IndexByte("-0123456789", s[0]) >= 0 {
		// strtod accepts these spellings without ERANGE. They are real
		// values, never card numbers or PCM names (nor evaluated gains).
		switch strings.ToLower(s) {
		case "-inf", "-infinity", "-nan":
			return pcmTok{kind: tokReal, val: s}, true
		}
		// Go accepts underscores and 0b/0o integer prefixes that the
		// C conversions used here do not. Keep them as opaque strings.
		unsigned := strings.TrimPrefix(s, "-")
		if strings.ContainsRune(s, '_') || strings.HasPrefix(unsigned, "0b") || strings.HasPrefix(unsigned, "0B") || strings.HasPrefix(unsigned, "0o") || strings.HasPrefix(unsigned, "0O") {
			return pcmTok{kind: tokString, val: s, opaque: true}, true
		}
		if n, err := strconv.ParseInt(s, 0, 64); err == nil {
			return pcmTok{kind: tokNumber, val: strconv.FormatInt(n, 10)}, true
		}
		// C strtod accepts hex reals even without a binary exponent.
		real := s
		if (strings.HasPrefix(unsigned, "0x") || strings.HasPrefix(unsigned, "0X")) && !strings.ContainsAny(s, "pP") {
			real += "p0"
		}
		n, err := strconv.ParseFloat(real, 64)
		if err != nil || (n == 0 && nonzeroPCMMantissa(s)) {
			return pcmTok{kind: tokString, val: s, opaque: true}, true
		}
		return pcmTok{kind: tokReal, val: s}, true
	}
	// Preserve the resolver's former subset of bare strings. Paths,
	// dotted words and device arguments can be consumed, but cannot
	// turn an otherwise opaque definition into a resolved chain.
	plain := true
	for i, r := range s {
		if (i == 0 && !isPCMIdentStart(r)) || (i != 0 && !isPCMIdentCont(r)) {
			plain = false
		}
	}
	return pcmTok{kind: tokString, val: s, opaque: !plain}, true
}

// nonzeroPCMMantissa distinguishes underflow to zero from a written zero;
// ParseFloat does not report the former as ERANGE as strtod does.
func nonzeroPCMMantissa(s string) bool {
	s = strings.TrimPrefix(s, "-")
	end := "eE"
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		s, end = s[2:], "pP"
	}
	if i := strings.IndexAny(s, end); i >= 0 {
		s = s[:i]
	}
	return strings.ContainsAny(s, "123456789abcdefABCDEF")
}

// lexPCMKey retains the deliberately restricted identifier grammar.
func lexPCMKey(src string) ([]pcmTok, bool) {
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
	if src == "" || (src[0] != '"' && src[0] != '\'') {
		return "", 0, false
	}
	var b strings.Builder
	for i := 1; i < len(src); i++ {
		if src[i] == src[0] {
			return b.String(), i + 1, true
		}
		switch src[i] {
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
