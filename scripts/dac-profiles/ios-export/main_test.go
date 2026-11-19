package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
)

// golden is the export the iOS app bundles, beside the package.
const golden = "DACCatalog.ios.json"

// baselineSHA256 is the SHA-256 of the iOS registry's baseline the dispatch
// gave (testdata/ios-registry-3145606a.json), a dump of the live registry's
// objects at iOS 3145606a: an input, never derived from the catalog.
const baselineSHA256 = "a91415f917dcf6f18da58e6d265ad9af46b65b9c2a0e6e0f7852d92105749167"

// fresh is the embedded catalog's export.
func fresh(t *testing.T) []byte {
	t.Helper()
	db, err := dacprofile.Default()
	if err != nil {
		t.Fatal(err)
	}
	b, err := build(db)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTheExportIsItsGolden: a fresh export of the embedded catalog is
// DACCatalog.ios.json byte for byte; the file decodes strictly into the
// shape iOS PR 2 reads, every key present and in its order (its decoding
// encodes to the same bytes); -check passes on it, writing nothing, and
// refuses a copy a byte of which differs, which a run without -check
// rewrites. It logs the SHA-256 the iOS app pins.
func TestTheExportIsItsGolden(t *testing.T) {
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: sha256 %x", golden, sha256.Sum256(want))
	if got := fresh(t); !bytes.Equal(got, want) {
		t.Errorf("%s is not a fresh export of the catalog: write it with ios-export", golden)
	}
	type src struct {
		Kind string  `json:"kind"`
		Date *string `json:"date"`
	}
	var shape struct {
		ExportVersion int      `json:"exportVersion"`
		GenericNames  []string `json:"genericNames"`
		Rows          []struct {
			ID             string   `json:"id"`
			Name           string   `json:"name"`
			Maker          string   `json:"maker"`
			LegacyPatterns []string `json:"legacyPatterns"`
			LegacyCeiling  string   `json:"legacyCeiling"`
			PCM            struct {
				MaxRateHz int `json:"maxRateHz"`
				BitDepth  int `json:"bitDepth"`
			} `json:"pcm"`
			VolumeTopology string `json:"volumeTopology"`
			Timing         struct {
				IOBufferMs          *int `json:"ioBufferMs"`
				DSDResyncMs         *int `json:"dsdResyncMs"`
				PCMEntryResyncMs    *int `json:"pcmEntryResyncMs"`
				ColdStartExtraMs    *int `json:"coldStartExtraMs"`
				TransitionCushionMs *int `json:"transitionCushionMs"`
			} `json:"timing"`
			HiddenMode bool `json:"hiddenMode"`
			NameKeys   struct {
				PortNames      []string `json:"portNames"`
				Tokens         []string `json:"tokens"`
				ExcludedTokens []string `json:"excludedTokens"`
				Scope          string   `json:"scope"`
			} `json:"nameKeys"`
			Evidence struct {
				Heard []struct {
					Rate   string `json:"rate"`
					Source src    `json:"source"`
				} `json:"heard"`
				Failed []struct {
					Rate    string `json:"rate"`
					Failure string `json:"failure"`
					Source  src    `json:"source"`
				} `json:"failed"`
				Statement struct {
					Case   string  `json:"case"`
					UpTo   *string `json:"upTo"`
					Source *src    `json:"source"`
				} `json:"statement"`
				SiblingNote *string `json:"siblingNote"`
			} `json:"evidence"`
		} `json:"rows"`
	}
	dec := json.NewDecoder(bytes.NewReader(want))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&shape); err != nil {
		t.Fatalf("%s does not decode into the export's shape: %v", golden, err)
	}
	var again bytes.Buffer
	enc := json.NewEncoder(&again)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(shape); err != nil || !bytes.Equal(again.Bytes(), want) {
		t.Errorf("%s holds a key out of the shape's order or misses one (%v)", golden, err)
	}
	if shape.ExportVersion != exportVersion || len(shape.Rows) == 0 {
		t.Errorf("%s: export version %d, %d rows", golden, shape.ExportVersion, len(shape.Rows))
	}

	var out bytes.Buffer
	if err := run(golden, true, &out); err != nil || !strings.Contains(out.String(), fmt.Sprintf("sha256 %x", sha256.Sum256(want))) {
		t.Errorf("-check on %s: %v, %q", golden, err, out.String())
	}
	copied := filepath.Join(t.TempDir(), golden)
	flipped := slices.Clone(want)
	flipped[len(flipped)/2] ^= 0x01
	if err := os.WriteFile(copied, flipped, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(copied, true, &out); !errors.Is(err, errDiffers) {
		t.Errorf("-check on a copy with a byte flipped: %v, want errDiffers", err)
	}
	if b, err := os.ReadFile(copied); err != nil || !bytes.Equal(b, flipped) {
		t.Errorf("-check wrote the file (%v)", err)
	}
	if err := run(copied, false, &out); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(copied); err != nil || !bytes.Equal(b, want) {
		t.Errorf("a run without -check did not write the fresh export (%v)", err)
	}
}

// TestTheExportIsDeterministic: two exports of the catalog are the same
// bytes, and so are exports of its profiles in another order, reversed and
// rotated: rows go by ios.order, never by the database's order.
func TestTheExportIsDeterministic(t *testing.T) {
	first := fresh(t)
	if second := fresh(t); !bytes.Equal(first, second) {
		t.Error("two exports of the catalog differ")
	}
	for name, order := range map[string]func(ps []dacprofile.Profile){
		"reversed": slices.Reverse[[]dacprofile.Profile],
		"rotated":  func(ps []dacprofile.Profile) { copy(ps, append(slices.Clone(ps[7:]), ps[:7]...)) },
	} {
		db, err := dacprofile.Default()
		if err != nil {
			t.Fatal(err)
		}
		order(db.Profiles)
		if got, err := build(db); err != nil || !bytes.Equal(got, first) {
			t.Errorf("the catalog's profiles %s export otherwise (%v)", name, err)
		}
	}
}

// TestAnUnrecordedRowExportsNoEvidence: an iOS row with no rate heard or
// failed and no statement holds no DoP claim (dop null, and no dop line,
// which the lint takes), and exports no heard or failed entry and the
// statement unrecorded, with no rate and no source.
func TestAnUnrecordedRowExportsNoEvidence(t *testing.T) {
	db := &dacprofile.Database{SchemaVersion: dacprofile.SchemaVersion, IOSGenericNames: []dacprofile.GenericName{},
		Profiles: []dacprofile.Profile{{
			ID: "fixture.unrecorded", Name: "Fixture Unrecorded", Match: []dacprofile.Match{}, Caveats: []dacprofile.Caveat{},
			IOS: &dacprofile.IOS{Order: 1, Maker: "Fixture", LegacyPatterns: []string{"Unrecorded"}, LegacyCeilingBitRate: 11289600,
				PortNames: []string{}, NameTokens: []string{"Unrecorded"}, ExcludedTokens: []string{}, NameScope: "model",
				PCM: dacprofile.IOSPCM{MaxRateHz: 384000, BitDepth: 32}, VolumeTopology: "unknown"},
			Provenance: []string{"ios: DACProfile.swift fixture.unrecorded at iOS 6a1241eb"},
		}}}
	if problems := db.Lint(); len(problems) > 0 {
		t.Fatalf("an iOS row with no DoP evidence lints as %v", problems)
	}
	b, err := build(db)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Rows []struct {
			Evidence map[string]json.RawMessage `json:"evidence"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(b, &got); err != nil || len(got.Rows) != 1 {
		t.Fatalf("the export: %v, %s", err, b)
	}
	ev := got.Rows[0].Evidence
	for key, want := range map[string]string{"heard": `[]`, "failed": `[]`, "siblingNote": `null`} {
		if string(ev[key]) != want {
			t.Errorf("evidence.%s is %s, want %s", key, ev[key], want)
		}
	}
	var s map[string]json.RawMessage
	if err := json.Unmarshal(ev["statement"], &s); err != nil || len(s) != 3 || string(s["case"]) != `"unrecorded"` ||
		string(s["upTo"]) != "null" || string(s["source"]) != "null" {
		t.Errorf("the statement is %s, want unrecorded with no rate and no source (%v)", ev["statement"], err)
	}
}

// corpusName is a name of the baseline's parity corpus or its known
// siblings, with the live matcher's answer at iOS 3145606a (null for none),
// the strict matcher's, and whether iOS PR 1 declares the two apart; or a
// name declared past the baseline (declared), with the answers each matcher
// must give it.
type corpusName struct {
	Name      string
	Live      *string
	Strict    json.RawMessage
	Exception bool
}

// live is c's live answer, "" for none.
func (c corpusName) live() string {
	if c.Live == nil {
		return ""
	}
	return *c.Live
}

// fold is s with A to Z in lower case: Swift's lowercased() on the printable
// ASCII every catalog name and corpus name is.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		return r
	}, s)
}

// firstHit is the live matcher's answer for name: the first row, in order,
// one of whose patterns the folded name holds, or "" for none.
func firstHit(name string, ids []string, patterns [][]string) string {
	for i, ps := range patterns {
		if slices.ContainsFunc(ps, func(p string) bool { return strings.Contains(fold(name), fold(p)) }) {
			return ids[i]
		}
	}
	return ""
}

// strictMatch is iOS PR 1's strict matcher on a USB port, over the export's
// name keys, in the baseline's form of its answer: a generic name is
// unknown, then a port name, trimmed with its case kept, then a token one row
// alone holds and none of its excluded tokens rules out.
func strictMatch(name string, x export) string {
	if slices.ContainsFunc(x.GenericNames, func(g string) bool { return fold(strings.TrimSpace(g)) == fold(strings.TrimSpace(name)) }) {
		return `{"unknown":"generic"}`
	}
	holds := func(ts []string) bool {
		return slices.ContainsFunc(ts, func(tk string) bool { return strings.Contains(fold(name), fold(tk)) })
	}
	tiers := []func(r row) bool{
		func(r row) bool {
			return slices.ContainsFunc(r.NameKeys.PortNames, func(p string) bool { return strings.TrimSpace(p) == strings.TrimSpace(name) })
		},
		func(r row) bool { return holds(r.NameKeys.Tokens) && !holds(r.NameKeys.ExcludedTokens) },
	}
	for i, in := range tiers {
		var hits []row
		for _, r := range x.Rows {
			if in(r) {
				hits = append(hits, r)
			}
		}
		switch {
		case len(hits) == 1:
			return fmt.Sprintf(`{"known":{"id":%q,"scope":%q,"tier":%q}}`, hits[0].ID, hits[0].NameKeys.Scope, []string{"exact", "token"}[i])
		case len(hits) > 1:
			return `{"unknown":"ambiguous"}`
		}
	}
	return `{"unknown":"noMatch"}`
}

// strictKnown is the row a strict answer knows, "" for none.
func strictKnown(answer string) string {
	var a struct{ Known *struct{ ID string } }
	if json.Unmarshal([]byte(answer), &a) != nil || a.Known == nil {
		return ""
	}
	return a.Known.ID
}

// foldAll is a decoded JSON list of strings, folded.
func foldAll(v any) any {
	out := []any{}
	for _, s := range v.([]any) {
		out = append(out, fold(s.(string)))
	}
	return out
}

// baseline is the iOS registry's baseline as the parity reads it: its generic
// names, iOS PR 1's parity corpus and known siblings with each matcher's
// answer at iOS 3145606a, and its rows in the registry's shape.
type baseline struct {
	GenericNames                []string
	ParityCorpus, KnownSiblings []corpusName
	Rows                        []map[string]any
}

// readBaseline is testdata/ios-registry-3145606a.json, refused unless its
// SHA-256 is the dispatch's.
func readBaseline(t *testing.T) baseline {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ios-registry-3145606a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != baselineSHA256 {
		t.Fatalf("the baseline's SHA-256 is %s, not the dispatch's %s", got, baselineSHA256)
	}
	var base baseline
	if err := json.Unmarshal(b, &base); err != nil {
		t.Fatal(err)
	}
	return base
}

// declared is what the catalog holds past the baseline (B944): its rows in
// the export's own shape, in order after the baseline's; the names their
// units reported; and the known siblings they bring, after the baseline's,
// each name with the answers both matchers must give it.
type declared struct {
	rows            []row
	names, siblings []corpusName
}

// additions are the catalog's rows, names and known siblings past the
// baseline, declared in this file by hand from each row's submission and its
// maker's figures, never from the catalog or the export, so the export is
// held to them as it is to the baseline.
var additions = declared{
	rows: []row{{
		// The Fosi Audio ZH3 (B944), from its wizard submission of 2026-11-19
		// (app 2.2 (17), iOS 27.3) by the iOS procedure's rules, and its
		// maker's page read that day: DSD64 heard after a click; DSD128 and
		// DSD256 held the lock proxy with the listening not sure, neither
		// heard nor failed; DSD512, never attempted, its 1411.2 kHz carrier
		// asked in the PCM sweep and 768 kHz negotiated, refused; every PCM
		// rate to 768 kHz negotiated exactly, the submitter's 32 bits the
		// maker's USB figure too; its own volume control, which the iOS slider
		// does not move, and DoP held below iOS maximum with the tone heard,
		// so the volume unknown; no time a rung measured clean; and the
		// maker's DSD512, which names no transport.
		ID: "fosiaudio.zh3", Name: "Fosi Audio ZH3", Maker: "Fosi Audio", LegacyPatterns: []string{"Fosi Audio ZH3"},
		LegacyCeiling: "dsd64", PCM: dacprofile.IOSPCM{MaxRateHz: 768000, BitDepth: 32}, VolumeTopology: "unknown",
		NameKeys: nameKeys{PortNames: []string{"Fosi Audio ZH3"}, Tokens: []string{"Fosi Audio ZH3"}, ExcludedTokens: []string{}, Scope: "model"},
		Evidence: evidence{
			Heard:     []heard{{Rate: "dsd64", Source: source{Kind: "submission", Date: ptr("2026-11-19")}}},
			Failed:    []failed{{Rate: "dsd512", Failure: "carrierRefused", Source: source{Kind: "submission", Date: ptr("2026-11-19")}}},
			Statement: statement{Case: "dsdWithoutDoPStatement", UpTo: ptr("dsd512"), Source: &source{Kind: "maker", Date: ptr("2026-11-19")}},
		},
	}},
	// The name iOS reported for the ZH3's port, verbatim, with a trailing
	// space and in two other cases.
	names: []corpusName{
		{Name: "Fosi Audio ZH3", Live: ptr("fosiaudio.zh3"), Strict: known("fosiaudio.zh3", "exact")},
		{Name: "Fosi Audio ZH3 ", Live: ptr("fosiaudio.zh3"), Strict: known("fosiaudio.zh3", "exact")},
		{Name: "fosi audio zh3", Live: ptr("fosiaudio.zh3"), Strict: known("fosiaudio.zh3", "token")},
		{Name: "FOSI AUDIO ZH3", Live: ptr("fosiaudio.zh3"), Strict: known("fosiaudio.zh3", "token")},
	},
	// Fosi Audio's products that take USB audio from a host, as its store
	// listed them on 2026-11-19, each named as the ZH3 names itself: no row
	// may reach one, by either matcher.
	siblings: noMatch("Fosi Audio ZD3", "Fosi Audio K7", "Fosi Audio DS1", "Fosi Audio DS2", "Fosi Audio SK02",
		"Fosi Audio GR40", "Fosi Audio ZH1", "Fosi Audio Q6", "Fosi Audio DS3", "Fosi Audio MD3", "Fosi Audio K5 Pro",
		"Fosi Audio Q4", "Fosi Audio C1", "Fosi Audio C3", "Fosi Audio MC331", "Fosi Audio MC351"),
}

// ptr is a pointer to v.
func ptr[T any](v T) *T { return &v }

// known is the strict matcher's answer naming the model row id, by tier.
func known(id, tier string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"known":{"id":%q,"scope":"model","tier":%q}}`, id, tier))
}

// noMatch are names neither matcher gives a row.
func noMatch(names ...string) []corpusName {
	var cs []corpusName
	for _, n := range names {
		cs = append(cs, corpusName{Name: n, Strict: json.RawMessage(`{"unknown":"noMatch"}`)})
	}
	return cs
}

// asMap is v encoded and decoded again, as a row of the export decodes.
func asMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		panic(err)
	}
	return m
}

// parity is every way an export and the catalog's known siblings differ from
// the baseline and the declarations past it (add), a sentence each: the
// generic names; the known siblings, the baseline's in its order and then
// the declared ones; the rows, the baseline's at their places and field by
// field, the legacy patterns, the tokens and the excluded tokens compared
// after iOS's fold and the times in whole milliseconds, then each declared
// row field by field as declared, and no row past them; for every name of the
// baseline's corpus and known siblings and of the declarations, the live
// matcher's first hit over the export's legacy patterns and the strict
// matcher's answer over its keys, both over the whole export, the rows added
// past the baseline included, so a regression they bring is seen; for names
// two rows' patterns hold, the first hit the baseline's rows give; and the
// corpus names the two matchers answer apart, exactly the four iOS PR 1
// declares and never a declared name: an added row's names answer the same
// through both matchers (namesApart asks both to name its row), so a declared
// name the two answer apart is refused, and so is one whose Exception says
// they do.
func parity(base baseline, add declared, exported []byte, siblings []string) []string {
	var problems []string
	say := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	var x export
	var rows struct{ Rows []map[string]any }
	if err := errors.Join(json.Unmarshal(exported, &x), json.Unmarshal(exported, &rows)); err != nil {
		return []string{err.Error()}
	}
	if !slices.Equal(x.GenericNames, base.GenericNames) {
		say("the generic names are %q, the baseline's %q", x.GenericNames, base.GenericNames)
	}
	var want []string
	for _, c := range append(slices.Clone(base.KnownSiblings), add.siblings...) {
		want = append(want, c.Name)
	}
	if !slices.Equal(siblings, want) {
		say("the catalog's known siblings are %q, the baseline's and then the declared ones %q", siblings, want)
	}
	if len(rows.Rows) != len(base.Rows)+len(add.rows) {
		say("the export has %d rows, the baseline %d and the declarations %d", len(rows.Rows), len(base.Rows), len(add.rows))
		return problems
	}
	var ids, baseIDs []string
	var patterns, basePatterns [][]string
	for _, r := range x.Rows {
		ids, patterns = append(ids, r.ID), append(patterns, r.LegacyPatterns)
	}
	for i, w := range base.Rows {
		g := rows.Rows[i]
		baseIDs = append(baseIDs, w["id"].(string))
		var ps []string
		for _, p := range w["legacyPatterns"].([]any) {
			ps = append(ps, p.(string))
		}
		basePatterns = append(basePatterns, ps)
		g["order"] = float64(i + 1)
		g["legacyPatterns"] = foldAll(g["legacyPatterns"])
		keys := g["nameKeys"].(map[string]any)
		keys["tokens"], keys["excludedTokens"] = foldAll(keys["tokens"]), foldAll(keys["excludedTokens"])
		g["evidence"].(map[string]any)["hiddenMode"] = g["hiddenMode"]
		for _, f := range slices.Sorted(maps.Keys(w)) {
			if !reflect.DeepEqual(g[f], w[f]) {
				say("row %d (%v), %s: the export's %v, the baseline's %v", i+1, w["id"], f, g[f], w[f])
			}
		}
		if len(g) != len(w) {
			say("row %d (%v): the export holds %d fields, the baseline %d", i+1, w["id"], len(g), len(w))
		}
	}
	for j, d := range add.rows {
		i := len(base.Rows) + j
		g, w := rows.Rows[i], asMap(d)
		for _, f := range slices.Sorted(maps.Keys(w)) {
			if !reflect.DeepEqual(g[f], w[f]) {
				say("row %d (%s), %s: the export's %v, the declaration's %v", i+1, d.ID, f, g[f], w[f])
			}
		}
		if len(g) != len(w) {
			say("row %d (%s): the export holds %d fields, the declaration %d", i+1, d.ID, len(g), len(w))
		}
	}
	replay := func(whose string, cs []corpusName) {
		for _, c := range cs {
			if got := firstHit(c.Name, ids, patterns); got != c.live() {
				say("%q: the export's first hit is %q, %s live answer %q", c.Name, got, whose, c.live())
			}
			if got := strictMatch(c.Name, x); !jsonEqual(got, c.Strict) {
				say("%q: the strict matcher over the export's keys answers %s, %s strict answer %s", c.Name, got, whose, c.Strict)
			}
		}
	}
	replay("the baseline's", append(slices.Clone(base.ParityCorpus), base.KnownSiblings...))
	replay("its declared", append(slices.Clone(add.names), add.siblings...))
	for _, name := range []string{"iFi (by AMR) iFi USB Audio SE", "Hugo 2 Mojo 2 Qutest"} {
		if got, want := firstHit(name, ids, patterns), firstHit(name, baseIDs, basePatterns); got != want {
			say("%q, a name two rows' patterns hold: the export's first hit is %q, the baseline's rows' %q", name, got, want)
		}
	}
	var apart []string
	for _, c := range append(slices.Clone(base.ParityCorpus), add.names...) {
		differ := strictKnown(strictMatch(c.Name, x)) != c.live()
		if differ != c.Exception {
			say("%q: the strict and live answers differ %v, and its declaration says they differ %v", c.Name, differ, c.Exception)
		}
		if differ && !slices.Contains(apart, fold(c.Name)) {
			apart = append(apart, fold(c.Name))
		}
	}
	slices.Sort(apart)
	if want := []string{"brooklyn", "brooklyn dac", "ka3", "topping e30"}; !slices.Equal(apart, want) {
		say("the corpus names the strict and live matchers answer apart are %q, iOS PR 1 declares %q", apart, want)
	}
	return problems
}

// iosLineProblems is every ios line of ps that names its source otherwise than
// the baseline says, a sentence each: a row the baseline holds names its own
// registry row ("ios: DACProfile.swift <id> at iOS <commit>"), and a row added
// past it the dated wizard submission it came from ("ios: wizard submission
// <date>"), never an iOS commit, which holds no such row. The lint takes
// either form on any row, having no baseline, so this boundary is held here
// alone (B944).
func iosLineProblems(base baseline, ps []dacprofile.Profile) []string {
	inBase := map[string]bool{}
	for _, w := range base.Rows {
		inBase[w["id"].(string)] = true
	}
	var problems []string
	for _, p := range ps {
		for _, line := range p.Provenance {
			text, ok := strings.CutPrefix(line, "ios: ")
			if !ok {
				continue
			}
			if cites := strings.HasPrefix(text, "DACProfile.swift "); cites != inBase[p.ID] {
				problems = append(problems, fmt.Sprintf("%s: its ios line is %q, and the baseline holds the row %v: a row of the baseline names its own registry row, a row added past it its submission", p.ID, line, inBase[p.ID]))
			}
		}
	}
	return problems
}

// TestTheExportMatchesTheIOSBaseline: the baseline is the dispatch's, by its
// SHA-256; the export and the catalog's known siblings are the baseline's and
// then the declared additions', every check parity makes; and every ios line
// of the catalog names the source the baseline says (iosLineProblems).
func TestTheExportMatchesTheIOSBaseline(t *testing.T) {
	base := readBaseline(t)
	db, err := dacprofile.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parity(base, additions, fresh(t), db.IOSKnownSiblings) {
		t.Error(p)
	}
	for _, p := range iosLineProblems(base, db.Profiles) {
		t.Error(p)
	}
}

// TestTheBaselineDecidesWhichIOSLineARowNames: iosLineProblems holds the
// boundary the lint cannot, with no baseline, on catalogs the lint takes: a
// row of the baseline (the FiiO KA17) whose ios line is rewritten as a wizard
// submission, its DoP claim moved to the listening, and the row added past it
// (the ZH3) whose ios line cites its registry row at iOS 3145606a are each
// refused, naming the row; each has a counterpart that differs in the form
// alone and passes, the KA17 citing its own row at that commit and the ZH3
// naming a submission on another day.
func TestTheBaselineDecidesWhichIOSLineARowNames(t *testing.T) {
	base := readBaseline(t)
	ios := func(line string) func(p *dacprofile.Profile) {
		return func(p *dacprofile.Profile) {
			for j, l := range p.Provenance {
				if strings.HasPrefix(l, "ios: ") {
					p.Provenance[j] = line
				}
			}
		}
	}
	for _, tc := range []struct {
		name, id string
		edit     func(p *dacprofile.Profile)
		want     string // the problem's words, or "" for none
	}{
		{"a baseline row naming a submission", "fiio.ka17", func(p *dacprofile.Profile) {
			ios("ios: wizard submission 2026-11-02")(p)
			p.DoP.Source = dacprofile.Source{Kind: dacprofile.SourceListening, Ref: "wizard submission 2026-11-02"}
		}, `fiio.ka17: its ios line is "ios: wizard submission 2026-11-02", and the baseline holds the row true`},
		{"a baseline row citing its own row", "fiio.ka17", ios("ios: DACProfile.swift fiio.ka17 at iOS 3145606a"), ""},
		{"an added row citing the registry", "fosiaudio.zh3", ios("ios: DACProfile.swift fosiaudio.zh3 at iOS 3145606a"),
			`fosiaudio.zh3: its ios line is "ios: DACProfile.swift fosiaudio.zh3 at iOS 3145606a", and the baseline holds the row false`},
		{"an added row naming a submission", "fosiaudio.zh3", ios("ios: wizard submission 2026-11-20"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := dacprofile.Default()
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(db.Profiles, func(p dacprofile.Profile) bool { return p.ID == tc.id })
			if i < 0 {
				t.Fatalf("the catalog holds no %s row", tc.id)
			}
			tc.edit(&db.Profiles[i])
			if problems := db.Lint(); len(problems) > 0 {
				t.Fatalf("the edited catalog lints as %v, and each case is one the lint takes", problems)
			}
			problems := iosLineProblems(base, db.Profiles)
			switch {
			case tc.want == "" && len(problems) > 0:
				t.Errorf("iosLineProblems says %q, want nothing", problems)
			case tc.want != "" && !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }):
				t.Errorf("iosLineProblems says %q, want a problem saying %q", problems, tc.want)
			}
		})
	}
}

// TestAnAdditionIsHeldToItsDeclaration: past the baseline the parity takes a
// row and a known sibling only as declared, and replays the baseline's corpus
// over the whole export: a fixture row added to the catalog at the next
// order and declared passes, and each of these is refused: the row exported
// and not declared, declared and not exported, exported otherwise than
// declared, or put before the baseline's rows; a legacy pattern of it that
// takes a corpus name the baseline's live matcher gives no row, which a
// replay over the baseline's rows alone would not see; a known sibling the
// catalog lists and no declaration names, and one declared that it does not
// list; and a declared sibling a row reaches.
func TestAnAdditionIsHeldToItsDeclaration(t *testing.T) {
	base := readBaseline(t)
	order := len(base.Rows) + len(additions.rows) + 1
	added := func() dacprofile.Profile {
		return dacprofile.Profile{ID: "fixture.added", Name: "Fixture Added", Match: []dacprofile.Match{}, Caveats: []dacprofile.Caveat{},
			IOS: &dacprofile.IOS{Order: order, Maker: "Fixture", LegacyPatterns: []string{"Fixture Added"}, PortNames: []string{"FIXTURE ADDED"},
				NameTokens: []string{"Fixture Added"}, ExcludedTokens: []string{}, NameScope: "model",
				PCM: dacprofile.IOSPCM{MaxRateHz: 384000, BitDepth: 32}, VolumeTopology: "unknown"},
			Provenance: []string{"ios: wizard submission 2026-11-19"}}
	}
	addedRow := func() row {
		return row{ID: "fixture.added", Name: "Fixture Added", Maker: "Fixture", LegacyPatterns: []string{"Fixture Added"}, LegacyCeiling: "none",
			PCM: dacprofile.IOSPCM{MaxRateHz: 384000, BitDepth: 32}, VolumeTopology: "unknown",
			NameKeys: nameKeys{PortNames: []string{"FIXTURE ADDED"}, Tokens: []string{"Fixture Added"}, ExcludedTokens: []string{}, Scope: "model"},
			Evidence: evidence{Heard: []heard{}, Failed: []failed{}, Statement: statement{Case: "unrecorded"}}}
	}
	lite := corpusName{Name: "Fixture Added Lite", Strict: json.RawMessage(`{"unknown":"noMatch"}`)}
	check := func(mutate func(db *dacprofile.Database, add *declared)) []string {
		t.Helper()
		db, err := dacprofile.Default()
		if err != nil {
			t.Fatal(err)
		}
		db.Profiles = append(db.Profiles, added())
		add := declared{rows: append(slices.Clone(additions.rows), addedRow()), names: slices.Clone(additions.names),
			siblings: slices.Clone(additions.siblings)}
		if mutate != nil {
			mutate(db, &add)
		}
		exported, err := build(db)
		if err != nil {
			t.Fatal(err)
		}
		return parity(base, add, exported, db.IOSKnownSiblings)
	}
	if problems := check(nil); len(problems) > 0 {
		t.Fatalf("a fixture row declared past the baseline is refused: %q", problems)
	}
	last := func(db *dacprofile.Database) *dacprofile.Profile { return &db.Profiles[len(db.Profiles)-1] }
	for _, tc := range []struct {
		name   string
		mutate func(db *dacprofile.Database, add *declared)
		words  string
	}{
		{"exported and not declared", func(_ *dacprofile.Database, add *declared) { add.rows = add.rows[:len(add.rows)-1] },
			"the export has"},
		{"declared and not exported", func(db *dacprofile.Database, _ *declared) { db.Profiles = db.Profiles[:len(db.Profiles)-1] },
			"the export has"},
		{"exported otherwise than declared", func(db *dacprofile.Database, _ *declared) { last(db).IOS.PCM.MaxRateHz = 768000 },
			"(fixture.added), pcm: the export's"},
		{"put before the baseline's rows", func(db *dacprofile.Database, _ *declared) {
			for _, p := range db.Profiles {
				if p.IOS != nil {
					p.IOS.Order++
				}
			}
			last(db).IOS.Order = 1
		}, "row 1 (chord.hugo2), id: the export's fixture.added"},
		{"a legacy pattern that takes a corpus name", func(db *dacprofile.Database, add *declared) {
			last(db).IOS.LegacyPatterns = []string{"FIIO KA1"}
			add.rows[len(add.rows)-1].LegacyPatterns = []string{"FIIO KA1"}
		}, `"FIIO KA1": the export's first hit is "fixture.added", the baseline's live answer ""`},
		{"a known sibling no declaration names", func(db *dacprofile.Database, _ *declared) {
			db.IOSKnownSiblings = append(db.IOSKnownSiblings, lite.Name)
		}, "the catalog's known siblings are"},
		{"a declared sibling the catalog does not list", func(_ *dacprofile.Database, add *declared) {
			add.siblings = append(add.siblings, lite)
		}, "the catalog's known siblings are"},
		{"a declared sibling a row reaches", func(db *dacprofile.Database, add *declared) {
			db.IOSKnownSiblings = append(db.IOSKnownSiblings, lite.Name)
			add.siblings = append(add.siblings, lite)
		}, `"Fixture Added Lite": the export's first hit is "fixture.added", its declared live answer ""`},
	} {
		if problems := check(tc.mutate); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.words) }) {
			t.Errorf("%s: the parity says %q, want a problem saying %q", tc.name, problems, tc.words)
		}
	}
}

// namesApart is every way the rows declared past the baseline (add) and the
// other rows of an export (x) fail to keep their names apart, a sentence each,
// the corpus (the baseline's parity corpus) and add's declared names giving
// the names either matcher answers with a row, as the iOS procedure's step 6
// asks, both ways: no legacy pattern or token of another row, and no generic
// name, lies inside the added row's names (its port names, patterns and
// tokens, and the names its unit reported), and none of its patterns or tokens
// lies inside another row's names (its port names, patterns and tokens, and
// the corpus names either matcher gives it) or a generic name, all folded,
// each said as the kind of key it is; the live matcher's first hit and the
// strict matcher's answer for each of the added row's names is that row, and
// for each of another row's names never is; and every known sibling declared
// with it is reached by no row: by no legacy pattern, which the live matcher
// takes it by and no excluded token rules out, and by neither matcher.
func namesApart(corpus []corpusName, add declared, x export) []string {
	var problems []string
	say := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	var ids []string
	var patterns [][]string
	for _, r := range x.Rows {
		ids, patterns = append(ids, r.ID), append(patterns, r.LegacyPatterns)
	}
	namesOf := func(r row) []string {
		names := slices.Concat(r.NameKeys.PortNames, r.LegacyPatterns, r.NameKeys.Tokens)
		for _, c := range slices.Concat(corpus, add.names) {
			if c.live() == r.ID || strictKnown(string(c.Strict)) == r.ID {
				names = append(names, c.Name)
			}
		}
		return names
	}
	inside := func(name, key string) bool { return strings.Contains(fold(strings.TrimSpace(name)), fold(key)) }
	for _, d := range add.rows {
		i := slices.IndexFunc(x.Rows, func(r row) bool { return r.ID == d.ID })
		if i < 0 {
			say("%s is declared past the baseline and not in the export", d.ID)
			continue
		}
		a := x.Rows[i]
		own := namesOf(a)
		for _, n := range own {
			if got := firstHit(n, ids, patterns); got != a.ID {
				say("%s: its name %q is the live matcher's %q", a.ID, n, got)
			}
			if got := strictKnown(strictMatch(n, x)); got != a.ID {
				say("%s: its name %q is the strict matcher's %q", a.ID, n, got)
			}
		}
		for _, g := range x.GenericNames {
			for _, n := range own {
				if inside(n, g) {
					say("%s: the generic name %q lies inside its name %q", a.ID, g, n)
				}
			}
			for _, k := range slices.Concat(a.LegacyPatterns, a.NameKeys.Tokens) {
				if inside(g, k) {
					say("%s: its key %q lies inside the generic name %q", a.ID, k, g)
				}
			}
		}
		for _, r := range x.Rows {
			if r.ID == a.ID {
				continue
			}
			for _, way := range [][2]row{{r, a}, {a, r}} {
				from, to := way[0], way[1]
				for _, n := range namesOf(to) {
					for _, k := range from.LegacyPatterns {
						if inside(n, k) {
							say("%s's legacy pattern %q lies inside %s's name %q", from.ID, k, to.ID, n)
						}
					}
					for _, k := range from.NameKeys.Tokens {
						if inside(n, k) {
							say("%s's token %q lies inside %s's name %q", from.ID, k, to.ID, n)
						}
					}
				}
			}
			for _, n := range namesOf(r) {
				if firstHit(n, ids, patterns) == a.ID {
					say("%s's name %q is %s's to the live matcher", r.ID, n, a.ID)
				}
				if strictKnown(strictMatch(n, x)) == a.ID {
					say("%s's name %q is %s's to the strict matcher", r.ID, n, a.ID)
				}
			}
		}
	}
	for _, s := range add.siblings {
		for i, ps := range patterns {
			for _, p := range ps {
				if inside(s.Name, p) {
					say("the sibling %q holds %s's legacy pattern %q, which the live matcher takes it by", s.Name, ids[i], p)
				}
			}
		}
		if got := firstHit(s.Name, ids, patterns); got != "" {
			say("the sibling %q is the live matcher's %q", s.Name, got)
		}
		if got := strictMatch(s.Name, x); !jsonEqual(got, json.RawMessage(`{"unknown":"noMatch"}`)) {
			say("the sibling %q is the strict matcher's %s", s.Name, got)
		}
	}
	return problems
}

// TestAnAddedRowAndEveryOtherRowKeepTheirOwnNames: each row declared past the
// baseline and every other row of the export keep their names apart, both
// ways, every check namesApart makes over the baseline's corpus; and every
// known sibling declared with them is among the catalog's, which its lint
// keeps off every model row.
func TestAnAddedRowAndEveryOtherRowKeepTheirOwnNames(t *testing.T) {
	base := readBaseline(t)
	db, err := dacprofile.Default()
	if err != nil {
		t.Fatal(err)
	}
	var x export
	if err := json.Unmarshal(fresh(t), &x); err != nil {
		t.Fatal(err)
	}
	if len(additions.rows) == 0 {
		t.Fatal("no row is declared past the baseline, so nothing was checked")
	}
	for _, p := range namesApart(base.ParityCorpus, additions, x) {
		t.Error(p)
	}
	for _, s := range additions.siblings {
		if !slices.Contains(db.IOSKnownSiblings, s.Name) {
			t.Errorf("the sibling %q is declared and not among the catalog's known siblings, which its lint keeps off every model row", s.Name)
		}
	}
}

// TestALegacyPatternSharedWithAnAddedRowIsACollision: a row that collides
// with the ZH3 through a legacy pattern alone, which the lint does not compare
// across rows, is refused by namesApart, each case by one diagnostic: a
// fixture row (fixture.shared, its token Fixture Shared and port name FIXTURE
// SHARED, which nothing else holds) added to the export's rows only, never
// declared, after the ZH3 with the pattern "Audio ZH3", which lies inside the
// ZH3's names, or "Fosi Audio ZH3 Pro", inside which the ZH3's pattern lies,
// and before the ZH3 with "Audio ZH3", whose first hit then takes the ZH3's
// port; each beside a counterpart whose pattern misses by a character (ZH4)
// and which namesApart passes.
func TestALegacyPatternSharedWithAnAddedRowIsACollision(t *testing.T) {
	base := readBaseline(t)
	var x export
	if err := json.Unmarshal(fresh(t), &x); err != nil {
		t.Fatal(err)
	}
	zh3 := slices.IndexFunc(x.Rows, func(r row) bool { return r.ID == "fosiaudio.zh3" })
	if zh3 < 0 {
		t.Fatal("the export holds no fosiaudio.zh3 row")
	}
	shared := func(pattern string, before bool) export {
		s := row{ID: "fixture.shared", Name: "Fixture Shared", Maker: "Fixture", LegacyPatterns: []string{pattern}, LegacyCeiling: "none",
			NameKeys: nameKeys{PortNames: []string{"FIXTURE SHARED"}, Tokens: []string{"Fixture Shared"}, ExcludedTokens: []string{}, Scope: "model"}}
		at := len(x.Rows)
		if before {
			at = zh3
		}
		y := x
		y.Rows = slices.Insert(slices.Clone(x.Rows), at, s)
		return y
	}
	for _, tc := range []struct {
		name    string
		pattern string
		before  bool
		want    string // the one diagnostic the case asserts, or "" for none at all
	}{
		{"its pattern inside the ZH3's names", "Audio ZH3", false,
			`fixture.shared's legacy pattern "Audio ZH3" lies inside fosiaudio.zh3's name "Fosi Audio ZH3"`},
		{"its pattern a character off the ZH3's names", "Audio ZH4", false, ""},
		{"the ZH3's pattern inside its names", "Fosi Audio ZH3 Pro", false,
			`fosiaudio.zh3's legacy pattern "Fosi Audio ZH3" lies inside fixture.shared's name "Fosi Audio ZH3 Pro"`},
		{"the ZH3's pattern a character off its names", "Fosi Audio ZH4 Pro", false, ""},
		{"an earlier row's pattern taking the ZH3's port", "Audio ZH3", true,
			`fosiaudio.zh3: its name "Fosi Audio ZH3" is the live matcher's "fixture.shared"`},
		{"an earlier row's pattern a character off the ZH3's port", "Audio ZH4", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := namesApart(base.ParityCorpus, additions, shared(tc.pattern, tc.before))
			switch {
			case tc.want == "" && len(problems) > 0:
				t.Errorf("namesApart says %q, want nothing", problems)
			case tc.want != "" && !slices.Contains(problems, tc.want):
				t.Errorf("namesApart says %q, want %q among them", problems, tc.want)
			}
		})
	}
}

func jsonEqual(a string, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
