package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// updateTestCfg returns a minimal Save/Load-round-trippable Config
// rooted under dir (mirrors the TestSaveRoundTrip fixture).
func updateTestCfg(t *testing.T, dir string) *Config {
	t.Helper()
	libRoot := filepath.Join(dir, "Music")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Config{
		LibraryRoots:    []string{libRoot},
		ListenAddress:   "127.0.0.1:7788",
		AdminAddress:    "127.0.0.1:7789",
		DataDir:         filepath.Join(dir, "data"),
		ScanIntervalSec: 1800,
		LibraryName:     "Home",
	}
}

// TestRuntimeConfigUpdate pins the read-modify-write contract:
// success publishes to both disk and the live snapshot, an fn
// rejection is returned verbatim and leaves no observable change,
// and a Save failure keeps the pre-Update snapshot live.
func TestRuntimeConfigUpdate(t *testing.T) {
	errFn := errors.New("fn rejected")

	tests := []struct {
		name string
		// mutate is the fn passed to Update; nil means "no-op fn".
		mutate func(*Config) error
		// pathOverride, when set, replaces the default bridge.yaml
		// path (used to force a Save failure).
		pathOverride string
		// holderEmpty constructs the holder with no config loaded.
		holderEmpty bool

		wantErr     error  // non-nil: Update must return an error matching it (errors.Is)
		wantErrText string // substring check for non-sentinel errors
		wantName    string // expected LibraryName in the live snapshot afterwards
		wantSaved   bool   // whether bridge.yaml must exist + match the live snapshot
	}{
		{
			name: "success saves and stores",
			mutate: func(c *Config) error {
				c.LibraryName = "Renamed"
				return nil
			},
			wantName:  "Renamed",
			wantSaved: true,
		},
		{
			name: "fn error returned verbatim, no save, no store",
			mutate: func(c *Config) error {
				c.LibraryName = "Discarded"
				return errFn
			},
			wantErr:  errFn,
			wantName: "Home",
		},
		{
			name:         "save failure keeps live snapshot",
			pathOverride: filepath.Join("no-such-dir", "bridge.yaml"),
			wantErrText:  "temp file",
			wantName:     "Home",
		},
		{
			name:        "empty holder rejected",
			holderEmpty: true,
			wantErrText: "no config loaded",
			wantName:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "bridge.yaml")
			var rc *RuntimeConfig
			if tc.holderEmpty {
				rc = NewRuntimeConfig(nil)
			} else {
				rc = NewRuntimeConfig(updateTestCfg(t, dir))
			}

			path := cfgPath
			if tc.pathOverride != "" {
				path = filepath.Join(dir, tc.pathOverride)
			}
			fn := tc.mutate
			if fn == nil {
				fn = func(*Config) error { return nil }
			}
			err := rc.Update(path, fn)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Update error = %v, want errors.Is %v", err, tc.wantErr)
				}
			} else if tc.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("Update error = %v, want substring %q", err, tc.wantErrText)
				}
			} else if err != nil {
				t.Fatalf("Update: %v", err)
			}

			live := rc.Load()
			if tc.holderEmpty {
				if live != nil {
					t.Errorf("live snapshot = %+v, want nil", live)
				}
				return
			}
			if live.LibraryName != tc.wantName {
				t.Errorf("live LibraryName = %q, want %q", live.LibraryName, tc.wantName)
			}
			_, statErr := os.Stat(cfgPath)
			if !tc.wantSaved {
				if statErr == nil {
					t.Errorf("bridge.yaml exists after failed Update; want no file")
				}
				return
			}
			reloaded, err := Load(cfgPath)
			if err != nil {
				t.Fatalf("Load after Update: %v", err)
			}
			if reloaded.LibraryName != live.LibraryName {
				t.Errorf("disk LibraryName = %q, live = %q — Save/Store diverged",
					reloaded.LibraryName, live.LibraryName)
			}
		})
	}
}

// TestRuntimeConfigUpdateSerializesConcurrentWriters is the 2026-07-21
// review M13 regression test: a settings-style field mutation and a
// UPnP-style slice-append mutation running concurrently must BOTH
// survive — on disk and in the live snapshot. Pre-fix, each writer
// cloned the same base under its own mutex (admin's s.mu vs the UPnP
// adapter's crudMu), so the last Save won and the loser's field was
// silently dropped while both callers returned success.
func TestRuntimeConfigUpdateSerializesConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.yaml")
	rc := NewRuntimeConfig(updateTestCfg(t, dir))

	const n = 16
	var wg sync.WaitGroup
	// Settings-style writer: replaces a scalar field.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("name-%d", i)
			if err := rc.Update(cfgPath, func(c *Config) error {
				c.LibraryName = name
				return nil
			}); err != nil {
				t.Errorf("settings-style Update: %v", err)
				return
			}
		}
	}()
	// UPnP-style writer: appends to a slice field.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			udn := fmt.Sprintf("uuid:srv-%d", i)
			if err := rc.Update(cfgPath, func(c *Config) error {
				c.UPnPUpstream.Servers = append(c.UPnPUpstream.Servers,
					UPnPUpstreamServerConfig{Name: udn, UDN: udn})
				return nil
			}); err != nil {
				t.Errorf("upnp-style Update: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// Only the settings writer touches LibraryName and every clone
	// preserves it, so the final value is deterministic: its last write.
	live := rc.Load()
	if want := fmt.Sprintf("name-%d", n-1); live.LibraryName != want {
		t.Errorf("live LibraryName = %q, want %q (a writer overwrote a stale clone)",
			live.LibraryName, want)
	}
	if len(live.UPnPUpstream.Servers) != n {
		t.Errorf("live servers = %d, want %d (appends lost to a stale clone)",
			len(live.UPnPUpstream.Servers), n)
	}

	reloaded, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load after concurrent Updates: %v", err)
	}
	if reloaded.LibraryName != live.LibraryName {
		t.Errorf("disk LibraryName = %q, live = %q — last Save dropped a concurrent write",
			reloaded.LibraryName, live.LibraryName)
	}
	got := make(map[string]bool, len(reloaded.UPnPUpstream.Servers))
	for _, s := range reloaded.UPnPUpstream.Servers {
		got[s.UDN] = true
	}
	for i := 0; i < n; i++ {
		if udn := fmt.Sprintf("uuid:srv-%d", i); !got[udn] {
			t.Errorf("disk missing server %q — append lost to a stale clone", udn)
		}
	}
}
