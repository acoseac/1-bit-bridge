package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestShippedEnvVarNamesStillWork is the back-compat gate.
//
// These eleven names were hand-written before the table was derived
// from the struct, and something out there is already setting them —
// the Docker image documents several. A derivation that renamed any of
// them would not fail: the bridge would simply stop honouring an
// override, silently, and the operator would find out from behaviour.
//
// BRIDGE_MUSICBRAINZ_BASE_URL is the sharpest of those: losing it sends
// a bridge configured for a self-hosted Atlas mirror back to public
// MusicBrainz — at the self-hosted 150 ms pace, which is roughly 6.7
// requests a second against a service that asks anonymous clients for
// one.
func TestShippedEnvVarNamesStillWork(t *testing.T) {
	cases := []struct {
		name, value string
		check       func(*Config) bool
	}{
		{"BRIDGE_LISTEN_ADDRESS", ":9999", func(c *Config) bool { return c.ListenAddress == ":9999" }},
		{"BRIDGE_ADMIN_ADDRESS", "127.0.0.1:9998", func(c *Config) bool { return c.AdminAddress == "127.0.0.1:9998" }},
		{"BRIDGE_DATA_DIR", "/tmp/d", func(c *Config) bool { return c.DataDir == "/tmp/d" }},
		{"BRIDGE_LIBRARY_NAME", "Env Library", func(c *Config) bool { return c.LibraryName == "Env Library" }},
		{"BRIDGE_LIBRARY_ROOTS", "/a", func(c *Config) bool {
			return len(c.LibraryRoots) == 1 && c.LibraryRoots[0] == "/a"
		}},
		{"BRIDGE_DISABLE_HTTP3", "true", func(c *Config) bool { return c.DisableHTTP3 }},
		{"BRIDGE_UPSCALE_ENABLED", "true", func(c *Config) bool { return c.Upscale.Enabled }},
		{"BRIDGE_ANALYSIS_ENABLED", "true", func(c *Config) bool { return c.Analysis.Enabled }},
		{"BRIDGE_FINGERPRINT_ENABLED", "true", func(c *Config) bool { return c.Fingerprint.Enabled }},
		{"BRIDGE_MUSICBRAINZ_BASE_URL", "https://atlas.test/ws/2", func(c *Config) bool {
			return c.Enrich.MusicBrainzBaseURL == "https://atlas.test/ws/2"
		}},
		{"BRIDGE_COVERART_BASE_URL", "https://atlas.test", func(c *Config) bool {
			return c.Enrich.CoverArtBaseURL == "https://atlas.test"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(c.name, c.value)
			cfg := &Config{}
			cfg.applyEnvOverrides()
			if !c.check(cfg) {
				t.Errorf("%s=%q had no effect — a shipped override stopped working, "+
					"which is silent: the bridge just uses the yaml value", c.name, c.value)
			}
		})
	}
}

// TestEveryConfigLeafIsEnvSettable is the completeness gate, and the
// reason the table is derived rather than hand-listed.
//
// The hand-written form covered 11 of ~90 fields and had no way to stay
// complete: every config field added after it was silently absent, and
// nothing anywhere said so. A tenant could not be given an autocert
// domain or a deployment mode without someone hand-writing a YAML file,
// which is precisely the step that does not scale.
//
// An exemption is a decision written down (envExemptPaths carries the
// reason), never an omission.
func TestEveryConfigLeafIsEnvSettable(t *testing.T) {
	bound := map[string]bool{}
	for _, b := range envBindings() {
		bound[b.YAMLPath] = true
	}
	if len(bound) < 50 {
		t.Fatalf("only %d bindings derived — the walk is broken, so this test "+
			"proves nothing", len(bound))
	}

	var missing []string
	var walk func(t reflect.Type, prefix string)
	walk = func(rt reflect.Type, prefix string) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("yaml")
			if tag == "" || tag == "-" {
				continue
			}
			key := strings.Split(tag, ",")[0]
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			ft := f.Type
			if ft.Kind() == reflect.Struct {
				walk(ft, path)
				continue
			}
			// Only kinds a variable can express. A map or a slice of
			// structs is not something an env var should be carrying.
			settable := ft.Kind() == reflect.String || ft.Kind() == reflect.Bool ||
				ft.Kind() == reflect.Int || ft.Kind() == reflect.Int64 ||
				(ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.String) ||
				(ft.Kind() == reflect.Pointer && ft.Elem().Kind() == reflect.Bool)
			if !settable {
				continue
			}
			if _, exempt := envExemptPaths[path]; exempt {
				continue
			}
			if !bound[path] {
				missing = append(missing, path)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")

	if len(missing) > 0 {
		t.Errorf("config fields with no env override: %v\n"+
			"A bridge cannot be provisioned declaratively without them. Either the "+
			"derivation missed a kind, or the field belongs in envExemptPaths WITH "+
			"a reason.", missing)
	}
}

// TestEnvVarNamesAreUnique: two config paths deriving the same variable
// name would make one of them silently unreachable, and which one wins
// would depend on map iteration order.
func TestEnvVarNamesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, b := range envBindings() {
		if prev, dup := seen[b.EnvVar]; dup {
			t.Errorf("%s is derived from both %q and %q — one of them is unreachable",
				b.EnvVar, prev, b.YAMLPath)
		}
		seen[b.EnvVar] = b.YAMLPath
	}
	for name := range legacyEnvAliases {
		if path, clash := seen[name]; clash {
			t.Errorf("legacy alias %s collides with the derived name for %q", name, path)
		}
	}
}

func TestUpperSnake(t *testing.T) {
	cases := map[string]string{
		"listenAddress":      "LISTEN_ADDRESS",
		"dataDir":            "DATA_DIR",
		"disableHttp3":       "DISABLE_HTTP3",
		"coverArtBaseURL":    "COVER_ART_BASE_URL",
		"tlsCertPath":        "TLS_CERT_PATH",
		"tokenSHA256":        "TOKEN_SHA256",
		"rendererTTLSeconds": "RENDERER_TTL_SECONDS",
		"enabled":            "ENABLED",
	}
	for in, want := range cases {
		if got := upperSnake(in); got != want {
			t.Errorf("upperSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUnparseableEnvIsIgnoredNotCoerced pins the direction of the
// failure. Coercing a typo like `yes` or `on` to false would silently
// turn a feature OFF, and a bridge that quietly stopped doing something
// is far harder to notice than one that ignored an override and logged
// that it did.
func TestUnparseableEnvIsIgnoredNotCoerced(t *testing.T) {
	t.Setenv("BRIDGE_UPSCALE_ENABLED", "yes")
	cfg := &Config{Upscale: UpscaleConfig{Enabled: true}}
	cfg.applyEnvOverrides()
	if !cfg.Upscale.Enabled {
		t.Error("an unparseable boolean turned the feature off — it must be ignored, " +
			"leaving the yaml value standing")
	}

	t.Setenv("BRIDGE_UPSCALE_WORKERS", "lots")
	cfg2 := &Config{Upscale: UpscaleConfig{Workers: 4}}
	cfg2.applyEnvOverrides()
	if cfg2.Upscale.Workers != 4 {
		t.Errorf("workers = %d, want 4 — an unparseable integer must not zero the field",
			cfg2.Upscale.Workers)
	}
}

// TestEnvSetsNestedAndPointerFields covers the two shapes the old
// hand-written form could not reach at all: a nested section, and the
// pointer-to-bool "unset means default on" idiom.
func TestEnvSetsNestedAndPointerFields(t *testing.T) {
	t.Setenv("BRIDGE_AUTOCERT_DOMAIN", "bridge.example.test")
	t.Setenv("BRIDGE_DEPLOYMENT_MODE", "public")
	t.Setenv("BRIDGE_SMART_PLAYLISTS_ENABLED", "false")
	t.Setenv("BRIDGE_UPSCALE_AUTO_OPTIMIZE_MAX_PER_SWEEP", "50")

	cfg := &Config{}
	cfg.applyEnvOverrides()

	if cfg.Autocert.Domain != "bridge.example.test" {
		t.Errorf("autocert.domain = %q", cfg.Autocert.Domain)
	}
	if cfg.Deployment.Mode != "public" {
		t.Errorf("deployment.mode = %q — without this a tenant cannot be provisioned "+
			"in public mode from the environment at all", cfg.Deployment.Mode)
	}
	if cfg.SmartPlaylists.Enabled == nil || *cfg.SmartPlaylists.Enabled {
		t.Error("smartPlaylists.enabled: an env override must allocate the pointer, " +
			"which is exactly the explicit choice the pointer exists to represent")
	}
	if cfg.Upscale.AutoOptimize.MaxPerSweep != 50 {
		t.Errorf("maxPerSweep = %d, want 50", cfg.Upscale.AutoOptimize.MaxPerSweep)
	}
}

func TestEnvOverrideDocsCoversEverything(t *testing.T) {
	docs := EnvOverrideDocs()
	if len(docs) != len(envBindings())+len(legacyEnvAliases) {
		t.Errorf("docs list %d entries, table has %d — the list an operator reads "+
			"must be generated from the table the loader uses",
			len(docs), len(envBindings())+len(legacyEnvAliases))
	}
	for _, d := range docs {
		if !strings.HasPrefix(d, envPrefix) {
			t.Errorf("doc line %q does not start with %s", d, envPrefix)
		}
	}
	_ = fmt.Sprint(docs)
}
