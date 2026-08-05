package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// TestDuplicatesHasNoMutatingFlag walks every registered flag and fails
// on anything that smells like a mutation. `bridge duplicates` is
// report-only BY STRUCTURE: there is no subaction and no flag through
// which a delete/move could later be added "for symmetry". If this test
// is in your way, the answer is a different command, not a new flag here.
func TestDuplicatesHasNoMutatingFlag(t *testing.T) {
	fs, _ := duplicatesFlagSet(io.Discard)
	forbidden := []string{"fix", "delete", "remove", "move", "prune", "apply", "clean", "gc"}
	fs.VisitAll(func(f *flag.Flag) {
		lower := strings.ToLower(f.Name)
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Errorf("flag --%s looks mutating (%q) — bridge duplicates must stay report-only", f.Name, bad)
			}
		}
	})
}

// sampleReport builds a report holding one different-format group (the
// ABBA Voyage shape: 96/24 vs 48/24) and one self-nested chain.
func sampleReport() *dupeReport {
	groups := []dupes.Group{
		{
			Key:  dupes.Key{AlbumID: "abba|voyage|2021", Disc: 1, Track: 1, NormTitle: "i still have faith in you"},
			Tier: dupes.TierDifferentFormat,
			Members: []dupes.Row{
				{Path: "ABBA/Voyage (96k)/01.flac", Codec: "FLAC", SampleRate: 96000, BitsPerSample: 24, Duration: 2483, Size: 1_400_000_000},
				{Path: "ABBA/Voyage (48k)/01.flac", Codec: "FLAC", SampleRate: 48000, BitsPerSample: 24, Duration: 2483, Size: 700_000_000},
			},
		},
		{
			Key:  dupes.Key{AlbumID: "chicago|christmas|", Disc: 1, Track: 1, NormTitle: "jingle"},
			Tier: dupes.TierSelfNested,
			Members: []dupes.Row{
				{Path: "Chicago/CD 01/x.flac", Codec: "FLAC", SampleRate: 44100, BitsPerSample: 16, Duration: 180, Size: 30_000_000},
				{Path: "Chicago/CD 01/CD 01/x.flac", Codec: "FLAC", SampleRate: 44100, BitsPerSample: 16, Duration: 180, Size: 30_000_000},
			},
		},
	}
	return buildDupeReport("", 100, groups, 50, map[string]bool{
		"Chicago/CD 01/CD 01/x.flac": true,
	})
}

// TestPrintDupeReportNeverFramesGeometryDifferenceAsRedundant pins the
// presentation contract from the plan: tier names are evidence claims,
// the different-format preamble says DIFFERENT MASTERS, every member's
// geometry renders inline so the difference cannot be missed, and no
// output anywhere frames bytes as waste or savings.
func TestPrintDupeReportNeverFramesGeometryDifferenceAsRedundant(t *testing.T) {
	var out bytes.Buffer
	printDupeReport(&out, sampleReport(), &duplicatesOpts{limit: 50})
	s := out.String()

	if !strings.Contains(s, "DIFFERENT MASTERS") {
		t.Errorf("different-format preamble missing:\n%s", s)
	}
	if !strings.Contains(s, "96000/24") || !strings.Contains(s, "48000/24") {
		t.Errorf("per-member geometry must render inline:\n%s", s)
	}
	if !strings.Contains(s, "never deletes or moves anything") {
		t.Errorf("fixed read-only header missing:\n%s", s)
	}
	for _, banned := range []string{"wasted", "reclaimable", "savings", "safe to delete"} {
		if strings.Contains(strings.ToLower(s), banned) {
			t.Errorf("output frames duplication as %q — banned vocabulary:\n%s", banned, s)
		}
	}
}

func TestDupeReportJSONCarriesSchemaVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(sampleReport()); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if v, _ := decoded["schemaVersion"].(float64); int(v) != duplicatesJSONSchemaVersion {
		t.Fatalf("schemaVersion = %v, want %d", decoded["schemaVersion"], duplicatesJSONSchemaVersion)
	}
}

func TestPrintDupeReport_NestedOnlyPrintsJustTheNestSection(t *testing.T) {
	var out bytes.Buffer
	printDupeReport(&out, sampleReport(), &duplicatesOpts{limit: 50, nestedOnly: true})
	s := out.String()
	if !strings.Contains(s, string(dupes.TierSelfNested)) {
		t.Errorf("--nested-only must print the self-nested section:\n%s", s)
	}
	if strings.Contains(s, "DIFFERENT MASTERS") {
		t.Errorf("--nested-only must not print other tiers:\n%s", s)
	}
}

// TestDuplicatesCmd_UnknownTierRefused pins the flag validation without
// touching a store.
func TestDuplicatesCmd_UnknownTierRefused(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := duplicatesCmd(context.Background(), []string{"--tier", "bogus"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--tier") {
		t.Fatalf("stderr should name the flag: %s", stderr.String())
	}
}

// TestBuildDupeReport_LimitZeroIsCountsOnly pins the --limit 0 contract
// for the JSON path: counts only, zero samples, truncation marker set
// (pre-fix the guard inverted and dumped every group into the JSON).
func TestBuildDupeReport_LimitZeroIsCountsOnly(t *testing.T) {
	groups := []dupes.Group{{
		Key:  dupes.Key{AlbumID: "a|b|", Track: 1, NormTitle: "x"},
		Tier: dupes.TierSameFormat,
		Members: []dupes.Row{
			{Path: "A/b/x.flac", Codec: "FLAC", SampleRate: 44100, BitsPerSample: 16, Size: 10},
			{Path: "C/b/x.flac", Codec: "FLAC", SampleRate: 44100, BitsPerSample: 16, Size: 9},
		},
	}}
	rep := buildDupeReport("", 2, groups, 0)
	for _, tr := range rep.Tiers {
		if len(tr.Samples) != 0 {
			t.Fatalf("limit 0 must emit no samples, tier %s has %d", tr.Tier, len(tr.Samples))
		}
		if tr.Tier == string(dupes.TierSameFormat) {
			if tr.Groups != 1 || !tr.SamplesTruncated {
				t.Fatalf("counts + truncation marker must survive: %+v", tr)
			}
		}
	}
}
