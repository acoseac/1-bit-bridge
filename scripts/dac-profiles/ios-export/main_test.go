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
var additions = declared{}

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
// declares, a declared name apart only where it says so.
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

// TestTheExportMatchesTheIOSBaseline: the baseline is the dispatch's, by its
// SHA-256; the export and the catalog's known siblings are the baseline's and
// then the declared additions', every check parity makes; and a row of the
// baseline names its own registry row in its ios line while a row added past
// it names its dated submission, never an iOS commit that does not hold it.
func TestTheExportMatchesTheIOSBaseline(t *testing.T) {
	base := readBaseline(t)
	db, err := dacprofile.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parity(base, additions, fresh(t), db.IOSKnownSiblings) {
		t.Error(p)
	}
	inBase := map[string]bool{}
	for _, w := range base.Rows {
		inBase[w["id"].(string)] = true
	}
	for _, p := range db.Profiles {
		for _, line := range p.Provenance {
			if text, ok := strings.CutPrefix(line, "ios: "); ok && strings.HasPrefix(text, "DACProfile.swift ") != inBase[p.ID] {
				t.Errorf("%s: its ios line is %q, and the baseline holds the row %v: a row of the baseline names its own registry row, a row added past it its submission", p.ID, line, inBase[p.ID])
			}
		}
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

func jsonEqual(a string, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
