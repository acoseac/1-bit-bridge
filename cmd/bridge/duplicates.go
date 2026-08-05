// `bridge duplicates` — a READ-ONLY report of duplicate track groups,
// grouped by the iOS client's own collapse identity (internal/dupes).
//
// This command never deletes, moves, or rewrites anything, and by design
// has no subaction and NO MUTATING FLAG — so there is no place a
// `--delete` can later be added by symmetry (pinned by
// TestDuplicatesHasNoMutatingFlag). The library is read-only to the
// bridge; acting on the report happens at the storage source.
//
// Unlike `bridge enrichment` there is no admin-API preference and no
// probeBridge tri-state: that machinery exists because `enrichment retry`
// WRITES. This is a pure read, so it always reads the store cold —
// which also makes it work against a public-mode bridge whose admin API
// the CLI can't reach.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// duplicatesJSONSchemaVersion identifies the --json report shape. CLI-only
// (the doctorJSONSchemaVersion precedent) — bump it on a shape change,
// never internal/version.ProtocolVersion.
const duplicatesJSONSchemaVersion = 1

// dupeTierOrder is the display order: the different-masters tier prints
// FIRST, with its preamble, so the least-redundant tier can't be skimmed
// as deletion fodder; the self-nested section renders separately at the
// end so a bad upload isn't read as hundreds of real duplicate albums.
var dupeTierOrder = []dupes.Tier{
	dupes.TierDifferentFormat, dupes.TierSameFormat, dupes.TierInconclusive,
}

type duplicatesOpts struct {
	configPath    string
	pathScope     string
	tier          string
	limit         int
	asJSON        bool
	nestedOnly    bool
	includeRouted bool
}

// duplicatesFlagSet is factored out so the no-mutating-flag guard test
// can walk every registered flag.
func duplicatesFlagSet(stderr io.Writer) (*flag.FlagSet, *duplicatesOpts) {
	o := &duplicatesOpts{}
	fs := flag.NewFlagSet("duplicates", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.configPath, "config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
	fs.StringVar(&o.pathScope, "path", "", "restrict to a library subtree (default: whole library)")
	fs.StringVar(&o.tier, "tier", "", "narrow to one tier: different-format | same-format | inconclusive | self-nested")
	fs.IntVar(&o.limit, "limit", 50, "maximum groups to print per tier (0 = counts only)")
	fs.BoolVar(&o.asJSON, "json", false, "emit JSON instead of a human summary")
	fs.BoolVar(&o.nestedOnly, "nested-only", false, "print only the self-nested (upload accident) section")
	fs.BoolVar(&o.includeRouted, "include-routed", false, "include UPnP-routed upstream tracks (excluded by default)")
	return fs, o
}

func duplicatesCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs, o := duplicatesFlagSet(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.tier != "" && !validDupeTierName(o.tier) {
		fmt.Fprintf(stderr, "duplicates: --tier must be one of different-format, same-format, inconclusive, self-nested (got %q)\n", o.tier)
		return 2
	}
	if o.limit < 0 {
		fmt.Fprintln(stderr, "duplicates: --limit must not be negative")
		return 2
	}
	cfg, _, err := loadCLIConfig(o.configPath)
	if err != nil {
		fmt.Fprintf(stderr, "duplicates: load config: %v\n", err)
		return 2
	}
	report, err := collectDuplicates(ctx, cfg, o)
	if err != nil {
		fmt.Fprintf(stderr, "duplicates: %v\n", err)
		return 1
	}
	if o.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "duplicates: encode: %v\n", err)
			return 1
		}
		return 0
	}
	printDupeReport(stdout, report, o)
	return 0
}

func validDupeTierName(s string) bool {
	switch dupes.Tier(s) {
	case dupes.TierDifferentFormat, dupes.TierSameFormat, dupes.TierInconclusive, dupes.TierSelfNested:
		return true
	}
	return false
}

// --- report shapes (CLI-local DTOs — never dupes.Group on the encoder) ---

type dupeReport struct {
	SchemaVersion int              `json:"schemaVersion"`
	Path          string           `json:"path"`
	Scanned       int              `json:"scanned"`
	GroupsTotal   int              `json:"groupsTotal"`
	Tiers         []dupeTierReport `json:"tiers"`
}

type dupeTierReport struct {
	Tier   string `json:"tier"`
	Groups int    `json:"groups"`
	// RedundantFiles counts every member beyond one per group; for the
	// different-format tier these are DIFFERENT MASTERS, not redundancy —
	// the human renderer says so, and NonLargestBytes is deliberately not
	// named "wasted" anywhere.
	RedundantFiles   int             `json:"redundantFiles"`
	NonLargestBytes  int64           `json:"bytesInNonLargestCopies"`
	Samples          []dupeGroupJSON `json:"groupSamples,omitempty"`
	SamplesTruncated bool            `json:"groupSamplesTruncated,omitempty"`
}

type dupeGroupJSON struct {
	AlbumID   string           `json:"albumID"`
	Disc      int              `json:"disc"`
	Track     int              `json:"track"`
	NormTitle string           `json:"normTitle"`
	Members   []dupeMemberJSON `json:"members"`
}

type dupeMemberJSON struct {
	Path          string  `json:"path"`
	Codec         string  `json:"codec,omitempty"`
	SampleRate    int     `json:"sampleRate,omitempty"`
	BitsPerSample int     `json:"bitsPerSample,omitempty"`
	IsDSD         bool    `json:"isDSD,omitempty"`
	SizeBytes     int64   `json:"sizeBytes"`
	DurationSec   float64 `json:"durationSec,omitempty"`
	NestDepth     int     `json:"nestDepth,omitempty"`
}

// collectDuplicates walks the store twice through the two-pass collector
// (pass 1 keys only, pass 2 members for keys seen twice — the streaming
// OOM discipline) and builds the CLI report.
func collectDuplicates(ctx context.Context, cfg *config.Config, o *duplicatesOpts) (*dupeReport, error) {
	store, err := openManifestStore(cfg)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	c := dupes.NewCollector()
	if err := store.StreamTrackDupeRefsUnderPrefix(ctx, o.pathScope, o.includeRouted, func(r dupes.Row) error {
		c.Note(r)
		return nil
	}); err != nil {
		return nil, err
	}
	c.Seal()
	if err := store.StreamTrackDupeRefsUnderPrefix(ctx, o.pathScope, o.includeRouted, func(r dupes.Row) error {
		c.Collect(r)
		return nil
	}); err != nil {
		return nil, err
	}
	return buildDupeReport(o.pathScope, c.Observed(), c.Groups(), o.limit), nil
}

func buildDupeReport(scope string, scanned int, groups []dupes.Group, limit int) *dupeReport {
	rep := &dupeReport{
		SchemaVersion: duplicatesJSONSchemaVersion,
		Path:          scope,
		Scanned:       scanned,
		GroupsTotal:   len(groups),
	}
	byTier := map[dupes.Tier][]dupes.Group{}
	for _, g := range groups {
		byTier[g.Tier] = append(byTier[g.Tier], g)
	}
	order := append(append([]dupes.Tier{}, dupeTierOrder...), dupes.TierSelfNested)
	for _, tier := range order {
		tg := byTier[tier]
		tr := dupeTierReport{Tier: string(tier)}
		for _, g := range tg {
			tr.Groups++
			tr.RedundantFiles += len(g.Members) - 1
			tr.NonLargestBytes += g.RedundantBytes()
		}
		for i, g := range tg {
			if limit > 0 && i >= limit {
				tr.SamplesTruncated = true
				break
			}
			tr.Samples = append(tr.Samples, dupeGroupToJSON(g))
		}
		rep.Tiers = append(rep.Tiers, tr)
	}
	return rep
}

func dupeGroupToJSON(g dupes.Group) dupeGroupJSON {
	out := dupeGroupJSON{
		AlbumID: g.Key.AlbumID, Disc: g.Key.Disc, Track: g.Key.Track,
		NormTitle: g.Key.NormTitle,
	}
	for _, m := range g.Members {
		out.Members = append(out.Members, dupeMemberJSON{
			Path: m.Path, Codec: m.Codec, SampleRate: m.SampleRate,
			BitsPerSample: m.BitsPerSample, IsDSD: m.IsDSD,
			SizeBytes: m.Size, DurationSec: m.Duration,
			NestDepth: dupes.SelfNestDepth(m.Path),
		})
	}
	return out
}

// --- human rendering ---

func printDupeReport(w io.Writer, rep *dupeReport, o *duplicatesOpts) {
	scope := rep.Path
	if scope == "" {
		scope = "(whole library)"
	}
	fmt.Fprintf(w, "Duplicate groups — %s\n", scope)
	fmt.Fprintln(w, "  This report never deletes or moves anything — the bridge has no code")
	fmt.Fprintln(w, "  path that modifies library files.")
	fmt.Fprintf(w, "\n  scanned %d tracks · %d groups\n", rep.Scanned, rep.GroupsTotal)

	for _, tr := range rep.Tiers {
		if o.nestedOnly && tr.Tier != string(dupes.TierSelfNested) {
			continue
		}
		if o.tier != "" && tr.Tier != o.tier {
			continue
		}
		if tr.Groups == 0 && (o.tier == "" && !o.nestedOnly) {
			continue
		}
		printDupeTier(w, tr, o.limit)
	}
}

func printDupeTier(w io.Writer, tr dupeTierReport, limit int) {
	fmt.Fprintf(w, "\n  %s — %d groups · %d non-primary files · %s in non-largest copies\n",
		tr.Tier, tr.Groups, tr.RedundantFiles, humanBytes(tr.NonLargestBytes))
	switch tr.Tier {
	case string(dupes.TierDifferentFormat):
		fmt.Fprintln(w, "  Different sample rates, bit depths or codecs mean DIFFERENT MASTERS —")
		fmt.Fprintln(w, "  these are not redundant copies.")
	case string(dupes.TierInconclusive):
		fmt.Fprintln(w, "  Durations disagree, geometry is unknown, or version markers differ —")
		fmt.Fprintln(w, "  treat these as distinct recordings until proven otherwise.")
	case string(dupes.TierSelfNested):
		fmt.Fprintln(w, "  The same file at multiple self-nesting depths (an upload accident,")
		fmt.Fprintln(w, "  e.g. CD 01/CD 01/CD 01/…) — fix at the storage source.")
	}
	if limit <= 0 {
		return
	}
	for _, g := range tr.Samples {
		fmt.Fprintf(w, "    %s · disc %d · track %d · %q\n", g.AlbumID, g.Disc, g.Track, g.NormTitle)
		for _, m := range g.Members {
			fmt.Fprintf(w, "      %-24s %s\n", memberGeometry(m), m.Path)
		}
	}
	if tr.SamplesTruncated {
		fmt.Fprintf(w, "    … (%d of %d groups shown; raise --limit or use --json for all)\n",
			len(tr.Samples), tr.Groups)
	}
}

// memberGeometry renders "FLAC 96000/24 · 41:23 · 1.4 GiB" so a
// different-format group's members can never be mistaken for identical
// copies.
func memberGeometry(m dupeMemberJSON) string {
	var b strings.Builder
	if m.Codec != "" {
		b.WriteString(m.Codec)
	} else {
		b.WriteString("unknown")
	}
	if m.SampleRate > 0 && m.BitsPerSample > 0 {
		fmt.Fprintf(&b, " %d/%d", m.SampleRate, m.BitsPerSample)
	}
	if m.IsDSD {
		b.WriteString(" DSD")
	}
	if m.DurationSec > 0 {
		fmt.Fprintf(&b, " · %d:%02d", int(m.DurationSec)/60, int(m.DurationSec)%60)
	}
	fmt.Fprintf(&b, " · %s", humanBytes(m.SizeBytes))
	return b.String()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
