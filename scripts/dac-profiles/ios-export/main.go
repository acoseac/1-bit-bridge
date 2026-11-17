// Command ios-export writes the DAC catalog's iOS projection,
// DACCatalog.ios.json, beside its source (B730 B1): the rows of
// internal/player/dacprofile's embedded database that carry one, in the
// registry's order (ios.order), each with what the iOS app reads and nothing
// only the bridge reads (no USB ID, product string, native claim, bridge
// timing or control). iOS PR 2 bundles the file and pins its SHA-256 and the
// bridge commit it came from, so a catalog change is a bridge PR, then a
// re-pin.
//
// Run it from the repository root; -check writes nothing and exits 1 when
// the file is not what the catalog exports now. Both print the file's
// SHA-256. The export is deterministic: rows by ios.order, heard and failed
// entries by rate, every other list as the catalog writes it, two spaces of
// indent, no HTML escaping, LF and one trailing newline.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
)

// exportVersion is the shape iOS PR 2's decoder reads.
const exportVersion = 1

// defaultFile is the export's path from the repository root.
const defaultFile = "scripts/dac-profiles/ios-export/DACCatalog.ios.json"

type export struct {
	ExportVersion int      `json:"exportVersion"`
	GenericNames  []string `json:"genericNames"`
	Rows          []row    `json:"rows"`
}

// row is a registry row as iOS spells it: its legacy ceiling none or dsd64
// to dsd512, its times in whole milliseconds or null.
type row struct {
	ID             string               `json:"id"`
	Name           string               `json:"name"`
	Maker          string               `json:"maker"`
	LegacyPatterns []string             `json:"legacyPatterns"`
	LegacyCeiling  string               `json:"legacyCeiling"`
	PCM            dacprofile.IOSPCM    `json:"pcm"`
	VolumeTopology string               `json:"volumeTopology"`
	Timing         dacprofile.IOSTiming `json:"timing"`
	HiddenMode     bool                 `json:"hiddenMode"`
	NameKeys       nameKeys             `json:"nameKeys"`
	Evidence       evidence             `json:"evidence"`
}

type nameKeys struct {
	PortNames      []string `json:"portNames"`
	Tokens         []string `json:"tokens"`
	ExcludedTokens []string `json:"excludedTokens"`
	Scope          string   `json:"scope"`
}

// evidence is iOS PR 1's DoPEvidence: each heard and failed rate with its
// source, the statement (unrecorded where the catalog holds none), and a
// family row's sibling note.
type evidence struct {
	Heard       []heard   `json:"heard"`
	Failed      []failed  `json:"failed"`
	Statement   statement `json:"statement"`
	SiblingNote *string   `json:"siblingNote"`
}

// source is a clause's head: its kind as iOS spells it (fieldLog) and its
// date, null where the head names none.
type source struct {
	Kind string  `json:"kind"`
	Date *string `json:"date"`
}

type heard struct {
	Rate   string `json:"rate"`
	Source source `json:"source"`
}

type failed struct {
	Rate    string `json:"rate"`
	Failure string `json:"failure"`
	Source  source `json:"source"`
}

type statement struct {
	Case   string  `json:"case"`
	UpTo   *string `json:"upTo"`
	Source *source `json:"source"`
}

// The iOS words for the catalog's failure and statement markers.
var (
	failures   = map[string]string{"refused": "carrierRefused", "silence at": "heardSilence", "noise at": "heardNoise", "no frames at": "noLockProxy"}
	statements = map[string]string{"stated DoP to": "dop", "stated DSD to": "dsdWithoutDoPStatement", "stated no DSD": "noDSD"}
)

func main() {
	check := flag.Bool("check", false, "compare the file with a fresh export, write nothing, and exit 1 when they differ")
	file := flag.String("file", defaultFile, "the export's path")
	flag.Parse()
	if err := run(*file, *check, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ios-export:", err)
		os.Exit(1)
	}
}

// errDiffers is -check's answer for a file that is not a fresh export.
var errDiffers = errors.New("the file is not a fresh export of the catalog: run ios-export without -check to write it")

// run exports the embedded catalog and writes it to file, or with check
// compares it with file and writes nothing, printing the file's SHA-256.
func run(file string, check bool, w io.Writer) error {
	db, err := dacprofile.Default()
	if err != nil {
		return err
	}
	fresh, err := build(db)
	if err != nil {
		return err
	}
	if !check {
		if err := os.WriteFile(file, fresh, 0o644); err != nil {
			return err
		}
		_, err := fmt.Fprintf(w, "%s: written, sha256 %x\n", file, sha256.Sum256(fresh))
		return err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: sha256 %x\n", file, sha256.Sum256(b))
	if !bytes.Equal(b, fresh) {
		return errDiffers
	}
	_, err = fmt.Fprintln(w, "the same as a fresh export of the catalog")
	return err
}

// build is db's iOS projection, encoded.
func build(db *dacprofile.Database) ([]byte, error) {
	out := export{ExportVersion: exportVersion, GenericNames: []string{}, Rows: []row{}}
	for _, g := range db.IOSGenericNames {
		out.GenericNames = append(out.GenericNames, g.Name)
	}
	var ps []dacprofile.Profile
	for _, p := range db.Profiles {
		if p.IOS != nil {
			ps = append(ps, p)
		}
	}
	slices.SortStableFunc(ps, func(a, b dacprofile.Profile) int { return a.IOS.Order - b.IOS.Order })
	for _, p := range ps {
		r, err := rowOf(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.ID, err)
		}
		out.Rows = append(out.Rows, r)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// rowOf is p's row: its iOS projection, and its DoP evidence read from its
// clauses, each entry's source the head of the one clause that credits it.
func rowOf(p dacprofile.Profile) (row, error) {
	x := p.IOS
	r := row{ID: p.ID, Name: p.Name, Maker: x.Maker, LegacyPatterns: list(x.LegacyPatterns), LegacyCeiling: "none",
		PCM: x.PCM, VolumeTopology: x.VolumeTopology, Timing: x.Timing,
		HiddenMode: slices.ContainsFunc(p.Caveats, func(c dacprofile.Caveat) bool { return c.Code == dacprofile.CaveatDoPNoiseInHiddenMode }),
		NameKeys:   nameKeys{PortNames: list(x.PortNames), Tokens: list(x.NameTokens), ExcludedTokens: list(x.ExcludedTokens), Scope: x.NameScope},
		Evidence:   evidence{Heard: []heard{}, Failed: []failed{}, Statement: statement{Case: "unrecorded"}, SiblingNote: x.SiblingNote}}
	if x.LegacyCeilingBitRate != 0 {
		r.LegacyCeiling = rateName(x.LegacyCeilingBitRate)
	}
	if p.DoP == nil {
		return r, nil
	}
	cs := p.Credits()
	for _, rate := range slices.Sorted(slices.Values(p.DoP.HeardBitRates)) {
		c, ok := creditOf(cs, rate, func(m string) bool { return m == "heard" })
		if !ok {
			return row{}, fmt.Errorf("no clause credits %s as heard", rateName(rate))
		}
		r.Evidence.Heard = append(r.Evidence.Heard, heard{Rate: rateName(rate), Source: sourceOf(c)})
	}
	for _, rate := range slices.Sorted(slices.Values(p.DoP.RefusedBitRates)) {
		c, ok := creditOf(cs, rate, func(m string) bool { return failures[m] != "" })
		if !ok {
			return row{}, fmt.Errorf("no clause credits %s as failed", rateName(rate))
		}
		r.Evidence.Failed = append(r.Evidence.Failed, failed{Rate: rateName(rate), Failure: failures[c.Marker], Source: sourceOf(c)})
	}
	for _, c := range cs {
		if kind, ok := statements[c.Marker]; ok {
			s := sourceOf(c)
			r.Evidence.Statement = statement{Case: kind, Source: &s}
			if len(c.BitRates) == 1 {
				up := rateName(c.BitRates[0])
				r.Evidence.Statement.UpTo = &up
			}
		}
	}
	return r, nil
}

// creditOf is the clause of cs whose marker is one is wants that credits rate.
func creditOf(cs []dacprofile.Credit, rate int, is func(marker string) bool) (dacprofile.Credit, bool) {
	for _, c := range cs {
		if is(c.Marker) && slices.Contains(c.BitRates, rate) {
			return c, true
		}
	}
	return dacprofile.Credit{}, false
}

// sourceOf is c's head as iOS spells it: its kind's words joined in lower
// camel case (field log is fieldLog), and its date, null where it has none.
func sourceOf(c dacprofile.Credit) source {
	words := strings.Fields(c.Kind)
	for i := 1; i < len(words); i++ {
		words[i] = strings.ToUpper(words[i][:1]) + words[i][1:]
	}
	s := source{Kind: strings.Join(words, "")}
	if c.Date != "" {
		s.Date = &c.Date
	}
	return s
}

// rateName is a DSD rate as iOS names it (dsd128).
func rateName(rate int) string { return fmt.Sprintf("dsd%d", rate/44100) }

// list is s, or an empty list where s is nil, so the export writes [].
func list(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
