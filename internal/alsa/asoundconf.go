package alsa

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxPCMHops is how many slave.pcm follows a name may take. One more
// leaves the device unresolved.
const maxPCMHops = 8

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
// is unresolved. A ctl or defaults stanza is skipped. One file that
// cannot be read, or that this parser does not fully understand, makes
// the whole configuration unresolved. A dmix, a softvol, any other
// plugin, a hook, an include, a cycle, more than maxPCMHops follows, or
// syntax this parser does not understand leaves the device unresolved.
func ResolveOutputDevice(root, device string, cards []Card) (cardIndex, pcmDevice int, ok bool) {
	if idx, pcm, found := ResolveALSADevice(device, cards); found {
		return idx, pcm, true
	}
	device = strings.TrimSpace(device)
	if device == "" || strings.Contains(device, ":") {
		return 0, 0, false
	}
	defs, ok := loadPCMDefs(root)
	if !ok {
		return 0, 0, false
	}
	return walkPCM(defs, device, cards, map[string]bool{}, 0)
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
}

func walkPCM(defs map[string]pcmDef, name string, cards []Card, seen map[string]bool, hops int) (int, int, bool) {
	if strings.Contains(name, ":") {
		return ResolveALSADevice(name, cards)
	}
	if hops > maxPCMHops || seen[name] {
		return 0, 0, false
	}
	def, ok := defs[name]
	if !ok || def.opaque {
		return 0, 0, false
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
				return 0, 0, false
			}
			return def.cardIndex, pcm, true
		}
		if def.card == "" {
			return 0, 0, false
		}
		idx, found := uniqueCardID(cards, def.card)
		if !found {
			return 0, 0, false
		}
		return idx, pcm, true
	case "empty", "plug":
		if def.slave == "" {
			return 0, 0, false
		}
		return walkPCM(defs, def.slave, cards, seen, hops+1)
	default:
		return 0, 0, false
	}
}

// loadPCMDefs reads conf.d and then asound.conf. ok is false when any
// file that is present cannot be read, or parses as something this
// reader does not fully model. A missing file is not that: the other
// files still count. A false ok means no definition may be used.
func loadPCMDefs(root string) (map[string]pcmDef, bool) {
	defs := map[string]pcmDef{}
	var files int
	var total int
	bad := false
	add := func(path string) {
		if bad {
			return
		}
		b, err := readCapped(path, maxFileBytes)
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			bad = true
			return
		}
		if files >= maxAsoundFiles || total+len(b) > maxAsoundBytes {
			bad = true
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
				prev.opaque = true
				defs[name] = prev
				continue
			}
			defs[name] = def
		}
	}
	matches, _ := filepath.Glob(filepath.Join(root, "etc", "alsa", "conf.d", "*.conf"))
	sort.Strings(matches)
	for _, path := range matches {
		add(path)
	}
	add(filepath.Join(root, "etc", "asound.conf"))
	if bad {
		return nil, false
	}
	return defs, true
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
		if p.peek().kind == tokIdent && p.peek().val == "pcm" {
			if !p.isPCM() {
				return nil, false
			}
			name, def, ok := p.parsePCMDef()
			if !ok {
				return nil, false
			}
			if prev, exists := defs[name]; exists {
				prev.opaque = true
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

func (p *pcmParser) skipStanza() bool {
	if p.next().kind != tokIdent {
		return false
	}
	for p.peek().kind == tokDot {
		p.next()
		if p.peek().kind == tokBang {
			p.next()
		}
		n := p.next()
		if n.kind != tokIdent && n.kind != tokString && n.kind != tokNumber {
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
		if p.peek().kind == tokRBrace {
			break
		}
		if p.peek().kind == tokEOF {
			return pcmDef{}, false
		}
		key, ok := p.parseKey()
		if !ok {
			return pcmDef{}, false
		}
		if p.peek().kind == tokLBrace {
			inner, ok := p.parseSlaveBlock()
			if !ok {
				return pcmDef{}, false
			}
			if key != "slave" || def.slave != "" || inner.unknown || inner.pcm == "" {
				def.opaque = true
			} else {
				def.slave = inner.pcm
			}
			continue
		}
		val, ok := p.parseValue()
		if !ok {
			return pcmDef{}, false
		}
		switch key {
		case "type":
			if sawType {
				def.opaque = true
			}
			sawType = true
			def.typ = val
		case "slave.pcm":
			if def.slave != "" {
				def.opaque = true
			} else {
				def.slave = val
			}
		case "card":
			if sawCard {
				def.opaque = true
			}
			sawCard = true
			if n, err := strconv.Atoi(val); err == nil && cardNumber(val) {
				def.cardIsIndex = true
				def.cardIndex = n
				def.card = ""
			} else {
				def.cardIsIndex = false
				def.card = val
			}
		case "device":
			if sawDevice {
				def.opaque = true
			}
			sawDevice = true
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
	}
	p.next()
	if def.typ != "hw" && def.typ != "empty" && def.typ != "plug" {
		def.opaque = true
	}
	return def, true
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
			break
		}
		if p.peek().kind == tokEOF {
			return slaveBlock{}, false
		}
		key, ok := p.parseKey()
		if !ok {
			return slaveBlock{}, false
		}
		if p.peek().kind == tokLBrace {
			if _, ok := p.parseSlaveBlock(); !ok {
				return slaveBlock{}, false
			}
			b.unknown = true
			continue
		}
		val, ok := p.parseValue()
		if !ok {
			return slaveBlock{}, false
		}
		if key == "pcm" && b.pcm == "" {
			b.pcm = val
		} else {
			b.unknown = true
		}
	}
	p.next()
	return b, true
}

func (p *pcmParser) parseKey() (string, bool) {
	t := p.next()
	if t.kind != tokIdent {
		return "", false
	}
	key := t.val
	if p.peek().kind == tokDot {
		p.next()
		rest := p.next()
		if rest.kind != tokIdent {
			return "", false
		}
		key += "." + rest.val
	}
	return key, true
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
	return r == '_' || unicode.IsLetter(r)
}

func isPCMIdentCont(r rune) bool {
	return isPCMIdentStart(r) || (r >= '0' && r <= '9') || r == '-'
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
