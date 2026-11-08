package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/enrich"
)

// TestARewriteKeepsTheEnrichmentOptOuts: `bridge init --force` keeps each
// enrichment switch the config it replaces writes off (backlog B617), in
// either posture, and printKept says so, for the reason it keeps output.mpd:
// off: the default would turn the operator's opt-out back on, and init asks
// for neither. A switch written on is the default and is written as no key;
// one that does not read is not kept and is named, by its line and never by
// its value; and the environment of whoever runs init is not written.
func TestARewriteKeepsTheEnrichmentOptOuts(t *testing.T) {
	for _, p := range rewritePostures {
		t.Run(p.name+": both off are kept", func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := p.setUp(t, cfgDir)
			appendToConfig(t, cfgDir, "enrich:\n  enabled: false\n  deezerEnabled: false\n")

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			got := loadInstallConfig(t, cfgDir).Enrich
			if got.Enabled == nil || *got.Enabled || got.DeezerEnabled == nil || *got.DeezerEnabled {
				t.Errorf("the rewritten config has enrich.enabled %v, enrich.deezerEnabled %v; want both written false",
					got.Enabled, got.DeezerEnabled)
			}
			for _, key := range []string{"enrich.enabled", "enrich.deezerEnabled"} {
				if v, ok := keptValue(out, key); !ok || v != "false" {
					t.Errorf("the run's list of what it kept gives %s %q (listed %v), want false", key, v, ok)
				}
			}
		})
	}

	t.Run("one written on is the default", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		// off is false to Load's decoder as to the rewrite's.
		appendToConfig(t, cfgDir, "enrich:\n  enabled: true\n  deezerEnabled: off\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		got := loadInstallConfig(t, cfgDir).Enrich
		if got.Enabled != nil {
			t.Errorf("enrich.enabled = %v, want no key: on is the default", *got.Enabled)
		}
		if got.DeezerEnabled == nil || *got.DeezerEnabled {
			t.Errorf("enrich.deezerEnabled = %v, want false kept", got.DeezerEnabled)
		}
		if _, listed := keptValue(out, "enrich.enabled"); listed {
			t.Error("the run lists enrich.enabled as kept; a first install writes on too")
		}
	})

	t.Run("one that does not read is named and not kept", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		const secret = "maybe-s3cret"
		appendToConfig(t, cfgDir, "enrich:\n  enabled: "+secret+"\n  deezerEnabled: false\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d: a switch that does not read must not stop it", code)
		}
		got := loadInstallConfig(t, cfgDir).Enrich
		if got.Enabled != nil {
			t.Errorf("enrich.enabled = %v, want not kept", *got.Enabled)
		}
		if got.DeezerEnabled == nil || *got.DeezerEnabled {
			t.Errorf("enrich.deezerEnabled = %v, want false kept beside the one that does not read", got.DeezerEnabled)
		}
		if !strings.Contains(out, "enrich.enabled (line ") || !strings.Contains(out, "does not read as on or off") {
			t.Errorf("the run does not name the switch it did not keep:\n%s", out)
		}
		if strings.Contains(strings.ToLower(out), secret) {
			t.Error("the run printed the value it did not keep")
		}
	})

	t.Run("a block that is no block keeps nothing and says so", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		appendToConfig(t, cfgDir, "enrich: false\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if got := loadInstallConfig(t, cfgDir).Enrich; got.Enabled != nil || got.DeezerEnabled != nil {
			t.Errorf("the rewritten config has enrich switches %v, %v; want none", got.Enabled, got.DeezerEnabled)
		}
		if !strings.Contains(out, "enrich (line ") {
			t.Errorf("the run does not say the block kept nothing:\n%s", out)
		}
	})

	t.Run("the environment is not written", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		t.Setenv("BRIDGE_ENRICH_ENABLED", "false")
		t.Setenv("BRIDGE_ENRICH_DEEZER_ENABLED", "false")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if body := readConfigFile(t, cfgDir); strings.Contains(body, "enrich") {
			t.Errorf("the rewrite wrote the environment's switches into the file:\n%s", body)
		}
	})
}

// baseSecret is what the enrich bases in these tests carry in their user
// information and query, where a mirror's credential rides (backlog B69).
const baseSecret = "s3cret-Pw"

// nextConfigLine is the line the first line appendToConfig adds to the
// config at cfgDir takes: a config init wrote ends with a line feed.
func nextConfigLine(t *testing.T, cfgDir string) int {
	t.Helper()
	return strings.Count(readConfigFile(t, cfgDir), "\n") + 1
}

// requireBaseNotKept fails unless the run's output names setting, a base the
// rewrite did not keep, by the line of its key, says the default is used in
// its place, and shows nothing of its value: its credential and its host.
func requireBaseNotKept(t *testing.T, out, setting string, line int, host string) {
	t.Helper()
	if want := fmt.Sprintf("%s (line %d): ", setting, line); !strings.Contains(out, want) {
		t.Errorf("the run does not name %s, not kept, by its line (%q)", setting, want)
	}
	if !strings.Contains(out, "the default is used now") {
		t.Error("the run does not say the default is used in place of a base it did not keep")
	}
	if strings.Contains(strings.ToLower(out), strings.ToLower(baseSecret)) {
		t.Error("the run printed the credential of a base it did not keep")
	}
	if strings.Contains(out, host) {
		t.Errorf("the run printed the host of a base it did not keep (%s)", host)
	}
}

// TestARewriteKeepsTheEnrichmentBases: `bridge init --force` keeps the enrich
// block's two base URLs (backlog B761), each on its own, in either posture,
// as it keeps the block's opt-outs: init asks for neither. A base Load takes
// that is not the default is written as Load keeps it, and listed among what
// the run kept, never with the user information or the query a mirror's
// credential rides in; one written empty, or as the default, is written as no
// key, as a first install writes it; one Load refuses is not kept, and the
// run names it by its line, never its value, and says the default is used
// now. A base is kept beside a sibling Load refuses and beside switches that
// do not read, and what is kept is the file's, never the environment's.
func TestARewriteKeepsTheEnrichmentBases(t *testing.T) {
	for _, p := range rewritePostures {
		t.Run(p.name+": both kept", func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := p.setUp(t, cfgDir)
			// Each written as Load keeps it: trimmed, with no trailing slash.
			appendToConfig(t, cfgDir, "enrich:\n  musicbrainzBaseURL: https://mirror.example/ws/2/\n"+
				"  coverArtBaseURL: \"  https://covers.example  \"\n")

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			got := loadInstallConfig(t, cfgDir).Enrich
			if got.MusicBrainzBaseURL != "https://mirror.example/ws/2" || got.CoverArtBaseURL != "https://covers.example" {
				t.Errorf("the rewritten config has enrich bases %q and %q, want both kept as Load keeps them",
					got.MusicBrainzBaseURL, got.CoverArtBaseURL)
			}
			for key, want := range map[string]string{
				"enrich.musicbrainzBaseURL": "https://mirror.example/ws/2",
				"enrich.coverArtBaseURL":    "https://covers.example",
			} {
				if v, ok := keptValue(out, key); !ok || v != want {
					t.Errorf("the run's list of what it kept gives %s %q (listed %v), want %q", key, v, ok, want)
				}
			}
		})

		t.Run(p.name+": one kept beside one Load refuses", func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := p.setUp(t, cfgDir)
			line := nextConfigLine(t, cfgDir) + 1
			appendToConfig(t, cfgDir, "enrich:\n  musicbrainzBaseURL: ftp://user:"+baseSecret+"@refused.example/ws/2\n"+
				"  coverArtBaseURL: https://covers.example\n")

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d: a base Load refuses must not stop it", code)
			}
			got := loadInstallConfig(t, cfgDir).Enrich
			if got.MusicBrainzBaseURL != "" {
				t.Errorf("enrich.musicbrainzBaseURL = %q, want not kept: Load refuses it", got.MusicBrainzBaseURL)
			}
			if got.CoverArtBaseURL != "https://covers.example" {
				t.Errorf("enrich.coverArtBaseURL = %q, want kept beside the base Load refuses", got.CoverArtBaseURL)
			}
			requireBaseNotKept(t, out, "enrich.musicbrainzBaseURL", line, "refused.example")
		})
	}

	t.Run("a base kept beside switches that do not read", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		line := nextConfigLine(t, cfgDir)
		appendToConfig(t, cfgDir, "enrich:\n  enabled: maybe-"+baseSecret+"\n  deezerEnabled: [off]\n"+
			"  musicbrainzBaseURL: https://mirror.example/ws/2\n  coverArtBaseURL: [https://refused.example]\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		got := loadInstallConfig(t, cfgDir).Enrich
		if got.MusicBrainzBaseURL != "https://mirror.example/ws/2" {
			t.Errorf("enrich.musicbrainzBaseURL = %q, want kept beside the settings that do not read", got.MusicBrainzBaseURL)
		}
		if got.CoverArtBaseURL != "" || got.Enabled != nil || got.DeezerEnabled != nil {
			t.Errorf("the rewritten config keeps coverArtBaseURL %q, enabled %v, deezerEnabled %v; want none of them",
				got.CoverArtBaseURL, got.Enabled, got.DeezerEnabled)
		}
		for setting, l := range map[string]int{"enrich.enabled": line + 1, "enrich.deezerEnabled": line + 2} {
			if want := fmt.Sprintf("%s (line %d): ", setting, l); !strings.Contains(out, want) {
				t.Errorf("the run does not name %s, not kept, by its line (%q)", setting, want)
			}
		}
		requireBaseNotKept(t, out, "enrich.coverArtBaseURL", line+4, "refused.example")
	})

	for i, block := range []string{
		"enrich:\n  musicbrainzBaseURL: https://musicbrainz.org/ws/2\n  coverArtBaseURL: \"\"\n",
		"enrich:\n  musicbrainzBaseURL: \"  \"\n  coverArtBaseURL: https://coverartarchive.org/\n",
	} {
		t.Run(fmt.Sprintf("an explicit default and an empty base write no key, %d", i), func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := rewritePostures[0].setUp(t, cfgDir)
			appendToConfig(t, cfgDir, block)

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			if body := readConfigFile(t, cfgDir); strings.Contains(body, "enrich") {
				t.Errorf("the rewrite wrote an enrich key for a base a first install writes none for:\n%s", body)
			}
			for _, key := range []string{"enrich.musicbrainzBaseURL", "enrich.coverArtBaseURL"} {
				if v, listed := keptValue(out, key); listed {
					t.Errorf("the run lists %s %q as kept; a first install writes the default too", key, v)
				}
			}
			if strings.Contains(out, "did not keep") {
				t.Error("the run warns of a setting it did not keep; the default and an empty base are no loss")
			}
		})
	}

	// Each base carries its credential in one part alone, so that each part
	// is shown or not on its own: a password beside a user name, a token
	// written as the user name, a query, a fragment.
	const login = "mirror-login"
	for _, c := range []struct{ name, mb, caa string }{
		{"a password, and a query",
			"https://" + login + ":" + baseSecret + "@mirror.example/ws/2",
			"https://covers.example/?apikey=" + baseSecret},
		{"a query, and a token as the user name",
			"https://mirror.example/ws/2?apikey=" + baseSecret,
			"https://" + baseSecret + "@covers.example"},
		{"a fragment, and a password",
			"https://mirror.example/ws/2#" + baseSecret,
			"https://" + login + ":" + baseSecret + "@covers.example"},
	} {
		t.Run("a base with a credential is kept and the credential never shown: "+c.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := rewritePostures[0].setUp(t, cfgDir)
			appendToConfig(t, cfgDir, "enrich:\n  musicbrainzBaseURL: \""+c.mb+"\"\n  coverArtBaseURL: \""+c.caa+"\"\n")

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			got := loadInstallConfig(t, cfgDir).Enrich
			// The Cover Art Archive's base with a query keeps its slash: Load
			// trims only a slash that ends the value.
			if got.MusicBrainzBaseURL != c.mb || got.CoverArtBaseURL != c.caa {
				t.Errorf("the rewritten config has enrich bases %q and %q, want both kept whole", got.MusicBrainzBaseURL, got.CoverArtBaseURL)
			}
			for _, key := range []string{"enrich.musicbrainzBaseURL", "enrich.coverArtBaseURL"} {
				if _, listed := keptValue(out, key); !listed {
					t.Errorf("the run's list of what it kept leaves %s out", key)
				}
			}
			for _, part := range []string{baseSecret, login, "apikey"} {
				if strings.Contains(strings.ToLower(out), strings.ToLower(part)) {
					t.Errorf("the run printed %q, part of a kept base's user information, query or fragment", part)
				}
			}
		})
	}

	t.Run("a base Load refuses, then a rewrite that loads", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		line := nextConfigLine(t, cfgDir) + 1
		appendToConfig(t, cfgDir, "enrich:\n  coverArtBaseURL: ftp://"+baseSecret+"@refused.example/\n")
		if _, err := config.Load(filepath.Join(cfgDir, "bridge.yaml")); err == nil {
			t.Fatal("the fixture's config loads; it must hold a base Load refuses")
		}

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if got := loadInstallConfig(t, cfgDir).Enrich.CoverArtBaseURL; got != "" {
			t.Errorf("enrich.coverArtBaseURL = %q, want not kept: Load refuses it", got)
		}
		requireBaseNotKept(t, out, "enrich.coverArtBaseURL", line, "refused.example")
	})

	t.Run("a block that is no block keeps no base either, and says so", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		line := nextConfigLine(t, cfgDir)
		appendToConfig(t, cfgDir, "enrich: [https://mirror.example/ws/2]\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		said := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, fmt.Sprintf("enrich (line %d): ", line)) {
				said = l
			}
		}
		if !strings.Contains(said, "base URL") {
			t.Errorf("the run's line for the block does not say its base URLs were not kept: %q", said)
		}
		if !strings.Contains(out, "the default is used now") {
			t.Error("the run does not say the default is used in place of the bases it did not keep")
		}
	})

	t.Run("the file's bases are kept, never the environment's", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		t.Setenv("BRIDGE_MUSICBRAINZ_BASE_URL", "https://legacy-env.example/ws/2")
		t.Setenv("BRIDGE_ENRICH_MUSICBRAINZ_BASE_URL", "https://derived-env.example/ws/2")
		t.Setenv("BRIDGE_ENRICH_COVER_ART_BASE_URL", "https://covers-env.example")
		appendToConfig(t, cfgDir, "enrich:\n  musicbrainzBaseURL: https://mirror.example/ws/2\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		cfgPath := filepath.Join(cfgDir, "bridge.yaml")
		if v, ok := enrichKeyInFile(t, cfgPath, "musicbrainzBaseURL"); !ok || v != "https://mirror.example/ws/2" {
			t.Errorf("the file holds enrich.musicbrainzBaseURL %v (there %v), want the file's own base", v, ok)
		}
		if v, ok := enrichKeyInFile(t, cfgPath, "coverArtBaseURL"); ok {
			t.Errorf("the file holds enrich.coverArtBaseURL %v, the environment's: the file wrote none", v)
		}
		if strings.Contains(out, "env.example") {
			t.Error("the run shows a base the environment sets")
		}
	})
}

// TestEnrichBaseDefaultsAgree pins config's two enrich base defaults, which
// a rewrite writes no key for, to the enricher's, which say what an empty
// base asks: internal/config imports no feature package, so it writes them
// out, and cmd/bridge, which imports both, is where the two can be held equal.
// The enricher's are the source of truth.
func TestEnrichBaseDefaultsAgree(t *testing.T) {
	if config.DefaultMusicBrainzBaseURL != enrich.DefaultMusicBrainzBase {
		t.Errorf("config.DefaultMusicBrainzBaseURL = %q, enrich.DefaultMusicBrainzBase = %q: update config's",
			config.DefaultMusicBrainzBaseURL, enrich.DefaultMusicBrainzBase)
	}
	if config.DefaultCoverArtBaseURL != enrich.DefaultCoverArtBase {
		t.Errorf("config.DefaultCoverArtBaseURL = %q, enrich.DefaultCoverArtBase = %q: update config's",
			config.DefaultCoverArtBaseURL, enrich.DefaultCoverArtBase)
	}
}

// TestARewriteOverTheDefaultsWritesWhatAFirstInstallWrites is a regression
// guard for backlog B761: a rewrite of an install whose config holds no
// enrich block, given the answers the install was made with, writes the
// bytes the install's first init wrote, so keeping the bases adds nothing
// where the config gives none.
func TestARewriteOverTheDefaultsWritesWhatAFirstInstallWrites(t *testing.T) {
	for _, p := range rewritePostures {
		t.Run(p.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := p.install(t, cfgDir, "Same")
			before := readConfigFile(t, cfgDir)

			code, out := rewrite("Same")
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			if after := readConfigFile(t, cfgDir); after != before {
				t.Errorf("a rewrite over the defaults wrote\n%s\nwhere the first init wrote\n%s", after, before)
			}
		})
	}
}
