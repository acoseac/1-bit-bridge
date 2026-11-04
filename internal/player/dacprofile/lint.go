package dacprofile

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// dsdFormats are ALSA's DSD formats, as the kernel names them.
var dsdFormats = []string{"DSD_U8", "DSD_U16_LE", "DSD_U32_LE", "DSD_U16_BE", "DSD_U32_BE"}

// dsdRates are the one-bit rates of the 44.1 kHz family a claim may name:
// DSD64, DSD128, DSD256 and DSD512.
var dsdRates = []int{2822400, 5644800, 11289600, 22579200}

// controlInterfaces are ALSA's control interfaces, named as amixer prints
// them.
var controlInterfaces = []string{"CARD", "HWDEP", "MIXER", "PCM", "RAWMIDI", "TIMER", "SEQUENCER"}

// controlNameMax is the size of ALSA's control name field (snd_ctl_elem_id).
const controlNameMax = 44

// maxTimingMs bounds a timing a profile gives.
const maxTimingMs = 5000

// The measurement methods a raw-maximum entry's evidence may name.
const (
	methodDigitalCapture = "digital-capture"
	methodDoPHeard       = "dop-indicator-heard"
)

var (
	reProfileID = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)+$`)
	reDate      = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	reTag       = regexp.MustCompile(`\bv\d+\.\d+\b`)
	reIOSCommit = regexp.MustCompile(`\biOS [0-9a-f]{7,40}\b`)
	reKernelRef = regexp.MustCompile(`^sound/usb/quirks\.c at v\d+\.\d+$`)
	reIOSRef    = regexp.MustCompile(`^DACProfile\.swift [a-z0-9]+(?:[.-][a-z0-9]+)+ at iOS [0-9a-f]{7,40}$`)
	// reNeverSaid are the words never used of DSD on the bridge's player (the
	// design's section 1.3), with their inflections: no note claims a DAC
	// decodes DSD.
	reNeverSaid = regexp.MustCompile(`(?i)\b(verif(?:y|ies|ied)|decod(?:e|es|ed|ing)|bit-exact|bit-perfect|lock(?:s|ed|ing)?|seal(?:s|ed)?|qualif(?:y|ies|ied))\b`)
	// reHeard and reRefusal are what a clause of a dop provenance line says of
	// a heard rate and of a refused one, beside the rate's name: that it was
	// heard, a listening, never a negotiation or a DoP lock (section 1.1: the
	// iOS lock counts the packer's own frames and hears nothing back); and
	// that a device refused it or played it as silence.
	reHeard   = regexp.MustCompile(`(?i)\bheard\b`)
	reRefusal = regexp.MustCompile(`(?i)\b(?:refus\w*|silen\w*)\b`)
)

// dsdName is a rate of dsdRates as a claim names it: DSD and the rate's
// multiple of 44.1 kHz (DSD128).
func dsdName(rate int) string { return fmt.Sprintf("DSD%d", rate/44100) }

// linter collects a database's problems.
type linter struct{ problems []error }

func (l *linter) add(format string, args ...any) {
	l.problems = append(l.problems, fmt.Errorf(format, args...))
}

// Lint is every problem the database has, each naming its profile or its
// shared reference ID (the design's section 2.4): the schema; IDs in
// usbid.Hex16's form and product strings the sysfs reader returns; a shared
// reference ID matched without its product string; two profiles one unit
// could match; a claim, a shared ID, an analogue control or a raw-maximum
// entry without its provenance or evidence; a caveat off the closed list.
// The kernel-derived claims against the pinned tag, and the iOS registry's
// rates against their rows, are checked against those files
// (TestTheKernelSeedIsThePinnedTags, scripts/dac-profiles).
func (db *Database) Lint() []error {
	var l linter
	if db.SchemaVersion != SchemaVersion {
		l.add("the database: schema version %d, not %d", db.SchemaVersion, SchemaVersion)
	}
	shared := map[[2]uint16]bool{}
	for _, s := range db.SharedReferenceIDs {
		who := "shared reference ID " + s.VendorID + ":" + s.ProductID
		v, okV := hexID(s.VendorID)
		p, okP := hexID(s.ProductID)
		if !okV || !okP {
			l.add("%s: an ID is four lowercase hexadecimal digits", who)
			continue
		}
		if shared[[2]uint16{v, p}] {
			l.add("%s: listed twice", who)
		}
		shared[[2]uint16{v, p}] = true
		if strings.TrimSpace(s.Why) == "" {
			l.add("%s: says no why", who)
		} else {
			l.sourced(who+": its why", s.Why)
		}
	}
	ids := map[string]bool{}
	for i := range db.Profiles {
		p := &db.Profiles[i]
		who := "profile " + p.ID
		if p.ID == "" {
			who = fmt.Sprintf("profile #%d", i+1)
		}
		if !reProfileID.MatchString(p.ID) {
			l.add("%s: an ID is lowercase words joined by dots or hyphens (chord.hugo2)", who)
		}
		if ids[p.ID] {
			l.add("%s: listed twice", who)
		}
		ids[p.ID] = true
		l.profile(who, p, shared)
	}
	l.overlaps(db)
	return l.problems
}

// hexID reads an ID in usbid.Hex16's form, four lowercase hexadecimal
// digits, and no other.
func hexID(s string) (uint16, bool) {
	v, err := usbid.ParseHex16(s)
	return v, err == nil && usbid.Hex16(v) == s
}

func (l *linter) profile(who string, p *Profile, shared map[[2]uint16]bool) {
	if p.Name == "" || strings.TrimSpace(p.Name) != p.Name {
		l.add("%s: a name is set, with no white space around it", who)
	}
	if len(p.Match) == 0 {
		l.add("%s: no match alternative", who)
	}
	for j, m := range p.Match {
		mw := fmt.Sprintf("%s, match %d", who, j+1)
		l.match(mw, m, shared)
		for _, earlier := range p.Match[:j] {
			if sameMatch(earlier, m) {
				l.add("%s: given twice", mw)
			}
		}
	}
	if p.Native != nil {
		l.native(who, p.Native)
	}
	if p.DoP != nil {
		l.dop(who, p.DoP)
		l.dopEvidence(who, p)
	}
	if p.Timing != nil {
		l.timing(who, p.Timing)
	}
	l.caveats(who, p.Caveats)
	l.provenance(who, p)
}

func (l *linter) match(who string, m Match, shared map[[2]uint16]bool) {
	v, okV := hexID(m.VendorID)
	p, okP := hexID(m.ProductID)
	if !okV {
		l.add("%s: vendorId %q is not four lowercase hexadecimal digits", who, m.VendorID)
	}
	if !okP {
		l.add("%s: productId %q is not four lowercase hexadecimal digits", who, m.ProductID)
	}
	if m.Interface != nil && (*m.Interface < 0 || *m.Interface > 255) {
		l.add("%s: interface %d is not a USB interface number", who, *m.Interface)
	}
	if m.Product != nil {
		if err := checkProduct(*m.Product); err != nil {
			l.add("%s: product: %v", who, err)
		}
	} else if okV && okP && shared[[2]uint16{v, p}] {
		l.add("%s: %s:%s is a shared reference ID, matched only with its product string", who, m.VendorID, m.ProductID)
	}
	l.firmware(who, m.BCDDevice, false)
}

// firmware checks a bcdDevice list: nil is any firmware unless one is
// required, and an empty list, which would match none, is refused.
func (l *linter) firmware(who string, list []string, required bool) {
	if list == nil {
		if required {
			l.add("%s: no bcdDevice", who)
		}
		return
	}
	if len(list) == 0 {
		l.add("%s: bcdDevice lists a firmware or is null: an empty list matches none", who)
		return
	}
	seen := map[string]bool{}
	for _, b := range list {
		if _, ok := hexID(b); !ok {
			l.add("%s: bcdDevice %q is not four lowercase hexadecimal digits", who, b)
		}
		if seen[b] {
			l.add("%s: bcdDevice %s is listed twice", who, b)
		}
		seen[b] = true
	}
}

func sameMatch(a, b Match) bool {
	eqInt := a.Interface == nil && b.Interface == nil || a.Interface != nil && b.Interface != nil && *a.Interface == *b.Interface
	eqStr := a.Product == nil && b.Product == nil || a.Product != nil && b.Product != nil && *a.Product == *b.Product
	return a.VendorID == b.VendorID && a.ProductID == b.ProductID && eqInt && eqStr &&
		(a.BCDDevice == nil) == (b.BCDDevice == nil) && slices.Equal(a.BCDDevice, b.BCDDevice)
}

func (l *linter) native(who string, n *Native) {
	if len(n.Formats) == 0 {
		l.add("%s: the native claim names no format", who)
	}
	seen := map[string]bool{}
	for _, f := range n.Formats {
		if !slices.Contains(dsdFormats, f) {
			l.add("%s: native format %q is not one of ALSA's DSD formats", who, f)
		}
		if seen[f] {
			l.add("%s: native format %s is listed twice", who, f)
		}
		seen[f] = true
	}
	switch n.Source.Kind {
	case SourceKernelQuirk:
		if !reKernelRef.MatchString(n.Source.Ref) {
			l.add("%s: a kernel quirk's source is \"sound/usb/quirks.c at v<X.Y>\", the file at a kernel tag", who)
		}
		if n.MaxBitRate != 0 {
			l.add("%s: a kernel quirk states no rate, so its maxBitRate is 0", who)
		}
	case SourceCapture:
		if !hasDate(n.Source.Ref) {
			l.add("%s: a capture's source names its date", who)
		}
		if n.MaxBitRate == 0 {
			l.add("%s: a capture shows its rates, so its maxBitRate is set", who)
		}
	default:
		l.add("%s: the native claim's source is a kernel quirk or a capture, not %q", who, n.Source.Kind)
	}
	if n.MaxBitRate != 0 && !slices.Contains(dsdRates, n.MaxBitRate) {
		l.add("%s: native maxBitRate %d is not a DSD rate from DSD64 to DSD512", who, n.MaxBitRate)
	}
}

func (l *linter) dop(who string, d *DoP) {
	for _, r := range []struct {
		name string
		rate int
	}{{"heardMaxBitRate", d.HeardMaxBitRate}, {"statedMaxBitRate", d.StatedMaxBitRate}} {
		if r.rate != 0 && !slices.Contains(dsdRates, r.rate) {
			l.add("%s: DoP %s %d is not a DSD rate from DSD64 to DSD512", who, r.name, r.rate)
		}
	}
	seen := map[int]bool{}
	for _, r := range d.RefusedBitRates {
		if !slices.Contains(dsdRates, r) {
			l.add("%s: refused rate %d is not a DSD rate from DSD64 to DSD512", who, r)
		}
		if seen[r] {
			l.add("%s: refused rate %d is listed twice", who, r)
		}
		seen[r] = true
		if d.HeardMaxBitRate != 0 && r <= d.HeardMaxBitRate {
			l.add("%s: refused rate %d is at or below the rate heard, %d", who, r, d.HeardMaxBitRate)
		}
	}
	if d.HeardMaxBitRate == 0 && d.StatedMaxBitRate == 0 && len(d.RefusedBitRates) == 0 {
		l.add("%s: the DoP claim claims nothing: no rate heard, stated or refused", who)
	}
	if d.Source.Kind != SourceIOSRegistry || !reIOSRef.MatchString(d.Source.Ref) {
		l.add("%s: the DoP claim's source is the iOS registry, \"DACProfile.swift <row> at iOS <commit>\"", who)
	}
	analogue := map[ControlID]bool{}
	for _, c := range d.AnalogVolumeControls {
		cw := fmt.Sprintf("%s, analogue control %s", who, c.ControlID)
		l.control(cw, c.ControlID)
		if analogue[c.ControlID] {
			l.add("%s: listed twice", cw)
		}
		analogue[c.ControlID] = true
		if strings.TrimSpace(c.Provenance) == "" {
			l.add("%s: names no provenance", cw)
		} else {
			l.sourced(cw+": its provenance", c.Provenance)
		}
	}
	rawMax := map[ControlID]bool{}
	for _, c := range d.UnityAtRawMax {
		cw := fmt.Sprintf("%s, raw-maximum control %s", who, c.ControlID)
		l.control(cw, c.ControlID)
		if rawMax[c.ControlID] {
			l.add("%s: listed twice", cw)
		}
		rawMax[c.ControlID] = true
		if c.RawMax < 1 {
			l.add("%s: rawMax is 1 or more", cw)
		}
		l.firmware(cw, c.BCDDevice, true)
		l.evidence(cw, c)
	}
}

// dopEvidence asks a DoP claim's provenance for the evidence each of its
// rates is of the kind its field names: for the heard rate, a clause of a dop
// line (the text between two semicolons) that names the rate and says it was
// heard, and for each refused rate, one that names it and says a device
// refused it or played it as silence. The word and the rate share a clause,
// so a line that tells of one rate's refusal and another's lock is evidence
// of the refusal alone. A negotiation, a hardware confirmation or a DoP lock
// is evidence of the transport, which no field here holds (the review of
// #1402). A rate dop refuses as no DSD rate is not asked for again.
func (l *linter) dopEvidence(who string, p *Profile) {
	says := func(rate int, re *regexp.Regexp) bool {
		name := regexp.MustCompile(`\b` + dsdName(rate) + `\b`)
		for _, line := range p.Provenance {
			if !strings.HasPrefix(line, "dop: ") {
				continue
			}
			for _, clause := range strings.Split(line, ";") {
				if name.MatchString(clause) && re.MatchString(clause) {
					return true
				}
			}
		}
		return false
	}
	if h := p.DoP.HeardMaxBitRate; slices.Contains(dsdRates, h) && !says(h, reHeard) {
		l.add("%s: a heard DoP rate is a listening, and no clause of a dop provenance line names %s and says it was heard: a negotiation, a hardware confirmation or a DoP lock hears nothing (section 1.1)", who, dsdName(h))
	}
	for _, r := range p.DoP.RefusedBitRates {
		if slices.Contains(dsdRates, r) && !says(r, reRefusal) {
			l.add("%s: refused rate %s is one a device refused or played as silence, and no clause of a dop provenance line names it and says refused or silence", who, dsdName(r))
		}
	}
}

// String names a control as interface/name/index.
func (c ControlID) String() string { return fmt.Sprintf("%s/%s/%d", c.Interface, c.Name, c.Index) }

func (l *linter) control(who string, c ControlID) {
	if !slices.Contains(controlInterfaces, c.Interface) {
		l.add("%s: interface %q is not one of ALSA's control interfaces", who, c.Interface)
	}
	if c.Name == "" || len(c.Name) > controlNameMax || strings.ContainsRune(c.Name, 0) || !utf8.ValidString(c.Name) {
		l.add("%s: a control's name is 1 to %d bytes of UTF-8 with no NUL", who, controlNameMax)
	}
	if c.Index < 0 {
		l.add("%s: a control's index is 0 or more", who)
	}
}

// evidence checks a raw-maximum entry's evidence (the design's section 2.1):
// the maker's document by its title and section, or a measurement by its
// method, on a firmware the entry lists, with its dated record.
func (l *linter) evidence(who string, c RawMaxControl) {
	e := c.Evidence
	switch e.Kind {
	case "document":
		if strings.TrimSpace(e.Title) == "" {
			l.add("%s: a document's evidence names its title", who)
		}
		if strings.TrimSpace(e.Section) == "" {
			l.add("%s: a document's evidence names its section", who)
		}
		if e.Method != "" || e.BCDDevice != "" || e.Ref != "" {
			l.add("%s: a document's evidence holds a measurement's fields", who)
		}
	case "measured":
		if e.Method != methodDigitalCapture && e.Method != methodDoPHeard {
			l.add("%s: a measurement's method is %s or %s", who, methodDigitalCapture, methodDoPHeard)
		}
		switch {
		case e.BCDDevice == "":
			l.add("%s: a measurement names the bcdDevice it was taken on", who)
		case !slices.Contains(c.BCDDevice, e.BCDDevice):
			l.add("%s: the measurement's bcdDevice %s is not one the entry lists", who, e.BCDDevice)
		}
		if !hasDate(e.Ref) {
			l.add("%s: a measurement's record names its date", who)
		}
		if e.Title != "" || e.Section != "" {
			l.add("%s: a measurement holds a document's fields", who)
		}
	default:
		l.add("%s: the evidence is a document or a measurement, not %q", who, e.Kind)
	}
}

func (l *linter) timing(who string, t *Timing) {
	if t.DSDPrefillMs == 0 && t.PCMEntryMs == 0 {
		l.add("%s: the timing gives none", who)
	}
	for _, v := range []struct {
		name string
		ms   int
	}{{"dsdPrefillMs", t.DSDPrefillMs}, {"pcmEntryMs", t.PCMEntryMs}} {
		if v.ms < 0 || v.ms > maxTimingMs {
			l.add("%s: the timing's %s is 0 to %d ms", who, v.name, maxTimingMs)
		}
	}
}

func (l *linter) caveats(who string, cs []Caveat) {
	seen := map[[2]string]bool{}
	for _, c := range cs {
		cw := who + ", caveat " + c.Code
		if !slices.Contains(CaveatCodes(), c.Code) {
			l.add("%s: the code is not on the closed list", cw)
		}
		switch c.Transport {
		case "", "native", "dop":
		default:
			l.add("%s: transport %q is native, dop, or none for both", cw, c.Transport)
		}
		if c.Code == CaveatDoPNoiseInHiddenMode && c.Transport != "dop" {
			l.add("%s: the hidden mode's caveat is DoP's, transport dop", cw)
		}
		if strings.TrimSpace(c.Note) == "" {
			l.add("%s: carries no note", cw)
		}
		if w := reNeverSaid.FindString(c.Note); w != "" {
			l.add("%s: the note says %q, never said of DSD on the bridge's player", cw, strings.ToLower(w))
		}
		k := [2]string{c.Code, c.Transport}
		if seen[k] {
			l.add("%s: carried twice", cw)
		}
		seen[k] = true
	}
}

// claimsOf are the claims p makes, by the name a provenance line starts with.
func claimsOf(p *Profile) []string {
	claims := []string{"identity"}
	if p.Native != nil {
		claims = append(claims, "native")
	}
	if p.DoP != nil {
		claims = append(claims, "dop")
	}
	if p.Timing != nil {
		claims = append(claims, "timing")
	}
	for _, c := range p.Caveats {
		if !slices.Contains(claims, "caveat "+c.Code) {
			claims = append(claims, "caveat "+c.Code)
		}
	}
	return claims
}

// knownClaim reports whether name is a claim a profile can make.
func knownClaim(name string) bool {
	switch name {
	case "identity", "native", "dop", "timing":
		return true
	}
	code, ok := strings.CutPrefix(name, "caveat ")
	return ok && slices.Contains(CaveatCodes(), code)
}

// provenance checks that each claim p makes has a line, "<claim>: …", naming
// a dated or versioned source, and that no line names another claim.
func (l *linter) provenance(who string, p *Profile) {
	made := claimsOf(p)
	lined := map[string]bool{}
	for i, line := range p.Provenance {
		lw := fmt.Sprintf("%s, provenance line %d", who, i+1)
		claim, text, ok := strings.Cut(line, ": ")
		switch {
		case !ok || !knownClaim(claim):
			l.add("%s names no claim (identity, native, dop, timing or caveat <code>)", lw)
		case !slices.Contains(made, claim):
			l.add("%s is for a %s claim the profile does not make", lw, claim)
		default:
			lined[claim] = true
			l.sourced(lw, text)
		}
	}
	for _, c := range made {
		if !lined[c] {
			l.add("%s: the %q claim names no provenance", who, c)
		}
	}
}

// sourced checks that text names its source's date or version, a date
// (YYYY-MM-DD), a kernel tag (v6.12) or an iOS commit ("iOS cd125088"), and
// holds no email address.
func (l *linter) sourced(who, text string) {
	if strings.Contains(text, "@") {
		l.add("%s holds an email address", who)
	}
	if !hasDate(text) && !reTag.MatchString(text) && !reIOSCommit.MatchString(text) {
		l.add("%s names no date or version of its source", who)
	}
}

// hasDate reports whether text holds a date, YYYY-MM-DD, that is one.
func hasDate(text string) bool {
	for _, d := range reDate.FindAllString(text, -1) {
		if _, err := time.Parse(time.DateOnly, d); err == nil {
			return true
		}
	}
	return false
}

// overlaps refuses two profiles whose alternatives one unit could match.
func (l *linter) overlaps(db *Database) {
	for i := range db.Profiles {
		for j := i + 1; j < len(db.Profiles); j++ {
			p, q := &db.Profiles[i], &db.Profiles[j]
			if p.ID == q.ID {
				continue
			}
			if a, ok := overlapping(p.Match, q.Match); ok {
				l.add("profiles %s and %s both match a unit on %s:%s", p.ID, q.ID, a.VendorID, a.ProductID)
			}
		}
	}
}

// overlapping is the first of as that a unit matching one of bs could
// match, and whether there is one.
func overlapping(as, bs []Match) (Match, bool) {
	for _, a := range as {
		for _, b := range bs {
			if overlap(a, b) {
				return a, true
			}
		}
	}
	return Match{}, false
}

// overlap reports whether one unit could match both a and b: the same IDs,
// and for each field the two set, the same value (an unset field matches any,
// and two firmware lists overlap where they share one).
func overlap(a, b Match) bool {
	av, e1 := usbid.ParseHex16(a.VendorID)
	ap, e2 := usbid.ParseHex16(a.ProductID)
	bv, e3 := usbid.ParseHex16(b.VendorID)
	bp, e4 := usbid.ParseHex16(b.ProductID)
	if err := errors.Join(e1, e2, e3, e4); err != nil || av != bv || ap != bp {
		return false
	}
	if a.Interface != nil && b.Interface != nil && *a.Interface != *b.Interface {
		return false
	}
	if a.Product != nil && b.Product != nil && *a.Product != *b.Product {
		return false
	}
	if a.BCDDevice != nil && b.BCDDevice != nil {
		return slices.ContainsFunc(a.BCDDevice, func(x string) bool {
			return slices.ContainsFunc(b.BCDDevice, func(y string) bool { return sameHex(x, y) })
		})
	}
	return true
}

// sameHex reports whether two IDs ParseHex16 reads are one value.
func sameHex(x, y string) bool {
	a, e1 := usbid.ParseHex16(x)
	b, e2 := usbid.ParseHex16(y)
	return e1 == nil && e2 == nil && a == b
}
