package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// A console save and the BRIDGE_* variables beyond the two opt-outs
// (backlog B761). Serve applies a variable over the file at boot and the
// console shows the value it set; every console save goes through the live
// holder, which writes the file's own value for each leaf a variable set
// that the save left at its live value, so the variable never lands in the
// file, and a start without it finds what the file said.

// topLevelInFile reads key, a top-level key of the bridge.yaml at path, as
// the file holds it: its value and whether the key is there.
func topLevelInFile(t *testing.T, path, key string) (any, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("bridge.yaml does not parse: %v", err)
	}
	v, ok := doc[key]
	return v, ok
}

// postConsoleJSON posts body to the console's route and returns the status
// and the answer.
func postConsoleJSON(t *testing.T, b *consoleBridge, route, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, b.adminBase+route, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.console.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v; stderr=%s", route, err, b.stderr.String())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestAConsoleSaveKeepsTheFilesOwnValueForEachVariable boots serve with
// BRIDGE_SCAN_INTERVAL_SEC and BRIDGE_LIBRARY_NAME set against the file and
// drives two of the console's saves: the variants folder's, which is not
// /api/settings at all, and the settings form's, which sends each field as
// the console shows it. Each writes the file's own value for both leaves,
// the running bridge keeps the variables', and a start without the
// variables finds the file's; a change made in the console is written and
// runs, and the variable sets the leaf again at the next start.
func TestAConsoleSaveKeepsTheFilesOwnValueForEachVariable(t *testing.T) {
	persistentMedium(t)
	vars := map[string]string{"BRIDGE_SCAN_INTERVAL_SEC": "120", "BRIDGE_LIBRARY_NAME": "Env Name"}
	for name, v := range vars {
		t.Setenv(name, v)
	}
	b := startConsoleBridge(t, "scanIntervalSec: 600\nlibraryName: File Name\n", nil)
	cfgPath := filepath.Join(filepath.Dir(b.lib), "bridge.yaml")

	shown := func() map[string]any {
		t.Helper()
		var got map[string]any
		getJSON(t, b.console, b.adminBase+"/api/settings", "", &got)
		return got
	}
	requireShown := func(when string, interval float64, name string) {
		t.Helper()
		got := shown()
		if got["scanIntervalSec"] != interval || got["libraryName"] != name {
			t.Errorf("%s: the console shows scanIntervalSec %v and libraryName %v, want %v and %q",
				when, got["scanIntervalSec"], got["libraryName"], interval, name)
		}
	}
	requireFile := func(when string, interval int, name string) {
		t.Helper()
		if v, ok := topLevelInFile(t, cfgPath, "scanIntervalSec"); !ok || v != interval {
			t.Errorf("%s: the file holds scanIntervalSec %v (there %v), want %d", when, v, ok, interval)
		}
		if v, ok := topLevelInFile(t, cfgPath, "libraryName"); !ok || v != name {
			t.Errorf("%s: the file holds libraryName %v (there %v), want %q", when, v, ok, name)
		}
	}
	// start is a next start's config, with the variables or without.
	start := func(with bool) *config.Config {
		t.Helper()
		if !with {
			for name := range vars {
				t.Setenv(name, "")
				os.Unsetenv(name)
			}
			defer func() {
				for name, v := range vars {
					t.Setenv(name, v)
				}
			}()
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("the saved file does not load: %v", err)
		}
		return cfg
	}

	requireShown("booted", 120, "Env Name")

	// The variants folder's save: another route, another setting.
	variants := filepath.Join(filepath.Dir(b.lib), "variants")
	body, _ := json.Marshal(map[string]string{"path": variants})
	if code, resp := postConsoleJSON(t, b, "/api/upscale/variants-dir", string(body)); code != http.StatusOK {
		t.Fatalf("POST /api/upscale/variants-dir answered %d %s", code, resp)
	}
	requireFile("the variants folder's save", 600, "File Name")
	if got := start(true).Upscale.VariantsDir; got != variants {
		t.Errorf("the variants folder's save wrote upscale.variantsDir %q, want %q", got, variants)
	}
	requireShown("the variants folder's save", 120, "Env Name")

	// The settings form's save: each field as the console shows it, and one
	// changed.
	form, _ := json.Marshal(map[string]any{"scanIntervalSec": 120, "libraryName": "Env Name", "allowDelete": true})
	if code, resp := patchSettings(t, b, string(form)); code != http.StatusOK {
		t.Fatalf("PATCH %s answered %d %s", form, code, resp)
	}
	requireFile("the settings form's save", 600, "File Name")
	requireShown("the settings form's save", 120, "Env Name")
	if cfg := start(true); cfg.ScanIntervalSec != 120 || cfg.LibraryName != "Env Name" {
		t.Errorf("a start with the variables reads %d and %q, want theirs", cfg.ScanIntervalSec, cfg.LibraryName)
	}
	if cfg := start(false); cfg.ScanIntervalSec != 600 || cfg.LibraryName != "File Name" {
		t.Errorf("a start without the variables reads %d and %q, want the file's own 600 and %q",
			cfg.ScanIntervalSec, cfg.LibraryName, "File Name")
	}

	// A change of a leaf a variable sets is the operator's: written and run,
	// until the variable sets it again at the next start.
	if code, resp := patchSettings(t, b, `{"scanIntervalSec": 300}`); code != http.StatusOK {
		t.Fatalf("PATCH scanIntervalSec answered %d %s", code, resp)
	}
	requireFile("the interval changed", 300, "File Name")
	requireShown("the interval changed", 300, "Env Name")
	if got := start(true).ScanIntervalSec; got != 120 {
		t.Errorf("after the change, a start with the variable reads %d, want its 120", got)
	}
	if got := start(false).ScanIntervalSec; got != 300 {
		t.Errorf("after the change, a start without the variable reads %d, want the change's 300", got)
	}
}
