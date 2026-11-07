package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// A console save and the switches' variables (backlog B617). Serve applies
// BRIDGE_ENRICH_ENABLED and BRIDGE_ENRICH_DEEZER_ENABLED over the file at
// boot, the console shows that effective value, and its form sends both
// switches with every save. A save that does not change a switch writes the
// file's own value for it back, and the running bridge keeps the variable's;
// a save that changes one writes the change (off as false, on as no key) and
// runs it. So the variable never lands in the file, and a restart without it
// finds what the operator chose.

// enrichKeyInFile reads enrich.<key> from the bridge.yaml at path as the file
// holds it: its value and whether the key is there.
func enrichKeyInFile(t *testing.T, path, key string) (any, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Enrich map[string]any `yaml:"enrich"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("bridge.yaml does not parse: %v", err)
	}
	v, ok := doc.Enrich[key]
	return v, ok
}

// TestAConsoleSaveKeepsTheFilesOwnSwitches boots serve with each switch's
// variable set against the file, both ways, and drives the console's real
// settings save.
func TestAConsoleSaveKeepsTheFilesOwnSwitches(t *testing.T) {
	persistentMedium(t)
	type fileKey struct {
		value any
		there bool
	}
	absent := fileKey{}
	off := fileKey{false, true}
	for _, sw := range []struct{ field, key, env string }{
		{"enrichEnabled", "enabled", "BRIDGE_ENRICH_ENABLED"},
		{"enrichDeezerEnabled", "deezerEnabled", "BRIDGE_ENRICH_DEEZER_ENABLED"},
	} {
		for _, c := range []struct {
			name     string
			env      string  // the variable, set at the boot
			file     fileKey // what the file holds at the boot
			live     bool    // what the variable makes it
			reloaded bool    // what the file alone makes it
		}{
			{name: "the variable off over a file that says nothing", env: "false", file: absent, live: false, reloaded: true},
			{name: "the variable on over a file that says off", env: "on", file: off, live: true, reloaded: false},
		} {
			t.Run(sw.field+": "+c.name, func(t *testing.T) {
				clearEnrichSwitchEnv(t)
				t.Setenv("BRIDGE_ENRICH_DEEZER_ENABLED", "")
				os.Unsetenv("BRIDGE_ENRICH_DEEZER_ENABLED")
				t.Setenv(sw.env, c.env)
				tail := ""
				if c.file.there {
					tail = fmt.Sprintf("enrich:\n  %s: false\n", sw.key)
				}
				var asked atomic.Int64
				var lastAnswer atomic.Bool
				b := startConsoleBridge(t, tail, nil, func(o *serveOpts) {
					o.enricherPoll = 20 * time.Millisecond
					o.enricherAsked = func(on bool) { lastAnswer.Store(on); asked.Add(1) }
				})
				cfgPath := filepath.Join(filepath.Dir(b.lib), "bridge.yaml")

				// shown is the switch as the console reads the running config,
				// and, for enrich.enabled, as the enricher's gate answers it:
				// the next two turns after a save answer what runs.
				shown := func() bool {
					t.Helper()
					var got map[string]any
					getJSON(t, b.console, b.adminBase+"/api/settings", "", &got)
					on, ok := got[sw.field].(bool)
					if !ok {
						t.Fatalf("GET /api/settings carries no %s: %v", sw.field, got[sw.field])
					}
					if sw.key == "enabled" {
						from := asked.Load()
						eventually(t, func() bool { return asked.Load() >= from+2 }, "two turns of the enricher")
						if gate := lastAnswer.Load(); gate != on {
							t.Errorf("the enricher's gate answers %v while the console shows %v", gate, on)
						}
					}
					return on
				}
				save := func(body map[string]any) {
					t.Helper()
					raw, _ := json.Marshal(body)
					if code, resp := patchSettings(t, b, string(raw)); code != http.StatusOK {
						t.Fatalf("PATCH %s answered %d %s", raw, code, resp)
					}
				}
				requireFile := func(when string, want fileKey) {
					t.Helper()
					v, ok := enrichKeyInFile(t, cfgPath, sw.key)
					if ok != want.there || (ok && v != want.value) {
						t.Errorf("%s: the file holds enrich.%s = %v (there %v), want %v (there %v)",
							when, sw.key, v, ok, want.value, want.there)
					}
				}
				reload := func() bool {
					t.Helper()
					t.Setenv(sw.env, "")
					os.Unsetenv(sw.env)
					defer t.Setenv(sw.env, c.env)
					cfg, err := config.Load(cfgPath)
					if err != nil {
						t.Fatalf("the saved file does not load: %v", err)
					}
					if sw.key == "enabled" {
						return cfg.Enrich.EffectiveEnabled()
					}
					return cfg.Enrich.EffectiveDeezerEnabled()
				}

				if got := shown(); got != c.live {
					t.Fatalf("booted with %s=%s, the console shows %v, want %v", sw.env, c.env, got, c.live)
				}
				// The form's saves: another field changed, both switches sent
				// as the console shows them.
				for i := 1; i <= 2; i++ {
					save(map[string]any{"libraryName": fmt.Sprintf("Saved %d", i),
						"enrichEnabled": shownField(t, b, "enrichEnabled"), "enrichDeezerEnabled": shownField(t, b, "enrichDeezerEnabled")})
					requireFile(fmt.Sprintf("full-form save %d", i), c.file)
					if got := shown(); got != c.live {
						t.Errorf("full-form save %d: the running switch is %v, want the variable's %v", i, got, c.live)
					}
				}
				if got := reload(); got != c.reloaded {
					t.Errorf("after the full-form saves, a start without %s reads %v, want the file's own %v", sw.env, got, c.reloaded)
				}

				// An explicit toggle is the operator's choice: written and run.
				save(map[string]any{sw.field: !c.live})
				want := absent
				if c.live {
					want = off
				}
				requireFile("the switch toggled", want)
				if got := shown(); got != !c.live {
					t.Errorf("the switch toggled: running %v, want %v", got, !c.live)
				}
				if got := reload(); got != !c.live {
					t.Errorf("the switch toggled: a start without %s reads %v, want the toggle's %v", sw.env, got, !c.live)
				}
				save(map[string]any{sw.field: c.live})
				want = off
				if c.live {
					want = absent
				}
				requireFile("the switch toggled back", want)
				if got := shown(); got != c.live {
					t.Errorf("the switch toggled back: running %v, want %v", got, c.live)
				}
			})
		}
	}
}

// shownField is a boolean the console's settings show, as the form sends it.
func shownField(t *testing.T, b *consoleBridge, field string) bool {
	t.Helper()
	var got map[string]any
	getJSON(t, b.console, b.adminBase+"/api/settings", "", &got)
	v, ok := got[field].(bool)
	if !ok {
		t.Fatalf("GET /api/settings carries no %s", field)
	}
	return v
}
