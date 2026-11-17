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

// MaxTimingMs bounds a timing a profile gives, in milliseconds: the DSD
// sink takes a prefill of up to as much (alsapcm.MaxDSDPrefill), so a
// profile's never fails an open.
const MaxTimingMs = 5000

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
	// reIOSRef is a row of the iOS registry at a commit, the row its group.
	reIOSRef = regexp.MustCompile(`^DACProfile\.swift ([a-z0-9]+(?:[.-][a-z0-9]+)+) at iOS [0-9a-f]{7,40}$`)
	// reNeverSaid are the words never used of DSD on the bridge's player (the
	// design's section 1.3), with their inflections: no note claims a DAC
	// decodes DSD. It reads caveat notes and a family's sibling note, never
	// provenance, whose free text says what was observed, a DoP lock
	// included.
	reNeverSaid = regexp.MustCompile(`(?i)\b(verif(?:y|ies|ied)|decod(?:e|es|ed|ing)|bit-exact|bit-perfect|lock(?:s|ed|ing)?|seal(?:s|ed)?|qualif(?:y|ies|ied))\b`)
	// reMarker is a dop clause that starts with a marker, in any case, with a
	// word boundary after it: stated opens each of the three statements.
	reMarker = regexp.MustCompile(`^(?i:heard|refused|silence at|noise at|no frames at|stated)\b`)
	// reEvidence is a rate marker's clause in the form evidence is written
	// in: the marker, its rates separated by commas, then a colon and anything
	// else, which nothing reads but a head (reHead), or the clause's end.
	reEvidence = regexp.MustCompile(`^(?i:heard|refused|silence at|noise at|no frames at)\s+(DSD\d+(?:\s*,\s*DSD\d+)*)\s*(?::|$)`)
	// reStatement is a statement's clause in that form: stated DoP to or
	// stated DSD to and its one rate, or stated no DSD.
	reStatement = regexp.MustCompile(`^(?i:stated (dop|dsd) to)\s+(DSD\d+)\s*(?::|$)|^(?i:stated no dsd)\s*(?::|$)`)
	reRateName  = regexp.MustCompile(`DSD\d+`)
	// reHead is the head a clause's text after its colon opens with in a
	// profile with an iOS projection: its source's kind, iOS's (submission,
	// fieldLog, maker, pcmCeiling, chipSpecification, retailer) in words, and
	// a date where the source has one, then the text's end, ", " or " (".
	reHead = regexp.MustCompile(`^(submission|field log|maker|pcm ceiling|chip specification|retailer)(?: (\d{4}-\d{2}-\d{2}))?(?:$|, | \()`)
	// reCaseName is a Swift enum case's name, as an iOS volume topology is.
	reCaseName = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
)

// The markers a dop clause starts with to credit what it lists (the design's
// section 2.1; B730): heard; the failures, a carrier refused, silence, noise
// or no attempt's frames taken; and the statements.
const (
	markerHeard      = "heard"
	markerRefused    = "refused"
	markerSilence    = "silence at"
	markerNoise      = "noise at"
	markerNoFrames   = "no frames at"
	markerStatedDoP  = "stated DoP to"
	markerStatedDSD  = "stated DSD to"
	markerStatedNone = "stated no DSD"
)

// minTokenLen is the fewest characters an iOS name token holds (iOS PR 1).
const minTokenLen = 4

// dsdName is a rate of dsdRates as a claim names it: DSD and the rate's
// multiple of 44.1 kHz (DSD128).
func dsdName(rate int) string { return fmt.Sprintf("DSD%d", rate/44100) }

// dsdRateNamed is the rate of dsdRates name names, and whether it names one.
func dsdRateNamed(name string) (int, bool) {
	for _, r := range dsdRates {
		if dsdName(r) == name {
			return r, true
		}
	}
	return 0, false
}

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
// entry without its provenance or evidence; a caveat off the closed list;
// DoP evidence its clauses do not credit rate by rate; and an iOS projection
// iOS PR 1's checks refuse. The kernel-derived claims are checked against
// the pinned tag (TestTheKernelSeedIsThePinnedTags), and the iOS projection
// against the registry's baseline by its export (scripts/dac-profiles).
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
	var generic []string
	for _, g := range db.IOSGenericNames {
		generic = append(generic, g.Name)
		if who := fmt.Sprintf("iOS generic name %q", g.Name); strings.TrimSpace(g.Why) == "" {
			l.add("%s: says no why", who)
		} else {
			l.sourced(who+": its why", g.Why)
		}
	}
	l.names("iOS generic name", generic, true)
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
	l.nameKeys(db)
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
	if len(p.Match) == 0 && p.IOS == nil {
		l.add("%s: no match alternative", who)
	}
	if len(p.Match) == 0 && p.IOS != nil {
		for _, c := range []struct {
			name string
			set  bool
		}{
			{"native claim", p.Native != nil},
			{"timing", p.Timing != nil},
			{"analogue control", p.DoP != nil && len(p.DoP.AnalogVolumeControls) > 0},
			{"raw-maximum control", p.DoP != nil && len(p.DoP.UnityAtRawMax) > 0},
		} {
			if c.set {
				l.add("%s: a profile with no match alternative is iOS's alone and has no %s, which only a match reaches", who, c.name)
			}
		}
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
		l.dop(who, p)
		l.dopEvidence(who, p)
	}
	if p.Timing != nil {
		l.timing(who, p.Timing)
	}
	l.caveats(who, p.Caveats)
	if p.IOS != nil {
		l.ios(who, p)
	}
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

func (l *linter) dop(who string, p *Profile) {
	d := p.DoP
	if d.StatedMaxBitRate != 0 && !slices.Contains(dsdRates, d.StatedMaxBitRate) {
		l.add("%s: DoP statedMaxBitRate %d is not a DSD rate from DSD64 to DSD512", who, d.StatedMaxBitRate)
	}
	for i, r := range d.HeardBitRates {
		if !slices.Contains(dsdRates, r) {
			l.add("%s: heard rate %d is not a DSD rate from DSD64 to DSD512", who, r)
		} else if i > 0 && r <= d.HeardBitRates[i-1] {
			l.add("%s: heardBitRates lists %d after %d: ascending, each rate once", who, r, d.HeardBitRates[i-1])
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
	}
	if s := d.StatedWithoutDoP; s != nil {
		if !(s.Kind == StatementDSD && slices.Contains(dsdRates, s.MaxBitRate) || s.Kind == StatementNone && s.MaxBitRate == 0) {
			l.add("%s: statedWithoutDoP is {\"kind\": \"dsd\", \"maxBitRate\": <a DSD rate>} or {\"kind\": \"none\", \"maxBitRate\": 0}", who)
		}
		if d.StatedMaxBitRate != 0 {
			l.add("%s: the DoP claim holds a statement of DoP and one without it: at most one is set", who)
		}
		if s.Kind == StatementNone && len(d.HeardBitRates) > 0 {
			l.add("%s: a statement of no DSD beside a heard rate", who)
		}
	}
	if len(d.HeardBitRates) == 0 && d.StatedMaxBitRate == 0 && d.StatedWithoutDoP == nil && len(d.RefusedBitRates) == 0 {
		l.add("%s: the DoP claim claims nothing: no rate heard, stated or refused", who)
	}
	switch d.Source.Kind {
	case SourceIOSRegistry:
		if !ownRow(d.Source.Ref, p.ID) {
			l.add("%s: the DoP claim's source is the profile's own row of the iOS registry, \"DACProfile.swift %s at iOS <commit>\"", who, p.ID)
		}
	case SourceListening, SourceMaker:
		if !hasDate(d.Source.Ref) || strings.Contains(d.Source.Ref, "@") {
			l.add("%s: a DoP claim's %s source names its date, and no email address", who, d.Source.Kind)
		}
	default:
		l.add("%s: the DoP claim's source is the iOS registry, \"DACProfile.swift <row> at iOS <commit>\", a listening or a maker's document, not %q", who, d.Source.Kind)
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

// Credit is a marker clause of a profile's dop lines: its marker as written
// above (markerHeard), the rates it credits, and the kind and the date ("" for
// none) of its head (reHead), which ios-export reads as the entry's source.
type Credit struct {
	Marker     string
	BitRates   []int
	Kind, Date string
	clause     string
}

// Credits are the marker clauses of p's dop lines in evidence's form, in
// order, as Lint reads them.
func (p *Profile) Credits() []Credit { return p.credits(func(string, ...any) {}) }

// credits are p's marker clauses in the form Provenance gives, each clause
// of another shape and each rate that is none told to problem. What follows
// a clause's colon is read by nothing but its head, so it may say anything, a
// negation included; a clause that starts with no marker is evidence of
// nothing ("DSD128's carrier reached a DoP lock": a lock hears nothing).
func (p *Profile) credits(problem func(format string, args ...any)) []Credit {
	var cs []Credit
	for _, line := range p.Provenance {
		text, ok := strings.CutPrefix(line, "dop: ")
		if !ok {
			continue
		}
		for _, clause := range strings.Split(text, ";") {
			clause = strings.TrimSpace(clause)
			marker := strings.ToLower(reMarker.FindString(clause))
			if marker == "" {
				continue
			}
			c := Credit{Marker: marker, clause: clause}
			var f []string
			if marker == "stated" {
				if f = reStatement.FindStringSubmatch(clause); f == nil {
					problem("the dop clause %q is a statement not in the form evidence is written in: %q, %q or %q", clause, markerStatedDoP+" DSD256: <source>", markerStatedDSD+" DSD256: <source>", markerStatedNone+": <source>")
					continue
				}
				c.Marker = map[string]string{"dop": markerStatedDoP, "dsd": markerStatedDSD, "": markerStatedNone}[strings.ToLower(f[1])]
				f[1] = f[2]
			} else if f = reEvidence.FindStringSubmatch(clause); f == nil {
				problem("the dop clause %q starts with the marker %q and is not in the form evidence is written in, %q: the marker, its rates separated by commas, then a colon and anything else, or the clause's end", clause, marker, marker+" DSD64, DSD128: <source>")
				continue
			}
			for _, name := range reRateName.FindAllString(f[1], -1) {
				if rate, ok := dsdRateNamed(name); ok {
					c.BitRates = append(c.BitRates, rate)
				} else {
					problem("the dop clause %q lists %s, which is not a DSD rate from DSD64 to DSD512", clause, name)
				}
			}
			if strings.HasSuffix(f[0], ":") {
				if h := reHead.FindStringSubmatch(strings.TrimSpace(clause[len(f[0]):])); h != nil && (h[2] == "" || hasDate(h[2])) {
					c.Kind, c.Date = h[1], h[2]
				}
			}
			cs = append(cs, c)
		}
	}
	return cs
}

// dopEvidence holds a DoP claim to its clauses (credits), written in one form
// since a lint cannot read prose (round 3 read negation, and the review of
// #1402 broke the reading from both sides), rate by rate and both ways: the
// rates heard clauses credit are heardBitRates, failure clauses' are
// refusedBitRates, and the statement clauses credit the claim's statement. A
// rate heard and failed is a contradiction; the four failures are one result,
// which two units may give in two ways. In a profile with an iOS projection
// every marker clause has its head, and one clause credits each rate and the
// statement.
func (l *linter) dopEvidence(who string, p *Profile) {
	d := p.DoP
	cs := p.credits(func(format string, args ...any) { l.add("%s: "+format, append([]any{who}, args...)...) })
	heard, failed := map[int][]string{}, map[int][]string{}
	var stated []string
	for _, c := range cs {
		if c.Kind == "" && p.IOS != nil {
			l.add("%s: the dop clause %q, in a profile with an iOS projection, opens its text after a colon with its source's head: %s, then a date where it has one (%q)", who, c.clause, "submission, field log, maker, pcm ceiling, chip specification or retailer", "heard DSD128: submission 2026-10-04 (…)")
		}
		switch {
		case c.Marker == markerHeard:
			for _, r := range c.BitRates {
				heard[r] = append(heard[r], c.Marker)
			}
		case strings.HasPrefix(c.Marker, "stated"):
			s := c.Marker
			for _, r := range c.BitRates {
				s += " " + dsdName(r)
			}
			stated = append(stated, s)
		default:
			for _, r := range c.BitRates {
				failed[r] = append(failed[r], c.Marker)
			}
		}
	}
	for _, r := range dsdRates {
		if len(heard[r]) > 0 && len(failed[r]) > 0 {
			l.add("%s: %s is both heard and refused in its dop clauses (%q): heard is one result, and refused, silence at, noise at and no frames at, which two units may give in two ways, the other", who, dsdName(r), append(heard[r], failed[r]...))
		}
		if listed := slices.Contains(d.HeardBitRates, r); listed && len(heard[r]) == 0 {
			l.add("%s: a heard DoP rate is a listening, and no dop clause credits %s as heard: write %q in the dop line (a clause that starts with no marker is evidence of nothing, and a negotiation, a hardware confirmation or a DoP lock hears nothing, section 1.1)", who, dsdName(r), markerHeard+" "+dsdName(r)+": <source>")
		} else if !listed && len(heard[r]) > 0 {
			l.add("%s: a dop clause credits %s as heard, and heardBitRates does not list it", who, dsdName(r))
		}
		if listed := slices.Contains(d.RefusedBitRates, r); listed && len(failed[r]) == 0 {
			l.add("%s: refused rate %s has no dop clause that credits it: write %q, or %q, %q or %q for a rate a device played as silence or noise or took no frames of", who, dsdName(r), markerRefused+" "+dsdName(r)+": <source>", markerSilence+" "+dsdName(r), markerNoise+" "+dsdName(r), markerNoFrames+" "+dsdName(r))
		} else if !listed && len(failed[r]) > 0 {
			l.add("%s: a dop clause credits %s as failed (%q), and refusedBitRates does not list it", who, dsdName(r), failed[r])
		}
		if n := len(heard[r]) + len(failed[r]); p.IOS != nil && n > 1 {
			l.add("%s: %s is credited by %d dop clauses, and in a profile with an iOS projection one clause credits each rate", who, dsdName(r), n)
		}
	}
	want := statement(d)
	for _, s := range stated {
		if s != want {
			l.add("%s: a dop clause credits %q, and the DoP claim's statement is %q", who, s, want)
		}
	}
	if want != "" && len(stated) == 0 {
		l.add("%s: the DoP claim's statement has no dop clause that credits it: write %q", who, want+": <source>")
	}
	if p.IOS != nil && len(stated) > 1 {
		l.add("%s: the statement is credited by %d dop clauses, and in a profile with an iOS projection one clause credits it", who, len(stated))
	}
}

// statement is the clause that credits d's statement, its marker and its
// rate, or "" for none.
func statement(d *DoP) string {
	switch s := d.StatedWithoutDoP; {
	case d.StatedMaxBitRate != 0:
		return markerStatedDoP + " " + dsdName(d.StatedMaxBitRate)
	case s != nil && s.Kind == StatementNone:
		return markerStatedNone
	case s != nil:
		return markerStatedDSD + " " + dsdName(s.MaxBitRate)
	}
	return ""
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
		if v.ms < 0 || v.ms > MaxTimingMs {
			l.add("%s: the timing's %s is 0 to %d ms", who, v.name, MaxTimingMs)
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

// ios checks a profile's iOS projection on its own; nameKeys checks it
// beside the others'.
func (l *linter) ios(who string, p *Profile) {
	x := p.IOS
	if x.Maker == "" || strings.TrimSpace(x.Maker) != x.Maker {
		l.add("%s: the iOS maker is set, with no white space around it", who)
	}
	for _, s := range x.LegacyPatterns {
		l.printable(who+": a legacy pattern", s)
	}
	if c := x.LegacyCeilingBitRate; c != 0 && !slices.Contains(dsdRates, c) {
		l.add("%s: the legacy ceiling %d is 0 or a DSD rate from DSD64 to DSD512", who, c)
	}
	l.names(who+": the port name", x.PortNames, false)
	l.names(who+": the name token", x.NameTokens, true)
	l.names(who+": the excluded token", x.ExcludedTokens, true)
	for _, t := range x.NameTokens {
		if len(t) < minTokenLen {
			l.add("%s: the name token %q is shorter than %d characters", who, t, minTokenLen)
		}
		for _, e := range x.ExcludedTokens {
			if strings.Contains(fold(t), fold(e)) {
				l.add("%s: the excluded token %q lies inside the row's own token %q, whose names it would rule out", who, e, t)
			}
		}
	}
	switch x.NameScope {
	case "model":
		if x.SiblingNote != nil {
			l.add("%s: a model row carries no sibling note", who)
		}
	case "family":
		if x.SiblingNote == nil || strings.TrimSpace(*x.SiblingNote) == "" {
			l.add("%s: a family row's sibling note says what its siblings showed", who)
		} else if w := reNeverSaid.FindString(*x.SiblingNote); w != "" {
			l.add("%s: the sibling note says %q, never said of DSD on the bridge's player", who, strings.ToLower(w))
		}
		// A family row has no heard or failed rate of its own: its siblings' results live in the note alone, as
		// iOS PR 1's evidence table keeps ifi.generic's empty (its DoPEvidenceTests). Without this check, only the
		// export's parity with the pinned baseline would refuse one.
		if p.DoP != nil && (len(p.DoP.HeardBitRates) > 0 || len(p.DoP.RefusedBitRates) > 0) {
			l.add("%s: a family row has no heard or failed rate of its own: its siblings' results go in the sibling note", who)
		}
	default:
		l.add("%s: nameScope %q is model or family", who, x.NameScope)
	}
	if x.PCM.MaxRateHz <= 0 || !slices.Contains([]int{16, 24, 32}, x.PCM.BitDepth) {
		l.add("%s: the iOS PCM ceiling is a rate and a depth of 16, 24 or 32 bits", who)
	}
	if !reCaseName.MatchString(x.VolumeTopology) {
		l.add("%s: the volume topology %q is an iOS case name (dacOwnsGain)", who, x.VolumeTopology)
	}
	t := x.Timing
	for _, v := range []*int{t.IOBufferMs, t.DSDResyncMs, t.PCMEntryResyncMs, t.ColdStartExtraMs, t.TransitionCushionMs} {
		if v != nil && (*v < 0 || *v > MaxTimingMs) {
			l.add("%s: an iOS time is null or 0 to %d ms, not %d", who, MaxTimingMs, *v)
		}
	}
	l.legacyCeiling(who, p)
}

// legacyCeiling holds an iOS row's legacy ceiling to its DoP evidence as iOS
// PR 1 does: 0 exactly for a statement of no DSD, every heard rate at or
// below it, and no failed rate, where iOS sends DoP.
func (l *linter) legacyCeiling(who string, p *Profile) {
	c, d := p.IOS.LegacyCeilingBitRate, DoP{}
	if p.DoP != nil {
		d = *p.DoP
	}
	if none := d.StatedWithoutDoP != nil && d.StatedWithoutDoP.Kind == StatementNone; none != (c == 0) {
		l.add("%s: the legacy ceiling is 0 exactly where the maker states no DSD", who)
	}
	for _, r := range d.HeardBitRates {
		if r > c {
			l.add("%s: heard %s lies above the legacy ceiling", who, dsdName(r))
		}
	}
	for _, r := range d.RefusedBitRates {
		if r <= c {
			l.add("%s: failed %s lies at or below the legacy ceiling, where iOS sends DoP", who, dsdName(r))
		}
	}
}

// names checks a list of iOS names: each printable ASCII and not blank,
// trimmed where trim, and none twice, compared trimmed and folded.
func (l *linter) names(who string, list []string, trim bool) {
	seen := map[string]bool{}
	for _, s := range list {
		l.printable(who, s)
		k := fold(strings.TrimSpace(s))
		switch {
		case k == "":
			l.add("%s %q is blank", who, s)
		case trim && strings.TrimSpace(s) != s:
			l.add("%s %q has white space around it", who, s)
		case seen[k]:
			l.add("%s %q is listed twice", who, s)
		}
		seen[k] = true
	}
}

// printable refuses an iOS name that is not printable ASCII, on which Swift's
// lowercased() and fold agree.
func (l *linter) printable(who, s string) {
	if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
		l.add("%s %q is not printable ASCII", who, s)
	}
}

// fold is s with A to Z in lower case and nothing else changed.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		return r
	}, s)
}

// nameKeys holds the iOS rows to iOS PR 1's lint, names folded: orders 1 to
// N, each once; no port name twice or a generic name; no token and generic
// name one inside the other; and across two profiles only, no token and
// another's token or port name one inside the other (a profile's own token
// inside its own port name is how it matches).
func (l *linter) nameKeys(db *Database) {
	var rows []*Profile
	for i := range db.Profiles {
		if db.Profiles[i].IOS != nil {
			rows = append(rows, &db.Profiles[i])
		}
	}
	orders := map[int]int{}
	for _, p := range rows {
		orders[p.IOS.Order]++
	}
	for o := 1; o <= len(rows); o++ {
		if orders[o] != 1 {
			l.add("the iOS rows: %d hold order %d, and the orders are 1 to %d, each once", orders[o], o, len(rows))
		}
	}
	ports := map[string]string{}
	for _, p := range rows {
		for _, n := range p.IOS.PortNames {
			k := fold(strings.TrimSpace(n))
			if id, ok := ports[k]; ok && id != p.ID {
				l.add("profiles %s and %s both give the port name %q", id, p.ID, strings.TrimSpace(n))
			}
			ports[k] = p.ID
			for _, g := range db.IOSGenericNames {
				if fold(strings.TrimSpace(g.Name)) == k {
					l.add("profile %s: the port name %q is the generic name %q", p.ID, n, g.Name)
				}
			}
		}
		for _, t := range p.IOS.NameTokens {
			ft := fold(t)
			for _, g := range db.IOSGenericNames {
				if fg := fold(strings.TrimSpace(g.Name)); strings.Contains(fg, ft) || strings.Contains(ft, fg) {
					l.add("profile %s: the token %q and the generic name %q lie one inside the other", p.ID, t, g.Name)
				}
			}
			for _, q := range rows {
				if q == p {
					continue
				}
				for _, u := range q.IOS.NameTokens {
					if strings.Contains(fold(u), ft) {
						l.add("profile %s: the token %q lies inside profile %s's token %q", p.ID, t, q.ID, u)
					}
				}
				for _, n := range q.IOS.PortNames {
					if fn := fold(strings.TrimSpace(n)); strings.Contains(fn, ft) || strings.Contains(ft, fn) {
						l.add("profile %s: the token %q and profile %s's port name %q lie one inside the other", p.ID, t, q.ID, n)
					}
				}
			}
		}
	}
}

// claimsOf are the claims p makes, by the name a provenance line starts with:
// identity only for a profile with a match.
func claimsOf(p *Profile) []string {
	var claims []string
	if len(p.Match) > 0 {
		claims = append(claims, "identity")
	}
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
	if p.IOS != nil {
		claims = append(claims, "ios")
	}
	return claims
}

// knownClaim reports whether name is a claim a profile can make.
func knownClaim(name string) bool {
	switch name {
	case "identity", "native", "dop", "timing", "ios":
		return true
	}
	code, ok := strings.CutPrefix(name, "caveat ")
	return ok && slices.Contains(CaveatCodes(), code)
}

// provenance checks that each claim p makes has a line, "<claim>: …", naming
// a dated or versioned source, that no line names another claim, and that
// an ios line names the profile's own registry row.
func (l *linter) provenance(who string, p *Profile) {
	made := claimsOf(p)
	lined := map[string]bool{}
	for i, line := range p.Provenance {
		lw := fmt.Sprintf("%s, provenance line %d", who, i+1)
		claim, text, ok := strings.Cut(line, ": ")
		switch {
		case !ok || !knownClaim(claim):
			l.add("%s names no claim (identity, native, dop, timing, caveat <code> or ios)", lw)
		case !slices.Contains(made, claim):
			l.add("%s is for a %s claim the profile does not make", lw, claim)
		case claim == "ios" && !ownRow(text, p.ID):
			l.add("%s is the profile's own registry row, \"ios: DACProfile.swift %s at iOS <commit>\"", lw, p.ID)
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

// ownRow reports whether ref is the profile id's own row of the iOS
// registry at a commit.
func ownRow(ref, id string) bool {
	m := reIOSRef.FindStringSubmatch(ref)
	return m != nil && m[1] == id
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
