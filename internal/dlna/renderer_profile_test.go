package dlna

import (
	"strings"
	"testing"
)

func TestMatchProfile(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
		wantID    string
	}{
		// Sony — matched by "Sony" substring
		{"sony_srs_hg1", "Sony SRS-HG1/3.2", "sony"},
		{"sony_with_firmware", "Sony Music Streamer 5.1.0", "sony"},

		// Chord family — matched by "Chord", "2go", "Poly"
		{"chord_brand", "Chord 2go/1.5.7", "chordFamily"},
		{"2go_firmware_string", "2go-firmware/1.6", "chordFamily"},
		{"chord_poly", "Chord Poly/2.1", "chordFamily"},
		{"bare_2go_substring", "DeviceModel-2go-1.0", "chordFamily"},

		// Integra / Onkyo
		{"integra", "Integra DRX-3/1.0", "integraOnkyo"},
		{"onkyo", "Onkyo TX-NR686/1.0", "integraOnkyo"},

		// MPD generic — Chord 2go's actual playback engine identity
		{"mpd_chord_2go_runtime", "Music Player Daemon 0.21.26", "mpdGeneric"},
		{"mpd_different_version", "Music Player Daemon 0.23.5", "mpdGeneric"},

		// Lavf (FFmpeg libavformat — mConnect's metadata probe path)
		{"lavf_mconnect", "Lavf/58.45.100", "lavf"},
		{"lavf_different_version", "Lavf/61.0.0", "lavf"},

		// Unknown catch-all
		{"empty_ua", "", "unknown"},
		{"generic_unrecognized", "GenericPlayer/1.0", "unknown"},
		{"whitespace_only", "   ", "unknown"},
		{"random_string", "some-random-app/0.0.1", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchProfile(tc.userAgent)
			if got.ID != tc.wantID {
				t.Errorf("MatchProfile(%q) returned profile ID %q, want %q",
					tc.userAgent, got.ID, tc.wantID)
			}
		})
	}
}

// TestMatchProfile_branchOrderingSonyWinsOverMPDandLavf is the LOAD-BEARING
// regression guard for the Sony-vs-MPD ordering invariant. Vendor-specific
// overrides MUST be matched before generic playback-engine profiles so a
// Sony renderer that internally uses MPD or libavformat still hits the
// Sony-correct branch (returns `audio/dsd` for DSF, not the generic
// `audio/x-dsf`). A future refactor that reorders `Profiles` to put
// `mpdGeneric` or `lavf` before `sony` will fail this test.
func TestMatchProfile_branchOrderingSonyWinsOverMPDandLavf(t *testing.T) {
	// Hypothetical Sony device that runs MPD internally
	got := MatchProfile("Sony Music Player Daemon 0.21")
	if got.ID != "sony" {
		t.Errorf("Sony+MPD UA matched %q, want sony (vendor override must precede mpdGeneric in Profiles slice)", got.ID)
	}
	if got.PreferredMIME[".dsf"] != "audio/dsd" {
		t.Errorf("Sony profile DSF mime = %q, want audio/dsd", got.PreferredMIME[".dsf"])
	}
	// Hypothetical Sony control point using libavformat
	got = MatchProfile("Sony with embedded Lavf/60")
	if got.ID != "sony" {
		t.Errorf("Sony+Lavf UA matched %q, want sony (vendor override must precede lavf in Profiles slice)", got.ID)
	}
}

// TestProfilesRegistry_unknownIsLast pins the structural invariant that
// the `unknown` catch-all profile (with its empty-string matcher) MUST
// be the LAST entry in the `Profiles` slice. Otherwise it would match
// every UA via `strings.Contains(anything, "")` and shadow every
// downstream profile.
func TestProfilesRegistry_unknownIsLast(t *testing.T) {
	if len(Profiles) == 0 {
		t.Fatal("Profiles slice is empty — registry must contain at least the unknown catch-all")
	}
	last := Profiles[len(Profiles)-1]
	if last.ID != "unknown" {
		t.Fatalf("last profile in registry has ID %q, want %q — unknown catch-all MUST be last", last.ID, "unknown")
	}
	if len(last.UserAgentMatchers) != 1 || last.UserAgentMatchers[0] != "" {
		t.Fatalf("unknown profile UserAgentMatchers = %v, want [\"\"] — the empty-string matcher is what makes it the catch-all", last.UserAgentMatchers)
	}
	// Sanity: every other profile must NOT carry an empty-string matcher
	// (which would shadow downstream profiles).
	for i := 0; i < len(Profiles)-1; i++ {
		for _, m := range Profiles[i].UserAgentMatchers {
			if m == "" {
				t.Errorf("profile %q at index %d carries an empty-string matcher — only the LAST (unknown) profile may", Profiles[i].ID, i)
			}
		}
	}
}

// TestProfilesRegistry_sonyBeforeMPDAndLavf pins the slice-order invariant
// that drives the branch-ordering test above. Documented as a structural
// invariant so a future refactor that touches `Profiles` ordering fails
// at the test level with a clear message about the ordering contract.
func TestProfilesRegistry_sonyBeforeMPDAndLavf(t *testing.T) {
	idxOf := func(id string) int {
		for i, p := range Profiles {
			if p.ID == id {
				return i
			}
		}
		return -1
	}
	sonyIdx := idxOf("sony")
	mpdIdx := idxOf("mpdGeneric")
	lavfIdx := idxOf("lavf")
	if sonyIdx < 0 || mpdIdx < 0 || lavfIdx < 0 {
		t.Fatalf("missing required profile: sony=%d mpd=%d lavf=%d", sonyIdx, mpdIdx, lavfIdx)
	}
	if sonyIdx >= mpdIdx {
		t.Errorf("sony profile (idx %d) must precede mpdGeneric (idx %d) — vendor override-first invariant", sonyIdx, mpdIdx)
	}
	if sonyIdx >= lavfIdx {
		t.Errorf("sony profile (idx %d) must precede lavf (idx %d) — vendor override-first invariant", sonyIdx, lavfIdx)
	}
}

func TestPreferredMIMEFor(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
		extension string
		wantMIME  string
	}{
		// Sony per-vendor overrides
		{"sony_dsf", "Sony SRS-HG1", ".dsf", "audio/dsd"},
		{"sony_dff", "Sony SRS-HG1", ".dff", "audio/dsd"},
		{"sony_flac_falls_through_to_default", "Sony SRS-HG1", ".flac", "audio/x-flac"},

		// Chord / Integra / MPD — same MIME as default but explicit branch
		{"chord_dsf", "Chord 2go", ".dsf", "audio/x-dsf"},
		{"chord_dff", "Chord 2go", ".dff", "audio/x-dff"},
		{"integra_dsf", "Integra DRX-3", ".dsf", "audio/x-dsf"},
		{"mpd_dsf", "Music Player Daemon 0.21.26", ".dsf", "audio/x-dsf"},
		{"lavf_dsf", "Lavf/58.45.100", ".dsf", "audio/x-dsf"},

		// Unknown profile falls through to defaults
		{"unknown_dsf", "GenericPlayer", ".dsf", "audio/x-dsf"},
		{"unknown_flac", "GenericPlayer", ".flac", "audio/x-flac"},
		{"unknown_unrecognized_ext", "GenericPlayer", ".xyz", "application/octet-stream"},

		// Case-folding on extension (lowercased input + uppercased input both work)
		{"sony_dsf_uppercase_ext", "Sony", ".DSF", "audio/dsd"},
		{"chord_flac_uppercase_ext", "Chord", ".FLAC", "audio/x-flac"},

		// Empty extension → octet-stream fallback
		{"empty_ext", "any-ua", "", "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PreferredMIMEFor(tc.userAgent, tc.extension)
			if got != tc.wantMIME {
				t.Errorf("PreferredMIMEFor(%q, %q) = %q, want %q",
					tc.userAgent, tc.extension, got, tc.wantMIME)
			}
		})
	}
}

func TestDefaultMIMEForExtension(t *testing.T) {
	cases := []struct {
		extension string
		want      string
	}{
		{".dsf", "audio/x-dsf"},
		{".dff", "audio/x-dff"},
		{".flac", "audio/x-flac"},
		{".m4a", "audio/mp4"},
		{".mp4", "audio/mp4"},
		{".wav", "audio/wav"},
		{".aiff", "audio/aiff"},
		{".aif", "audio/aiff"},
		{".mp3", "audio/mpeg"},
		{".ogg", "audio/ogg"},
		{".xyz", "application/octet-stream"},
		{"", "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(strings.TrimPrefix(tc.extension, "."), func(t *testing.T) {
			got := defaultMIMEForExtension(tc.extension)
			if got != tc.want {
				t.Errorf("defaultMIMEForExtension(%q) = %q, want %q", tc.extension, got, tc.want)
			}
		})
	}
}

// TestChordProfile_knownBugsAndMaxFileSize pins the Phase 0 empirical
// findings on the Chord profile entry. Future telemetry / file-handler
// features that read these fields depend on them being present.
func TestChordProfile_knownBugsAndMaxFileSize(t *testing.T) {
	profile := MatchProfile("Chord 2go")
	if profile.ID != "chordFamily" {
		t.Fatalf("expected chordFamily, got %q", profile.ID)
	}
	if !profile.KnownBugs[BugRapidPauseResumeArtifact] {
		t.Error("Chord profile must carry BugRapidPauseResumeArtifact (Phase 0 finding 2026-05-26 — Hugo 2 PLL relock + mute relay)")
	}
	if !profile.KnownBugs[BugID3OffsetOverflowOver2GB] {
		t.Error("Chord profile must carry BugID3OffsetOverflowOver2GB (assumed per field reports; not empirically confirmed in Phase 0)")
	}
	const expectedMaxSize int64 = 2147483647 // 2^31-1
	if profile.MaxSafeFileSize != expectedMaxSize {
		t.Errorf("Chord MaxSafeFileSize = %d, want %d (2^31-1; defensive cap per BugID3OffsetOverflowOver2GB)",
			profile.MaxSafeFileSize, expectedMaxSize)
	}
}

// TestUnknownProfile_hasNoPreferredMIMEOverrides pins the contract that
// the unknown catch-all has zero per-vendor MIME overrides — every
// extension falls through to `defaultMIMEForExtension`. If a future
// refactor adds entries to the unknown profile's PreferredMIME, that's
// an architectural mistake (per-vendor data belongs in a specific
// profile, not the catch-all).
func TestUnknownProfile_hasNoPreferredMIMEOverrides(t *testing.T) {
	profile := MatchProfile("totally-unknown-renderer")
	if profile.ID != "unknown" {
		t.Fatalf("expected unknown profile, got %q", profile.ID)
	}
	if len(profile.PreferredMIME) != 0 {
		t.Errorf("unknown profile has %d PreferredMIME entries, want 0 — per-vendor data belongs in specific profiles, not the catch-all. Entries: %v",
			len(profile.PreferredMIME), profile.PreferredMIME)
	}
	if len(profile.KnownBugs) != 0 {
		t.Errorf("unknown profile has %d KnownBugs entries, want 0 — known bugs are per-vendor data", len(profile.KnownBugs))
	}
	if profile.MaxSafeFileSize != 0 {
		t.Errorf("unknown profile has MaxSafeFileSize = %d, want 0 — per-vendor data", profile.MaxSafeFileSize)
	}
}

// TestResolveMIMEType_stillProducesSameResultsAsPreferredMIMEFor pins the
// invariant that the refactored ResolveMIMEType (now a thin wrapper over
// PreferredMIMEFor) produces byte-identical results to the registry
// lookup. Defends against a future refactor that re-introduces inline
// branch logic in ResolveMIMEType and drifts from the registry.
func TestResolveMIMEType_stillProducesSameResultsAsPreferredMIMEFor(t *testing.T) {
	cases := []struct{ ua, ext string }{
		{"Sony SRS-HG1", ".dsf"},
		{"Sony SRS-HG1", ".dff"},
		{"Sony Music Player Daemon", ".dsf"},
		{"Chord 2go", ".dsf"},
		{"Music Player Daemon 0.21.26", ".dsf"},
		{"Lavf/58.45.100", ".dsf"},
		{"GenericPlayer", ".dsf"},
		{"any-ua", ".flac"},
		{"any-ua", ".wav"},
		{"any-ua", ".m4a"},
		{"any-ua", ".unknown-ext"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.ua+"_"+tc.ext, func(t *testing.T) {
			resolved := ResolveMIMEType(tc.ua, tc.ext)
			direct := PreferredMIMEFor(tc.ua, tc.ext)
			if resolved != direct {
				t.Errorf("ResolveMIMEType(%q,%q)=%q but PreferredMIMEFor(%q,%q)=%q — refactor drift",
					tc.ua, tc.ext, resolved, tc.ua, tc.ext, direct)
			}
		})
	}
}
