package dacprofile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// unit is the Unit a reading of sysfs gives: iface and product nil where the
// unit has none.
func unit(t *testing.T, vendorID, productID uint16, iface *int, product *string, bcdDevice string) Unit {
	t.Helper()
	k, err := NewKey(vendorID, productID, iface, product)
	if err != nil {
		t.Fatalf("NewKey(%04x:%04x): %v", vendorID, productID, err)
	}
	return Unit{Key: k, BCDDevice: bcdDevice}
}

// The four units the owner captured on Linux on 2026-11-04, as the sysfs
// reader returns them: the interface the card is (its audio control
// interface), the product string trimmed, the bcdDevice lsusb printed.
func capturedAuralic(t *testing.T) Unit {
	return unit(t, 0x1511, 0x0062, ptr(0), ptr("ALTAIR G1 USB Audio 2.0"), "06f2")
}
func capturedMytek(t *testing.T) Unit {
	return unit(t, 0x25ce, 0x0025, ptr(0), ptr("Brooklyn DAC+"), "0100")
}
func capturedHugo2(t *testing.T) Unit {
	return unit(t, 0x245f, 0x0815, ptr(1), ptr("Hugo2"), "0197")
}
func capturedIFi(t *testing.T) Unit {
	return unit(t, 0x20b1, 0x0010, ptr(0), ptr("iFi (by AMR) HD USB Audio"), "0170")
}

// mustDefault is the embedded database.
func mustDefault(t *testing.T) *Database {
	t.Helper()
	db, err := Default()
	if err != nil {
		t.Fatalf("the embedded database does not parse: %v", err)
	}
	return db
}

// matched is the ID of the profile that knows u, or "" when none does.
func matched(db *Database, u Unit) string {
	p, ok := db.Match(u)
	if !ok {
		return ""
	}
	return p.ID
}

// fixtureProfile is a profile on a placeholder ID no unit carries (vendor
// ffff), valid in every field, with a claim of each kind: the tests change
// one field at a time.
func fixtureProfile(id, productID string) Profile {
	return Profile{
		ID:    id,
		Name:  "Fixture DAC " + productID,
		Match: []Match{{VendorID: "ffff", ProductID: productID}},
		Native: &Native{MaxBitRate: 11289600, Formats: []string{"DSD_U32_BE"},
			Source: Source{Kind: SourceCapture, Ref: "owner capture, Linux, 2026-11-04: interface 1 altset 2"}},
		DoP: &DoP{HeardBitRates: []int{5644800}, RefusedBitRates: []int{11289600},
			AnalogVolumeControls: []AnalogControl{}, UnityAtRawMax: []RawMaxControl{},
			Source: Source{Kind: SourceIOSRegistry, Ref: "DACProfile.swift " + id + " at iOS cd125088"}},
		Timing:  &Timing{DSDPrefillMs: 700},
		Caveats: []Caveat{{Code: CaveatTransitionClicks, Transport: "dop", Note: "This DAC clicks as DoP starts at DSD128 and DSD256."}},
		Provenance: []string{
			"identity: test fixture, 2026-11-04: a placeholder ID no unit carries",
			"native: owner capture, Linux, 2026-11-04: interface 1's altset 2 lists DSD_U32_BE",
			"dop: heard DSD128: iOS profile fixture.dac (iOS cd125088); refused DSD256: iOS profile fixture.dac (iOS cd125088)",
			"timing: iOS profile fixture.dac's DSD resync delay, 0.7 s (iOS cd125088)",
			"caveat transition_clicks: iOS profile fixture.dac, clicks heard at DSD128 and DSD256 (iOS cd125088)",
		},
	}
}

// iosFixture is a profile of the iOS projection alone at order in the
// registry, valid in every field, its name keys built on token: no match,
// DoP heard at DSD64 and failed at DSD256 beside a maker's DSD figure to
// DSD256, each clause headed, under a legacy ceiling of DSD128.
func iosFixture(id string, order int, token string) Profile {
	return Profile{
		ID: id, Name: "Fixture " + token, Match: []Match{}, Caveats: []Caveat{},
		DoP: &DoP{HeardBitRates: []int{2822400}, StatedWithoutDoP: &Statement{Kind: StatementDSD, MaxBitRate: 11289600},
			RefusedBitRates: []int{11289600}, AnalogVolumeControls: []AnalogControl{}, UnityAtRawMax: []RawMaxControl{},
			Source: Source{Kind: SourceIOSRegistry, Ref: "DACProfile.swift " + id + " at iOS 6a1241eb"}},
		IOS: &IOS{Order: order, Maker: "Fixture", LegacyPatterns: []string{token}, LegacyCeilingBitRate: 5644800,
			PortNames: []string{"FIXTURE " + token + " "}, NameTokens: []string{token}, ExcludedTokens: []string{"Lite"},
			NameScope: "model", PCM: IOSPCM{MaxRateHz: 384000, BitDepth: 32}, VolumeTopology: "unknown",
			Timing: IOSTiming{IOBufferMs: ptr(40)}},
		Provenance: []string{
			"dop: heard DSD64: submission 2026-10-04 (with PEQ off); refused DSD256: field log 2026-06-21; stated DSD to DSD256: maker (the row's note, iOS 6a1241eb)",
			"ios: DACProfile.swift " + id + " at iOS 6a1241eb",
		},
	}
}

// fixture is a database of the given profiles and shared reference IDs.
func fixture(shared []SharedID, profiles ...Profile) *Database {
	return &Database{SchemaVersion: SchemaVersion, SharedReferenceIDs: shared, Profiles: profiles}
}

// iosPair is a database of two iOS rows, iosFixture's on the tokens Alpha
// and Bravo, beside two of the generic names.
func iosPair() *Database {
	db := fixture(nil, iosFixture("fixture.alpha", 1, "Alpha"), iosFixture("fixture.bravo", 2, "Bravo"))
	db.IOSGenericNames = []GenericName{
		{Name: "Headphones", Why: "the name iOS gives a Lightning headphone accessory (iOS 6a1241eb)"},
		{Name: "Combo384 Amanero", Why: "an interface board's default string (iOS 6a1241eb)"},
	}
	return db
}

// dbCase is a mutation of iosPair and what the lint must say of the result.
type dbCase struct {
	name   string
	mutate func(db *Database)
	words  string
}

// runDBCases lints iosPair under each mutation and requires a problem saying
// the case's words; the pair itself must lint clean.
func runDBCases(t *testing.T, cases []dbCase) {
	t.Helper()
	if problems := iosPair().Lint(); len(problems) > 0 {
		t.Fatalf("the iOS pair lints as %v", problems)
	}
	for _, tc := range cases {
		db := iosPair()
		tc.mutate(db)
		if problems := db.Lint(); !lintSays(problems, "", tc.words) {
			t.Errorf("%s: the lint says %v, want a problem saying %q", tc.name, problems, tc.words)
		}
	}
}

// sharedIFi is the XMOS reference ID the captured iFi unit carries, listed as
// the embedded database lists it.
func sharedIFi() []SharedID {
	return []SharedID{{VendorID: "20b1", ProductID: "0010", Why: "XMOS's reference product ID (owner capture, Linux, 2026-11-04)"}}
}

// lintSays reports whether one of problems names the profile or the entry
// "who" and says words.
func lintSays(problems []error, who, words string) bool {
	for _, p := range problems {
		if s := p.Error(); strings.Contains(s, who) && strings.Contains(s, words) {
			return true
		}
	}
	return false
}

// TestAProfileMatchesByUSBIdentityNeverByName: each captured unit, read as
// sysfs reads it, is known by its vendor and product IDs and its product
// string, compared exactly, and a kernel-seeded model by its IDs alone; a unit
// with another ID is known by no profile, whatever its product string says,
// the profile's name included; a product string that differs by its case, a
// space or a word is another model's; and the interface and the bcdDevice an
// alternative sets narrow it, a unit with neither read matching neither.
func TestAProfileMatchesByUSBIdentityNeverByName(t *testing.T) {
	db := mustDefault(t)
	other := func(product string) Unit { return unit(t, 0x1234, 0x5678, ptr(1), ptr(product), "0197") }
	hugo := func(product *string) Unit { return unit(t, 0x245f, 0x0815, ptr(1), product, "0197") }
	for _, tc := range []struct {
		name string
		u    Unit
		want string
	}{
		{"the AURALiC", capturedAuralic(t), "auralic.altair.g1"},
		{"the Mytek", capturedMytek(t), "mytek.brooklyn.dac.plus"},
		{"the Hugo 2", capturedHugo2(t), "chord.hugo2"},
		{"the Hugo 2 on any firmware", unit(t, 0x245f, 0x0815, ptr(1), ptr("Hugo2"), ""), "chord.hugo2"},
		{"the Hugo 2's alternative sets no interface", unit(t, 0x245f, 0x0815, ptr(3), ptr("Hugo2"), "0197"), "chord.hugo2"},
		{"a kernel-seeded model by its IDs", unit(t, 0x0644, 0x8043, ptr(0), ptr("UD-501"), "0100"), "kernel.0644-8043"},
		{"a kernel-seeded model with nothing else read", unit(t, 0x0644, 0x8043, nil, nil, ""), "kernel.0644-8043"},
		{"another ID with the Hugo 2's product string", other("Hugo2"), ""},
		{"another ID with the Hugo 2's profile name", other("Chord Hugo 2"), ""},
		{"another ID with the Mytek's profile name", other("Mytek Brooklyn DAC+"), ""},
		{"the Hugo 2's IDs with its profile name as the product", hugo(ptr("Chord Hugo 2")), ""},
		{"the Hugo 2's IDs with iOS's other spelling", hugo(ptr("Hugo 2")), ""},
		{"the Hugo 2's IDs in capitals", hugo(ptr("HUGO2")), ""},
		{"the Hugo 2's IDs with a word after", hugo(ptr("Hugo2 TT")), ""},
		{"the Hugo 2's IDs with no product string", hugo(nil), ""},
		{"the Mytek's vendor and product string under another product ID", unit(t, 0x25ce, 0x001f, ptr(0), ptr("Brooklyn DAC+"), "0100"), ""},
		{"the AURALiC's product string under another product ID", unit(t, 0x1511, 0x0063, ptr(0), ptr("ALTAIR G1 USB Audio 2.0"), "06f2"), ""},
	} {
		if got := matched(db, tc.u); got != tc.want {
			t.Errorf("%s: matched %q, want %q", tc.name, got, tc.want)
		}
	}

	// The interface and the bcdDevice an alternative sets narrow it.
	narrow := fixtureProfile("fixture.narrow", "00a1")
	narrow.Match = []Match{{VendorID: "ffff", ProductID: "00a1", Interface: ptr(1), BCDDevice: []string{"0101", "01a2"}}}
	db = fixture(nil, narrow)
	if problems := db.Lint(); len(problems) > 0 {
		t.Fatalf("the narrowing fixture does not lint: %v", problems)
	}
	for _, tc := range []struct {
		name  string
		iface *int
		bcd   string
		want  bool
	}{
		{"its interface and a listed firmware", ptr(1), "0101", true},
		{"the other listed firmware", ptr(1), "01a2", true},
		{"a listed firmware in capitals, as ParseHex16 takes it", ptr(1), "01A2", true},
		{"a listed firmware after 0x, as ParseHex16 takes it", ptr(1), "0x01a2", true},
		{"another interface", ptr(2), "0101", false},
		{"no interface read", nil, "0101", false},
		{"another firmware", ptr(1), "0103", false},
		{"no firmware read", ptr(1), "", false},
		{"a firmware that does not read", ptr(1), "01g2", false},
	} {
		if _, ok := db.Match(unit(t, 0xffff, 0x00a1, tc.iface, ptr("Anything"), tc.bcd)); ok != tc.want {
			t.Errorf("narrowed, %s: matched %v, want %v", tc.name, ok, tc.want)
		}
	}
}

// TestASharedReferenceIDNeedsItsProductString: the database lists the XMOS
// reference ID the captured iFi unit carries (20b1:0010), the Amanero board's
// (16d0:071a) and its newer firmware's (16d0:0a23, B990), the C-Media ID the
// kernel gives a DSD quirk under one maker's name while another maker's units
// ship it too (0d8c:0316), and the WaveIO board's (20a0:4143), each with why;
// a unit on any of them is unknown, the
// iFi unit included (its capture establishes no DoP, raw-maximum or native
// claim), whatever product string it carries, until a capture gives one; a
// profile on a shared ID without its product string is refused by the lint,
// and with it knows the unit that carries it and no other on the ID; and Match
// asks again, so a database built past the lint knows no unit on a shared ID by
// an alternative without its product string.
func TestASharedReferenceIDNeedsItsProductString(t *testing.T) {
	db := mustDefault(t)
	for _, want := range [][2]string{{"20b1", "0010"}, {"16d0", "071a"}, {"16d0", "0a23"}, {"0d8c", "0316"}, {"20a0", "4143"}} {
		i := slices.IndexFunc(db.SharedReferenceIDs, func(s SharedID) bool { return s.VendorID == want[0] && s.ProductID == want[1] })
		if i < 0 {
			t.Errorf("the database does not list %s:%s as a shared reference ID", want[0], want[1])
		} else if db.SharedReferenceIDs[i].Why == "" {
			t.Errorf("%s:%s is listed with no why", want[0], want[1])
		}
	}
	ifi := capturedIFi(t)
	if got := matched(db, ifi); got != "" {
		t.Errorf("the captured iFi unit is known as %q; its capture establishes no claim", got)
	}
	for _, u := range []Unit{
		unit(t, 0x0d8c, 0x0316, ptr(0), ptr("USB Audio"), "0100"),
		unit(t, 0x0d8c, 0x0316, ptr(1), nil, ""),
		unit(t, 0x20a0, 0x4143, ptr(0), ptr("A DAC built on the board"), "0100"),
		unit(t, 0x20a0, 0x4143, nil, nil, ""),
		unit(t, 0x16d0, 0x071a, ptr(0), ptr("Combo384"), "019b"),
		unit(t, 0x16d0, 0x0a23, ptr(0), ptr("Combo384"), "0100"),
	} {
		if got := matched(db, u); got != "" {
			t.Errorf("a unit on the shared ID %04x:%04x is known as %q", u.Key.VendorID(), u.Key.ProductID(), got)
		}
	}
	for _, p := range db.Profiles {
		for _, m := range p.Match {
			for _, s := range db.SharedReferenceIDs {
				if m.VendorID == s.VendorID && m.ProductID == s.ProductID && m.Product == nil {
					t.Errorf("%s matches the shared reference ID %s:%s without its product string", p.ID, s.VendorID, s.ProductID)
				}
			}
		}
	}

	bare := fixtureProfile("fixture.ifi", "0010")
	bare.Match = []Match{{VendorID: "20b1", ProductID: "0010"}}
	if problems := fixture(sharedIFi(), bare).Lint(); !lintSays(problems, "fixture.ifi", "shared reference ID") {
		t.Errorf("a profile on 20b1:0010 without its product string lints as %v", problems)
	}
	named := bare
	named.Match = []Match{{VendorID: "20b1", ProductID: "0010", Product: ptr("iFi (by AMR) HD USB Audio")}}
	db = fixture(sharedIFi(), named)
	if problems := db.Lint(); len(problems) > 0 {
		t.Fatalf("a profile on 20b1:0010 with its product string lints as %v", problems)
	}
	if got := matched(db, ifi); got != "fixture.ifi" {
		t.Errorf("the unit carrying the product string matched %q", got)
	}
	for _, u := range []Unit{
		unit(t, 0x20b1, 0x0010, ptr(0), ptr("XMOS USB Audio 2.0"), "0170"),
		unit(t, 0x20b1, 0x0010, ptr(0), nil, "0170"),
	} {
		if got := matched(db, u); got != "" {
			t.Errorf("another unit on the shared ID (%s) matched %q", u.Key.Canonical(), got)
		}
	}
	if got := matched(fixture(sharedIFi(), bare), ifi); got != "" {
		t.Errorf("past the lint, an alternative on the shared ID without its product string matched %q", got)
	}
	// An ID nobody listed as shared needs no product string.
	plain := bare
	plain.Match = []Match{{VendorID: "ffff", ProductID: "00b1"}}
	if got := matched(fixture(sharedIFi(), plain), unit(t, 0xffff, 0x00b1, ptr(0), ptr("Anything"), "")); got != "fixture.ifi" {
		t.Errorf("an alternative on an ID that is not shared, without a product string, matched %q", got)
	}
}

// TestNoTwoProfilesMatchOneIdentity: the embedded database lints clean; the
// lint refuses two profiles whose alternatives one unit could match, naming
// both (an alternative that leaves a field unset overlaps any value of it,
// two firmware lists overlap where they share one, and two alternatives of one
// profile never conflict); and at run time a unit two profiles match, in a
// database built past the lint, is unknown.
func TestNoTwoProfilesMatchOneIdentity(t *testing.T) {
	if problems := mustDefault(t).Lint(); len(problems) > 0 {
		t.Errorf("the embedded database lints as %v", problems)
	}
	alt := func(product *string, iface *int, bcd ...string) Match {
		m := Match{VendorID: "ffff", ProductID: "00c1", Interface: iface, Product: product}
		if bcd != nil {
			m.BCDDevice = bcd
		}
		return m
	}
	two := func(a, b Match) *Database {
		p, q := fixtureProfile("fixture.first", "00c1"), fixtureProfile("fixture.second", "00c1")
		p.Match, q.Match = []Match{a}, []Match{b}
		return fixture(nil, p, q)
	}
	for _, tc := range []struct {
		name    string
		a, b    Match
		overlap bool
	}{
		{"one product string and none", alt(ptr("A"), nil), alt(nil, nil), true},
		{"no product string on either", alt(nil, nil), alt(nil, nil), true},
		{"one product string on both", alt(ptr("A"), nil), alt(ptr("A"), nil), true},
		{"two product strings", alt(ptr("A"), nil), alt(ptr("B"), nil), false},
		{"one interface and none", alt(ptr("A"), ptr(1)), alt(ptr("A"), nil), true},
		{"two interfaces", alt(ptr("A"), ptr(1)), alt(ptr("A"), ptr(2)), false},
		{"one firmware list and none", alt(ptr("A"), nil, "0101"), alt(ptr("A"), nil), true},
		{"two firmware lists sharing one", alt(ptr("A"), nil, "0101", "0102"), alt(ptr("A"), nil, "0102"), true},
		{"two firmware lists sharing none", alt(ptr("A"), nil, "0101"), alt(ptr("A"), nil, "0102"), false},
		{"another product ID", alt(nil, nil), Match{VendorID: "ffff", ProductID: "00c2"}, false},
	} {
		problems := two(tc.a, tc.b).Lint()
		refused := lintSays(problems, "fixture.first", "fixture.second")
		if refused != tc.overlap {
			t.Errorf("%s: refused %v, want %v (%v)", tc.name, refused, tc.overlap, problems)
		}
	}
	one := fixtureProfile("fixture.alone", "00c1")
	one.Match = []Match{alt(ptr("A"), nil), alt(nil, nil)}
	if problems := fixture(nil, one).Lint(); len(problems) > 0 {
		t.Errorf("two alternatives of one profile that overlap lint as %v", problems)
	}

	db := two(alt(nil, nil), alt(ptr("A"), nil))
	if got := matched(db, unit(t, 0xffff, 0x00c1, ptr(0), ptr("A"), "")); got != "" {
		t.Errorf("a unit two profiles match is known as %q", got)
	}
	if got := matched(db, unit(t, 0xffff, 0x00c1, ptr(0), ptr("B"), "")); got != "fixture.first" {
		t.Errorf("a unit one of them matches is known as %q", got)
	}
}

// TestAnIOSOnlyRowMatchesNoUnit: a profile with no match alternative, iOS's
// row alone, knows no unit: not one of the four captured units, not a unit
// built from any other profile's alternative, and not a unit whose product
// string is one of the row's port names or tokens, under the placeholder
// vendor or a captured unit's IDs.
func TestAnIOSOnlyRowMatchesNoUnit(t *testing.T) {
	db := mustDefault(t)
	iosOnly := map[string]bool{}
	units := []Unit{capturedAuralic(t), capturedMytek(t), capturedHugo2(t), capturedIFi(t)}
	for _, p := range db.Profiles {
		if len(p.Match) == 0 {
			iosOnly[p.ID] = true
			for _, name := range append(slices.Clone(p.IOS.PortNames), p.IOS.NameTokens...) {
				name = strings.TrimSpace(name)
				units = append(units, unit(t, 0xffff, 0x0013, ptr(0), &name, ""), unit(t, 0x245f, 0x0815, ptr(1), &name, "0197"))
			}
		}
		for _, m := range p.Match {
			v, _ := strconv.ParseUint(m.VendorID, 16, 16)
			pid, _ := strconv.ParseUint(m.ProductID, 16, 16)
			bcd := ""
			if len(m.BCDDevice) > 0 {
				bcd = m.BCDDevice[0]
			}
			units = append(units, unit(t, uint16(v), uint16(pid), m.Interface, m.Product, bcd))
		}
	}
	if len(iosOnly) == 0 {
		t.Fatal("the embedded database holds no iOS-only row, so nothing was checked")
	}
	for _, u := range units {
		if p, ok := db.Match(u); ok && iosOnly[p.ID] {
			t.Errorf("the unit %q is known as the iOS-only row %s", u.Key.Canonical(), p.ID)
		}
	}
}

// TestTheQX13CarriesItsHiddenModeCaveat: the QX13 shape (the design's
// section 2.1 example), as a test fixture on a placeholder ID, parses, and
// the unit on it is known with the dop_noise_in_hidden_mode caveat on DoP
// and its note; the embedded database's QX13 is iOS's row alone, with no
// match, since the QX13's USB identity has not been captured, nothing it
// ships names the placeholder vendor or knows the fixture's unit; and a
// placeholder written where an ID goes does not parse.
func TestTheQX13CarriesItsHiddenModeCaveat(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "qx13.json"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := Parse(b)
	if err != nil {
		t.Fatalf("the QX13 fixture does not parse: %v", err)
	}
	qx13 := unit(t, 0xffff, 0x0013, ptr(0), ptr("FIIO QX13"), "")
	p, ok := db.Match(qx13)
	if !ok || p.ID != "fiio.qx13" {
		t.Fatalf("the fixture's unit matched %q, %v", p.ID, ok)
	}
	i := slices.IndexFunc(p.Caveats, func(c Caveat) bool { return c.Code == CaveatDoPNoiseInHiddenMode })
	if i < 0 || p.Caveats[i].Transport != "dop" || p.Caveats[i].Note == "" {
		t.Errorf("the QX13's caveats: %+v", p.Caveats)
	}
	if p.DoP == nil || !slices.Equal(p.DoP.HeardBitRates, []int{2822400, 5644800, 11289600}) {
		t.Errorf("the QX13's DoP: %+v", p.DoP)
	}

	shipped := mustDefault(t)
	if i := slices.IndexFunc(shipped.Profiles, func(q Profile) bool { return q.ID == "fiio.qx13" }); i < 0 ||
		len(shipped.Profiles[i].Match) != 0 || shipped.Profiles[i].IOS == nil {
		t.Errorf("the embedded database's QX13 is not iOS's row alone, its identity not captured")
	}
	for _, q := range shipped.Profiles {
		if slices.ContainsFunc(q.Match, func(m Match) bool { return m.VendorID == "ffff" }) {
			t.Errorf("the embedded database's %s names the placeholder vendor ffff", q.ID)
		}
	}
	if got := matched(shipped, qx13); got != "" {
		t.Errorf("the embedded database knows the fixture's placeholder unit as %q", got)
	}
	placeholder := bytes.Replace(b, []byte(`"vendorId": "ffff"`), []byte(`"vendorId": "<captured>"`), 1)
	if bytes.Equal(placeholder, b) {
		t.Fatal("the fixture has no vendorId to replace")
	}
	if _, err := Parse(placeholder); err == nil {
		t.Error("a placeholder vendorId parses")
	}
}

// TestTheCapturedModelsAreSeededAsTheirEvidenceSays: each captured model's
// profile holds what its capture and its iOS row establish, and nothing they
// do not: the AURALiC native DSD to DSD512 from its stream file and no DoP
// claim (it has no iOS row); the Mytek native DSD to DSD256 from its stream
// file, and DoP refused at DSD256 (its carrier refused on a device, a field
// log of 2026-06-21) with no rate heard, since the row's DSD128 is a DoP
// lock, which section 1.1 says hears nothing, and its maker's DSD256, a
// figure that names no transport, held as a statement without DoP, never as
// DoP's; the Hugo 2 no native claim (its stream file lists PCM alone), no
// rate heard (its row's DSD256 is a negotiation and a DoP lock, not a
// hearing), and its maker's statement of DoP to DSD256, with its DSD resync
// delay as the prefill; none lists an analogue or raw-maximum control, which
// no capture showed.
func TestTheCapturedModelsAreSeededAsTheirEvidenceSays(t *testing.T) {
	db := mustDefault(t)
	byID := map[string]Profile{}
	for _, p := range db.Profiles {
		byID[p.ID] = p
	}
	native := func(rate int, kind string) *Native {
		return &Native{MaxBitRate: rate, Formats: []string{"DSD_U32_BE"}, Source: Source{Kind: kind}}
	}
	dop := func(stated int, without *Statement, refused []int, row string) *DoP {
		return &DoP{HeardBitRates: []int{}, StatedMaxBitRate: stated, StatedWithoutDoP: without, RefusedBitRates: refused,
			AnalogVolumeControls: []AnalogControl{}, UnityAtRawMax: []RawMaxControl{},
			Source: Source{Kind: SourceIOSRegistry, Ref: "DACProfile.swift " + row + " at iOS 6a1241eb"}}
	}
	for _, tc := range []struct {
		id      string
		match   Match
		native  *Native
		dop     *DoP
		prefill int
		caveats int
	}{
		{"auralic.altair.g1", Match{VendorID: "1511", ProductID: "0062", Product: ptr("ALTAIR G1 USB Audio 2.0")},
			native(22579200, SourceCapture), nil, 0, 0},
		{"mytek.brooklyn.dac.plus", Match{VendorID: "25ce", ProductID: "0025", Product: ptr("Brooklyn DAC+")},
			native(11289600, SourceCapture), dop(0, &Statement{Kind: StatementDSD, MaxBitRate: 11289600}, []int{11289600}, "mytek.brooklyn.dac.plus"), 0, 0},
		{"chord.hugo2", Match{VendorID: "245f", ProductID: "0815", Product: ptr("Hugo2")},
			nil, dop(11289600, nil, []int{}, "chord.hugo2"), 700, 0},
	} {
		p, ok := byID[tc.id]
		if !ok {
			t.Errorf("no %s profile", tc.id)
			continue
		}
		if !reflect.DeepEqual(p.Match, []Match{tc.match}) {
			t.Errorf("%s matches %+v", tc.id, p.Match)
		}
		if got := p.Native; (got == nil) != (tc.native == nil) ||
			got != nil && (got.MaxBitRate != tc.native.MaxBitRate || !slices.Equal(got.Formats, tc.native.Formats) || got.Source.Kind != tc.native.Source.Kind) {
			t.Errorf("%s's native claim is %+v, want %+v", tc.id, got, tc.native)
		}
		if !reflect.DeepEqual(p.DoP, tc.dop) {
			t.Errorf("%s's DoP is %+v, want %+v", tc.id, p.DoP, tc.dop)
		}
		prefill := 0
		if p.Timing != nil {
			prefill = p.Timing.DSDPrefillMs
		}
		if prefill != tc.prefill || len(p.Caveats) != tc.caveats {
			t.Errorf("%s: prefill %d ms, %d caveats", tc.id, prefill, len(p.Caveats))
		}
	}
}

// TestTheEmbeddedDatabaseIsWrittenInItsCanonicalForm: profiles.json is what
// the encoder writes from its own decoding (two-space indent, the schema's
// field order, every field present), so a database PR's diff holds the
// change alone.
func TestTheEmbeddedDatabaseIsWrittenInItsCanonicalForm(t *testing.T) {
	var db Database
	if err := json.Unmarshal(embedded, &db); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&db); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), embedded) {
		t.Errorf("profiles.json is not in its canonical form; it reads as:\n%s", out.Bytes())
	}
}

// TestMatchHandsOutACopy: what Match returns shares nothing with the
// database, so a caller that changes it changes no later answer: a profile
// with every slice populated and every pointer set, its DoP evidence and its
// iOS projection included, each element and pointee changed.
func TestMatchHandsOutACopy(t *testing.T) {
	full := fixtureProfile("fixture.copy", "00f1")
	full.Match[0] = Match{VendorID: "ffff", ProductID: "00f1", Interface: ptr(1), Product: ptr("Copy DAC"), BCDDevice: []string{"0101"}}
	full.DoP.StatedWithoutDoP = &Statement{Kind: StatementDSD, MaxBitRate: 11289600}
	full.DoP.AnalogVolumeControls = []AnalogControl{{ControlID: ControlID{Interface: "MIXER", Name: "Master", Index: 0}, Provenance: "a test, 2026-11-17"}}
	full.DoP.UnityAtRawMax = []RawMaxControl{{ControlID: ControlID{Interface: "MIXER", Name: "PCM", Index: 0}, RawMax: 127, BCDDevice: []string{"0101"}}}
	full.IOS = iosFixture("fixture.copy", 1, "Copy").IOS
	full.IOS.SiblingNote = ptr("a sibling's result (a submission, 2026-09-23)")
	full.IOS.Timing = IOSTiming{IOBufferMs: ptr(40), DSDResyncMs: ptr(700), PCMEntryResyncMs: ptr(800), ColdStartExtraMs: ptr(600), TransitionCushionMs: ptr(30)}
	db := fixture(nil, full)
	u := unit(t, 0xffff, 0x00f1, ptr(1), ptr("Copy DAC"), "0101")
	p, ok := db.Match(u)
	if !ok {
		t.Fatal("the fixture is unknown")
	}
	want, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	m := &p.Match[0]
	*m.Interface, *m.Product, m.BCDDevice[0] = 2, "", "0000"
	p.Native.Formats[0] = "DSD_U8"
	p.DoP.HeardBitRates[0] = 0
	p.DoP.StatedWithoutDoP.MaxBitRate = 0
	p.DoP.RefusedBitRates[0] = 0
	p.DoP.AnalogVolumeControls[0].Name = ""
	p.DoP.UnityAtRawMax[0].BCDDevice[0] = ""
	p.Timing.DSDPrefillMs = 0
	p.Caveats[0].Note = ""
	p.Provenance[0] = ""
	x := p.IOS
	x.Order, x.PCM.BitDepth = 9, 16
	x.LegacyPatterns[0], x.PortNames[0], x.NameTokens[0], x.ExcludedTokens[0], *x.SiblingNote = "", "", "", "", ""
	for _, v := range []*int{x.Timing.IOBufferMs, x.Timing.DSDResyncMs, x.Timing.PCMEntryResyncMs, x.Timing.ColdStartExtraMs, x.Timing.TransitionCushionMs} {
		*v = -1
	}
	q, ok := db.Match(u)
	if !ok {
		t.Fatal("the fixture is unknown after a change to an answer")
	}
	if got, err := json.Marshal(q); err != nil || !bytes.Equal(got, want) {
		t.Errorf("a change to an answer reached the database:\n got %s\nwant %s", got, want)
	}
}
