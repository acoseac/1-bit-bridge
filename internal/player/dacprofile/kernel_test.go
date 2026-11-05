package dacprofile

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// kernelQuirks names a copy of the Linux kernel's sound/usb/quirks.c at the
// tag internal/alsapcm/uapi.go pins (uapiKernel), which
// scripts/dac-profiles/kernel-check.sh fetches and passes. The file is never
// part of the bridge: it is read here, and nothing of it but its USB IDs and
// DSD formats, facts, is printed.
var kernelQuirks = flag.String("kernel-quirks", "", "sound/usb/quirks.c at the pinned kernel tag, for TestTheKernelSeedIsThePinnedTags")

// kernelFacts is what a sound/usb/quirks.c says of native DSD over USB: the
// facts the database's kernel-seeded profiles must match.
type kernelFacts struct {
	// devices are the device-specific facts, by vendor and product ID: the
	// DSD formats the driver gives the unit and how it finds the altsetting.
	devices map[[2]uint16]*kernelDevice
	// vendors are the vendor-wide rules, by vendor ID: they name no model
	// and seed nothing.
	vendors map[uint16][]string
	// itfFormat and rawFormat are the formats of the two flag rules: a unit
	// flagged ITF_USB_DSD_DAC gets itfFormat on its last altsetting, one
	// flagged DSD_RAW rawFormat on each altsetting its descriptor marks raw.
	itfFormat, rawFormat string
}

// kernelDevice is one ID's facts.
type kernelDevice struct {
	formats []string
	how     []string
}

func (f *kernelFacts) add(id [2]uint16, format, how string) {
	d := f.devices[id]
	if d == nil {
		d = &kernelDevice{}
		f.devices[id] = d
	}
	if !slices.Contains(d.formats, format) {
		d.formats = append(d.formats, format)
		sort.Strings(d.formats)
	}
	if !slices.Contains(d.how, how) {
		d.how = append(d.how, how)
	}
}

// The ways the driver finds a DSD altsetting, as the seed's provenance names
// them.
const (
	howITF = "the last altsetting, after the vendor mode switch"
	howRaw = "each altsetting whose descriptor marks raw data"
)

func howAltsetting(n int) string { return "altsetting " + strconv.Itoa(n) }

func howBCD(n int, bcd uint16) string {
	return fmt.Sprintf("altsetting %d at bcdDevice %04x", n, bcd)
}

var (
	reFormatFunc = regexp.MustCompile(`\bu64\s+snd_usb_interface_dsd_format_quirks\s*\(`)
	reIDSwitch   = regexp.MustCompile(`switch\s*\(\s*chip->usb_id\s*\)\s*\{`)
	reVendorRule = regexp.MustCompile(`USB_ID_VENDOR\s*\(\s*chip->usb_id\s*\)\s*==\s*0x([0-9a-fA-F]{4})`)
	reCaseID     = regexp.MustCompile(`case\s+USB_ID\s*\(\s*0x([0-9a-fA-F]{4})\s*,\s*0x([0-9a-fA-F]{4})\s*\)\s*:`)
	reDefault    = regexp.MustCompile(`default\s*:`)
	reAltReturn  = regexp.MustCompile(`if\s*\(\s*fp->altsetting\s*==\s*(\d+)\s*\)\s*return\s+SNDRV_PCM_FMTBIT_(\w+)\s*;`)
	reAltOpen    = regexp.MustCompile(`if\s*\(\s*fp->altsetting\s*==\s*(\d+)\s*\)\s*\{`)
	reBCDSwitch  = regexp.MustCompile(`switch\s*\(\s*le16_to_cpu\s*\(\s*chip->dev->descriptor\.bcdDevice\s*\)\s*\)\s*\{`)
	reBCDCase    = regexp.MustCompile(`case\s+0x([0-9a-fA-F]+)\s*:`)
	reReturnFmt  = regexp.MustCompile(`return\s+SNDRV_PCM_FMTBIT_(\w+)\s*;`)
	reReturn     = regexp.MustCompile(`\breturn\b`)
	reITFRule    = regexp.MustCompile(`if\s*\(\s*chip->quirk_flags\s*&\s*QUIRK_FLAG_ITF_USB_DSD_DAC\s*\)\s*\{`)
	reLastAlt    = regexp.MustCompile(`fp->altsetting\s*==\s*iface->num_altsetting\s*-\s*1`)
	reRawRule    = regexp.MustCompile(`if\s*\(\s*\(\s*chip->quirk_flags\s*&\s*QUIRK_FLAG_DSD_RAW\s*\)\s*&&\s*fp->dsd_raw\s*\)\s*return\s+SNDRV_PCM_FMTBIT_(\w+)\s*;`)
	reFlagsTable = regexp.MustCompile(`quirk_flags_table\s*\[\s*\]\s*=\s*\{`)
	reDeviceFlg  = regexp.MustCompile(`DEVICE_FLG\s*\(\s*0x([0-9a-fA-F]{4})\s*,\s*0x([0-9a-fA-F]{4})\s*,([^)]*)\)`)
	reVendorFlg  = regexp.MustCompile(`VENDOR_FLG\s*\(\s*0x([0-9a-fA-F]{4})\s*,([^)]*)\)`)
	reFlag       = regexp.MustCompile(`^QUIRK_FLAG_\w+$`)
)

// readKernelQuirks reads src, a sound/usb/quirks.c: its DSD format quirk
// (snd_usb_interface_dsd_format_quirks: the vendor rules, the switch on the
// unit's ID and its arms, the ITF_USB_DSD_DAC and DSD_RAW rules) and the
// flags table. Comments are blanked first, so a comment's text is never read
// as code, and a shape the reader does not know is an error, never a fact
// left out.
func readKernelQuirks(src string) (kernelFacts, error) {
	f := kernelFacts{devices: map[[2]uint16]*kernelDevice{}, vendors: map[uint16][]string{}}
	text := stripCComments(src)
	locs := reFormatFunc.FindAllStringIndex(text, -1)
	if len(locs) != 1 {
		return f, fmt.Errorf("the DSD format quirk is defined %d times", len(locs))
	}
	open := strings.IndexByte(text[locs[0][1]:], '{')
	if open < 0 {
		return f, errors.New("the DSD format quirk has no body")
	}
	body, err := braced(text, locs[0][1]+open)
	if err != nil {
		return f, err
	}
	for _, m := range reVendorRule.FindAllStringSubmatch(body, -1) {
		v := hex16(m[1])
		f.vendors[v] = append(f.vendors[v], "the DSD format quirk's vendor rule")
	}
	sw := reIDSwitch.FindStringIndex(body)
	if sw == nil {
		return f, errors.New("the DSD format quirk has no switch on the unit's ID")
	}
	arms, err := braced(body, sw[1]-1)
	if err != nil {
		return f, err
	}
	if err := readArms(&f, arms); err != nil {
		return f, err
	}
	rest := body[sw[1]-1+len(arms)+2:]
	itf := reITFRule.FindStringIndex(rest)
	if itf == nil {
		return f, errors.New("the DSD format quirk has no ITF_USB_DSD_DAC rule")
	}
	itfBody, err := braced(rest, itf[1]-1)
	if err != nil {
		return f, err
	}
	rets := reReturnFmt.FindAllStringSubmatch(itfBody, -1)
	if len(rets) != 1 || !reLastAlt.MatchString(itfBody) {
		return f, errors.New("the ITF_USB_DSD_DAC rule is not one format on the last altsetting")
	}
	f.itfFormat = rets[0][1]
	raw := reRawRule.FindAllStringSubmatch(rest, -1)
	if len(raw) != 1 {
		return f, fmt.Errorf("the DSD format quirk has %d DSD_RAW rules", len(raw))
	}
	f.rawFormat = raw[0][1]

	tbl := reFlagsTable.FindStringIndex(text)
	if tbl == nil {
		return f, errors.New("there is no quirk_flags_table")
	}
	table, err := braced(text, tbl[1]-1)
	if err != nil {
		return f, err
	}
	devices, vendors := reDeviceFlg.FindAllStringSubmatch(table, -1), reVendorFlg.FindAllStringSubmatch(table, -1)
	if len(devices) == 0 || len(vendors) == 0 {
		return f, fmt.Errorf("the flags table holds %d device and %d vendor entries", len(devices), len(vendors))
	}
	for _, m := range devices {
		flags, err := quirkFlags(m[3])
		if err != nil {
			return f, fmt.Errorf("DEVICE_FLG(%s, %s): %w", m[1], m[2], err)
		}
		id := [2]uint16{hex16(m[1]), hex16(m[2])}
		if slices.Contains(flags, "QUIRK_FLAG_ITF_USB_DSD_DAC") {
			f.add(id, f.itfFormat, howITF)
		}
		if slices.Contains(flags, "QUIRK_FLAG_DSD_RAW") {
			f.add(id, f.rawFormat, howRaw)
		}
	}
	for _, m := range vendors {
		flags, err := quirkFlags(m[2])
		if err != nil {
			return f, fmt.Errorf("VENDOR_FLG(%s): %w", m[1], err)
		}
		for _, flag := range []string{"QUIRK_FLAG_ITF_USB_DSD_DAC", "QUIRK_FLAG_DSD_RAW"} {
			if slices.Contains(flags, flag) {
				v := hex16(m[1])
				f.vendors[v] = append(f.vendors[v], "the flags table's vendor entry, "+strings.TrimPrefix(flag, "QUIRK_FLAG_"))
			}
		}
	}
	return f, nil
}

// readArms reads the switch on the unit's ID: each run of case labels and
// the statements after it, to the next label at the switch's own depth.
func readArms(f *kernelFacts, arms string) error {
	depth := depths(arms)
	type label struct {
		start, end int
		id         *[2]uint16
	}
	var labels []label
	for _, m := range reCaseID.FindAllStringSubmatchIndex(arms, -1) {
		if depth[m[0]] == 0 {
			id := [2]uint16{hex16(arms[m[2]:m[3]]), hex16(arms[m[4]:m[5]])}
			labels = append(labels, label{m[0], m[1], &id})
		}
	}
	for _, m := range reDefault.FindAllStringIndex(arms, -1) {
		if depth[m[0]] == 0 {
			labels = append(labels, label{m[0], m[1], nil})
		}
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].start < labels[j].start })
	if len(labels) < 2 {
		return fmt.Errorf("the switch on the unit's ID has %d labels", len(labels))
	}
	var group [][2]uint16
	for i, l := range labels {
		if l.id == nil {
			group = nil
			continue
		}
		group = append(group, *l.id)
		next := len(arms)
		if i+1 < len(labels) {
			next = labels[i+1].start
		}
		stmts := arms[l.end:next]
		if strings.TrimSpace(stmts) == "" {
			continue // the next label shares these statements
		}
		facts, err := readArm(stmts)
		if err != nil {
			return fmt.Errorf("the arm of %04x:%04x: %w", group[0][0], group[0][1], err)
		}
		for _, id := range group {
			for _, fact := range facts {
				f.add(id, fact[0], fact[1])
			}
		}
		group = nil
	}
	return nil
}

// readArm reads one arm's statements: a return of a format on an altsetting,
// or on an altsetting by the unit's bcdDevice; each fact is a format and how.
func readArm(stmts string) ([][2]string, error) {
	var facts [][2]string
	if reBCDSwitch.MatchString(stmts) {
		alt := reAltOpen.FindStringSubmatch(stmts)
		if alt == nil {
			return nil, errors.New("a bcdDevice switch with no altsetting")
		}
		n, _ := strconv.Atoi(alt[1])
		inner := stmts[reBCDSwitch.FindStringIndex(stmts)[1]:]
		type tok struct {
			at          int
			bcd, format string
		}
		var toks []tok
		for _, m := range reBCDCase.FindAllStringSubmatchIndex(inner, -1) {
			toks = append(toks, tok{at: m[0], bcd: inner[m[2]:m[3]]})
		}
		for _, m := range reReturnFmt.FindAllStringSubmatchIndex(inner, -1) {
			toks = append(toks, tok{at: m[0], format: inner[m[2]:m[3]]})
		}
		sort.Slice(toks, func(i, j int) bool { return toks[i].at < toks[j].at })
		var pending []uint16
		for _, t := range toks {
			if t.format == "" {
				v, err := strconv.ParseUint(t.bcd, 16, 16)
				if err != nil {
					return nil, err
				}
				pending = append(pending, uint16(v))
				continue
			}
			if len(pending) == 0 {
				return nil, errors.New("a return under no bcdDevice")
			}
			for _, bcd := range pending {
				facts = append(facts, [2]string{t.format, howBCD(n, bcd)})
			}
			pending = nil
		}
	} else {
		alts := reAltReturn.FindAllStringSubmatchIndex(stmts, -1)
		for _, m := range alts {
			n, _ := strconv.Atoi(stmts[m[2]:m[3]])
			facts = append(facts, [2]string{stmts[m[4]:m[5]], howAltsetting(n)})
		}
		// Every format the arm returns is one of those facts: one returned
		// outside an altsetting test, or under a test the reader does not
		// read, beside one it reads keeps the counts below equal and would be
		// left out (CodeRabbit on #1402).
		for _, r := range reReturnFmt.FindAllStringIndex(stmts, -1) {
			if !slices.ContainsFunc(alts, func(m []int) bool { return m[0] <= r[0] && r[1] <= m[1] }) {
				return nil, errors.New("a format returned outside an altsetting test")
			}
		}
	}
	if len(facts) == 0 || len(reReturn.FindAllStringIndex(stmts, -1)) != len(reReturnFmt.FindAllStringIndex(stmts, -1)) {
		return nil, errors.New("statements the reader does not read")
	}
	return facts, nil
}

// quirkFlags splits a flags expression, A | B | C.
func quirkFlags(expr string) ([]string, error) {
	var out []string
	for _, f := range strings.Split(expr, "|") {
		f = strings.TrimSpace(f)
		if !reFlag.MatchString(f) {
			return nil, errors.New("a flag the reader does not read")
		}
		out = append(out, f)
	}
	return out, nil
}

// braced is what lies between text[open], a '{', and the brace that closes
// it.
func braced(text string, open int) (string, error) {
	if open < 0 || open >= len(text) || text[open] != '{' {
		return "", errors.New("no opening brace")
	}
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[open+1 : i], nil
			}
		}
	}
	return "", errors.New("a brace that never closes")
}

// depths is the brace depth at each byte of text.
func depths(text string) []int {
	d := make([]int, len(text)+1)
	n := 0
	for i := 0; i < len(text); i++ {
		d[i] = n
		switch text[i] {
		case '{':
			n++
		case '}':
			n--
		}
	}
	d[len(text)] = n
	return d
}

// stripCComments blanks every comment of src, block and line, keeping string
// and character literals whole and every line break, so a comment's text is
// never read as code.
func stripCComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				end = len(src) - i - 2
			}
			b.WriteByte(' ')
			b.WriteString(strings.Repeat("\n", strings.Count(src[i:i+2+end], "\n")))
			i += 2 + end + 2
		case strings.HasPrefix(src[i:], "//"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			i += end
		case src[i] == '"' || src[i] == '\'':
			j := i + 1
			for j < len(src) && src[j] != src[i] && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(src))
			b.WriteString(src[i:j])
			i = j
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

func hex16(s string) uint16 {
	v, _ := strconv.ParseUint(s, 16, 16)
	return uint16(v)
}

// kernelRef is the source ref of a claim from sound/usb/quirks.c at tag.
func kernelRef(tag string) string { return "sound/usb/quirks.c at " + tag }

// pinnedKernelTag is the kernel tag internal/alsapcm/uapi.go pins
// (uapiKernel), the one scripts/alsa-abi checks the PCM interface against.
func pinnedKernelTag(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "alsapcm", "uapi.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^const uapiKernel = "(v\d+\.\d+)"$`).FindSubmatch([]byte(strings.ReplaceAll(string(b), "\r\n", "\n")))
	if m == nil {
		t.Fatal("no const uapiKernel line in internal/alsapcm/uapi.go")
	}
	return string(m[1])
}

// compareKernelSeed is every way db's kernel-seeded profiles differ from
// what facts, read at tag, say: a claim of an ID the tag gives no
// device-specific quirk, other formats than the tag's, a source or a
// provenance line naming another tag or not saying how the driver finds the
// altsetting; and a device-specific ID of the tag neither seeded nor a shared
// reference ID, which seeds nothing without its product string.
func compareKernelSeed(db *Database, facts kernelFacts, tag string) []string {
	var problems []string
	claimed := map[[2]uint16]bool{}
	for _, p := range db.Profiles {
		if p.Native == nil || p.Native.Source.Kind != SourceKernelQuirk {
			continue
		}
		if p.Native.Source.Ref != kernelRef(tag) {
			problems = append(problems, fmt.Sprintf("%s: the source is %q, not %q", p.ID, p.Native.Source.Ref, kernelRef(tag)))
		}
		var native string
		for _, l := range p.Provenance {
			if strings.HasPrefix(l, "native: ") {
				native = l
			}
		}
		for _, m := range p.Match {
			id := [2]uint16{hex16(m.VendorID), hex16(m.ProductID)}
			claimed[id] = true
			d := facts.devices[id]
			if d == nil {
				problems = append(problems, fmt.Sprintf("%s: %s gives %s:%s no device-specific DSD quirk", p.ID, tag, m.VendorID, m.ProductID))
				continue
			}
			if got := slices.Sorted(slices.Values(p.Native.Formats)); !slices.Equal(got, d.formats) {
				problems = append(problems, fmt.Sprintf("%s: formats %v, the tag's %v", p.ID, got, d.formats))
			}
			for _, how := range d.how {
				if !strings.Contains(native, how) || !strings.Contains(native, tag) {
					problems = append(problems, fmt.Sprintf("%s: its native line %q does not say %q at %s", p.ID, native, how, tag))
				}
			}
		}
	}
	shared := map[[2]uint16]bool{}
	for _, s := range db.SharedReferenceIDs {
		shared[[2]uint16{hex16(s.VendorID), hex16(s.ProductID)}] = true
	}
	for _, id := range sortedIDs(facts.devices) {
		if !claimed[id] && !shared[id] {
			problems = append(problems, fmt.Sprintf("%04x:%04x: the tag's device-specific DSD quirk is neither seeded nor a shared reference ID", id[0], id[1]))
		}
	}
	return problems
}

func sortedIDs(m map[[2]uint16]*kernelDevice) [][2]uint16 {
	ids := make([][2]uint16, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i][0] < ids[j][0] || ids[i][0] == ids[j][0] && ids[i][1] < ids[j][1] })
	return ids
}

// TestTheKernelSeedIsThePinnedTags: with the pinned tag's sound/usb/quirks.c
// (-kernel-quirks), every kernel-seeded profile claims an ID the tag gives a
// device-specific DSD quirk, with the tag's formats, its source and its native
// line naming the tag and how the driver finds the altsetting; and every
// device-specific ID of the tag is seeded or listed as a shared reference ID.
// A vendor-wide rule seeds nothing. The facts it read are logged: IDs, formats
// and rules, never the file's text.
func TestTheKernelSeedIsThePinnedTags(t *testing.T) {
	if *kernelQuirks == "" {
		t.Skip("no -kernel-quirks file: scripts/dac-profiles/kernel-check.sh fetches sound/usb/quirks.c at the pinned tag and passes it")
	}
	src, err := os.ReadFile(*kernelQuirks)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := readKernelQuirks(string(src))
	if err != nil {
		t.Fatalf("the file does not read: %v", err)
	}
	tag := pinnedKernelTag(t)
	t.Logf("%s: %d device-specific DSD facts, %d vendor-wide rules; ITF_USB_DSD_DAC gives %s, DSD_RAW %s",
		tag, len(facts.devices), len(facts.vendors), facts.itfFormat, facts.rawFormat)
	for _, id := range sortedIDs(facts.devices) {
		d := facts.devices[id]
		t.Logf("  device %04x:%04x: %s on %s", id[0], id[1], strings.Join(d.formats, ", "), strings.Join(d.how, "; "))
	}
	vendors := make([]uint16, 0, len(facts.vendors))
	for v := range facts.vendors {
		vendors = append(vendors, v)
	}
	slices.Sort(vendors)
	for _, v := range vendors {
		t.Logf("  vendor %04x: %s", v, strings.Join(facts.vendors[v], "; "))
	}
	for _, p := range compareKernelSeed(mustDefault(t), facts, tag) {
		t.Error(p)
	}
}

// TestTheKernelSeedNamesTheTagAlsaABIPins: the database holds kernel-seeded
// profiles, and each names the tag internal/alsapcm/uapi.go pins, in its
// source and in its identity and native lines, with no rate (a quirk states
// none), no product string, interface or firmware list in its match (the
// kernel keys a quirk by the IDs alone), and no DoP claim (the kernel's
// quirks are native's).
func TestTheKernelSeedNamesTheTagAlsaABIPins(t *testing.T) {
	tag := pinnedKernelTag(t)
	n := 0
	for _, p := range mustDefault(t).Profiles {
		if p.Native == nil || p.Native.Source.Kind != SourceKernelQuirk {
			continue
		}
		n++
		if p.Native.Source.Ref != kernelRef(tag) || p.Native.MaxBitRate != 0 || p.DoP != nil {
			t.Errorf("%s: source %q, rate %d, DoP %v", p.ID, p.Native.Source.Ref, p.Native.MaxBitRate, p.DoP)
		}
		for _, m := range p.Match {
			if m.Product != nil || m.Interface != nil || m.BCDDevice != nil {
				t.Errorf("%s: an alternative narrows past the kernel's IDs: %+v", p.ID, m)
			}
		}
		for _, claim := range []string{"identity: ", "native: "} {
			if !slices.ContainsFunc(p.Provenance, func(l string) bool { return strings.HasPrefix(l, claim) && strings.Contains(l, tag) }) {
				t.Errorf("%s: no %sline naming %s", p.ID, claim, tag)
			}
		}
	}
	if n == 0 {
		t.Error("the database holds no kernel-seeded profile")
	}
}

// syntheticQuirks is a file of made-up IDs, written for this test, in the
// shape readKernelQuirks reads: a vendor rule, the switch's arms (two IDs on
// one altsetting, another altsetting, a format by bcdDevice), the two flag
// rules, and a flags table with device and vendor entries. The IDs in its
// comments must never be read.
const syntheticQuirks = `
/* A comment naming case USB_ID(0xdead, 0xbeef): and DEVICE_FLG(0xdead, 0xbeef, QUIRK_FLAG_DSD_RAW), */
static const char *s = "a string with /* no comment */ in it";
u64 snd_usb_interface_dsd_format_quirks(struct snd_usb_audio *chip, struct audioformat *fp, unsigned int sample_bytes)
{
	struct usb_interface *iface;
	if (USB_ID_VENDOR(chip->usb_id) == 0xf0f0 && USB_ID_PRODUCT(chip->usb_id) < 0x0100) {
		switch (fp->altsetting) { case 1: return SNDRV_PCM_FMTBIT_DSD_U16_LE; }
	}
	switch (chip->usb_id) {
	case USB_ID(0xf001, 0x0001): // USB_ID(0xdead, 0xbeef) in a line comment
	case USB_ID(0xf001, 0x0002):
		if (fp->altsetting == 2) return SNDRV_PCM_FMTBIT_DSD_U32_BE;
		break;
	case USB_ID(0xf002, 0x0001): if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE; break;
	case USB_ID(0xf003, 0x0001):
		if (fp->altsetting == 2) {
			switch (le16_to_cpu(chip->dev->descriptor.bcdDevice)) {
			case 0x101: return SNDRV_PCM_FMTBIT_DSD_U32_LE;
			case 0x102: case 0x103: return SNDRV_PCM_FMTBIT_DSD_U32_BE;
			default: break;
			}
		}
		break;
	default: break;
	}
	if (chip->quirk_flags & QUIRK_FLAG_ITF_USB_DSD_DAC) {
		iface = usb_ifnum_to_if(chip->dev, fp->iface);
		if (fp->altsetting == iface->num_altsetting - 1) return SNDRV_PCM_FMTBIT_DSD_U32_BE;
	}
	if ((chip->quirk_flags & QUIRK_FLAG_DSD_RAW) && fp->dsd_raw) return SNDRV_PCM_FMTBIT_DSD_U32_BE;
	return 0;
}
static const struct usb_audio_quirk_flags_table quirk_flags_table[] = {
	DEVICE_FLG(0xf004, 0x0001, /* a comment between */ QUIRK_FLAG_ITF_USB_DSD_DAC |
		QUIRK_FLAG_CTL_MSG_DELAY),
	DEVICE_FLG(0xf005, 0x0001, QUIRK_FLAG_DSD_RAW),
	DEVICE_FLG(0xf006, 0x0001, QUIRK_FLAG_GET_SAMPLE_RATE),
	VENDOR_FLG(0xf007, QUIRK_FLAG_DSD_RAW),
	VENDOR_FLG(0xf008, QUIRK_FLAG_GET_SAMPLE_RATE),
	{}
};
`

// TestTheKernelQuirkReaderReadsEachShape: on the synthetic file, the reader
// finds each arm's facts, each flag rule's devices and every vendor-wide
// rule, and nothing in a comment; and it refuses a file in a shape it does
// not know rather than leave a fact out.
func TestTheKernelQuirkReaderReadsEachShape(t *testing.T) {
	f, err := readKernelQuirks(syntheticQuirks)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, id := range sortedIDs(f.devices) {
		got[fmt.Sprintf("%04x:%04x", id[0], id[1])] = strings.Join(f.devices[id].formats, ",") + " on " + strings.Join(f.devices[id].how, "; ")
	}
	want := map[string]string{
		"f001:0001": "DSD_U32_BE on altsetting 2",
		"f001:0002": "DSD_U32_BE on altsetting 2",
		"f002:0001": "DSD_U32_BE on altsetting 3",
		"f003:0001": "DSD_U32_BE,DSD_U32_LE on altsetting 2 at bcdDevice 0101; altsetting 2 at bcdDevice 0102; altsetting 2 at bcdDevice 0103",
		"f004:0001": "DSD_U32_BE on " + howITF,
		"f005:0001": "DSD_U32_BE on " + howRaw,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("devices:\n got %v\nwant %v", got, want)
	}
	if _, ok := f.vendors[0xf0f0]; !ok || len(f.vendors) != 2 || len(f.vendors[0xf007]) != 1 {
		t.Errorf("vendors: %v", f.vendors)
	}
	if f.itfFormat != "DSD_U32_BE" || f.rawFormat != "DSD_U32_BE" {
		t.Errorf("the flag rules give %s and %s", f.itfFormat, f.rawFormat)
	}

	for _, tc := range []struct{ name, old, new string }{
		{"no format quirk", "u64 snd_usb_interface_dsd_format_quirks(", "u64 another_function("},
		{"no switch on the ID", "switch (chip->usb_id) {", "switch (chip->other) {"},
		{"an arm of another shape", "if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE;", "fp->dsd_dop = true;"},
		{"an arm returning another way", "if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE;", "return quirk(fp);"},
		// A format returned outside any altsetting test, or under a test the
		// reader does not read, beside one it reads: the counts of returns and
		// of format returns still agree, and the second format is no fact.
		{"a format returned on every altsetting beside one returned on one", "if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE; break;",
			"if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE; return SNDRV_PCM_FMTBIT_DSD_U16_LE;"},
		{"a format returned under a test the reader does not read", "if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE; break;",
			"if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE; if (fp->altsetting == 4 || sample_bytes == 4) return SNDRV_PCM_FMTBIT_DSD_U16_LE; break;"},
		// In an arm with a bcdDevice switch, a format returned outside the
		// switch (before it, on another altsetting), or a second altsetting
		// with a switch of its own, whose cases would be read as the first
		// altsetting's (CodeRabbit on #1402's round 5).
		{"a format returned before the bcdDevice switch", "case USB_ID(0xf003, 0x0001):\n\t\tif (fp->altsetting == 2) {",
			"case USB_ID(0xf003, 0x0001):\n\t\tif (fp->altsetting == 1) return SNDRV_PCM_FMTBIT_DSD_U16_LE;\n\t\tif (fp->altsetting == 2) {"},
		{"a second altsetting with a bcdDevice switch of its own", "\t\t\tdefault: break;\n\t\t\t}\n\t\t}\n\t\tbreak;",
			"\t\t\tdefault: break;\n\t\t\t}\n\t\t}\n\t\tif (fp->altsetting == 3) {\n\t\t\tswitch (le16_to_cpu(chip->dev->descriptor.bcdDevice)) {\n" +
				"\t\t\tcase 0x104: return SNDRV_PCM_FMTBIT_DSD_U16_LE;\n\t\t\tdefault: break;\n\t\t\t}\n\t\t}\n\t\tbreak;"},
		// The same class in either shape: a statement the reader does not read
		// beside the ones it reads, which changes what they mean.
		{"a statement in the bcdDevice switch the reader does not read", "case 0x101: return SNDRV_PCM_FMTBIT_DSD_U32_LE;",
			"case 0x101: fp->dsd_bitrev = true; return SNDRV_PCM_FMTBIT_DSD_U32_LE;"},
		{"a format the bcdDevice switch returns by default", "\t\t\tdefault: break;\n\t\t\t}",
			"\t\t\tcase 0x104: default: return SNDRV_PCM_FMTBIT_DSD_U16_LE;\n\t\t\t}"},
		{"a statement beside an altsetting test", "if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE; break;",
			"fp->dsd_bitrev = true; if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE; break;"},
		{"an altsetting test under a test the reader does not read", "if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE; break;",
			"if (sample_bytes == 4) if (fp->altsetting == 3) return SNDRV_PCM_FMTBIT_DSD_U32_BE; break;"},
		{"no ITF rule", "QUIRK_FLAG_ITF_USB_DSD_DAC) {", "QUIRK_FLAG_OTHER) {"},
		{"no DSD_RAW rule", "&& fp->dsd_raw)", "&& fp->other)"},
		{"no flags table", "quirk_flags_table[] = {", "other_table[] = {"},
		{"a flag the reader does not read", "QUIRK_FLAG_GET_SAMPLE_RATE),\n\tVENDOR_FLG(0xf007", "1 << 3),\n\tVENDOR_FLG(0xf007"},
	} {
		src := strings.Replace(syntheticQuirks, tc.old, tc.new, 1)
		if src == syntheticQuirks {
			t.Fatalf("%s: the synthetic file has no %q", tc.name, tc.old)
		}
		if _, err := readKernelQuirks(src); err == nil {
			t.Errorf("%s: the reader takes the file", tc.name)
		}
	}
}
