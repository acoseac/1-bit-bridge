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
// gave (testdata/ios-registry-6a1241eb.json), a dump of the live registry's
// objects at iOS 6a1241eb: an input, never derived from the catalog.
const baselineSHA256 = "16f75f690368a46c50aa757d677da1c6bc192eeb4970d6b7c0c7b2fadb37a52f"

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
// siblings, with the live matcher's answer at iOS 6a1241eb (null for none),
// the strict matcher's, and whether iOS PR 1 declares the two apart.
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

// TestTheExportMatchesTheIOSBaseline: the baseline is the dispatch's, by its
// SHA-256; the export's rows are the baseline's, in its order and field by
// field, the legacy patterns, the tokens and the excluded tokens compared
// after iOS's fold, the times in whole milliseconds; its generic names are
// the baseline's, and so are the catalog's known siblings, which its lint
// keeps off every model row; for every name of the corpus and the known
// siblings, the live matcher's first hit over the export's legacy patterns is
// the baseline's, with no exception, and so it is for names two rows'
// patterns hold, by the baseline's rows; and the strict matcher over the
// export's keys gives each name the baseline's strict answer, which differs
// from the live one in the corpus exactly for the four names iOS PR 1
// declares.
func TestTheExportMatchesTheIOSBaseline(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "ios-registry-6a1241eb.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != baselineSHA256 {
		t.Fatalf("the baseline's SHA-256 is %s, not the dispatch's %s", got, baselineSHA256)
	}
	var base struct {
		GenericNames                []string
		ParityCorpus, KnownSiblings []corpusName
		Rows                        []map[string]any
	}
	if err := json.Unmarshal(b, &base); err != nil {
		t.Fatal(err)
	}
	exported := fresh(t)
	var x export
	var rows struct{ Rows []map[string]any }
	if err := errors.Join(json.Unmarshal(exported, &x), json.Unmarshal(exported, &rows)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(x.GenericNames, base.GenericNames) {
		t.Errorf("the generic names are %q, the baseline's %q", x.GenericNames, base.GenericNames)
	}
	db, err := dacprofile.Default()
	if err != nil {
		t.Fatal(err)
	}
	var siblings []string
	for _, c := range base.KnownSiblings {
		siblings = append(siblings, c.Name)
	}
	if !slices.Equal(db.IOSKnownSiblings, siblings) {
		t.Errorf("the catalog's known siblings are %q, the baseline's %q", db.IOSKnownSiblings, siblings)
	}
	if len(rows.Rows) != len(base.Rows) {
		t.Fatalf("the export has %d rows, the baseline %d", len(rows.Rows), len(base.Rows))
	}
	var ids, baseIDs []string
	var patterns, basePatterns [][]string
	for i, w := range base.Rows {
		g := rows.Rows[i]
		ids, baseIDs = append(ids, x.Rows[i].ID), append(baseIDs, w["id"].(string))
		var ps []string
		for _, p := range w["legacyPatterns"].([]any) {
			ps = append(ps, p.(string))
		}
		patterns, basePatterns = append(patterns, x.Rows[i].LegacyPatterns), append(basePatterns, ps)
		g["order"] = float64(i + 1)
		g["legacyPatterns"] = foldAll(g["legacyPatterns"])
		keys := g["nameKeys"].(map[string]any)
		keys["tokens"], keys["excludedTokens"] = foldAll(keys["tokens"]), foldAll(keys["excludedTokens"])
		g["evidence"].(map[string]any)["hiddenMode"] = g["hiddenMode"]
		for _, f := range slices.Sorted(maps.Keys(w)) {
			if !reflect.DeepEqual(g[f], w[f]) {
				t.Errorf("row %d (%v), %s: the export's %v, the baseline's %v", i+1, w["id"], f, g[f], w[f])
			}
		}
		if len(g) != len(w) {
			t.Errorf("row %d (%v): the export holds %d fields, the baseline %d", i+1, w["id"], len(g), len(w))
		}
	}
	for _, c := range append(slices.Clone(base.ParityCorpus), base.KnownSiblings...) {
		if got := firstHit(c.Name, ids, patterns); got != c.live() {
			t.Errorf("%q: the export's first hit is %q, the baseline's live matcher's %q", c.Name, got, c.live())
		}
		if got := strictMatch(c.Name, x); !jsonEqual(got, c.Strict) {
			t.Errorf("%q: the strict matcher over the export's keys answers %s, the baseline %s", c.Name, got, c.Strict)
		}
	}
	for _, name := range []string{"iFi (by AMR) iFi USB Audio SE", "Hugo 2 Mojo 2 Qutest"} {
		if got, want := firstHit(name, ids, patterns), firstHit(name, baseIDs, basePatterns); got != want {
			t.Errorf("%q, a name two rows' patterns hold: the export's first hit is %q, the baseline's rows' %q", name, got, want)
		}
	}
	var apart []string
	for _, c := range base.ParityCorpus {
		differ := strictKnown(strictMatch(c.Name, x)) != c.live()
		if differ != c.Exception {
			t.Errorf("%q: the strict and live answers differ %v, and iOS PR 1 declares them apart %v", c.Name, differ, c.Exception)
		}
		if differ && !slices.Contains(apart, fold(c.Name)) {
			apart = append(apart, fold(c.Name))
		}
	}
	slices.Sort(apart)
	if want := []string{"brooklyn", "brooklyn dac", "ka3", "topping e30"}; !slices.Equal(apart, want) {
		t.Errorf("the corpus names the strict and live matchers answer apart are %q, iOS PR 1 declares %q", apart, want)
	}
}

func jsonEqual(a string, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
